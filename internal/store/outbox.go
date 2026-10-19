package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Enqueue fans a transition out to every eligible watcher of the section.
//
// ON CONFLICT DO NOTHING against UNIQUE(subscription_id, transition_id) makes
// re-delivery of the same transition a no-op, so a crash between the differ and
// here is harmless. This is the idempotency guarantee that lets Redis be a
// cache rather than a system of record.
//
// Subscriptions inside their cooldown window are skipped, which is how an
// oscillating section is prevented from spamming. UWFlow has no equivalent;
// at a daily cadence it does not need one, at 60s it would.
func (s *Store) Enqueue(
	ctx context.Context, sectionID int64, transitionID string, seatsOpen int, detectedAt time.Time,
) (int64, error) {
	const q = `
INSERT INTO notifications
  (subscription_id, section_id, transition_id, status, next_attempt_at, seats_open, detected_at)
SELECT sub.id, $1, $2, 'pending', NOW(), $3, $4
FROM subscriptions sub
WHERE sub.section_id = $1
  AND sub.active
  AND (sub.cooldown_until IS NULL OR sub.cooldown_until < NOW())
ON CONFLICT (subscription_id, transition_id) DO NOTHING`
	tag, err := s.pool.Exec(ctx, q, sectionID, transitionID, seatsOpen, detectedAt)
	if err != nil {
		return 0, fmt.Errorf("store: enqueue %s: %w", transitionID, err)
	}
	return tag.RowsAffected(), nil
}

// Delivery is a claimed notification ready to send.
type Delivery struct {
	ID             int64
	SubscriptionID int64
	Attempts       int
	// Throttles counts how many times this row has been requeued because of a
	// Discord rate limit. Tracked separately from Attempts so throttling does
	// not burn the delivery budget, while still being bounded.
	Throttles    int
	TransitionID string
	SeatsOpen    int
	DetectedAt   time.Time

	DiscordUserID     string
	DMChannelID       string
	FallbackChannelID string

	CourseCode  string
	CourseTitle string
	SectionName string
	TermCode    string
	// ClassNumber is the 4-digit code students enter on Quest to enroll.
	ClassNumber int
	Capacity    int
	Enrolled    int
	ObservedAt  time.Time
}

