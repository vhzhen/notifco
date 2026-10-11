// Package integration exercises the full pipeline: mock upstream -> poller ->
// differ -> notifier -> transport, against real Postgres and Redis.
//
// These are the tests that would have caught a seat opening being announced
// twice, or not at all.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vhzhen/notifco/internal/differ"
	"github.com/vhzhen/notifco/internal/notifier"
	"github.com/vhzhen/notifco/internal/poller"
	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/source/mock"
	"github.com/vhzhen/notifco/internal/store"
)

// --- harness ----------------------------------------------------------------

type harness struct {
	t      *testing.T
	store  *store.Store
	redis  *redis.Client
	differ *differ.Differ
	src    *mock.Source
	notif  *notifier.Notifier
	pol    *poller.Poller
	fake   *fakeTransport
	term   source.TermCode
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://notifco:notifco@localhost:5432/notifco_test?sslmode=disable"
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/15"
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v); skipping integration test", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		st.Close()
		t.Skipf("redis unavailable (%v); skipping integration test", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}

	src, err := mock.New()
	if err != nil {
		t.Fatalf("mock source: %v", err)
	}

	// Discard logs so test output stays readable.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := differ.New(rdb)
	fake := &fakeTransport{}
	notif := notifier.New(notifier.Options{
		Store:       st,
		Transport:   fake,
		Logger:      log,
		Workers:     4,
		MaxAttempts: 3,
		Cooldown:    30 * time.Minute,
	})
	pol := poller.New(poller.Options{
		Source:   src,
		Store:    st,
		Differ:   d,
		Enqueuer: notif,
		Logger:   log,
	})

	h := &harness{
		t: t, store: st, redis: rdb, differ: d, src: src,
		notif: notif, pol: pol, fake: fake, term: "1269",
	}
	t.Cleanup(func() {
		st.Close()
		_ = rdb.Close()
	})
	return h
}

// fakeTransport records deliveries and can simulate Discord failures.
type fakeTransport struct {
	mu        sync.Mutex
	delivered []store.Delivery

	// failures maps a user id to a number of consecutive failures to inject.
	failures map[string]int
	// dmsClosed marks users whose DMs are closed, forcing channel fallback.
	dmsClosed map[string]bool
	// permanent marks users that should dead-letter immediately.
	permanent map[string]bool
}

func (f *fakeTransport) Deliver(_ context.Context, d store.Delivery) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.permanent[d.DiscordUserID] {
		return "", &notifier.Permanent{Err: fmt.Errorf("unknown user %s", d.DiscordUserID)}
	}
	if n := f.failures[d.DiscordUserID]; n > 0 {
		f.failures[d.DiscordUserID] = n - 1
		return "", fmt.Errorf("transient discord error for %s", d.DiscordUserID)
	}
	via := "dm"
	if f.dmsClosed[d.DiscordUserID] {
		if d.FallbackChannelID == "" {
			return "", &notifier.Permanent{Err: fmt.Errorf("dms closed, no fallback")}
		}
		via = "channel"
	}
	f.delivered = append(f.delivered, d)
	return via, nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.delivered)
}

func (f *fakeTransport) forUser(id string) []store.Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Delivery
	for _, d := range f.delivered {
		if d.DiscordUserID == id {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeTransport) failUser(id string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = map[string]int{}
	}
	f.failures[id] = times
}

func (f *fakeTransport) closeDMs(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dmsClosed == nil {
		f.dmsClosed = map[string]bool{}
	}
	f.dmsClosed[id] = true
}

func (f *fakeTransport) markPermanent(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.permanent == nil {
		f.permanent = map[string]bool{}
	}
	f.permanent[id] = true
}

// --- fixtures ---------------------------------------------------------------

// sections builds a one-section course at the given enrollment.
func sections(code string, class, enrolled, capacity int) []source.SectionState {
	return []source.SectionState{{
		TermCode:    "1269",
		ClassNumber: class,
		CourseCode:  code,
		CourseTitle: "Test Course",
		SectionName: "LEC 001",
		Component:   "LEC",
		Enrolled:    enrolled,
		Capacity:    capacity,
		ObservedAt:  time.Now().UTC(),
	}}
}

// subscribe registers a user watching a section, returning the user row id.
func (h *harness) subscribe(discordID string, sectionID int64, fallbackChannel string) int64 {
	h.t.Helper()
	ctx := context.Background()
	uid, err := h.store.UpsertUser(ctx, discordID)
	if err != nil {
		h.t.Fatalf("upsert user: %v", err)
	}
	if fallbackChannel != "" {
		if err := h.store.SetFallbackChannel(ctx, uid, "guild1", fallbackChannel); err != nil {
			h.t.Fatalf("set fallback: %v", err)
		}
	}
	if _, err := h.store.Subscribe(ctx, uid, sectionID); err != nil {
		h.t.Fatalf("subscribe: %v", err)
	}
	return uid
}

