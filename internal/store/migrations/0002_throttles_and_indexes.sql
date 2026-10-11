-- Bound throttled retries and speed up the reclaim path.
--
-- Throttled deliveries (Discord 429) deliberately do not consume a delivery
-- attempt, because the cause is our send rate rather than a bad recipient.
-- Without a separate counter that made the retry unbounded: Claim incremented
-- attempts and RetryWithoutAttempt decremented it again, so the net was zero
-- and the MaxAttempts check could never trip.
ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS throttles INTEGER NOT NULL DEFAULT 0;

-- ReclaimStale filters on status='sending'; without this it sequentially scans
-- the whole notification log.
CREATE INDEX IF NOT EXISTS notifications_sending_idx
    ON notifications (lease_expires_at) WHERE status = 'sending';

-- Fan-out and cascade deletes both filter by section.
CREATE INDEX IF NOT EXISTS notifications_section_idx
    ON notifications (section_id);

CREATE INDEX IF NOT EXISTS notifications_subscription_idx
    ON notifications (subscription_id);
