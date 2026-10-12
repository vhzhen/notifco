package schedpage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vhzhen/notifco/internal/source"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSite serves captured pages and counts what it is asked.
type fakeSite struct {
	srv      *httptest.Server
	hits     atomic.Int64
	mu       sync.Mutex
	queries  []string
	respond  func(w http.ResponseWriter, r *http.Request)
	delay    time.Duration
	failWith int
}

func newSite(t *testing.T) *fakeSite {
	t.Helper()
	f := &fakeSite{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		f.mu.Unlock()
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			return
		}
		if f.respond != nil {
			f.respond(w, r)
			return
		}
		// Default: serve the fixture matching the subject+course.
		q := r.URL.Query()
		name := q.Get("subject") + q.Get("cournum")
		if q.Get("level") == "grad" {
			name = "grad_" + name
		}
		b, err := os.ReadFile("testdata/" + name + ".html")
		if err != nil {
			b, _ = os.ReadFile("testdata/empty_ZZZ999.html")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSite) client(t *testing.T, mod func(*Options)) *Client {
	t.Helper()
	o := Options{BaseURL: f.srv.URL, Logger: quiet(), RatePerSecond: 1000}
	if mod != nil {
		mod(&o)
	}
	return New(o)
}

func ctx5() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// --- the basics -------------------------------------------------------------------

func TestReservesFetchesAndReturnsNotes(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	got, err := c.Reserves(ctx, "1269", "cs", "135")
	if err != nil {
		t.Fatal(err)
	}
	if n := got[6112]; len(n) != 2 || n[0] != "1A AFM students" {
		t.Errorf("class 6112 = %q", n)
	}
}

// TestOneRequestPerCourse: the promise that a whole course costs one request,
// however many sections it has.
func TestOneRequestPerCourse(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	if _, err := c.Reserves(ctx, "1269", "PSYCH", "101"); err != nil {
		t.Fatal(err)
	}
	if n := site.hits.Load(); n != 1 {
		t.Errorf("a 6-section course took %d requests, want 1", n)
	}
}

func TestRequestCarriesAnHonestUserAgentAndNoCredentials(t *testing.T) {
	site := newSite(t)
	var ua, auth, cookie string
	site.respond = func(w http.ResponseWriter, r *http.Request) {
		ua, auth, cookie = r.Header.Get("User-Agent"), r.Header.Get("Authorization"), r.Header.Get("Cookie")
		_, _ = w.Write([]byte("<html></html>"))
	}
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	_, _ = c.Reserves(ctx, "1269", "CS", "135")

	if !strings.Contains(ua, "notifco") || !strings.Contains(ua, "github.com/vhzhen/notifco") {
		t.Errorf("User-Agent %q does not identify the project", ua)
	}
	if auth != "" || cookie != "" {
		t.Errorf("sent credentials: Authorization=%q Cookie=%q", auth, cookie)
	}
}

// --- caching -----------------------------------------------------------------------

func TestSuccessIsCached(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	for i := 0; i < 20; i++ {
		if _, err := c.Reserves(ctx, "1269", "CS", "135"); err != nil {
			t.Fatal(err)
		}
	}
	if n := site.hits.Load(); n != 1 {
		t.Errorf("20 lookups made %d requests, want 1", n)
	}
}

func TestCacheExpiresAfterTheTTL(t *testing.T) {
	site := newSite(t)
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := site.client(t, func(o *Options) { o.Now = clock; o.TTL = time.Hour })
	ctx, cancel := ctx5()
	defer cancel()

	_, _ = c.Reserves(ctx, "1269", "CS", "135")
	mu.Lock()
	now = now.Add(59 * time.Minute)
	mu.Unlock()
	_, _ = c.Reserves(ctx, "1269", "CS", "135")
	if n := site.hits.Load(); n != 1 {
		t.Fatalf("refetched before the TTL: %d requests", n)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	_, _ = c.Reserves(ctx, "1269", "CS", "135")
	if n := site.hits.Load(); n != 2 {
		t.Errorf("did not refetch after the TTL: %d requests", n)
	}
}

// TestFailuresAreCachedBriefly: a site that is down must not be hit again on
// every single /search.
func TestFailuresAreCachedBriefly(t *testing.T) {
	site := newSite(t)
	site.failWith = http.StatusInternalServerError
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	for i := 0; i < 15; i++ {
		if _, err := c.Reserves(ctx, "1269", "CS", "135"); err == nil {
			t.Fatal("expected an error from a 500")
		}
	}
	if n := site.hits.Load(); n != 1 {
		t.Errorf("15 lookups against a failing site made %d requests, want 1", n)
	}
}

func TestFailureExpiresSoRecoveryIsNoticed(t *testing.T) {
	site := newSite(t)
	site.failWith = http.StatusBadGateway
	now := time.Now()
	var mu sync.Mutex
	c := site.client(t, func(o *Options) {
		o.FailTTL = 5 * time.Minute
		o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	ctx, cancel := ctx5()
	defer cancel()

	if _, err := c.Reserves(ctx, "1269", "CS", "135"); err == nil {
		t.Fatal("expected failure")
	}
	site.failWith = 0
	mu.Lock()
	now = now.Add(6 * time.Minute)
	mu.Unlock()
	got, err := c.Reserves(ctx, "1269", "CS", "135")
	if err != nil || len(got) == 0 {
		t.Errorf("did not recover after the failure TTL: (%v, %v)", got, err)
	}
}

func TestDifferentCoursesAreCachedSeparately(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	_, _ = c.Reserves(ctx, "1269", "CS", "135")
	_, _ = c.Reserves(ctx, "1269", "AFM", "101")
	_, _ = c.Reserves(ctx, "1271", "CS", "135") // a different term
	if n := site.hits.Load(); n != 3 {
		t.Errorf("%d requests, want 3 (distinct term+course keys)", n)
	}
}

func TestCacheIsBounded(t *testing.T) {
	site := newSite(t)
	site.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html></html>")) }
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	for i := 0; i < maxCacheEntries+300; i++ {
		_, _ = c.Reserves(ctx, "1269", "CS", pad3(100+i%900)+string(rune('A'+i/900)))
	}
	if n := c.CacheSize(); n > maxCacheEntries {
		t.Errorf("cache grew to %d entries, bound is %d", n, maxCacheEntries)
	}
}

// pad3 formats n as a three-digit catalog number.
func pad3(n int) string { return fmt.Sprintf("%03d", n%1000) }

// --- coalescing and load shedding ---------------------------------------------------

// TestConcurrentLookupsShareOneRequest: a burst of people searching the same
// course must cost UW one request, not one each.
func TestConcurrentLookupsShareOneRequest(t *testing.T) {
	site := newSite(t)
	site.delay = 150 * time.Millisecond
	c := site.client(t, nil)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := ctx5()
			defer cancel()
			if _, err := c.Reserves(ctx, "1269", "MATH", "135"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := site.hits.Load(); n != 1 {
		t.Errorf("40 concurrent lookups made %d upstream requests, want 1", n)
	}
}

// TestLoadIsShedWhenTooManyAreInFlight: a flood of distinct courses must be
// refused fast instead of queueing up behind the rate limiter.
func TestLoadIsShedWhenTooManyAreInFlight(t *testing.T) {
	site := newSite(t)
	release := make(chan struct{})
	site.respond = func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("<html></html>"))
	}
	c := site.client(t, nil)

	var busy, other atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < maxInflight+10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			_, err := c.Reserves(ctx, "1269", "CS", pad3(100+i))
			switch {
			case errors.Is(err, ErrBusy):
				busy.Add(1)
			case err != nil:
				other.Add(1)
			}
		}(i)
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()

	if busy.Load() < 10 {
		t.Errorf("only %d lookups were shed with %d in flight (cap %d)", busy.Load(), maxInflight+10, maxInflight)
	}
	if got := site.hits.Load(); got > int64(maxInflight) {
		t.Errorf("%d requests reached the site, more than the in-flight cap %d", got, maxInflight)
	}
}

// TestRateLimitIsEnforced: the shared ceiling on upstream requests.
func TestRateLimitIsEnforced(t *testing.T) {
	site := newSite(t)
	site.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html></html>")) }
	c := site.client(t, func(o *Options) { o.RatePerSecond = 5 })

	start := time.Now()
	for i := 0; i < 8; i++ {
		ctx, cancel := ctx5()
		_, _ = c.Reserves(ctx, "1269", "CS", pad3(100+i))
		cancel()
	}
	elapsed := time.Since(start)
	// 8 requests at 5/s with a burst of 2: the last six need ~1.2 s.
	if elapsed < 900*time.Millisecond {
		t.Errorf("8 requests finished in %s; the 5/s limiter is not holding", elapsed)
	}
}

// --- caller timeouts ----------------------------------------------------------------

// TestFetchSurvivesTheCallerGivingUp: a slow first /search must still warm the
// cache, so the next one is instant.
func TestFetchSurvivesTheCallerGivingUp(t *testing.T) {
	site := newSite(t)
	site.delay = 250 * time.Millisecond
	c := site.client(t, nil)

	short, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	_, err := c.Reserves(short, "1269", "CS", "135")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call: %v, want a deadline error", err)
	}

	time.Sleep(500 * time.Millisecond) // let the detached fetch finish
	ctx, cancel2 := ctx5()
	defer cancel2()
	got, err := c.Reserves(ctx, "1269", "CS", "135")
	if err != nil || len(got) == 0 {
		t.Fatalf("second call: (%v, %v), want the cached result", len(got), err)
	}
	if n := site.hits.Load(); n != 1 {
		t.Errorf("%d requests: the abandoned fetch was not reused", n)
	}
}

// --- input validation ----------------------------------------------------------------

// TestInvalidInputNeverReachesTheNetwork: nothing is sent upstream for input
// that could not be a course code, whatever it contains.
func TestInvalidInputNeverReachesTheNetwork(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	bad := [][3]string{
		{"1269", "CS&sess=1", "135"}, {"1269", "CS", "135;rm"}, {"1269", "CS", "13"}, {"1269", "CS", "1355"},
		{"1269", "", "135"}, {"1269", "CS", ""}, {"12", "CS", "135"}, {"abcd", "CS", "135"},
		{"1269", "../etc", "135"}, {"1269", "CS\n", "13\n5"}, {"1269", "C S", "135"},
		{"1269", strings.Repeat("A", 40), "135"}, {"1269", "CS", "135\x00"}, {"1269", "СS", "135"}, // Cyrillic С
	}
	for _, b := range bad {
		if _, err := c.Reserves(ctx, source.TermCode(b[0]), b[1], b[2]); !errors.Is(err, ErrInvalidCourse) {
			t.Errorf("%q: error = %v, want ErrInvalidCourse", b, err)
		}
	}
	if n := site.hits.Load(); n != 0 {
		t.Errorf("%d invalid lookups reached the network", n)
	}
}

func TestValidInputIsNormalised(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	if _, err := c.Reserves(ctx, "1269", " cs ", " 135 "); err != nil {
		t.Fatalf("lowercase/padded input rejected: %v", err)
	}
	site.mu.Lock()
	defer site.mu.Unlock()
	if len(site.queries) != 1 || !strings.Contains(site.queries[0], "subject=CS") || !strings.Contains(site.queries[0], "cournum=135") {
		t.Errorf("query = %v, want normalised subject and number", site.queries)
	}
}

func TestLetterSuffixCatalogNumbersAreAccepted(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	if _, err := c.Reserves(ctx, "1269", "MATH", "237L"); err != nil {
		t.Errorf("a real catalog number with a letter suffix was rejected: %v", err)
	}
}

// --- response handling -----------------------------------------------------------------

func TestNon200IsAnError(t *testing.T) {
	for _, code := range []int{301 + 100, 403, 404, 429, 500, 503} {
		site := newSite(t)
		site.failWith = code
		c := site.client(t, nil)
		ctx, cancel := ctx5()
		_, err := c.Reserves(ctx, "1269", "CS", "135")
		cancel()
		if err == nil {
			t.Errorf("status %d treated as success", code)
		}
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	site := newSite(t)
	site.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("<td>x</td>", (maxBodyBytes/9)+100)))
	}
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	_, err := c.Reserves(ctx, "1269", "CS", "135")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized body: err = %v, want a refusal rather than a truncated parse", err)
	}
}

