package notifier

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	// Jitter is +/-20%, so assert on ranges rather than exact values.
	tests := []struct {
		attempt int
		minWant time.Duration
		maxWant time.Duration
	}{
		{attempt: 1, minWant: 1600 * time.Millisecond, maxWant: 2400 * time.Millisecond},
		{attempt: 2, minWant: 3200 * time.Millisecond, maxWant: 4800 * time.Millisecond},
		{attempt: 3, minWant: 6400 * time.Millisecond, maxWant: 9600 * time.Millisecond},
		{attempt: 10, minWant: 8 * time.Minute, maxWant: 12 * time.Minute},
		{attempt: 100, minWant: 8 * time.Minute, maxWant: 12 * time.Minute},
	}
	for _, tc := range tests {
		for i := 0; i < 50; i++ {
			got := Backoff(tc.attempt)
			if got < tc.minWant || got > tc.maxWant {
				t.Fatalf("Backoff(%d) = %s, want within [%s, %s]",
					tc.attempt, got, tc.minWant, tc.maxWant)
			}
		}
	}
}

func TestBackoffNeverNegativeOrZero(t *testing.T) {
	for attempt := -5; attempt < 20; attempt++ {
		for i := 0; i < 20; i++ {
			if d := Backoff(attempt); d <= 0 {
				t.Fatalf("Backoff(%d) = %s, must be positive", attempt, d)
			}
		}
	}
}

func TestBackoffIsJittered(t *testing.T) {
	// Two hundred draws at the same attempt should not all be identical;
	// identical delays would mean concurrent retries realign into a burst.
	seen := make(map[time.Duration]bool)
	for i := 0; i < 200; i++ {
		seen[Backoff(3)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays in 200 draws; jitter looks broken", len(seen))
	}
}
