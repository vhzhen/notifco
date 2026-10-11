// Package differ detects full-to-open seat transitions.
//
// The rule is edge-triggered: a notification fires only when a section was full
// at the previous observation and has a free seat now. A section that is merely
// still open never re-fires.
//
// Correctness note. UWFlow's equivalent SQL tests "was full" as
//
//	old.enrollment_total >= new.enrollment_capacity
//
// comparing the OLD enrolled count against the NEW capacity. That silently
// misses the case where a department raises capacity on a full section
// (100/100 -> 100/120), which is one of the most valuable alerts there is.
// We compare old-vs-old. See NOTES.md §3.
package differ

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vhzhen/notifco/internal/source"
)

// stateTTL bounds how long last-seen state lives. Long enough to span a term,
// short enough that dead sections do not accumulate forever.
const stateTTL = 90 * 24 * time.Hour

// Snapshot is the enrollment pair the rule operates on.
type Snapshot struct {
	Enrolled int
	Capacity int
}

// IsFull reports whether the snapshot has no free seats.
func (s Snapshot) IsFull() bool { return s.Enrolled >= s.Capacity }

// IsOpening is the transition rule, kept pure so it can be exhaustively tested
// without Redis.
//
// Fires when the section was full before and has a seat now. Both comparisons
// use each snapshot's own capacity, which is what makes a capacity increase on
// a full section fire correctly.
func IsOpening(prev, cur Snapshot) bool {
	// Neither snapshot may be a non-section. A 0/0 previous state would
	// otherwise satisfy "enrolled >= capacity" and fire spuriously the moment
	// a real capacity appeared.
	if cur.Capacity <= 0 || prev.Capacity <= 0 {
		return false
	}
	wasFull := prev.Enrolled >= prev.Capacity
	nowOpen := cur.Enrolled < cur.Capacity
	return wasFull && nowOpen
}

// Transition is a detected full-to-open event.
type Transition struct {
	Section source.SectionState
	// Prev is the state the section was in before this observation.
	Prev Snapshot
	// Epoch is a monotonically increasing per-section counter, incremented
	// atomically when the transition fires.
	Epoch int64
	// ID is the stable, restart-safe identity of this event. Paired with a
	// UNIQUE constraint in Postgres, it is what makes delivery idempotent.
	//
	// Format: "{term}:{class}:{epoch}:{unixMilli}".
	//
	// The timestamp is not decoration. The epoch alone comes from a Redis
	// counter, and Redis is explicitly treated as a cache: if it is flushed or
	// its key expires, INCR restarts at 1 and would regenerate an ID that
	// Postgres has already seen. ON CONFLICT DO NOTHING would then silently
	// discard a real seat opening. Including the detection time makes the ID
	// collision-free across Redis state loss while still being fixed for the
	// lifetime of one Transition value, so replaying the same transition
	// remains a no-op.
	ID string
	// DetectedAt is when we observed the opening.
	DetectedAt time.Time
}

// observeScript performs compare-and-set plus the epoch bump in one atomic step,
// so two pollers observing the same change cannot both fire.
//
// KEYS[1] state hash, KEYS[2] epoch counter
// ARGV[1] enrolled, ARGV[2] capacity, ARGV[3] observedAt unix, ARGV[4] ttl secs
//
// Returns {fired, epoch, prevEnrolled, prevCapacity}; prev values are -1 when
// there was no prior state.
var observeScript = redis.NewScript(`
local prev = redis.call('HMGET', KEYS[1], 'enrolled', 'capacity')
local pe, pc = prev[1], prev[2]

redis.call('HSET', KEYS[1],
  'enrolled', ARGV[1], 'capacity', ARGV[2], 'observed_at', ARGV[3])
redis.call('EXPIRE', KEYS[1], ARGV[4])

-- Keep the epoch key's lifetime in lockstep with the state key. Refreshing it
-- only when a transition fires would let the two diverge: a section that sits
-- unchanged for longer than the TTL would lose its epoch while retaining its
-- state, so the next INCR restarts at 1 and regenerates a spent transition id.
if redis.call('EXISTS', KEYS[2]) == 1 then
  redis.call('EXPIRE', KEYS[2], ARGV[4])
end

if not pe or not pc then
  -- Cold start: record state, never fire. Without this a fresh Redis would
  -- alert every watcher that an already-open section is open.
  return {0, 0, -1, -1}
end

local prevEnrolled, prevCapacity = tonumber(pe), tonumber(pc)
local curEnrolled, curCapacity = tonumber(ARGV[1]), tonumber(ARGV[2])

-- Guard against malformed stored state rather than erroring inside Lua.
if not prevEnrolled or not prevCapacity then
  return {0, 0, -1, -1}
end

-- A zero-capacity snapshot on either side is not a real section.
if curCapacity <= 0 or prevCapacity <= 0 then
  return {0, 0, prevEnrolled, prevCapacity}
end

if prevEnrolled >= prevCapacity and curEnrolled < curCapacity then
  local epoch = redis.call('INCR', KEYS[2])
  redis.call('EXPIRE', KEYS[2], ARGV[4])
  return {1, epoch, prevEnrolled, prevCapacity}
end

return {0, 0, prevEnrolled, prevCapacity}
`)

