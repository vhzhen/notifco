# notifco — Design

A Discord bot that lets UW students search courses, watch sections, and get DMed the
moment a watched section goes from full to having a seat.

Derived from the UWFlow analysis in [NOTES.md](./NOTES.md). The design target is
explicit: **UWFlow informs a user up to ~28 hours after a seat opens. notifco targets
under 60 seconds.**

---

## 1. Design goals

| Goal | Target |
|---|---|
| Detection lag (seat opens upstream → notifco observes it) | ≤ 60 s, ≤ 15 s for hot sections |
| Delivery lag (observed → DM sent) | p99 < 2 s for a single watcher |
| Fan-out | 10k subscriptions on one section drained without loss |
| Upstream politeness | Hard ceiling 120 req/min, never exceeded |
| Duplicate alerts | Exactly one per subscription per open-transition, across restarts |
| Cost | $0. No paid services. |

Non-goals: registering users in courses, anything touching Quest, waitlist position
(upstream exposes no waitlist field), historical analytics, a web UI.

---

## 2. Architecture

```
                         ┌────────────────────────────────────────────┐
                         │        UW Open Data API v3 (public)        │
                         │  ClassSchedules/{term}[/{subj}/{cat}]      │
                         │  Courses/{term}  ·  Terms  ·  120 req/min  │
                         └────────────────────┬───────────────────────┘
                                              │ x-api-key
                      ┌───────────────────────▼────────────────────────┐
                      │  source.CourseSource  (interface)              │
                      │   ├─ uwapi.Client   real, token-bucket + backoff│
                      │   └─ mock.Source    fixtures, used by tests     │
                      └───────────────────────┬────────────────────────┘
                                              │ []SectionState
   ┌──────────────────────────────────────────▼──────────────────────────────────┐
   │ POLLER                                                                       │
   │  scheduler ──► job queue (priority heap, adaptive interval)                  │
   │                   │                                                          │
   │                   ├─► bulk sweep job   : 1 request = whole term              │
   │                   └─► hot section jobs : 1 request = one course              │
   │                           │                                                  │
   │                      worker pool (N) ──► shared rate limiter (120/min)       │
   └───────────────────────────┬──────────────────────────────────────────────────┘
                               │ observed states
   ┌───────────────────────────▼──────────────────────────────────────────────────┐
   │ DIFFER                            ┌──────────── Redis ───────────────┐      │
   │  atomic CAS via Lua script ◄─────►│ state:{term}:{class} → {tot,cap} │      │
   │  fires only on full → open        │ epoch:{term}:{class} → counter   │      │
   │  bumps epoch ⇒ transition_id      └──────────────────────────────────┘      │
   └───────────────────────────┬──────────────────────────────────────────────────┘
                               │ Transition{section, transition_id}
   ┌───────────────────────────▼──────────────────────────────────────────────────┐
   │ NOTIFIER (transactional outbox)   ┌─────────── Postgres ─────────────┐      │
   │  fan-out INSERT .. ON CONFLICT ──►│ users · sections · subscriptions │      │
   │  claim: FOR UPDATE SKIP LOCKED ◄──│ notifications (status, attempts) │      │
   │  retry: exp backoff + jitter      └──────────────────────────────────┘      │
   │  dead-letter after N attempts                                                │
   └───────────────────────────┬──────────────────────────────────────────────────┘
                               │
   ┌───────────────────────────▼──────────────────────────────────────────────────┐
   │ DISCORD (discordgo)                                                          │
   │  slash cmds: /search · /watch · /unwatch · /watchlist                        │
   │  deliver: DM ──(50007 DMs closed)──► fallback channel ping                   │
   └──────────────────────────────────────────────────────────────────────────────┘
                               │
                      Prometheus /metrics  +  /healthz
```

Processes: one binary, `notifco`, with subcommands `bot`, `poller`, and `all`.
`all` runs both in one process, which is what the free-tier deployment uses.

