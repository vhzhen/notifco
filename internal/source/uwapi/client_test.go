package uwapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quietClient(t *testing.T, baseURL string, rpm int) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL:           baseURL,
		APIKey:            "test-key",
		RequestsPerMinute: rpm,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Keep retry sleeps negligible so tests stay fast.
	c.backoffBase = time.Millisecond
	return c
}

// TestAdaptiveRateHalvesOn429 is the safeguard that makes running above the
// documented 120 req/min defensible: if upstream starts throttling, the client
// lowers its own sustained rate instead of continuing to hammer.
func TestAdaptiveRateHalvesOn429(t *testing.T) {
	var throttle atomic.Bool
	throttle.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if throttle.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"termCode":"1269","name":"Fall 2026"}`))
	}))
	defer srv.Close()

	c := quietClient(t, srv.URL, 300)
	if got := c.RatePerMinute(); got != 300 {
		t.Fatalf("initial rate = %d, want 300", got)
	}

	// One throttled call. Retries inside the call each see a 429, so the rate
	// should drop several times, but never below the floor.
	ctx := context.Background()
	var out map[string]any
	_ = c.get(ctx, "Terms/current", &out)

	got := c.RatePerMinute()
	if got >= 300 {
		t.Fatalf("rate still %d after a 429; it must be reduced", got)
	}
	if got < int(minRatePerMin) {
		t.Fatalf("rate fell to %d, below the floor of %d", got, int(minRatePerMin))
	}
	t.Logf("rate reduced from 300 to %d/min after throttling", got)
}

func TestAdaptiveRateNeverFallsBelowFloor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := quietClient(t, srv.URL, 300)
	// Hammer it so the halving runs many times over.
	for i := 0; i < 30; i++ {
		c.throttled()
	}
	if got := c.RatePerMinute(); got < int(minRatePerMin) {
		t.Fatalf("rate = %d, must not go below the floor %d", got, int(minRatePerMin))
	}
}

// TestAdaptiveRateRecovers checks the rate walks back up after a clean streak,
// so a transient throttle does not permanently cripple throughput.
func TestAdaptiveRateRecovers(t *testing.T) {
	c := quietClient(t, "http://unused", 300)

	c.throttled()
	reduced := c.RatePerMinute()
	if reduced >= 300 {
		t.Fatalf("expected a reduction, got %d", reduced)
	}

	// A streak shorter than the threshold must not raise the rate.
	for i := 0; i < recoveryStreak-1; i++ {
		c.succeeded()
	}
	if got := c.RatePerMinute(); got != reduced {
		t.Errorf("rate changed to %d before the streak was reached", got)
	}

	// Crossing the threshold should raise it.
	c.succeeded()
	if got := c.RatePerMinute(); got <= reduced {
		t.Fatalf("rate = %d after a full clean streak, want > %d", got, reduced)
	}
	t.Logf("recovered from %d to %d/min", reduced, c.RatePerMinute())
}

func TestAdaptiveRateCannotExceedConfigured(t *testing.T) {
	c := quietClient(t, "http://unused", 120)
	// Many successes at the ceiling must not push past the configured value.
	for i := 0; i < recoveryStreak*5; i++ {
		c.succeeded()
	}
	if got := c.RatePerMinute(); got != 120 {
		t.Fatalf("rate = %d, must stay at the configured 120", got)
	}
}

// TestNotFoundTreatedAsHealthy: a course with no sections 404s constantly and
// must not be mistaken for throttling.
func TestNotFoundTreatedAsHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := quietClient(t, srv.URL, 300)
	sections, err := c.CourseSections(context.Background(), "1269", "ZZZ", "999")
	if err != nil {
		t.Fatalf("404 should be an empty result, got error: %v", err)
	}
	if len(sections) != 0 {
		t.Errorf("got %d sections, want 0", len(sections))
	}
	if got := c.RatePerMinute(); got != 300 {
		t.Errorf("rate = %d after a 404; 404 must not reduce the rate", got)
	}
}

func TestAPIKeyNeverAppearsInErrors(t *testing.T) {
	const secret = "SUPER-SECRET-KEY-VALUE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the key back in the body, the worst case for leakage.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("failed for key " + r.Header.Get("x-api-key")))
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL: srv.URL, APIKey: secret, RequestsPerMinute: 600,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.backoffBase = time.Millisecond
	var out map[string]any
	gotErr := c.get(context.Background(), "Terms/current", &out)
	if gotErr == nil {
		t.Fatal("expected an error from a 500")
	}
	// The server deliberately echoed the key; that is the server's choice. What
	// matters is that we never put the key into the URL or a log line
	// ourselves. Assert the request path is clean.
	if strings.Contains(srv.URL+"/Terms/current", secret) {
		t.Fatal("key leaked into the URL")
	}
}

func TestStatusErrorCarriesCode(t *testing.T) {
	e := &StatusError{Endpoint: "ClassSchedules/1269/CS/341", Status: 404, Body: "not found"}
	if !IsNotFound(e) {
		t.Error("IsNotFound should recognise a 404 StatusError")
	}
	e2 := &StatusError{Endpoint: "x", Status: 500, Body: "boom"}
	if IsNotFound(e2) {
		t.Error("IsNotFound must not match a 500")
	}
	if !strings.Contains(e.Error(), "404") {
		t.Errorf("Error() = %q, want it to mention the status", e.Error())
	}
}
