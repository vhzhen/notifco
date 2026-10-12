package integration

import (
	"context"
	"testing"
	"time"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// These tests cover defects found by an independent audit of the first
// implementation. Each one failed before the corresponding fix.

// TestTransitionIDSurvivesRedisFlush is the regression test for the critical
// bug: transition_id used to be "{term}:{class}:{epoch}" with the epoch coming
// from a Redis INCR. Flushing Redis restarted the epoch at 1, regenerating an
// ID that Postgres had already recorded, and ON CONFLICT DO NOTHING then
// silently discarded a REAL seat opening.
func TestTransitionIDSurvivesRedisFlush(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs801",
		sections("cs801", 7101, 100, 100),
		sections("cs801", 7101, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs801"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-flush", h.sectionID(7101), "chan-flush")

	// Cycle 1: a genuine opening is detected and delivered.
	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.fake.forUser("user-flush")); got != 1 {
		t.Fatalf("cycle 1 delivered %d, want 1", got)
	}

	// Simulate total Redis loss, which docker-compose explicitly allows by
	// running Redis with no persistence.
	if err := h.redis.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	// Clear the cooldown so this test isolates transition-id collision rather
	// than re-testing cooldown suppression.
	if _, err := h.store.Pool().Exec(ctx,
		`UPDATE subscriptions SET cooldown_until = NULL`); err != nil {
		t.Fatal(err)
	}

	// Cycle 2: the section fills and opens again. Epoch restarts at 1.
	h.src.Script("cs801",
		sections("cs801", 7101, 100, 100),
		sections("cs801", 7101, 98, 100),
	)
	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.notif.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if got := len(h.fake.forUser("user-flush")); got != 2 {
		t.Fatalf("after a Redis flush the user received %d alerts, want 2. "+
			"A real seat opening was swallowed because the regenerated "+
			"transition_id collided with a spent one", got)
	}

	// The two IDs must differ.
	rows, err := h.store.Pool().Query(ctx,
		`SELECT DISTINCT transition_id FROM notifications ORDER BY transition_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d distinct transition ids (%v), want 2", len(ids), ids)
	}
}

// TestEpochTTLStaysInLockstep covers the second trigger for the same bug: the
// epoch key's TTL used to be refreshed only when a transition fired, while the
// state key was refreshed on every observation. The two diverged, so a
// long-quiet section lost its epoch while keeping its state, and the next INCR
// restarted at 1.
func TestEpochTTLStaysInLockstep(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	s := sections("cs802", 7102, 100, 100)[0]

	// Fire once so the epoch key exists.
	if _, err := h.differ.Observe(ctx, s); err != nil {
		t.Fatal(err)
	}
	open := s
	open.Enrolled = 99
	if tr, err := h.differ.Observe(ctx, open); err != nil || tr == nil {
		t.Fatalf("expected a transition: tr=%v err=%v", tr, err)
	}

	stateKey := "notifco:state:1269:7102"
	epochKey := "notifco:epoch:1269:7102"

	// Observe again; both TTLs must be refreshed together.
	if _, err := h.differ.Observe(ctx, open); err != nil {
		t.Fatal(err)
	}

	stateTTL, err := h.redis.TTL(ctx, stateKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	epochTTL, err := h.redis.TTL(ctx, epochKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if epochTTL <= 0 {
		t.Fatalf("epoch key TTL is %s; it must be refreshed alongside state", epochTTL)
	}
	diff := stateTTL - epochTTL
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*time.Second {
		t.Errorf("state TTL %s and epoch TTL %s have diverged by %s; "+
			"they must stay in lockstep", stateTTL, epochTTL, diff)
	}
}

// TestThrottleRetriesAreBounded covers the infinite-retry bug: Claim increments
// attempts and RetryWithoutAttempt decremented it again, netting zero, so the
// MaxAttempts check was unreachable and a throttled row looped forever.
func TestThrottleRetriesAreBounded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs803",
		sections("cs803", 7103, 100, 100),
		sections("cs803", 7103, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs803"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-throttle", h.sectionID(7103), "chan-throttle")
	h.fake.throttleUser("user-throttle", 1000) // always rate limited

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Drain far more times than any sane bound.
	for i := 0; i < 40; i++ {
		if _, err := h.store.Pool().Exec(ctx,
			`UPDATE notifications SET next_attempt_at = NOW() WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.notif.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
		stats, err := h.store.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stats.DeadLetter == 1 {
			return // bounded, as intended
		}
	}

	var throttles int
	_ = h.store.Pool().QueryRow(ctx, `SELECT throttles FROM notifications`).Scan(&throttles)
	t.Fatalf("a permanently throttled notification never gave up "+
		"(throttles=%d); throttle retries must be bounded", throttles)
}

