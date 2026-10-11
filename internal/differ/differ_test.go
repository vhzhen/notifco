package differ

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vhzhen/notifco/internal/source"
)

// TestIsOpening exhaustively covers the transition rule. This is pure logic,
// so it runs everywhere with no Redis.
func TestIsOpening(t *testing.T) {
	tests := []struct {
		name string
		prev Snapshot
		cur  Snapshot
		want bool
		why  string
	}{
		{
			name: "full to open fires",
			prev: Snapshot{Enrolled: 100, Capacity: 100},
			cur:  Snapshot{Enrolled: 99, Capacity: 100},
			want: true,
			why:  "the entire point: a seat was freed",
		},
		{
			name: "capacity raised while full fires",
			prev: Snapshot{Enrolled: 100, Capacity: 100},
			cur:  Snapshot{Enrolled: 100, Capacity: 120},
			want: true,
			why: "department added seats. UWFlow MISSES this because it compares " +
				"old enrolled against NEW capacity (100 >= 120 is false)",
		},
		{
			name: "open stays open does not fire",
			prev: Snapshot{Enrolled: 98, Capacity: 100},
			cur:  Snapshot{Enrolled: 97, Capacity: 100},
			want: false,
			why:  "edge-triggered, not level-triggered",
		},
		{
			name: "open to full does not fire",
			prev: Snapshot{Enrolled: 99, Capacity: 100},
			cur:  Snapshot{Enrolled: 100, Capacity: 100},
			want: false,
			why:  "wrong direction",
		},
		{
			name: "full stays full does not fire",
			prev: Snapshot{Enrolled: 100, Capacity: 100},
			cur:  Snapshot{Enrolled: 100, Capacity: 100},
			want: false,
		},
		{
			name: "over-enrolled to open fires",
			prev: Snapshot{Enrolled: 105, Capacity: 100},
			cur:  Snapshot{Enrolled: 99, Capacity: 100},
			want: true,
			why:  "registrar over-enrollment is still full",
		},
		{
			name: "over-enrolled stays over-enrolled does not fire",
			prev: Snapshot{Enrolled: 105, Capacity: 100},
			cur:  Snapshot{Enrolled: 101, Capacity: 100},
			want: false,
			why:  "still no real seat",
		},
		{
			name: "capacity shrink to full does not fire",
			prev: Snapshot{Enrolled: 90, Capacity: 100},
			cur:  Snapshot{Enrolled: 90, Capacity: 90},
			want: false,
			why:  "was not full before, and is not open now",
		},
		{
			name: "capacity shrink while open stays open does not fire",
			prev: Snapshot{Enrolled: 50, Capacity: 100},
			cur:  Snapshot{Enrolled: 50, Capacity: 60},
			want: false,
			why:  "never full, so no edge",
		},
		{
			name: "zero capacity never fires",
			prev: Snapshot{Enrolled: 0, Capacity: 0},
			cur:  Snapshot{Enrolled: 0, Capacity: 0},
			want: false,
			why:  "not a real section; 0 >= 0 would otherwise look full",
		},
		{
			name: "full to zero capacity does not fire",
			prev: Snapshot{Enrolled: 100, Capacity: 100},
			cur:  Snapshot{Enrolled: 0, Capacity: 0},
			want: false,
			why:  "section was cancelled, not opened",
		},
		{
			name: "cancelled then restored fires",
			prev: Snapshot{Enrolled: 10, Capacity: 10},
			cur:  Snapshot{Enrolled: 10, Capacity: 50},
			want: true,
			why:  "capacity restored on a full section is a genuine opening",
		},
		{
			name: "many seats freed at once fires",
			prev: Snapshot{Enrolled: 300, Capacity: 300},
			cur:  Snapshot{Enrolled: 268, Capacity: 300},
			want: true,
			why:  "real CS 341 TST 201 shape",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsOpening(tc.prev, tc.cur)
			if got != tc.want {
				t.Errorf("IsOpening(%+v, %+v) = %v, want %v\nreason: %s",
					tc.prev, tc.cur, got, tc.want, tc.why)
			}
		})
	}
}

// TestIsOpeningRejectsUWFlowBug documents the exact predicate difference.
func TestIsOpeningRejectsUWFlowBug(t *testing.T) {
	prev := Snapshot{Enrolled: 100, Capacity: 100} // full
	cur := Snapshot{Enrolled: 100, Capacity: 120}  // capacity raised, 20 seats

	if !IsOpening(prev, cur) {
		t.Fatal("capacity increase on a full section must fire")
	}

	// UWFlow's predicate, transcribed from its SQL:
	//   WHERE o.enrollment_total >= n.enrollment_capacity
	//     AND n.enrollment_total <  n.enrollment_capacity
	uwflowWouldFire := prev.Enrolled >= cur.Capacity && cur.Enrolled < cur.Capacity
	if uwflowWouldFire {
		t.Fatal("expected UWFlow's predicate to miss this case; " +
			"if it now fires, re-read NOTES.md §3")
	}
	t.Logf("confirmed: notifco fires, UWFlow's old-vs-new predicate does not")
}

// --- Redis-backed tests -----------------------------------------------------

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/15" // db 15 reserved for tests
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	c := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable (%v); skipping", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func section(class, enrolled, capacity int) source.SectionState {
	return source.SectionState{
		TermCode:    "1269",
		ClassNumber: class,
		CourseCode:  "cs341",
		SectionName: "LEC 001",
		Component:   "LEC",
		Enrolled:    enrolled,
		Capacity:    capacity,
		ObservedAt:  time.Now().UTC(),
	}
}