// TestRedirectOffHostIsRefused: the one way a request this client builds could
// end up somewhere it did not choose.
func TestRedirectOffHostIsRefused(t *testing.T) {
	var elsewhereHits atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHits.Add(1)
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer elsewhere.Close()

	site := newSite(t)
	site.respond = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	}
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	if _, err := c.Reserves(ctx, "1269", "CS", "135"); err == nil {
		t.Error("an off-host redirect was followed")
	}
	if n := elsewhereHits.Load(); n != 0 {
		t.Errorf("the redirect target received %d requests", n)
	}
}

func TestRedirectLoopTerminates(t *testing.T) {
	site := newSite(t)
	site.respond = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.String(), http.StatusFound) }
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	if _, err := c.Reserves(ctx, "1269", "CS", "135"); err == nil {
		t.Error("a redirect loop did not fail")
	}
}

// --- levels ---------------------------------------------------------------------------------

func TestLevelsFor(t *testing.T) {
	for _, tc := range []struct {
		cat  string
		want []string
	}{
		{"135", []string{"under"}}, {"499", []string{"under"}}, {"237L", []string{"under"}},
		{"500", []string{"grad", "under"}}, {"686", []string{"grad", "under"}}, {"640A", []string{"grad", "under"}},
	} {
		got := levelsFor(tc.cat)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("levelsFor(%s) = %v, want %v", tc.cat, got, tc.want)
		}
	}
}