// TestClaimRecoversExpiredLeaseWithoutRestart covers the stranded-row bug:
// ReclaimStale was only called once at boot, and Claim only ever selects
// 'pending', so a worker killed mid-send left a row in 'sending' forever.
func TestClaimRecoversExpiredLeaseWithoutRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs804",
		sections("cs804", 7104, 100, 100),
		sections("cs804", 7104, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs804"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-lease", h.sectionID(7104), "chan-lease")

	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Claim and abandon, exactly as a killed worker would.
	claimed, err := h.store.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}
	if _, err := h.store.Pool().Exec(ctx,
		`UPDATE notifications SET lease_expires_at = NOW() - interval '5 minutes'`); err != nil {
		t.Fatal(err)
	}

	// Without a restart, the running notifier must recover this itself.
	// Run drives the periodic reclaim; give it a bounded window.
	runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.notif.Run(runCtx) }()

	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.fake.forUser("user-lease")) == 1 {
			cancel()
			<-done
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	cancel()
	<-done

	stats, _ := h.store.Stats(ctx)
	t.Fatalf("an expired lease was never reclaimed by the running notifier "+
		"(stats=%+v); reclaim must not be boot-only", stats)
}

// TestZeroCapacityPreviousStateDoesNotFire covers the spurious-fire bug: a 0/0
// previous snapshot satisfied "enrolled >= capacity", so the moment a real
// capacity appeared the differ reported an opening that never happened.
func TestZeroCapacityPreviousStateDoesNotFire(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	zero := source.SectionState{
		TermCode: "1269", ClassNumber: 7105, CourseCode: "cs805",
		SectionName: "LEC 001", Component: "LEC",
		Enrolled: 0, Capacity: 0, ObservedAt: time.Now().UTC(),
	}
	if _, err := h.differ.Observe(ctx, zero); err != nil {
		t.Fatal(err)
	}

	// Capacity is published for the first time, with seats available. That is
	// a new section appearing, not a seat being freed.
	appeared := zero
	appeared.Capacity = 120
	appeared.Enrolled = 0
	appeared.ObservedAt = time.Now().UTC()

	tr, err := h.differ.Observe(ctx, appeared)
	if err != nil {
		t.Fatal(err)
	}
	if tr != nil {
		t.Fatalf("a 0/0 -> 0/120 change fired a transition (%s); "+
			"a section appearing is not a seat opening", tr.ID)
	}
}

