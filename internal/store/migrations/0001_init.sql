-- notifco initial schema.
--
-- Design notes:
--   * sections identity is (term_code, class_number), the only stable key the
--     upstream API offers.
--   * notifications is a transactional outbox, claimed with FOR UPDATE SKIP
--     LOCKED. UNIQUE(subscription_id, transition_id) is the idempotency
--     guarantee: it makes a duplicate enqueue impossible, so a crash between
--     differ and notifier is harmless and Redis loss can never double-send.
--   * Unlike UWFlow, this keeps a durable notification log with attempts,
--     errors, transport, dead-letter state, and both timestamps needed to
--     compute alert latency.

CREATE TABLE IF NOT EXISTS users (
    id                  BIGSERIAL PRIMARY KEY,
    discord_user_id     TEXT        NOT NULL UNIQUE,
    dm_channel_id       TEXT,                 -- cached; avoids a create-DM call
    dms_blocked_at      TIMESTAMPTZ,          -- set when Discord returns 50007
    fallback_guild_id   TEXT,
    fallback_channel_id TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Catalog cache. Powers /search and gives subscriptions a stable FK.
CREATE TABLE IF NOT EXISTS sections (
    id            BIGSERIAL PRIMARY KEY,
    term_code     TEXT        NOT NULL,
    class_number  INTEGER     NOT NULL,
    course_code   TEXT        NOT NULL,       -- "cs341"
    course_title  TEXT        NOT NULL DEFAULT '',
    section_name  TEXT        NOT NULL,       -- "LEC 001"
    component     TEXT        NOT NULL DEFAULT '',
    capacity      INTEGER     NOT NULL DEFAULT 0,
    enrolled      INTEGER     NOT NULL DEFAULT 0,
    is_online     BOOLEAN     NOT NULL DEFAULT FALSE,
    observed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT sections_term_class_key UNIQUE (term_code, class_number)
);

CREATE INDEX IF NOT EXISTS sections_course_idx ON sections (course_code, term_code);

CREATE TABLE IF NOT EXISTS subscriptions (
    id             BIGSERIAL PRIMARY KEY,
    user_id        BIGINT      NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    section_id     BIGINT      NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    active         BOOLEAN     NOT NULL DEFAULT TRUE,
    notify_once    BOOLEAN     NOT NULL DEFAULT FALSE,
    cooldown_until TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT subscriptions_user_section_key UNIQUE (user_id, section_id)
);

CREATE INDEX IF NOT EXISTS subscriptions_active_section_idx
    ON subscriptions (section_id) WHERE active;

DO $$ BEGIN
    CREATE TYPE notification_status AS ENUM ('pending', 'sending', 'sent', 'dead_letter');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS notifications (
    id               BIGSERIAL PRIMARY KEY,
    subscription_id  BIGINT              NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    section_id       BIGINT              NOT NULL REFERENCES sections(id)      ON DELETE CASCADE,
    transition_id    TEXT                NOT NULL,   -- "{term}:{class}:{epoch}"
    status           notification_status NOT NULL DEFAULT 'pending',
    attempts         INTEGER             NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
    lease_expires_at TIMESTAMPTZ,
    last_error       TEXT,
    delivered_via    TEXT,                           -- 'dm' | 'channel'
    seats_open       INTEGER             NOT NULL DEFAULT 0,
    detected_at      TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
    sent_at          TIMESTAMPTZ,
    created_at       TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
    CONSTRAINT notifications_dedup_key UNIQUE (subscription_id, transition_id)
);

-- The claim query's covering index.
CREATE INDEX IF NOT EXISTS notifications_claimable_idx
    ON notifications (next_attempt_at)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS notifications_sent_idx
    ON notifications (sent_at) WHERE status = 'sent';