// TestGraduateCourseUsesTheGraduatePage: CS 686 exists only on the graduate
// page, and an undergraduate-only lookup would have found nothing.
func TestGraduateCourseUsesTheGraduatePage(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()

	got, err := c.Reserves(ctx, "1269", "CS", "686")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Error("no notes found for a graduate course that has them")
	}
	site.mu.Lock()
	defer site.mu.Unlock()
	if len(site.queries) != 1 || !strings.Contains(site.queries[0], "level=grad") {
		t.Errorf("queries %v: the graduate page should be tried first and suffice", site.queries)
	}
}

func TestFallsBackToUndergradWhenGradHasNothing(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	// No grad fixture for this one, so the grad lookup is empty and it falls back.
	if _, err := c.Reserves(ctx, "1269", "ZZZ", "999"); err != nil {
		t.Fatal(err)
	}
	if n := site.hits.Load(); n != 2 {
		t.Errorf("a 500-level miss should try grad then under; got %d requests", n)
	}
}

func TestUndergraduateCourseNeverAsksTheGraduatePage(t *testing.T) {
	site := newSite(t)
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	_, _ = c.Reserves(ctx, "1269", "CS", "135")
	site.mu.Lock()
	defer site.mu.Unlock()
	for _, q := range site.queries {
		if strings.Contains(q, "level=grad") {
			t.Errorf("an undergraduate course queried the graduate page: %s", q)
		}
	}
}

// --- layout drift through the client ------------------------------------------------------

func TestLayoutDriftSurfacesAsAnErrorNotSilence(t *testing.T) {
	site := newSite(t)
	site.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><p>Reserve: Year 1 Math Students</p></body></html>`))
	}
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	if _, err := c.Reserves(ctx, "1269", "AFM", "101"); !errors.Is(err, ErrLayoutChanged) {
		t.Errorf("err = %v, want ErrLayoutChanged so the UI can say notes are unavailable", err)
	}
}

func TestACourseWithNoReservesIsEmptyNotAnError(t *testing.T) {
	site := newSite(t)
	site.respond = func(w http.ResponseWriter, _ *http.Request) {
		b, _ := os.ReadFile("testdata/CS341.html")
		// strip the one reserve row so the page is a real course with none
		s := strings.ReplaceAll(string(b), "Reserve:", "Note:")
		_, _ = w.Write([]byte(s))
	}
	c := site.client(t, nil)
	ctx, cancel := ctx5()
	defer cancel()
	got, err := c.Reserves(ctx, "1269", "CS", "341")
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("(%v, %v), want an empty non-nil map and no error", got, err)
	}
}