// sectionID looks up the stored section row id.
func (h *harness) sectionID(class int) int64 {
	h.t.Helper()
	var id int64
	err := h.store.Pool().QueryRow(context.Background(),
		`SELECT id FROM sections WHERE term_code = $1 AND class_number = $2`,
		string(h.term), class).Scan(&id)
	if err != nil {
		h.t.Fatalf("section id for class %d: %v", class, err)
	}
	return id
}

// --- tests ------------------------------------------------------------------

// TestFullToOpenDeliversExactlyOnce is the core end-to-end path.
func TestFullToOpenDeliversExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The course is full, then a seat opens, then stays open.
	h.src.Script("cs999",
		sections("cs999", 7001, 100, 100), // full
		sections("cs999", 7001, 99, 100),  // seat opens
		sections("cs999", 7001, 99, 100),  // still open
		sections("cs999", 7001, 99, 100),
	)

	// Seed the section so it can be subscribed to.
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs999"); err != nil {
		t.Fatalf("ensure course: %v", err)
	}
	secID := h.sectionID(7001)
	h.subscribe("user-A", secID, "chan-A")

	// Poll repeatedly: full -> open -> open -> open.
	for i := 0; i < 4; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if _, err := h.notif.DrainOnce(ctx); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
	}

	got := h.fake.forUser("user-A")
	if len(got) != 1 {
		t.Fatalf("user-A received %d messages, want exactly 1 "+
			"(full->open is one edge; subsequent open->open must not re-fire)", len(got))
	}
	if got[0].CourseCode != "cs999" || got[0].SectionName != "LEC 001" {
		t.Errorf("wrong content: %+v", got[0])
	}
	if got[0].SeatsOpen != 1 {
		t.Errorf("SeatsOpen = %d, want 1", got[0].SeatsOpen)
	}

	stats, err := h.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 || stats.Pending != 0 || stats.DeadLetter != 0 {
		t.Errorf("outbox = %+v, want exactly 1 sent", stats)
	}
}

// TestAlreadyOpenNeverNotifies guards the cold-start case: subscribing to an
// open section must not immediately alert.
func TestAlreadyOpenNeverNotifies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs998", sections("cs998", 7002, 50, 100)) // open throughout

	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs998"); err != nil {
		t.Fatalf("ensure course: %v", err)
	}
	h.subscribe("user-B", h.sectionID(7002), "chan-B")

	for i := 0; i < 3; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := h.notif.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if n := h.fake.count(); n != 0 {
		t.Fatalf("delivered %d messages for an always-open section, want 0", n)
	}
}

// TestFanOutToManyWatchers checks every subscriber gets exactly one message.
func TestFanOutToManyWatchers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs997",
		sections("cs997", 7003, 100, 100),
		sections("cs997", 7003, 95, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs997"); err != nil {
		t.Fatal(err)
	}
	secID := h.sectionID(7003)

	const watchers = 50
	for i := 0; i < watchers; i++ {
		h.subscribe(fmt.Sprintf("fan-%d", i), secID, "chan-fan")
	}

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if n := h.fake.count(); n != watchers {
		t.Fatalf("delivered %d, want %d (one per watcher)", n, watchers)
	}
	for i := 0; i < watchers; i++ {
		id := fmt.Sprintf("fan-%d", i)
		if got := len(h.fake.forUser(id)); got != 1 {
			t.Errorf("%s got %d messages, want 1", id, got)
		}
	}
}

