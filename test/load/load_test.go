//go:build load

// Package load measures notifco's fan-out behaviour under the scenario that
// matters: one popular section opens and every watcher must be told, exactly
// once, with no losses.
//
// Run with:
//
//	make load-test
//	go test -tags=load -run TestLoad -v ./test/load/
//
// It is behind a build tag because it writes tens of thousands of rows and
// takes a few seconds, which is not what you want on every `go test ./...`.
package load

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vhzhen/notifco/internal/differ"
	"github.com/vhzhen/notifco/internal/notifier"
	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// countingTransport records deliveries and detects duplicates, which is the
// property the whole idempotency design exists to guarantee.
type countingTransport struct {
	delivered atomic.Int64

	mu   sync.Mutex
	seen map[int64]int // subscription_id -> times delivered
	// latency per delivery, measured detected -> sent.
	latencies []time.Duration
	// simulate per-message Discord cost. Real Discord is ~50 req/s globally;
	// a tiny sleep keeps the test fast while still exercising concurrency.
	perMessage time.Duration
}

func newCountingTransport(perMessage time.Duration) *countingTransport {
	return &countingTransport{seen: make(map[int64]int), perMessage: perMessage}
}

func (c *countingTransport) Deliver(_ context.Context, d store.Delivery) (string, error) {
	if c.perMessage > 0 {
		time.Sleep(c.perMessage)
	}
	c.delivered.Add(1)
	c.mu.Lock()
	c.seen[d.SubscriptionID]++
	c.latencies = append(c.latencies, time.Since(d.DetectedAt))
	c.mu.Unlock()
	return "dm", nil
}

func (c *countingTransport) duplicates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, count := range c.seen {
		if count > 1 {
			n++
		}
	}
	return n
}

func (c *countingTransport) uniqueRecipients() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func (c *countingTransport) percentiles() (p50, p95, p99, max time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.latencies) == 0 {
		return
	}
	s := append([]time.Duration(nil), c.latencies...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration {
		idx := int(q * float64(len(s)-1))
		return s[idx]
	}
	return at(0.50), at(0.95), at(0.99), s[len(s)-1]
}

// TestLoadTenThousandSubscriptions is the headline benchmark.
func TestLoadTenThousandSubscriptions(t *testing.T) {
	const (
		subscribers = 10_000
		classNumber = 90001
		courseCode  = "cs999load"
	)

	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://notifco:notifco@localhost:5432/notifco_test?sslmode=disable"
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/15"
	}

	st, err := store.Open(ctx, dsn, 8)
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable (%v)", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := differ.New(rdb)
	transport := newCountingTransport(0)
	notif := notifier.New(notifier.Options{
		Store:       st,
		Transport:   transport,
		Logger:      log,
		Workers:     16,
		BatchSize:   256,
		MaxAttempts: 6,
		Cooldown:    30 * time.Minute,
	})

	// --- arrange: one full section, 10k watchers ---------------------------

	full := source.SectionState{
		TermCode:    "1269",
		ClassNumber: classNumber,
		CourseCode:  courseCode,
		CourseTitle: "Load Test Course",
		SectionName: "LEC 001",
		Component:   "LEC",
		Enrolled:    100,
		Capacity:    100,
		ObservedAt:  time.Now().UTC(),
	}
	if err := st.UpsertSections(ctx, []source.SectionState{full}); err != nil {
		t.Fatalf("seed section: %v", err)
	}
	var sectionID int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT id FROM sections WHERE term_code = $1 AND class_number = $2`,
		"1269", classNumber).Scan(&sectionID); err != nil {
		t.Fatalf("section id: %v", err)
	}

	setupStart := time.Now()
	// Bulk-insert users and subscriptions. COPY would be faster still, but this
	// is setup cost, not the thing under measurement.
	if _, err := st.Pool().Exec(ctx, `
