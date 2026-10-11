package poller

import (
	"container/heap"
	"testing"
	"time"
)

func newTestPoller() *Poller {
	return New(Options{
		BaseInterval: 60 * time.Second,
		HotInterval:  15 * time.Second,
		MaxInterval:  32 * time.Minute,
	})
}

// TestAdaptiveIntervalWidensOnQuiet: a course where nothing changes should be
// polled progressively less often, so the request budget goes to active ones.
func TestAdaptiveIntervalWidensOnQuiet(t *testing.T) {
	p := newTestPoller()

	// Jitter is +/-10%, so compare midpoints with tolerance.
	tests := []struct {
		noChange int
		wantBase time.Duration
	}{
		{0, 60 * time.Second},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{9, 32 * time.Minute}, // capped
	}
	for _, tc := range tests {
		got := p.NextInterval(TierBase, tc.noChange)
		lo := time.Duration(float64(tc.wantBase) * 0.89)
		hi := time.Duration(float64(tc.wantBase) * 1.11)
		if got < lo || got > hi {
			t.Errorf("NextInterval(base, noChange=%d) = %s, want ~%s (within +/-10%%)",
				tc.noChange, got, tc.wantBase)
		}
	}
}

// TestHotTierStaysTight: a course with a full watched section must keep polling
// fast even when nothing has changed for a while, because that is exactly the
// section a seat could open in.
func TestHotTierStaysTight(t *testing.T) {
	p := newTestPoller()
	for _, noChange := range []int{0, 1, 3, 5, 20} {
		got := p.NextInterval(TierHot, noChange)
		if got > 17*time.Second {
			t.Errorf("NextInterval(hot, noChange=%d) = %s, want <= ~15s; "+
				"hot courses must not back off", noChange, got)
		}
	}
}

func TestIntervalNeverBelowHotInterval(t *testing.T) {
	p := New(Options{
		BaseInterval: 10 * time.Second,
		HotInterval:  10 * time.Second,
		MaxInterval:  time.Minute,
	})
	for i := 0; i < 20; i++ {
		if got := p.NextInterval(TierBase, 0); got < 8*time.Second {
			t.Fatalf("NextInterval = %s, unexpectedly small", got)
		}
	}
}

func TestIntervalIsJittered(t *testing.T) {
	p := newTestPoller()
	seen := make(map[time.Duration]bool)
	for i := 0; i < 100; i++ {
		seen[p.NextInterval(TierBase, 0)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct intervals in 100 draws; jitter looks broken "+
			"(without it every course realigns into a synchronised burst)", len(seen))
	}
}

// TestPriorityQueueOrdersByDueThenWatchers checks the scheduler's ordering.
func TestPriorityQueueOrdersByDueThenWatchers(t *testing.T) {
	now := time.Now()
	var q priorityQueue

	a := &job{courseCode: "late-few", dueAt: now.Add(10 * time.Second), watchers: 1}
	b := &job{courseCode: "soon-few", dueAt: now.Add(time.Second), watchers: 1}
	c := &job{courseCode: "soon-many", dueAt: now.Add(time.Second), watchers: 99}

	for _, j := range []*job{a, b, c} {
		heap.Push(&q, j)
	}

	// Earliest due first; among equal due times, more watchers first.
	want := []string{"soon-many", "soon-few", "late-few"}
	for i, w := range want {
		got := heap.Pop(&q).(*job)
		if got.courseCode != w {
			t.Errorf("pop %d = %q, want %q", i, got.courseCode, w)
		}
	}
}

func TestPriorityQueueMaintainsIndices(t *testing.T) {
	now := time.Now()
	var q priorityQueue
	jobs := make([]*job, 0, 10)
	for i := 0; i < 10; i++ {
		j := &job{
			courseCode: string(rune('a' + i)),
			dueAt:      now.Add(time.Duration(10-i) * time.Second),
		}
		jobs = append(jobs, j)
		heap.Push(&q, j)
	}
	for i, j := range q {
		if j.index != i {
			t.Fatalf("job at position %d reports index %d", i, j.index)
		}
	}
	// Removing from the middle must keep indices consistent.
	heap.Remove(&q, 3)
	for i, j := range q {
		if j.index != i {
			t.Fatalf("after removal, job at position %d reports index %d", i, j.index)
		}
	}
	_ = jobs
}