// Differ holds last-seen state in Redis and reports transitions.
//
// State lives in Redis rather than alongside the serving data in Postgres so
// detection is decoupled from the catalog write path. That coupling is exactly
// what stops UWFlow from polling faster than its importer runs.
//
// Losing Redis is safe: it degrades detection for one poll cycle (state is
// re-seeded cold, and cold state never fires) and can never cause a duplicate
// notification, because duplicates are prevented by a Postgres constraint.
type Differ struct {
	rdb    redis.Scripter
	prefix string
}

// New returns a Differ backed by the given Redis client.
func New(rdb redis.Scripter) *Differ {
	return &Differ{rdb: rdb, prefix: "notifco"}
}

func (d *Differ) stateKey(k source.SectionKey) string {
	return fmt.Sprintf("%s:state:%s:%d", d.prefix, k.TermCode, k.ClassNumber)
}

func (d *Differ) epochKey(k source.SectionKey) string {
	return fmt.Sprintf("%s:epoch:%s:%d", d.prefix, k.TermCode, k.ClassNumber)
}

// Observe records a section's current state and reports a transition if the
// section just went from full to open. Returns nil when nothing fired.
func (d *Differ) Observe(ctx context.Context, s source.SectionState) (*Transition, error) {
	observedAt := s.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	res, err := observeScript.Run(ctx, d.rdb,
		[]string{d.stateKey(s.Key()), d.epochKey(s.Key())},
		s.Enrolled, s.Capacity, observedAt.Unix(), int(stateTTL.Seconds()),
	).Slice()
	if err != nil {
		return nil, fmt.Errorf("differ: observe %s: %w", s.Key(), err)
	}
	if len(res) != 4 {
		return nil, fmt.Errorf("differ: observe %s: unexpected script result %v", s.Key(), res)
	}

	fired, err := toInt64(res[0])
	if err != nil {
		return nil, fmt.Errorf("differ: observe %s: fired: %w", s.Key(), err)
	}
	if fired == 0 {
		return nil, nil
	}

	epoch, err := toInt64(res[1])
	if err != nil {
		return nil, fmt.Errorf("differ: observe %s: epoch: %w", s.Key(), err)
	}
	prevEnrolled, _ := toInt64(res[2])
	prevCapacity, _ := toInt64(res[3])

	detectedAt := time.Now().UTC()
	return &Transition{
		Section:    s,
		Prev:       Snapshot{Enrolled: int(prevEnrolled), Capacity: int(prevCapacity)},
		Epoch:      epoch,
		ID:         TransitionID(s.Key(), epoch, detectedAt),
		DetectedAt: detectedAt,
	}, nil
}

// ObserveAll runs Observe over a batch, returning every transition found.
// A failure on one section does not abandon the rest.
func (d *Differ) ObserveAll(ctx context.Context, states []source.SectionState) ([]Transition, error) {
	var out []Transition
	var firstErr error
	for _, s := range states {
		t, err := d.Observe(ctx, s)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, firstErr
}

// Peek returns the stored last-seen state, if any.
func (d *Differ) Peek(ctx context.Context, k source.SectionKey) (Snapshot, bool, error) {
	c, ok := d.rdb.(redis.Cmdable)
	if !ok {
		return Snapshot{}, false, fmt.Errorf("differ: client does not support reads")
	}
	vals, err := c.HMGet(ctx, d.stateKey(k), "enrolled", "capacity").Result()
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("differ: peek %s: %w", k, err)
	}
	if len(vals) != 2 || vals[0] == nil || vals[1] == nil {
		return Snapshot{}, false, nil
	}
	e, err1 := toInt64(vals[0])
	c2, err2 := toInt64(vals[1])
	if err1 != nil || err2 != nil {
		return Snapshot{}, false, nil
	}
	return Snapshot{Enrolled: int(e), Capacity: int(c2)}, true, nil
}

// Reset clears stored state for a section. Used by tests.
func (d *Differ) Reset(ctx context.Context, k source.SectionKey) error {
	c, ok := d.rdb.(redis.Cmdable)
	if !ok {
		return fmt.Errorf("differ: client does not support deletes")
	}
	return c.Del(ctx, d.stateKey(k), d.epochKey(k)).Err()
}

// TransitionID builds the stable identity for a transition.
//
// The epoch alone is not sufficient: it lives in Redis, which is a cache, so a
// flush or an expired key restarts it at 1 and would regenerate an ID that
// Postgres already recorded — causing a genuine seat opening to be silently
// dropped by ON CONFLICT DO NOTHING. Mixing in the detection time removes that
// collision while keeping the ID fixed for a given Transition value.
func TransitionID(k source.SectionKey, epoch int64, detectedAt time.Time) string {
	return fmt.Sprintf("%s:%d:%d:%d", k.TermCode, k.ClassNumber, epoch, detectedAt.UnixMilli())
}

// toInt64 normalises the several numeric shapes Redis can return.
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case string:
		var out int64
		if _, err := fmt.Sscanf(n, "%d", &out); err != nil {
			return 0, fmt.Errorf("not an integer: %q", n)
		}
		return out, nil
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}