// TestSubMillisecondIntervalsAreAccepted covers the ::interval bug: durations
// were passed as Go strings like "500µs", which Postgres rejects.
func TestSubMillisecondIntervalsAreAccepted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.src.Script("cs806",
		sections("cs806", 7106, 100, 100),
		sections("cs806", 7106, 99, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs806"); err != nil {
		t.Fatal(err)
	}
	h.subscribe("user-micro", h.sectionID(7106), "chan-micro")
	for i := 0; i < 2; i++ {
		if err := h.pol.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := h.store.Claim(ctx, 1, 900*time.Nanosecond)
	if err != nil {
		t.Fatalf("Claim with a sub-millisecond lease failed: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}
	id := claimed[0].ID

	for _, d := range []time.Duration{500 * time.Microsecond, 900 * time.Nanosecond, 0} {
		if err := h.store.Retry(ctx, id, d, "probe"); err != nil {
			t.Errorf("Retry(%s) failed: %v", d, err)
		}
		if err := h.store.RetryWithoutAttempt(ctx, id, d, "probe"); err != nil {
			t.Errorf("RetryWithoutAttempt(%s) failed: %v", d, err)
		}
	}
	if err := h.store.MarkSent(ctx, id, "dm", 750*time.Microsecond); err != nil {
		t.Errorf("MarkSent with a sub-millisecond cooldown failed: %v", err)
	}
}

// TestFindSectionHandlesShortNames covers a panic reachable from slash input:
// FindSection sliced the last three bytes of section_name unguarded.
func TestFindSectionHandlesShortNames(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A one-character section name, shorter than the old fixed slice.
	odd := source.SectionState{
		TermCode: "1269", ClassNumber: 7107, CourseCode: "cs807",
		SectionName: "X", Component: "LEC",
		Enrolled: 10, Capacity: 20, ObservedAt: time.Now().UTC(),
	}
	if err := h.store.UpsertSections(ctx, []source.SectionState{odd}); err != nil {
		t.Fatal(err)
	}

	// Must return not-found rather than panicking.
	if _, err := h.store.FindSection(ctx, "1269", "cs807", "001"); err == nil {
		t.Error("expected ErrNotFound for a non-matching section")
	} else if err != store.ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}

	// An empty section name must also be survivable.
	empty := odd
	empty.ClassNumber = 7108
	empty.SectionName = ""
	if err := h.store.UpsertSections(ctx, []source.SectionState{empty}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.FindSection(ctx, "1269", "cs807", "1"); err != nil && err != store.ErrNotFound {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSearchWildcardsAreEscaped: a user typing "%" must not match everything.
func TestSearchWildcardsAreEscaped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.pol.EnsureCourse(ctx, h.term, "cs341"); err != nil {
		t.Fatal(err)
	}

	hits, err := h.store.SearchCourses(ctx, "1269", "%", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("a bare %% matched %d courses; LIKE metacharacters in user "+
			"input must be escaped", len(hits))
	}
}

// TestEnsureCoursePersistsCatalogTitle: ClassSchedules carries no title, so a
// course fetched on demand used to be stored with an empty one and the UI could
// only show "MATH 135". The title must come from the catalog and survive later
// polls, which learn nothing about titles.
func TestEnsureCoursePersistsCatalogTitle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The source's section data has a junk title; the catalog title must win.
	h.src.Script("math135",
		sections("math135", 7301, 100, 100),
		sections("math135", 7301, 100, 100),
	)
	if _, err := h.pol.EnsureCourse(ctx, h.term, "math135"); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.SectionsByCourse(ctx, "1269", "math135")
	if err != nil || len(got) != 1 {
		t.Fatalf("SectionsByCourse: %v, %d rows", err, len(got))
	}
	if got[0].CourseTitle != "Algebra for Honours Mathematics" {
		t.Fatalf("stored title = %q, want the catalog title", got[0].CourseTitle)
	}

	// A routine poll upserts sections with no title; it must not blank it.
	if err := h.store.UpsertSections(ctx, []source.SectionState{{
		TermCode: "1269", ClassNumber: 7301, CourseCode: "math135", SectionName: "LEC 001",
		Component: "LEC", Capacity: 100, Enrolled: 99, ObservedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	got, _ = h.store.SectionsByCourse(ctx, "1269", "math135")
	if got[0].CourseTitle != "Algebra for Honours Mathematics" {
		t.Errorf("a title-less poll blanked the stored title: %q", got[0].CourseTitle)
	}
	if got[0].Enrolled != 99 {
		t.Errorf("the poll's enrolment update was lost: %d", got[0].Enrolled)
	}
}