---

## 3. Upstream access layer

### The interface

Everything above the client talks to this, so the real API and the mock are
interchangeable and the whole system is testable without a key:

```go
// internal/source/source.go
type TermCode string

type SectionState struct {
    TermCode    TermCode
    ClassNumber int       // unique within term — the identity key
    CourseCode  string    // "cs341", lowercased subject+catalog
    SectionName string    // "LEC 001"
    Component   string    // LEC | TUT | LAB | TST
    Capacity    int       // maxEnrollmentCapacity
    Enrolled    int       // enrolledStudents
    IsOnline    bool
    ObservedAt  time.Time // set by us; upstream has no timestamp
}

func (s SectionState) IsFull() bool      { return s.Enrolled >= s.Capacity }
func (s SectionState) SeatsOpen() int    { return max(0, s.Capacity-s.Enrolled) }

type Course struct {
    TermCode   TermCode
    CourseCode string
    CourseID   string
    Title      string
    Subject    string
    Catalog    string
}

type CourseSource interface {
    // CurrentTerms returns terms worth polling (current + next).
    CurrentTerms(ctx context.Context) ([]TermCode, error)
    // Courses lists a term's catalog. One request, ~7.1 MB, 7894 offerings. Seeds search.
    Courses(ctx context.Context, term TermCode) ([]Course, error)
    // ScheduledCourseIDs returns the courseIds that have classes in a term.
    // One request; 2179 of 7295 for term 1269. Bounds the polling universe.
    // NOTE: this is ClassSchedules/{term}, which returns ONLY ids, not enrollment.
    ScheduledCourseIDs(ctx context.Context, term TermCode) ([]string, error)
    // CourseSections returns every section of one course, with enrollment.
    // One request covers all sections — the unit the poller schedules on.
    CourseSections(ctx context.Context, term TermCode, subject, catalog string) ([]SectionState, error)
}
```

Implementations: `internal/source/uwapi` (real) and `internal/source/mock` (JSON
fixtures + a scriptable timeline for tests). Selected by `NOTIFCO_SOURCE=uwapi|mock`;
defaults to `mock` when `UW_API_KEY` is empty so the repo runs out of the box.

### Client behaviour

- `x-api-key` header, base `https://openapi.data.uwaterloo.ca/v3`.
- **Token bucket** (`golang.org/x/time/rate`), one instance shared by every worker,
  default 110 req/min with burst 10 — deliberately under the published 120 to leave
  headroom, same reasoning as UWFlow's 550 ms but enforced rather than slept.
- **Backoff**, fixing the two gaps in NOTES.md §1: retry on **429 and 5xx**, honour
  `Retry-After` when present, otherwise genuinely exponential with jitter
  (`base 1s · 2^attempt`, cap 60s, ±20% jitter), 5 attempts.
- 404 on a `(course, term)` pair is a normal empty result, not an error.
- Upstream quirks from NOTES.md §1 applied at the edge: drop `Capacity >= 9999` dummy
  classes, drop `courseComponent == nil`, dedupe on `(classNumber, termCode)`, derive
  `IsOnline` from the middle digit of the zero-padded section number, and map
  cross-listed `courseOfferNumber` back to the right course code.
- Never logs the key.

---

## 4. Poller

### Why the strategy differs most from UWFlow

> **CORRECTED 2026-10-11 against the live API — see [VERIFIED.md](./VERIFIED.md) §2.**
> This section originally proposed a bulk sweep built on `ClassSchedules/{termCode}`,
> which I assumed returned every class in a term. **It does not.** It returns a flat
> array of 2,179 courseId *strings* with no enrollment data. There is **no bulk
> enrollment endpoint**. Enrollment is per-course only, so UWFlow's per-course approach
> was the only option available, not a missed optimisation. The strategy below is the
> corrected one.