func TestObserveColdStartDoesNotFire(t *testing.T) {
	rdb := testRedis(t)
	d := New(rdb)
	ctx := context.Background()
	s := section(9001, 50, 100) // already open
	if err := d.Reset(ctx, s.Key()); err != nil {
		t.Fatal(err)
	}

	tr, err := d.Observe(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if tr != nil {
		t.Fatalf("cold start must not fire, got %+v", tr)
	}

	// State must still have been recorded.
	snap, ok, err := d.Peek(ctx, s.Key())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || snap.Enrolled != 50 || snap.Capacity != 100 {
		t.Fatalf("state not stored: %+v ok=%v", snap, ok)
	}
}

func TestObserveFiresOnceOnFullToOpen(t *testing.T) {
	rdb := testRedis(t)
	d := New(rdb)
	ctx := context.Background()
	key := section(9002, 0, 0).Key()
	if err := d.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}

	// Seed as full.
	if tr, err := d.Observe(ctx, section(9002, 100, 100)); err != nil || tr != nil {
		t.Fatalf("seed: tr=%v err=%v", tr, err)
	}
	// A seat opens.
	tr, err := d.Observe(ctx, section(9002, 99, 100))
	if err != nil {
		t.Fatal(err)
	}
	if tr == nil {
		t.Fatal("expected a transition")
	}
	if tr.Epoch != 1 {
		t.Errorf("epoch = %d, want 1", tr.Epoch)
	}
	if want := "1269:9002:1"; tr.ID != want {
		t.Errorf("ID = %q, want %q", tr.ID, want)
	}
	if tr.Prev != (Snapshot{Enrolled: 100, Capacity: 100}) {
		t.Errorf("Prev = %+v, want 100/100", tr.Prev)
	}

	// Observing the same open state again must NOT re-fire.
	if tr2, err := d.Observe(ctx, section(9002, 99, 100)); err != nil || tr2 != nil {
		t.Fatalf("re-observe must not fire: tr=%v err=%v", tr2, err)
	}
}

func TestObserveEpochIncrementsPerTransition(t *testing.T) {
	rdb := testRedis(t)
	d := New(rdb)
	ctx := context.Background()
	key := section(9003, 0, 0).Key()
	if err := d.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}

	var ids []string
	// Three independent full->open cycles.
	for i := 0; i < 3; i++ {
		if _, err := d.Observe(ctx, section(9003, 100, 100)); err != nil {
			t.Fatal(err)
		}
		tr, err := d.Observe(ctx, section(9003, 99, 100))
		if err != nil {
			t.Fatal(err)
		}
		if tr == nil {
			t.Fatalf("cycle %d: expected transition", i)
		}
		ids = append(ids, tr.ID)
	}

	want := []string{"1269:9003:1", "1269:9003:2", "1269:9003:3"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("transition %d = %q, want %q", i, ids[i], want[i])
		}
	}
}

// TestObserveConcurrentFiresOnce is the critical dedup property: several
// pollers observing the same change must produce exactly one transition.
func TestObserveConcurrentFiresOnce(t *testing.T) {
	rdb := testRedis(t)
	d := New(rdb)
	ctx := context.Background()
	key := section(9004, 0, 0).Key()
	if err := d.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Observe(ctx, section(9004, 100, 100)); err != nil {
		t.Fatal(err)
	}

	const workers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fired []*Transition

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			tr, err := d.Observe(ctx, section(9004, 99, 100))
			if err != nil {
				t.Error(err)
				return
			}
			if tr != nil {
				mu.Lock()
				fired = append(fired, tr)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(fired) != 1 {
		t.Fatalf("%d concurrent observers fired %d transitions, want exactly 1 "+
			"(the Lua CAS is supposed to make this atomic)", workers, len(fired))
	}
	if fired[0].Epoch != 1 {
		t.Errorf("epoch = %d, want 1", fired[0].Epoch)
	}
}

// TestObserveCapacityIncreaseThroughRedis proves the bug fix end-to-end through
// the actual Lua script, not just the pure function.
func TestObserveCapacityIncreaseThroughRedis(t *testing.T) {
	rdb := testRedis(t)
	d := New(rdb)
	ctx := context.Background()
	key := section(9005, 0, 0).Key()
	if err := d.Reset(ctx, key); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Observe(ctx, section(9005, 100, 100)); err != nil {
		t.Fatal(err)
	}
	// Capacity raised to 120, enrolled unchanged.
	tr, err := d.Observe(ctx, section(9005, 100, 120))
	if err != nil {
		t.Fatal(err)
	}
	if tr == nil {
		t.Fatal("capacity increase on a full section must fire through Redis too")
	}
	if tr.Section.SeatsOpen() != 20 {
		t.Errorf("SeatsOpen = %d, want 20", tr.Section.SeatsOpen())
	}
}

func TestObserveAllBatch(t *testing.T) {
	rdb := testRedis(t)
	d := New(rdb)
	ctx := context.Background()

	batch := []source.SectionState{
		section(9010, 100, 100),
		section(9011, 100, 100),
		section(9012, 50, 100),
	}
	for _, s := range batch {
		if err := d.Reset(ctx, s.Key()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ObserveAll(ctx, batch); err != nil {
		t.Fatal(err)
	}

	// Open the two full ones; leave the already-open one alone.
	next := []source.SectionState{
		section(9010, 99, 100),
		section(9011, 99, 100),
		section(9012, 49, 100),
	}
	trs, err := d.ObserveAll(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 2 {
		t.Fatalf("got %d transitions, want 2", len(trs))
	}
}