INSERT INTO users (discord_user_id)
SELECT 'load-user-' || g FROM generate_series(1, $1) g`, subscribers); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `
INSERT INTO subscriptions (user_id, section_id)
SELECT id, $1 FROM users`, sectionID); err != nil {
		t.Fatalf("seed subscriptions: %v", err)
	}
	setupDur := time.Since(setupStart)

	users, subs, err := st.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if subs != subscribers {
		t.Fatalf("seeded %d subscriptions, want %d", subs, subscribers)
	}

	// Seed differ state as full so the next observation is a real transition.
	if _, err := d.Observe(ctx, full); err != nil {
		t.Fatalf("seed differ: %v", err)
	}

	// --- act: a seat opens -------------------------------------------------

	open := full
	open.Enrolled = 99
	open.ObservedAt = time.Now().UTC()

	detectStart := time.Now()
	tr, err := d.Observe(ctx, open)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if tr == nil {
		t.Fatal("expected a transition")
	}
	detectDur := time.Since(detectStart)

	enqueueStart := time.Now()
	if err := notif.Enqueue(ctx, *tr); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	enqueueDur := time.Since(enqueueStart)

	depth, err := st.QueueDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != subscribers {
		t.Fatalf("queued %d notifications, want %d", depth, subscribers)
	}

	drainStart := time.Now()
	delivered, err := notif.DrainConcurrent(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	drainDur := time.Since(drainStart)

	// --- assert: exactly once, nothing lost --------------------------------

	if delivered != subscribers {
		t.Errorf("drained %d, want %d", delivered, subscribers)
	}
	if got := transport.delivered.Load(); got != int64(subscribers) {
		t.Errorf("transport saw %d messages, want %d", got, subscribers)
	}
	if dup := transport.duplicates(); dup != 0 {
		t.Errorf("%d subscriptions received a DUPLICATE message, want 0", dup)
	}
	if uniq := transport.uniqueRecipients(); uniq != subscribers {
		t.Errorf("%d unique recipients, want %d (messages were LOST)", uniq, subscribers)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != subscribers {
		t.Errorf("outbox sent = %d, want %d", stats.Sent, subscribers)
	}
	if stats.Pending != 0 || stats.DeadLetter != 0 {
		t.Errorf("outbox not fully drained: %+v", stats)
	}

	// Re-enqueueing the same transition must add nothing.
	if _, err := st.Enqueue(ctx, sectionID, tr.ID, 1, tr.DetectedAt); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountByTransition(ctx, tr.ID); err != nil {
		t.Fatal(err)
	} else if n != subscribers {
		t.Errorf("after replay there are %d rows, want %d (dedup failed)", n, subscribers)
	}

	// --- report ------------------------------------------------------------

	p50, p95, p99, max := transport.percentiles()
	rate := float64(subscribers) / drainDur.Seconds()

	t.Logf("")
	t.Logf("=== LOAD TEST RESULTS: 10k subscriptions, one section opening ===")
	t.Logf("  subscribers            %d", subscribers)
	t.Logf("  active users           %d", users)
	t.Logf("  setup (seed rows)      %s", setupDur.Round(time.Millisecond))
	t.Logf("  detect (Redis CAS)     %s", detectDur.Round(time.Microsecond))
	t.Logf("  fan-out enqueue        %s  (one INSERT..SELECT for all %d)", enqueueDur.Round(time.Millisecond), subscribers)
	t.Logf("  drain (16 workers)     %s", drainDur.Round(time.Millisecond))
	t.Logf("  throughput             %.0f messages/sec", rate)
	t.Logf("  latency p50            %s", p50.Round(time.Millisecond))
	t.Logf("  latency p95            %s", p95.Round(time.Millisecond))
	t.Logf("  latency p99            %s", p99.Round(time.Millisecond))
	t.Logf("  latency max            %s", max.Round(time.Millisecond))
	t.Logf("  duplicates             %d", transport.duplicates())
	t.Logf("  lost                   %d", subscribers-transport.uniqueRecipients())
	t.Logf("")
	t.Logf("  NOTE: latency is detected -> sent with an instant transport. It")
	t.Logf("  excludes real Discord API time, which dominates in production:")
	t.Logf("  Discord's global budget is ~50 req/s, so 10k real DMs is minutes")
	t.Logf("  of wall clock regardless of how fast this pipeline is.")
	t.Logf("")

	// Write a machine-readable summary for the README table.
	if out := os.Getenv("LOAD_RESULTS_FILE"); out != "" {
		f, err := os.Create(out)
		if err == nil {
			defer f.Close()
			fmt.Fprintf(f, "subscribers=%d\n", subscribers)
			fmt.Fprintf(f, "detect=%s\n", detectDur.Round(time.Microsecond))
			fmt.Fprintf(f, "enqueue=%s\n", enqueueDur.Round(time.Millisecond))
			fmt.Fprintf(f, "drain=%s\n", drainDur.Round(time.Millisecond))
			fmt.Fprintf(f, "throughput=%.0f\n", rate)
			fmt.Fprintf(f, "p50=%s\np95=%s\np99=%s\nmax=%s\n",
				p50.Round(time.Millisecond), p95.Round(time.Millisecond),
				p99.Round(time.Millisecond), max.Round(time.Millisecond))
			fmt.Fprintf(f, "duplicates=%d\nlost=%d\n",
				transport.duplicates(), subscribers-transport.uniqueRecipients())
		}
	}
}

// TestLoadThroughputWithRealisticDiscordCost models Discord's actual rate
// ceiling so the README can state an honest end-to-end figure.
func TestLoadThroughputWithRealisticDiscordCost(t *testing.T) {
	const subscribers = 1_000

	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://notifco:notifco@localhost:5432/notifco_test?sslmode=disable"
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/15"
	}

	st, err := store.Open(ctx, dsn, 8)
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatal(err)
	}

	opt, _ := redis.ParseURL(redisURL)
	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable (%v)", err)
	}
	_ = rdb.FlushDB(ctx).Err()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := differ.New(rdb)
	// 20ms per message ~= 50 messages/sec, Discord's rough global ceiling.
	transport := newCountingTransport(20 * time.Millisecond)
	notif := notifier.New(notifier.Options{
		Store: st, Transport: transport, Logger: log,
		Workers: 8, BatchSize: 128, MaxAttempts: 6,
	})

	full := source.SectionState{
		TermCode: "1269", ClassNumber: 90002, CourseCode: "cs998load",
		SectionName: "LEC 001", Component: "LEC",
		Enrolled: 50, Capacity: 50, ObservedAt: time.Now().UTC(),
	}
	if err := st.UpsertSections(ctx, []source.SectionState{full}); err != nil {
		t.Fatal(err)
	}
	var sectionID int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT id FROM sections WHERE class_number = $1`, 90002).Scan(&sectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `
INSERT INTO users (discord_user_id)
SELECT 'rl-user-' || g FROM generate_series(1, $1) g`, subscribers); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `
INSERT INTO subscriptions (user_id, section_id) SELECT id, $1 FROM users`, sectionID); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Observe(ctx, full); err != nil {
		t.Fatal(err)
	}
	open := full
	open.Enrolled = 49
	open.ObservedAt = time.Now().UTC()
	tr, err := d.Observe(ctx, open)
	if err != nil || tr == nil {
		t.Fatalf("observe: tr=%v err=%v", tr, err)
	}
	if err := notif.Enqueue(ctx, *tr); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	delivered, err := notif.DrainConcurrent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dur := time.Since(start)

	if delivered != subscribers {
		t.Errorf("delivered %d, want %d", delivered, subscribers)
	}
	if dup := transport.duplicates(); dup != 0 {
		t.Errorf("%d duplicates, want 0", dup)
	}

	p50, p95, p99, max := transport.percentiles()
	t.Logf("")
	t.Logf("=== THROTTLED FAN-OUT: %d subscribers, 20ms/message (~Discord's ceiling) ===", subscribers)
	t.Logf("  drain            %s", dur.Round(time.Millisecond))
	t.Logf("  throughput       %.0f messages/sec", float64(subscribers)/dur.Seconds())
	t.Logf("  latency p50      %s", p50.Round(time.Millisecond))
	t.Logf("  latency p95      %s", p95.Round(time.Millisecond))
	t.Logf("  latency p99      %s", p99.Round(time.Millisecond))
	t.Logf("  latency max      %s", max.Round(time.Millisecond))
	t.Logf("  extrapolated to 10k: ~%s", (dur * 10).Round(time.Second))
	t.Logf("")
}