The win over UWFlow is therefore **scope and concurrency, not request count per course**.
UWFlow sweeps 4 terms × every course unconditionally and serially, ~25–28k requests in
~4 hours. notifco polls only what someone is actually watching.

The decisive efficiency fact (VERIFIED.md §3): **one request returns every section of a
course.** A user watching `LEC 001`, `LAB 101` and `TST 201` of CS 341 costs *one*
request, not three. So the poller deduplicates watched sections to distinct
`(term, course)` pairs before scheduling anything.

**Tier 1 — watched sweep.** One `CourseSections` call per distinct watched course per
tick (default 60s), via `ClassSchedules/{term}/{subject}/{catalog}` — which takes the
course code directly, no courseId resolution. Cost = distinct watched courses, which is
bounded by how many courses users actually watch, not by subscription count.

**Tier 2 — hot courses.** Courses containing a full, watched, recently-churning section
get a tighter interval (default 15s). Capped by `NOTIFCO_HOT_MAX` so the budget stays
bounded.

Both tiers emit jobs into one priority queue drained by one worker pool through one rate
limiter, so the global ceiling holds however the tiers are tuned.

**Measured budget** (VERIFIED.md §6). A full-term sweep is 2,179 requests. Watched-only
at a 60s target supports ~120 distinct courses at the documented 120 req/min, and ~400 at
the ~409 req/min actually observed. Default pacing stays at the documented 120 because
the docs call the limit experimental; the observed headroom is treated as safety margin,
not budget.

`ClassSchedules/{termCode}` is retained but repurposed: one request enumerates which
courses have classes this term (2,179 of 7,295 catalog courseIds), bounding the polling
universe and validating `/watch` targets.

### Worker pool

```go
type Job struct {
    Kind     JobKind    // JobBulkSweep | JobHotSection
    Term     TermCode
    Subject  string     // hot only
    Catalog  string     // hot only
    Priority int        // watcher count + heat
    DueAt    time.Time
}
```

`scheduler` goroutine owns a `container/heap` ordered by `DueAt` then `Priority`, pushes
due jobs onto a buffered channel, `N` workers (`NOTIFCO_POLL_WORKERS`, default 4) read,
acquire a rate-limiter token, call the source, hand results to the differ, then reschedule
the job with its next interval. Context cancellation drains cleanly.

### Adaptive interval

Per target, widen when nothing changes, tighten when it does:

```
next = base * 2^min(consecutiveNoChange, 5)        // 60s → 32 min ceiling
if changed            : next = base                // snap back on any movement
if full && watched    : next = min(next, hotInterval)
if registrationPeriod : next = next / 2            // near term start
next = clamp(next, minInterval, maxInterval) ± 10% jitter
```

Jitter prevents every target from realigning into a synchronised burst. The registration
window is derived from `ImportantDates`, the same endpoint UWFlow uses for term bounds.

An unwatched section is not polled at all — Tier 1 observes it for free as a side effect
of the bulk sweep, but it generates no Tier 2 work and no differ state.

---

## 5. Differ

### Transition rule

Fires only on a full → open edge, with UWFlow's capacity bug (NOTES.md §3) corrected:

```go
func IsOpening(prev, cur SectionState) bool {
    wasFull := prev.Enrolled >= prev.Capacity   // old vs OLD capacity
    nowOpen := cur.Enrolled  <  cur.Capacity
    return wasFull && nowOpen
}
```

Comparing `prev.Enrolled` against `prev.Capacity` is the whole fix. It makes the
"department added 20 seats to a full section" case fire, which UWFlow silently drops.

Rules around it:

- **No prior state → store, never fire.** A cold start or a fresh Redis must not alert
  10k users that an already-open section is open.
- **Still-open → no fire.** Edge-triggered, not level-triggered.
- **Open → full** just updates state.
- **Capacity shrink** is handled naturally by the same predicate.

### State storage

Redis, deliberately separate from the Postgres serving data so detection is decoupled
from the catalog write path — the structural coupling that stops UWFlow from polling
faster:

