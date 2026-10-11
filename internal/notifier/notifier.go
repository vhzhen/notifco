// Package notifier drains the notification outbox and delivers messages.
//
// Delivery is at-least-once with an idempotent effect: the outbox row is keyed
// UNIQUE(subscription_id, transition_id), so a duplicate enqueue is impossible
// and the worst case after a crash mid-send is one redundant message rather
// than UWFlow's indefinite resend (see NOTES.md §5).
//
// Sends happen outside any database transaction. UWFlow sends inside its
// transaction, where a single SMTP error aborts the batch and an unknown
// payload kills the listener entirely.
package notifier

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/vhzhen/notifco/internal/differ"
	"github.com/vhzhen/notifco/internal/metrics"
	"github.com/vhzhen/notifco/internal/store"
)

const (
	backoffBase = 2 * time.Second
	backoffCap  = 10 * time.Minute
	leaseTTL    = 2 * time.Minute
)

// Transport delivers one message. Implemented by the Discord layer, and by a
// fake in tests.
type Transport interface {
	// Deliver sends the notification. The returned string records which
	// transport actually carried it ("dm" or "channel").
	Deliver(ctx context.Context, d store.Delivery) (via string, err error)
}

// Permanent marks an error that retrying cannot fix, e.g. an unknown recipient.
type Permanent struct{ Err error }

func (e *Permanent) Error() string { return e.Err.Error() }
func (e *Permanent) Unwrap() error { return e.Err }

// Throttled marks a rate-limit response. It is retried without consuming an
// attempt, because it reflects our send rate rather than a bad recipient.
type Throttled struct {
	Err        error
	RetryAfter time.Duration
}

func (e *Throttled) Error() string { return e.Err.Error() }
func (e *Throttled) Unwrap() error { return e.Err }

// Options configures a Notifier.
type Options struct {
	Store     *store.Store
	Transport Transport
	Metrics   *metrics.Metrics
	Logger    *slog.Logger

	Workers     int
	MaxAttempts int
	Cooldown    time.Duration
	// PollEvery is how often idle workers look for new work. Delivery normally
	// starts immediately via Notify; this is the safety net that also drains a
	// backlog left by a crash.
	PollEvery time.Duration
	BatchSize int
}

// Notifier drains the outbox.
type Notifier struct {
	opts Options
	log  *slog.Logger

	// transport is swappable after construction because the Discord session
	// must be connected before it can deliver, while the notifier is wired up
	// earlier. Guarded because Run reads it from several workers.
	mu        sync.RWMutex
	transport Transport

	// wake carries a non-blocking nudge from Enqueue so delivery begins
	// immediately instead of waiting for the next tick. This is the useful half
	// of UWFlow's LISTEN/NOTIFY doorbell, without using it as the queue itself.
	wake chan struct{}
}

// New builds a Notifier.
func New(opts Options) *Notifier {
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 6
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = 30 * time.Minute
	}
	if opts.PollEvery <= 0 {
		opts.PollEvery = 2 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 32
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Notifier{
		opts:      opts,
		log:       opts.Logger,
		transport: opts.Transport,
		wake:      make(chan struct{}, 1),
	}
}

// SetTransport swaps the delivery transport. Safe to call while Run is active.
func (n *Notifier) SetTransport(t Transport) {
	n.mu.Lock()
	n.transport = t
	n.mu.Unlock()
}

func (n *Notifier) currentTransport() Transport {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.transport
}

// Enqueue fans a transition out to watchers and nudges the workers.
// Implements poller.Enqueuer.
func (n *Notifier) Enqueue(ctx context.Context, t differ.Transition) error {
	// The section must already exist in Postgres; the poller upserts it.
	sectionID, err := n.sectionID(ctx, t)
	if err != nil {
		return err
	}
	queued, err := n.opts.Store.Enqueue(ctx, sectionID, t.ID,
		t.Section.SeatsOpen(), t.DetectedAt)
	if err != nil {
		return err
	}
	if queued > 0 {
		n.log.Info("notifications queued",
			"transition", t.ID, "watchers", queued,
			"course", t.Section.CourseCode, "section", t.Section.SectionName)
		n.Notify()
	}
	return nil
}