// Claim atomically leases up to limit pending notifications.
//
// FOR UPDATE SKIP LOCKED is the correct Postgres queue pattern: concurrent
// workers never block each other and never claim the same row, so the notifier
// scales by adding workers with no coordination.
//
// Note the send happens *after* this commits — never inside the transaction.
// UWFlow sends inside its transaction, where one SMTP error aborts the batch.
func (s *Store) Claim(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	q := `
WITH claimed AS (
  SELECT id FROM notifications
  WHERE status = 'pending' AND next_attempt_at <= NOW()
  ORDER BY next_attempt_at
  FOR UPDATE SKIP LOCKED
  LIMIT $1
)
UPDATE notifications n
SET status = 'sending',
    attempts = n.attempts + 1,
    lease_expires_at = NOW() + make_interval(secs => $2::double precision)
FROM claimed c
WHERE n.id = c.id
RETURNING n.id, n.subscription_id, n.attempts, n.throttles, n.transition_id,
          n.seats_open, n.detected_at`

	rows, err := s.pool.Query(ctx, q, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: claim: %w", err)
	}
	type claimRow struct {
		id, subID  int64
		attempts   int
		throttles  int
		transition string
		seats      int
		detected   time.Time
	}
	var claims []claimRow
	for rows.Next() {
		var c claimRow
		if err := rows.Scan(&c.id, &c.subID, &c.attempts, &c.throttles,
			&c.transition, &c.seats, &c.detected); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: claim scan: %w", err)
		}
		claims = append(claims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim rows: %w", err)
	}
	if len(claims) == 0 {
		return nil, nil
	}

	// Hydrate the claimed rows with everything needed to render a message.
	ids := make([]int64, len(claims))
	for i, c := range claims {
		ids[i] = c.id
	}
	const hq = `
SELECT n.id, u.discord_user_id, COALESCE(u.dm_channel_id, ''),
       COALESCE(u.fallback_channel_id, ''),
       sec.course_code, sec.course_title, sec.section_name, sec.term_code,
       sec.class_number, sec.capacity, sec.enrolled, sec.observed_at
FROM notifications n
JOIN subscriptions sub ON sub.id = n.subscription_id
JOIN users u           ON u.id = sub.user_id
JOIN sections sec      ON sec.id = n.section_id
WHERE n.id = ANY($1)`
	hrows, err := s.pool.Query(ctx, hq, ids)
	if err != nil {
		return nil, fmt.Errorf("store: claim hydrate: %w", err)
	}
	defer hrows.Close()

	byID := make(map[int64]*Delivery, len(claims))
	for hrows.Next() {
		var d Delivery
		if err := hrows.Scan(&d.ID, &d.DiscordUserID, &d.DMChannelID,
			&d.FallbackChannelID, &d.CourseCode, &d.CourseTitle, &d.SectionName,
			&d.TermCode, &d.ClassNumber, &d.Capacity, &d.Enrolled, &d.ObservedAt); err != nil {
			return nil, fmt.Errorf("store: claim hydrate scan: %w", err)
		}
		cp := d
		byID[d.ID] = &cp
	}
	if err := hrows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim hydrate rows: %w", err)
	}

	out := make([]Delivery, 0, len(claims))
	for _, c := range claims {
		d, ok := byID[c.id]
		if !ok {
			continue // row vanished (cascade delete); nothing to send
		}
		d.SubscriptionID = c.subID
		d.Attempts = c.attempts
		d.Throttles = c.throttles
		d.TransitionID = c.transition
		d.SeatsOpen = c.seats
		d.DetectedAt = c.detected
		out = append(out, *d)
	}
	return out, nil
}

// MarkSent records a successful delivery and applies the cooldown.
func (s *Store) MarkSent(ctx context.Context, id int64, via string, cooldown time.Duration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: mark sent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
UPDATE notifications
SET status = 'sent', sent_at = NOW(), delivered_via = $2,
    lease_expires_at = NULL, last_error = NULL
WHERE id = $1
RETURNING subscription_id`
	var subID int64
	if err := tx.QueryRow(ctx, q, id, via).Scan(&subID); err != nil {
		return fmt.Errorf("store: mark sent %d: %w", id, err)
	}

	// Cooldown suppresses repeat alerts for a flapping section, and notify_once
	// subscriptions retire themselves.
	const cq = `
UPDATE subscriptions
SET cooldown_until = NOW() + make_interval(secs => $2::double precision),
    active = CASE WHEN notify_once THEN FALSE ELSE active END
WHERE id = $1`
	if _, err := tx.Exec(ctx, cq, subID, cooldown.Seconds()); err != nil {
		return fmt.Errorf("store: mark sent cooldown %d: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: mark sent commit: %w", err)
	}
	return nil
}

// Retry reschedules a failed delivery with backoff.
func (s *Store) Retry(ctx context.Context, id int64, delay time.Duration, reason string) error {
	const q = `
UPDATE notifications
SET status = 'pending',
    next_attempt_at = NOW() + make_interval(secs => $2::double precision),
    lease_expires_at = NULL, last_error = $3
WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id, delay.Seconds(), truncate(reason, 500)); err != nil {
		return fmt.Errorf("store: retry %d: %w", id, err)
	}
	return nil
}