| Key | Type | Value | TTL |
|---|---|---|---|
| `notifco:state:{term}:{class}` | hash | `enrolled`, `capacity`, `observed_at` | 90 d |
| `notifco:epoch:{term}:{class}` | string | monotonic open-transition counter | 90 d |

### Atomicity and the idempotency key

Compare-and-set plus epoch bump run in **one Lua script**, so two workers observing the
same change cannot both fire:

```
-- KEYS[1]=state  KEYS[2]=epoch   ARGV[1]=enrolled ARGV[2]=capacity ARGV[3]=observed_at
-- returns {fired, epoch}
local prev = redis.call('HMGET', KEYS[1], 'enrolled', 'capacity')
redis.call('HSET', KEYS[1], 'enrolled', ARGV[1], 'capacity', ARGV[2], 'observed_at', ARGV[3])
redis.call('EXPIRE', KEYS[1], 7776000)
if not prev[1] then return {0, 0} end                                  -- cold: store only
local wasFull = tonumber(prev[1]) >= tonumber(prev[2])
local nowOpen = tonumber(ARGV[1])  <  tonumber(ARGV[2])
if wasFull and nowOpen then
  local e = redis.call('INCR', KEYS[2]); redis.call('EXPIRE', KEYS[2], 7776000)
  return {1, e}
end
return {0, 0}
```

The returned epoch yields a **stable, restart-safe** identity for the event:

```
transition_id = "{term}:{classNumber}:{epoch}"      e.g. "1259:4821:7"
```

Paired with `UNIQUE(subscription_id, transition_id)` in Postgres, this is what makes the
notifier idempotent across restarts, crashes, and redeliveries. Redis is a cache for
state, but the *dedup decision* is enforced by a Postgres constraint, so losing Redis
cannot cause duplicate sends — only missed detection until the next poll reseeds state.

### Re-notification control

UWFlow has none, which is survivable daily and a spam cannon at 60s (NOTES.md §3). Two
guards:

- **Cooldown**: `subscriptions.cooldown_until`, default 30 min. A section that flaps
  full/open/full/open notifies once per cooldown window.
- **Auto-unsubscribe**: opt-in per subscription (`notify_once`, default off). When set,
  a successful delivery deactivates the subscription.

---

## 6. Notifier

A transactional outbox. Discord sends happen **outside** the DB transaction, unlike
UWFlow which sends inside it (NOTES.md §5).

### Fan-out

On a transition, one statement enqueues every eligible watcher:

```sql
INSERT INTO notifications (subscription_id, transition_id, section_id, status, next_attempt_at)
SELECT s.id, $2, s.section_id, 'pending', NOW()
FROM subscriptions s
WHERE s.section_id = $1
  AND s.active
  AND (s.cooldown_until IS NULL OR s.cooldown_until < NOW())
ON CONFLICT (subscription_id, transition_id) DO NOTHING;
```

`ON CONFLICT DO NOTHING` makes re-delivery of the same transition a no-op, so a crash
between differ and notifier is harmless.

### Claiming

`SELECT ... FOR UPDATE SKIP LOCKED` — the correct Postgres queue pattern, lets workers
scale without contention and without a thundering herd:

```sql
UPDATE notifications SET status='sending', attempts=attempts+1, lease_expires_at=NOW()+interval '2 min'
WHERE id IN (
  SELECT id FROM notifications
  WHERE status='pending' AND next_attempt_at <= NOW()
  ORDER BY next_attempt_at
  FOR UPDATE SKIP LOCKED
  LIMIT $1
) RETURNING ...;
```

### Retry and failure handling