// TestRetryThenSucceed proves transient Discord failures are retried.
func TestRetryThenSucceed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs996",
		sections("cs996", 7004, 100, 100),
		sections("cs996", 7004, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs996"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-retry", h.sectionID(7004), "chan-retry")

	// Fail the first two attempts.
	h.fake.failUser("user-retry", 2)

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// First drain fails and schedules a retry.
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.count(); n != 0 {
		t.Fatalf("expected no delivery yet, got %d", n)
	}

	// Backoff is real time, so force the row due and drain again, twice.
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := h.store.Pool().Exec(ctx,
			`UPDATE notifications SET next_attempt_at = NOW() WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.notif.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(h.fake.forUser("user-retry")); got != 1 {
		t.Fatalf("after retries, delivered %d, want 1", got)
	}
	stats, err := h.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 {
		t.Errorf("stats = %+v, want 1 sent", stats)
	}
}

// TestDeadLetterAfterMaxAttempts proves retries are bounded.
func TestDeadLetterAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs995",
		sections("cs995", 7005, 100, 100),
		sections("cs995", 7005, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs995"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-dead", h.sectionID(7005), "chan-dead")

	// Always fail.
	h.fake.failUser("user-dead", 1000)

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// MaxAttempts is 3 in the harness; drain more than that.
	for i := 0; i < 6; i++ {
		if _, err := h.store.Pool().Exec(ctx,
			`UPDATE notifications SET next_attempt_at = NOW() WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.notif.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := h.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLetter != 1 {
		t.Fatalf("stats = %+v, want 1 dead_letter", stats)
	}
	if stats.Pending != 0 {
		t.Errorf("row should not still be pending: %+v", stats)
	}
}

// TestDMClosedFallsBackToChannel covers the fallback path.
func TestDMClosedFallsBackToChannel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs994",
		sections("cs994", 7006, 100, 100),
		sections("cs994", 7006, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs994"); err != nil {
		t.Fatal(err)
	}
	secID := h.sectionID(7006)
	h.subscribe("user-nodm", secID, "chan-nodm")
	h.fake.closeDMs("user-nodm")

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got := h.fake.forUser("user-nodm")
	if len(got) != 1 {
		t.Fatalf("delivered %d, want 1", len(got))
	}

	var via string
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT delivered_via FROM notifications WHERE status = 'sent'`).Scan(&via); err != nil {
		t.Fatal(err)
	}
	if via != "channel" {
		t.Errorf("delivered_via = %q, want \"channel\"", via)
	}
}

// TestDMClosedNoFallbackDeadLetters: nowhere to send means dead-letter, not an
// infinite retry loop.
func TestDMClosedNoFallbackDeadLetters(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs993",
		sections("cs993", 7007, 100, 100),
		sections("cs993", 7007, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs993"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-void", h.sectionID(7007), "") // no fallback channel
	h.fake.closeDMs("user-void")

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}

	stats, err := h.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLetter != 1 {
		t.Fatalf("stats = %+v, want 1 dead_letter", stats)
	}
}

// TestPermanentErrorDeadLettersImmediately: no retry budget wasted on an
// unknown recipient.
func TestPermanentErrorDeadLettersImmediately(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs992",
		sections("cs992", 7008, 100, 100),
		sections("cs992", 7008, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs992"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-gone", h.sectionID(7008), "chan-gone")
	h.fake.markPermanent("user-gone")

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var attempts int
	var status string
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT attempts, status::text FROM notifications`).Scan(&attempts, &status); err != nil {
		t.Fatal(err)
	}
	if status != "dead_letter" {
		t.Fatalf("status = %q, want dead_letter", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (permanent errors must not retry)", attempts)
	}
}

// TestCooldownSuppressesFlapping: a section bouncing full/open must not spam.
func TestCooldownSuppressesFlapping(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// full -> open -> full -> open: two genuine transitions.
	h.src.Script("cs991",
		sections("cs991", 7009, 100, 100),
		sections("cs991", 7009, 99, 100),
		sections("cs991", 7009, 100, 100),
		sections("cs991", 7009, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs991"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-flap", h.sectionID(7009), "chan-flap")

	for i := 0; i < 4; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := h.notif.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Two transitions fired, but the 30-minute cooldown means only the first
	// is delivered.
	if got := len(h.fake.forUser("user-flap")); got != 1 {
		t.Fatalf("delivered %d messages for a flapping section, want 1 "+
			"(cooldown should suppress the second)", got)
	}
}

// TestIdempotentAcrossRestart: re-enqueuing the same transition is a no-op,
// which is what makes the pipeline safe to crash.
func TestIdempotentAcrossRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs990",
		sections("cs990", 7010, 100, 100),
		sections("cs990", 7010, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs990"); err != nil {
		t.Fatal(err)
	}
	secID := h.sectionID(7010)
	h.subscribe("user-idem", secID, "chan-idem")

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	transitionID := differ.TransitionID(source.SectionKey{TermCode: h.term, ClassNumber: 7010}, 1)
	before, err := h.store.CountByTransition(ctx, transitionID)
	if err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("expected 1 row for %s, got %d", transitionID, before)
	}

	// Simulate a crash/replay: enqueue the very same transition again.
	for i := 0; i < 5; i++ {
		if _, err := h.store.Enqueue(ctx, secID, transitionID, 1, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	after, err := h.store.CountByTransition(ctx, transitionID)
	if err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Fatalf("after 5 replays there are %d rows for %s, want 1 "+
			"(UNIQUE(subscription_id, transition_id) should absorb these)", after, transitionID)
	}

	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.fake.forUser("user-idem")); got != 1 {
		t.Fatalf("delivered %d, want 1", got)
	}
}