// DeadLetter retires a delivery that cannot succeed.
func (s *Store) DeadLetter(ctx context.Context, id int64, reason string) error {
	const q = `
UPDATE notifications
SET status = 'dead_letter', lease_expires_at = NULL, last_error = $2
WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id, truncate(reason, 500)); err != nil {
		return fmt.Errorf("store: dead letter %d: %w", id, err)
	}
	return nil
}

// RetryWithoutAttempt reschedules without consuming a delivery attempt. Used
// for Discord 429s, which reflect our send rate rather than a bad recipient.
//
// The decrement undoes Claim's increment, so a separate throttles counter is
// incremented to keep the retry bounded. Without it the row would cycle
// forever: Claim adds one, this subtracts one, and the MaxAttempts check could
// never trip.
func (s *Store) RetryWithoutAttempt(ctx context.Context, id int64, delay time.Duration, reason string) error {
	const q = `
UPDATE notifications
SET status = 'pending',
    next_attempt_at = NOW() + make_interval(secs => $2::double precision),
    attempts = GREATEST(attempts - 1, 0),
    throttles = throttles + 1,
    lease_expires_at = NULL, last_error = $3
WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id, delay.Seconds(), truncate(reason, 500)); err != nil {
		return fmt.Errorf("store: retry without attempt %d: %w", id, err)
	}
	return nil
}

// ReclaimStale returns leases that expired (the process died mid-send) to the
// pending pool.
//
// This is the boot-reconciliation step UWFlow lacks: its mailer only ever
// reacts to a notification and never drains a backlog, so anything queued while
// it was down waits for the next unrelated event.
func (s *Store) ReclaimStale(ctx context.Context) (int64, error) {
	const q = `
UPDATE notifications
SET status = 'pending', lease_expires_at = NULL,
    last_error = COALESCE(last_error, 'reclaimed: lease expired')
WHERE status = 'sending'
  AND (lease_expires_at IS NULL OR lease_expires_at < NOW())`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("store: reclaim stale: %w", err)
	}
	return tag.RowsAffected(), nil
}

// QueueDepth counts notifications waiting to be sent.
func (s *Store) QueueDepth(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE status = 'pending'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: queue depth: %w", err)
	}
	return n, nil
}

// NotificationStats summarises the outbox, for tests and diagnostics.
type NotificationStats struct {
	Pending    int64
	Sending    int64
	Sent       int64
	DeadLetter int64
}

// Stats returns outbox counts by status.
func (s *Store) Stats(ctx context.Context) (NotificationStats, error) {
	const q = `
SELECT
  COUNT(*) FILTER (WHERE status = 'pending'),
  COUNT(*) FILTER (WHERE status = 'sending'),
  COUNT(*) FILTER (WHERE status = 'sent'),
  COUNT(*) FILTER (WHERE status = 'dead_letter')
FROM notifications`
	var st NotificationStats
	err := s.pool.QueryRow(ctx, q).Scan(&st.Pending, &st.Sending, &st.Sent, &st.DeadLetter)
	if err != nil {
		return st, fmt.Errorf("store: stats: %w", err)
	}
	return st, nil
}

// SentLatencies returns observed detected->sent durations, for benchmarking.
func (s *Store) SentLatencies(ctx context.Context) ([]time.Duration, error) {
	const q = `
SELECT EXTRACT(EPOCH FROM (sent_at - detected_at))
FROM notifications WHERE status = 'sent' AND sent_at IS NOT NULL
ORDER BY sent_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: latencies: %w", err)
	}
	defer rows.Close()
	var out []time.Duration
	for rows.Next() {
		var secs float64
		if err := rows.Scan(&secs); err != nil {
			return nil, err
		}
		out = append(out, time.Duration(secs*float64(time.Second)))
	}
	return out, rows.Err()
}

// CountByTransition counts rows for one transition, used to assert dedup.
func (s *Store) CountByTransition(ctx context.Context, transitionID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE transition_id = $1`, transitionID).Scan(&n)
	return n, err
}

// Truncate empties all tables. Tests only.
func (s *Store) Truncate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`TRUNCATE notifications, subscriptions, sections, users RESTART IDENTITY CASCADE`)
	return err
}

// WithTx runs fn inside a transaction.
func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