| Outcome | Action |
|---|---|
| Sent | `status='sent'`, `sent_at=NOW()`, record latency, apply cooldown |
| DMs closed (Discord 50007) | fall back to channel ping; if that fails → retry |
| 429 from Discord | respect `Retry-After`, requeue, do not count as a hard attempt |
| 5xx / network | `status='pending'`, `next_attempt_at = NOW() + backoff(attempts)` |
| Unknown user / 404 | `status='dead_letter'`, deactivate subscription |
| `attempts >= max` (default 6) | `status='dead_letter'`, reason recorded |

`backoff(n) = min(2^n seconds, 10 min) ± 20% jitter`.

### Restart safety

Three properties UWFlow lacks:

1. **Boot reconciliation.** On start, rows stuck in `sending` with an expired lease are
   reset to `pending`. UWFlow's mailer never drains a backlog on boot.
2. **Idempotent by construction.** `UNIQUE(subscription_id, transition_id)` means a
   duplicate enqueue is impossible, and a send that succeeded but failed to mark is
   bounded to one redundant message rather than UWFlow's indefinite resend.
3. **Nothing sent inside a transaction.** Claim → commit → send → mark.

### Discord fan-out realities

Discord's global budget is ~50 req/s and opening a DM channel is tighter. 10k DMs is
therefore minutes of wall clock, not seconds — a hard platform limit, not a design flaw.
Mitigations:

- **Group per user**: if several of a user's watched sections open together, send one
  message listing them. UWFlow does the same thing via `array_agg`.
- **Cache DM channel IDs** in Postgres to skip the channel-open call.
- **Bounded sender concurrency** (`NOTIFCO_NOTIFY_WORKERS`, default 8) under
  discordgo's own limiter.
- Report honestly: the load test measures time-to-last-message for 10k, and the README
  states both p50 and p99, not just the best case.

---

## 7. Data model

### Postgres

```sql
CREATE TABLE users (
  id                 BIGSERIAL PRIMARY KEY,
  discord_user_id    TEXT        NOT NULL UNIQUE,
  dm_channel_id      TEXT,                         -- cached, avoids a create-DM call
  dms_blocked_at     TIMESTAMPTZ,                  -- set on 50007
  fallback_guild_id  TEXT,                         -- where to ping if DMs are closed
  fallback_channel_id TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Catalog cache. Powers /search and gives subscriptions a stable FK.
CREATE TABLE sections (
  id            BIGSERIAL PRIMARY KEY,
  term_code     TEXT    NOT NULL,
  class_number  INT     NOT NULL,
  course_code   TEXT    NOT NULL,                  -- "cs341"
  course_title  TEXT    NOT NULL DEFAULT '',
  section_name  TEXT    NOT NULL,                  -- "LEC 001"
  component     TEXT    NOT NULL,
  capacity      INT     NOT NULL,
  enrolled      INT     NOT NULL,
  is_online     BOOLEAN NOT NULL DEFAULT FALSE,
  observed_at   TIMESTAMPTZ NOT NULL,
  UNIQUE (term_code, class_number)                 -- the real upstream key
);
CREATE INDEX ON sections (course_code, term_code);

CREATE TABLE subscriptions (
  id             BIGSERIAL PRIMARY KEY,
  user_id        BIGINT NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
  section_id     BIGINT NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
  active         BOOLEAN NOT NULL DEFAULT TRUE,
  notify_once    BOOLEAN NOT NULL DEFAULT FALSE,
  cooldown_until TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (user_id, section_id)                     -- same guard UWFlow uses
);
CREATE INDEX ON subscriptions (section_id) WHERE active;

CREATE TYPE notification_status AS ENUM ('pending','sending','sent','dead_letter');

CREATE TABLE notifications (
  id               BIGSERIAL PRIMARY KEY,
  subscription_id  BIGINT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
  section_id       BIGINT NOT NULL REFERENCES sections(id)      ON DELETE CASCADE,
  transition_id    TEXT   NOT NULL,                -- "{term}:{class}:{epoch}"
  status           notification_status NOT NULL DEFAULT 'pending',
  attempts         INT    NOT NULL DEFAULT 0,
  next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  lease_expires_at TIMESTAMPTZ,
  last_error       TEXT,
  delivered_via    TEXT,                           -- 'dm' | 'channel'
  detected_at      TIMESTAMPTZ NOT NULL,           -- when the differ saw the opening
  sent_at          TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (subscription_id, transition_id)          -- the idempotency guarantee
);
CREATE INDEX ON notifications (status, next_attempt_at) WHERE status = 'pending';
```