// TestReclaimStaleLease: a process dying mid-send must not lose the message.
func TestReclaimStaleLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs989",
		sections("cs989", 7011, 100, 100),
		sections("cs989", 7011, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs989"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-stale", h.sectionID(7011), "chan-stale")

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Claim without delivering, then expire the lease: this is exactly the
	// state a killed process leaves behind.
	claimed, err := h.store.Claim(ctx, 10, 1*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}
	if _, err := h.store.Pool().Exec(ctx,
		`UPDATE notifications SET lease_expires_at = NOW() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	n, err := h.store.ReclaimStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}

	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.fake.forUser("user-stale")); got != 1 {
		t.Fatalf("delivered %d after reclaim, want 1", got)
	}
}

// TestUpstreamFailureDoesNotLoseState: an API error must not cause a spurious
// alert or corrupt differ state.
func TestUpstreamFailureDoesNotLoseState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs988",
		sections("cs988", 7012, 100, 100),
		sections("cs988", 7012, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs988"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-fail", h.sectionID(7012), "chan-fail")

	// Seed as full.
	if err := h.pol.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Upstream breaks for the next two polls.
	h.src.FailNext(2)
	for i := 0; i < 2; i++ {
		_ = h.pol.RunOnce(ctx) // errors are logged, not fatal
	}
	if n := h.fake.count(); n != 0 {
		t.Fatalf("upstream failure produced %d deliveries, want 0", n)
	}

	// Recover: the seat opens and the alert still fires.
	if err := h.pol.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.fake.forUser("user-fail")); got != 1 {
		t.Fatalf("delivered %d after recovery, want 1", got)
	}
}

// TestUnwatchStopsNotifications verifies unsubscribing takes effect.
func TestUnwatchStopsNotifications(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs987",
		sections("cs987", 7013, 100, 100),
		sections("cs987", 7013, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs987"); err != nil {
		t.Fatal(err)
	}
	secID := h.sectionID(7013)
	uid := h.subscribe("user-unwatch", secID, "chan-unwatch")

	if err := h.pol.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.store.Unsubscribe(ctx, uid, secID); err != nil || !ok {
		t.Fatalf("unsubscribe: ok=%v err=%v", ok, err)
	}

	// The seat opens after unsubscribing.
	if err := h.pol.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.count(); n != 0 {
		t.Fatalf("delivered %d after unwatch, want 0", n)
	}
}

// TestWatchedCoursesDedupesToCourses is the efficiency property that makes the
// poller affordable: three watched sections of one course cost one request.
func TestWatchedCoursesDedupesToCourses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	three := []source.SectionState{
		{TermCode: "1269", ClassNumber: 7020, CourseCode: "cs986", SectionName: "LEC 001",
			Component: "LEC", Enrolled: 100, Capacity: 100, ObservedAt: time.Now()},
		{TermCode: "1269", ClassNumber: 7021, CourseCode: "cs986", SectionName: "LAB 101",
			Component: "LAB", Enrolled: 100, Capacity: 100, ObservedAt: time.Now()},
		{TermCode: "1269", ClassNumber: 7022, CourseCode: "cs986", SectionName: "TST 201",
			Component: "TST", Enrolled: 100, Capacity: 100, ObservedAt: time.Now()},
	}
	h.src.Script("cs986", three)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs986"); err != nil {
		t.Fatal(err)
	}

	for _, class := range []int{7020, 7021, 7022} {
		h.subscribe("user-multi", h.sectionID(class), "chan-multi")
	}

	watched, err := h.store.WatchedCourses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != 1 {
		t.Fatalf("WatchedCourses returned %d entries, want 1 "+
			"(three sections of one course must collapse to one poll target)", len(watched))
	}
	if watched[0].Watchers != 3 {
		t.Errorf("Watchers = %d, want 3", watched[0].Watchers)
	}
	if watched[0].FullSections != 3 {
		t.Errorf("FullSections = %d, want 3", watched[0].FullSections)
	}

	before := h.src.Calls("cs986")
	if err := h.pol.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.src.Calls("cs986") - before; got != 1 {
		t.Errorf("one poll cycle made %d upstream calls for 3 watched sections, want 1", got)
	}
}