func (n *Notifier) sectionID(ctx context.Context, t differ.Transition) (int64, error) {
	var id int64
	err := n.opts.Store.Pool().QueryRow(ctx,
		`SELECT id FROM sections WHERE term_code = $1 AND class_number = $2`,
		t.Section.TermCode, t.Section.ClassNumber).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Notify wakes the workers without blocking.
func (n *Notifier) Notify() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Run drains the outbox until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) error {
	// Boot reconciliation: anything left 'sending' by a dead process is
	// returned to the pending pool. UWFlow never does this, so a mailer restart
	// silently delays the backlog until some unrelated event fires.
	if reclaimed, err := n.opts.Store.ReclaimStale(ctx); err != nil {
		n.log.Warn("reclaim stale failed", "err", err)
	} else if reclaimed > 0 {
		n.log.Info("reclaimed stale deliveries", "count", reclaimed)
	}

	work := make(chan store.Delivery)
	var wg sync.WaitGroup
	for i := 0; i < n.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				n.deliver(ctx, d)
			}
		}()
	}
	defer func() {
		close(work)
		wg.Wait()
	}()

	tick := time.NewTicker(n.opts.PollEvery)
	defer tick.Stop()

	for {
		// Drain everything currently claimable before sleeping again.
		for {
			if ctx.Err() != nil {
				return nil
			}
			batch, err := n.opts.Store.Claim(ctx, n.opts.BatchSize, leaseTTL)
			if err != nil {
				n.log.Error("claim failed", "err", err)
				break
			}
			if len(batch) == 0 {
				break
			}
			for _, d := range batch {
				select {
				case work <- d:
				case <-ctx.Done():
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-n.wake:
		}
	}
}

// DrainOnce claims and delivers until the queue is empty. Used by tests.
func (n *Notifier) DrainOnce(ctx context.Context) (int, error) {
	total := 0
	for {
		batch, err := n.opts.Store.Claim(ctx, n.opts.BatchSize, leaseTTL)
		if err != nil {
			return total, err
		}
		if len(batch) == 0 {
			return total, nil
		}
		for _, d := range batch {
			n.deliver(ctx, d)
			total++
		}
	}
}

// DrainConcurrent drains with the configured worker count. Used by the load test.
func (n *Notifier) DrainConcurrent(ctx context.Context) (int, error) {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	work := make(chan store.Delivery, n.opts.Workers*2)
	for i := 0; i < n.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				n.deliver(ctx, d)
				mu.Lock()
				total++
				mu.Unlock()
			}
		}()
	}

	for {
		batch, err := n.opts.Store.Claim(ctx, n.opts.BatchSize, leaseTTL)
		if err != nil {
			close(work)
			wg.Wait()
			return total, err
		}
		if len(batch) == 0 {
			break
		}
		for _, d := range batch {
			work <- d
		}
	}
	close(work)
	wg.Wait()
	return total, nil
}

// deliver sends one notification and records the outcome.
func (n *Notifier) deliver(ctx context.Context, d store.Delivery) {
	via, err := n.currentTransport().Deliver(ctx, d)
	if err == nil {
		if err := n.opts.Store.MarkSent(ctx, d.ID, via, n.opts.Cooldown); err != nil {
			// The message went out but bookkeeping failed. The lease will
			// expire and the row be reclaimed, so the user may get one
			// duplicate. Bounded, and far better than UWFlow's silent resend
			// on every subsequent wake-up.
			n.log.Error("delivered but failed to mark sent",
				"id", d.ID, "transition", d.TransitionID, "err", err)
			return
		}
		latency := time.Since(d.DetectedAt)
		if n.opts.Metrics != nil {
			n.opts.Metrics.NotificationsTotal.WithLabelValues("sent", via).Inc()
			n.opts.Metrics.AlertLatency.Observe(latency.Seconds())
		}
		n.log.Info("notification sent",
			"user", d.DiscordUserID, "course", d.CourseCode,
			"section", d.SectionName, "via", via,
			"latency_ms", latency.Milliseconds())
		return
	}

	// Rate limited: retry without burning an attempt.
	var throttled *Throttled
	if errors.As(err, &throttled) {
		delay := throttled.RetryAfter
		if delay <= 0 {
			delay = backoffBase
		}
		if rerr := n.opts.Store.RetryWithoutAttempt(ctx, d.ID, delay, err.Error()); rerr != nil {
			n.log.Error("requeue after throttle failed", "id", d.ID, "err", rerr)
		}
		if n.opts.Metrics != nil {
			n.opts.Metrics.NotificationsTotal.WithLabelValues("throttled", "").Inc()
		}
		return
	}

	// Permanent: no amount of retrying helps.
	var perm *Permanent
	if errors.As(err, &perm) {
		if derr := n.opts.Store.DeadLetter(ctx, d.ID, err.Error()); derr != nil {
			n.log.Error("dead-letter failed", "id", d.ID, "err", derr)
		}
		if n.opts.Metrics != nil {
			n.opts.Metrics.NotificationsTotal.WithLabelValues("dead_letter", "").Inc()
		}
		n.log.Warn("notification dead-lettered",
			"id", d.ID, "user", d.DiscordUserID, "reason", err)
		return
	}

	// Transient: retry with capped exponential backoff and jitter, up to
	// MaxAttempts, then dead-letter.
	if d.Attempts >= n.opts.MaxAttempts {
		if derr := n.opts.Store.DeadLetter(ctx, d.ID, err.Error()); derr != nil {
			n.log.Error("dead-letter failed", "id", d.ID, "err", derr)
		}
		if n.opts.Metrics != nil {
			n.opts.Metrics.NotificationsTotal.WithLabelValues("dead_letter", "").Inc()
		}
		n.log.Warn("notification exhausted attempts",
			"id", d.ID, "attempts", d.Attempts, "reason", err)
		return
	}

	delay := Backoff(d.Attempts)
	if rerr := n.opts.Store.Retry(ctx, d.ID, delay, err.Error()); rerr != nil {
		n.log.Error("retry scheduling failed", "id", d.ID, "err", rerr)
	}
	if n.opts.Metrics != nil {
		n.opts.Metrics.NotificationsTotal.WithLabelValues("retry", "").Inc()
	}
	n.log.Warn("notification retry scheduled",
		"id", d.ID, "attempt", d.Attempts, "delay", delay.Round(time.Millisecond), "err", err)
}

// Backoff returns the delay before retry attempt n (1-based).
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(backoffBase) * math.Pow(2, float64(attempt-1))
	if d > float64(backoffCap) {
		d = float64(backoffCap)
	}
	jitter := 1 + 0.2*(2*rand.Float64()-1)
	out := time.Duration(d * jitter)
	if out < 0 {
		out = backoffBase
	}
	return out
}