This is the durable notification log UWFlow does not have: attempts, error, transport,
dead-letter state, and both timestamps needed to compute alert latency.

### Redis

Only ephemeral last-seen state and epochs (§5), plus a `notifco:bulk:{term}` response
ETag-ish hash to skip differ work when a sweep returns an unchanged payload. Redis is
replaceable: losing it degrades detection for one poll cycle and cannot cause duplicates.

---

## 8. Discord interface

| Command | Behaviour |
|---|---|
| `/search <code>` | Searches the `sections` cache by course code (and title, trigram). Shows each section with `enrolled/capacity`, component, online flag, and an "as of" timestamp. Ephemeral. |
| `/watch <code> <section>` | Resolves `(course, section)` for the active term, upserts user + subscription. Autocompletes `<section>` from the cache. Confirms with current seat state and the staleness disclaimer. |
| `/unwatch <code> [section]` | Deactivates one subscription, or all sections of the course when `section` is omitted. |
| `/watchlist` | Lists the caller's active subscriptions with live seat counts. Ephemeral. |

Behaviour details:

- Course codes are normalised: `CS 341`, `cs341`, `CS-341` all parse to `cs341`.
- Section accepts `LEC 001`, `001`, or `1`.
- All responses deferred (`DeferredChannelMessageWithSource`) — Discord's 3s interaction
  budget is tight and DB work should not race it.
- **Every seat figure carries "as of <ts>, verify on Quest"**, which is the disclaimer the
  UW OpenData project explicitly asks integrators to surface (NOTES.md §7).
- **Permissions**: the bot needs only `applications.commands` plus `Send Messages` in a
  fallback channel. No message-content intent, no privileged intents, no member scraping.
- **Fallback ping** names the user by mention in the configured channel and says nothing
  about which course — the section is disclosed only via DM, so a closed-DM user does not
  get their watchlist leaked into a public channel.

---

## 9. Running it, for free

Primary target is an **Oracle Cloud Always Free** ARM VM (4 OCPU / 24 GB, genuinely
always free) running everything in docker-compose: `notifco all`, Postgres, Redis. No
managed services, nothing to bill.

```
docker-compose.yml        postgres:16-alpine + redis:7-alpine + notifco
deploy/fly/fly.toml       alt: Fly.io app; needs external PG/Redis (Neon/Upstash free)
deploy/oracle/README.md   VM provisioning + systemd unit
```

Fly.io is included as a secondary path but flagged: its free allowance has changed and a
Fly deploy needs external free-tier Postgres and Redis, so the Oracle VM is the one I'd
actually recommend for $0.

Config, all env vars, no secrets in the repo:

```
DISCORD_TOKEN           required
DISCORD_APP_ID          required
UW_API_KEY              optional — absent ⇒ mock source
NOTIFCO_SOURCE          uwapi | mock          (default: mock if no key)
DATABASE_URL            postgres://...
REDIS_URL               redis://...
NOTIFCO_POLL_INTERVAL   60s
NOTIFCO_HOT_INTERVAL    15s
NOTIFCO_HOT_MAX         50
NOTIFCO_POLL_WORKERS    4
NOTIFCO_NOTIFY_WORKERS  8
NOTIFCO_RATE_LIMIT      120          # req/min, the documented cap (~409 observed; margin, not budget)
NOTIFCO_COOLDOWN        30m
METRICS_ADDR            :9090
```

### Plugging in the real API key

The one manual step. `UW_API_KEY` is the only thing standing between the mock and live
data:

```sh
curl -X POST https://openapi.data.uwaterloo.ca/v3/account/register \
  -H 'content-type: application/x-www-form-urlencoded' \
  --data 'email=YOU@uwaterloo.ca&project=notifco&uri=https://github.com/YOU/notifco'

curl -X POST https://openapi.data.uwaterloo.ca/v3/account/confirm \
  -F email=YOU@uwaterloo.ca -F code=CODE_FROM_EMAIL     # note: no boundary header
```

Then `UW_API_KEY=<key>` and `NOTIFCO_SOURCE=uwapi`. Nothing else changes — same
interface, same tests.

---

## 10. Metrics

Prometheus at `METRICS_ADDR/metrics`, the four the brief asks for plus what is needed to
debug them.

| Metric | Type | Meaning |
|---|---|---|
| `notifco_polls_total{kind,result}` | counter | polls/min is `rate()` of this |
| `notifco_alert_latency_seconds` | histogram | **detected → message sent** |
| `notifco_active_users` | gauge | users with ≥1 active subscription |
| `notifco_active_subscriptions` | gauge | active subscription rows |
| `notifco_api_requests_total{endpoint,code}` | counter | upstream calls by status |
| `notifco_rate_limit_wait_seconds` | histogram | time blocked on the token bucket |
| `notifco_transitions_total` | counter | full→open edges detected |
| `notifco_notifications_total{status,via}` | counter | sent / dead-lettered, DM vs channel |
| `notifco_queue_depth` | gauge | pending notifications |
| `notifco_poll_cycle_seconds` | histogram | sweep duration |

**Honest caveat on alert latency.** Upstream exposes no "last updated" field on a class
(NOTES.md §1), so the true seat-open instant is unknowable. `alert_latency_seconds`
measures **observed → sent**, which is what notifco controls. Detection lag is *bounded*
by the poll interval and reported separately as a configured bound, not a measurement.
The README states this rather than claiming an end-to-end number it cannot prove.

---

## 11. Testing

| Layer | What it proves |
|---|---|
| **Differ unit tests** | Table-driven over every transition: full→open fires; open→open silent; open→full silent; full→full silent; cold state stores without firing; capacity raised while full **does** fire (the UWFlow bug); capacity shrink; zero-capacity; enrolled > capacity overflow. |
| **Dedup unit tests** | Same transition applied twice yields one row; concurrent differ calls on one change produce one epoch bump (Lua atomicity); epoch increments across distinct transitions; restart mid-flight sends exactly once. |
| **Integration test** | `mock.Source` driving a scripted timeline through poller → differ → notifier → fake Discord, against real Postgres + Redis (docker-compose or testcontainers). Asserts exactly one message per subscriber, correct content, cooldown suppression, DM→channel fallback on 50007. |
| **Load test** | 10k subscriptions on one section, one opening. Asserts zero duplicates, zero losses, and records p50/p99 time-to-delivery and time-to-last-message into the README benchmark table. |
| **Failure injection** | Discord 429/5xx, Redis down mid-run, Postgres restart, upstream 429 storm. Asserts no duplicates and no permanent loss. |

CI (GitHub Actions): `go vet`, `staticcheck`, `go test -race ./...`, `golangci-lint`,
build, with Postgres and Redis service containers. No live API key, so CI runs entirely
on the mock — which is the point of the interface.

---

## 12. Repo layout

```
notifco/
├── cmd/notifco/main.go           # bot | poller | all
├── internal/
│   ├── config/                   # env parsing
│   ├── source/
│   │   ├── source.go             # CourseSource interface + SectionState
│   │   ├── uwapi/                # real client: ratelimit, backoff, quirks
│   │   └── mock/                 # fixtures + scriptable timeline
│   ├── poller/                   # scheduler, priority queue, worker pool
│   ├── differ/                   # transition rule + Redis CAS/Lua
│   ├── notifier/                 # outbox, claim, retry, dead-letter
│   ├── discord/                  # slash commands, delivery, fallback
│   ├── store/                    # Postgres (pgx), queries, migrations runner
│   ├── state/                    # Redis client
│   └── metrics/                  # Prometheus collectors
├── migrations/                   # 0001_init.sql ...
├── deploy/{oracle,fly}/
├── test/{integration,load}/
├── .github/workflows/ci.yml
├── docker-compose.yml
├── Makefile                      # build test lint migrate up down load-test
├── NOTES.md   DESIGN.md   README.md
└── LICENSE                       # MIT, independent work
```

---

## 13. Key decisions, and the risk I am carrying

| Decision | Why |
|---|---|
| ~~Bulk term endpoint as the default sweep~~ | **Retracted.** No bulk enrollment endpoint exists (VERIFIED.md §2). Poll per course, deduplicated to distinct watched courses. |
| One request per course, not per section | Measured: a single call returns every section of a course. Watching 3 sections of CS 341 costs 1 request. |
| Default pacing 120 req/min despite ~409 observed | The docs call the limit experimental and send no rate-limit headers. Observed headroom is margin, not budget. |
| Redis for differ state, not Postgres | Decouples detection from the catalog write path — the coupling that caps UWFlow's poll rate. |
| Idempotency enforced by a Postgres unique constraint | Redis loss degrades detection but can never cause duplicate sends. |
| `(subscription_id, transition_id)` as the key | Stable across restarts, unlike a timestamp or payload hash. |
| `FOR UPDATE SKIP LOCKED` instead of LISTEN/NOTIFY as the queue | UWFlow's LISTEN/NOTIFY is a great *doorbell* but a poor *queue*: it has no retry, no backlog drain, no attempt tracking. Keeping the doorbell as an optimisation, the outbox as the source of truth. |
| One binary, `all` mode | Free tier means one small VM. Splitting processes is a scaling option, not a starting point. |
| Mock source is the default | The repo must be runnable, testable, and CI-green with no API key. |

**The risk, now resolved.** The earlier draft rested on `ClassSchedules/{termCode}` being
a usable bulk endpoint, which I could not verify without a key. I have since registered a
key and tested it: **the assumption was wrong**, and the escape hatch I designed for is
now the only path. The measured consequence is that a 60s interval supports roughly 120
distinct watched courses at the documented rate — almost exactly the ~100 I predicted in
the original version of this section.

Remaining unverified: I never triggered a 429, so whether it carries `Retry-After` is
unconfirmed. The backoff handles both cases.

---

## 14. Assumptions

1. Only the public Open Data API v3 is used. Quest, SourceAPI, and Shibboleth are never
   touched, and no student credentials are collected.
2. `enrolledStudents < maxEnrollmentCapacity` is a sufficient definition of "a seat is
   open". Upstream exposes no waitlist or reserved-capacity field, so a section can
   report seats that a given student cannot actually take (reserve caps). The disclaimer
   covers this.
3. Upstream refresh frequency is unknown and uncontrollable. Polling faster than UW
   refreshes buys nothing; 60s is chosen as polite and almost certainly at or below
   upstream granularity.
4. 120 req/min is the real ceiling and is per key.
5. Current + next term is sufficient scope. UWFlow imports four terms; nobody watches a
   section in a term that already ended.
6. Discord's own rate limits, not notifco, dominate large fan-out wall time.
7. One bot instance. Horizontal scale would need a distributed lock on the poller; the
   differ and notifier are already safe for it.
8. Users accept ephemeral slash-command replies and a public mention-only fallback when
   DMs are closed.
9. MIT for notifco. Independent implementation; no UWFlow code, SQL, or templates copied.
10. Section identity is `(term_code, class_number)`, per NOTES.md §1.

---

**Status: awaiting approval before Step 3 (build).**
