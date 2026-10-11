# notifco

A Discord bot for University of Waterloo students. Search courses, watch sections, and
get a DM the moment a full section has a seat open up.

[![CI](https://github.com/vhzhen/notifco/actions/workflows/ci.yml/badge.svg)](https://github.com/vhzhen/notifco/actions/workflows/ci.yml)

## Why

[UWFlow](https://github.com/UWFlow/uwflow) already has "notify me when a course frees
up", but it rides on a nightly full-catalog importer: cron fires once a day, the sweep
takes ~4 hours, so a student hears about an open seat **up to ~28 hours later**. A seat
that opens and gets retaken within a day is often never reported at all.

notifco targets **under 60 seconds**, and fixes a detection bug in UWFlow that silently
drops one of the most valuable alerts (see [Correctness](#correctness)).

Full analysis of UWFlow is in [NOTES.md](./NOTES.md); the design is in
[DESIGN.md](./DESIGN.md); live API measurements are in [VERIFIED.md](./VERIFIED.md).

## Architecture

```
                         ┌─────────────────────────────────────────────┐
                         │      UW Open Data API v3 (public only)      │
                         │  Courses/{term}            1 req → 7.1 MB   │
                         │  ClassSchedules/{term}      courseIds only   │
                         │  ClassSchedules/{t}/{subj}/{cat}  enrollment │
                         │  x-api-key · 120 req/min documented          │
                         └────────────────────┬────────────────────────┘
                                              │
                      ┌───────────────────────▼────────────────────────┐
                      │  source.CourseSource (interface)              │
                      │   ├─ uwapi   real: token bucket + backoff     │
                      │   └─ mock    captured fixtures (CI uses this) │
                      └───────────────────────┬────────────────────────┘
                                              │ []SectionState
   ┌──────────────────────────────────────────▼───────────────────────────────────┐
   │ POLLER                                                                        │
   │  reload watched courses from Postgres every 30s                               │
   │  priority queue (due time, then watcher count) → worker pool (4)              │
   │  ONE request per COURSE covers every section of it                            │
   │  adaptive interval: 60s → 32m while quiet, 15s for hot, ±10% jitter           │
   └──────────────────────────────┬────────────────────────────────────────────────┘
                                  │
   ┌──────────────────────────────▼────────────────────────────────────────────────┐
   │ DIFFER                                  ┌────────── Redis ──────────────┐    │
   │  fires only on full → open              │ state:{term}:{class} → hash   │    │
   │  compares old-vs-OLD capacity           │ epoch:{term}:{class} → counter│    │
   │  Lua CAS: set state + INCR epoch,       └───────────────────────────────┘    │
   │  atomically ⇒ concurrent pollers fire exactly once                            │
   │  transition_id = "{term}:{class}:{epoch}:{unixMilli}"  ← collision-safe       │
   └──────────────────────────────┬────────────────────────────────────────────────┘
                                  │
   ┌──────────────────────────────▼────────────────────────────────────────────────┐
   │ NOTIFIER (transactional outbox)         ┌──────── Postgres ─────────────┐    │
   │  fan-out: INSERT..SELECT ON CONFLICT    │ users · sections              │    │
   │  claim:   FOR UPDATE SKIP LOCKED        │ subscriptions                 │    │
   │  retry:   2s→10m exp + 20% jitter       │ notifications (outbox + log)  │    │
   │  give up: dead_letter after 6 attempts  │  UNIQUE(sub_id, transition_id)│    │
   │  reclaim stale leases on boot           └───────────────────────────────┘    │
   └──────────────────────────────┬────────────────────────────────────────────────┘
                                  │ send happens OUTSIDE the transaction
   ┌──────────────────────────────▼────────────────────────────────────────────────┐
   │ DISCORD (discordgo)                                                           │
   │  /search · /watch · /unwatch · /watchlist          (ephemeral, autocompleted) │
   │  deliver: DM ──(50007 DMs closed)──► channel ping that names no course        │
   └──────────────────────────────┬────────────────────────────────────────────────┘
                                  │
                   Prometheus :9090/metrics  +  /healthz
```

One binary, `notifco`, with roles `bot`, `poller`, `all`, `poll`, `migrate`. The free-tier
deploy runs `all`.

## Correctness

The whole product is one predicate, so it is worth being precise about it.

```go
func IsOpening(prev, cur Snapshot) bool {
    wasFull := prev.Enrolled >= prev.Capacity   // old vs OLD capacity
    nowOpen := cur.Enrolled  <  cur.Capacity
    return wasFull && nowOpen
}
```

UWFlow's equivalent SQL tests `old.enrollment_total >= new.enrollment_capacity` — the old
enrolled count against the **new** capacity. So when a department raises capacity on a
full section (100/100 → 100/120, i.e. *twenty seats just appeared*), `100 >= 120` is false
and **no notification fires**. notifco compares old-vs-old.

There is a test that asserts both halves of this: that notifco fires, and that UWFlow's
predicate would not.

Three further guarantees, each with a test:

- **Edge-triggered.** A section that is merely still open never re-fires.
- **Cold start never fires.** A fresh Redis must not alert 10k people that an
  already-open section is open.
- **Exactly once, across restarts.** `UNIQUE(subscription_id, transition_id)` means a
  duplicate enqueue is impossible. Losing Redis degrades detection for one cycle but can
  never cause a duplicate DM.

## Benchmarks

Measured on an M-series Mac, Postgres 16 and Redis 8 on localhost.
Reproduce with `make load-test`.

### Fan-out: one section opens, 10,000 watchers

| Stage | Time |
|---|---|
| Detect transition (Redis Lua CAS) | **114 µs** |
| Fan out to 10,000 outbox rows (one `INSERT..SELECT`) | **90 ms** |
| Drain all 10,000 (16 workers, instant transport) | **616 ms** |
| Throughput | **16,231 msg/sec** |
| Latency p50 / p95 / p99 / max | **395 ms / 681 ms / 702 ms / 707 ms** |
| **Duplicates** | **0** |
| **Lost** | **0** |

### With Discord's real rate ceiling

The numbers above isolate notifco. In production Discord dominates: its global budget is
roughly 50 req/s. Modelling that at 20 ms/message:

| | |
|---|---|
| 1,000 subscribers | **2.77 s** (361 msg/sec, p99 2.75 s) |
| 10,000 extrapolated | **~28 s** |

So for a realistically popular section, end-to-end is **poll interval + a few seconds**.
For a pathological 10k-watcher section, Discord's ceiling adds ~30s — a platform limit,
not a pipeline one.

### Upstream API, measured live

| | |
|---|---|
| `Courses/{term}` | 7,894 offerings, 7.1 MB, **one request, 0.60 s** |
| `ClassSchedules/{term}/CS/341` | 7 sections with enrollment, 0.77 s |
| `ClassSchedules/{term}` | **courseIds only — no enrollment** |
| Courses with classes in term 1269 | 2,179 of 7,295 |
| Full-term enrollment sweep | 2,179 requests ≈ 18 min at 120 req/min |
| **60 s interval supports** | **~120 distinct watched courses** |
| Rate limit | 120/min documented; **150 requests in 22 s (~409/min) with zero 429s** |

Default pacing stays at the documented 120/min. The docs call the limit experimental and
return no rate-limit headers, so the observed headroom is treated as safety margin.

### Verification

```
$ gofmt -s -l .          (clean)
$ go vet ./...           (clean)
$ go mod verify          all modules verified
$ go build ./...         OK
$ go test -race ./...
ok  internal/config          1.550s
ok  internal/differ          1.416s
ok  internal/notifier        1.745s
ok  internal/poller          2.319s
ok  internal/source          1.221s
ok  internal/source/mock     2.122s
ok  test/integration         2.776s
```

65 tests, all passing under `-race`.

Live end-to-end against the real API:

```
$ notifco poll            # NOTIFCO_SOURCE=uwapi
level=INFO msg="notifco starting" role=poll term=1269 config="source=uwapi key=set ..."
level=INFO msg="one-shot poll complete" delivered=0

 course_code | section_name |  seats  | is_online
-------------+--------------+---------+-----------
 cs341       | LAB 101      | 99/100  | f
 cs341       | LEC 002      | 99/100  | f
 cs341       | TST 201      | 268/300 | f
 ... (7 sections from ONE request)
```

### Audit

The first working version was put through an independent audit that was told not
to trust this README. It found eight real defects, each reproduced against live
Postgres and Redis. All are fixed, and each has a regression test in
[test/integration/regression_test.go](./test/integration/regression_test.go) that
fails without the fix. The notable ones:

| Severity | Defect | Fix |
|---|---|---|
| **Critical** | `transition_id` was `{term}:{class}:{epoch}` with the epoch from a Redis `INCR`. A Redis flush — which docker-compose explicitly permitted — restarted the epoch at 1, regenerating an ID Postgres had already recorded, so `ON CONFLICT DO NOTHING` **silently discarded a real seat opening**. | ID now embeds the detection time, making it collision-free across state loss; Redis also persists now. |
| **Critical** | The epoch key's TTL was refreshed only when a transition fired, while the state key refreshed every observation. The two diverged, so a long-quiet section lost its epoch but kept its state — triggering the same collision with no outage at all. | Both TTLs refresh in lockstep inside the Lua script. |
| **High** | `Claim` did `attempts+1` and `RetryWithoutAttempt` did `attempts-1`, netting zero, so a throttled notification retried **forever** and the `MaxAttempts` check was unreachable. | Separate `throttles` counter, capped. |
| **High** | `ReclaimStale` ran only at boot and `Claim` only selects `pending`, so a worker killed mid-send stranded a row in `sending` **forever**. | Reclaim also runs every 30s while live. |
| **High** | Data race on `job.tier` / `job.watchers` between the poller's refresh and its workers, proven with `-race`. | All reads moved under the mutex. |
| Medium | A `0/0` previous snapshot satisfied "was full", so a section *appearing* fired a spurious alert. | Both snapshots must have positive capacity. |
| Medium | Durations were passed to Postgres as Go strings (`"500µs"`), which `::interval` rejects. | `make_interval(secs => ...)`. |
| Medium | No guard against Discord's 2,000-character limit, so `/watchlist` broke permanently for heavy users. | Output clamped and capped. |
| Low | `FindSection` sliced the last three bytes of a section name unguarded — a panic reachable from slash input, and discordgo has no recover. | Numeric suffix parsed safely; panic recovery added to the command handler. |
| Low | A bare `%` in search matched every course. | LIKE metacharacters escaped; empty prefix no longer matches all. |

The auditor's verdict on the parts it tried hardest to break and could not:
concurrent exactly-once firing holds (64 goroutines → one transition), cold-start
suppression is correct, there is no SQL injection anywhere (every query uses
placeholders), the API key cannot leak into logs or errors, `.env` is absent from
all of git history, and the rate limiter and backoff are correct.

Worth stating plainly: the test suite was green **before** the audit too. A green
suite proves tests and failure modes don't intersect, not that the code is right.

## Setup

### Prerequisites

- Go 1.26+, or just Docker
- Postgres 16 and Redis 7+ (docker-compose provides both)
- A Discord bot token
- Optionally a UW Open Data API key — **without one it runs on fixtures**

### Quickest path

```sh
git clone https://github.com/vhzhen/notifco.git
cd notifco
cp .env.sample .env
# fill in DISCORD_TOKEN and DISCORD_APP_ID
docker compose up -d
docker compose logs -f notifco
```

### Local development

```sh
make dev-infra     # Postgres + Redis only
make migrate
make run           # runs `notifco all`
make verify        # lint + build + race tests
make load-test     # the 10k benchmark
make help          # all targets
```

### Discord bot token

1. <https://discord.com/developers/applications> → **New Application**
2. **Bot** → **Reset Token** → copy into `DISCORD_TOKEN`
3. **General Information** → copy Application ID into `DISCORD_APP_ID`
4. **Installation** → invite with the `applications.commands` scope, plus
   **Send Messages** in whichever channel you want fallback pings in
5. While developing, set `DISCORD_GUILD_ID` so commands register instantly instead of
   taking up to an hour to propagate globally

notifco needs **no privileged intents** — no message content, no member list. It reads
nothing from chat.

### UW API key (optional)

The key is returned immediately in the response body; there is no email-confirmation step
despite what older docs suggest.

```sh
curl --request POST \
  --url https://openapi.data.uwaterloo.ca/v3/account/register \
  --header 'content-type: application/x-www-form-urlencoded' \
  --data 'email=YOU@example.com&project=notifco&uri=https%3A%2F%2Fgithub.com%2Fvhzhen%2Fnotifco'
```

Put it in `UW_API_KEY` and set `NOTIFCO_SOURCE=uwapi`. Leave it empty and notifco uses the
mock source backed by real captured responses in `internal/source/mock/testdata` — which
is exactly how CI runs, with no secrets.

## Commands

| Command | What it does |
|---|---|
| `/search <code>` | Section list with live seat counts. Fetches on demand if nobody has watched the course before. |
| `/watch <code> [section]` | Watch one section, or every section of the course if `section` is omitted. Autocompleted. |
| `/unwatch <code> [section]` | Stop watching one section, or the whole course. Also cancels any undelivered alert. |
| `/watchlist` | Your active watches with current seat counts. |

Codes are forgiving: `CS 341`, `cs341`, `CS-341` all work, and sections accept `LEC 001`,
`001`, or `1`. All replies are ephemeral.

## Configuration

Everything is environment variables; see [.env.sample](./.env.sample).

| Variable | Default | Notes |
|---|---|---|
| `DISCORD_TOKEN` | — | required for `bot`/`all` |
| `DISCORD_APP_ID` | — | required for `bot`/`all` |
| `DISCORD_GUILD_ID` | — | set for instant command registration while developing |
| `UW_API_KEY` | — | empty ⇒ mock source |
| `NOTIFCO_SOURCE` | auto | `uwapi` or `mock`; inferred from the key |
| `DATABASE_URL` | localhost | Postgres DSN |
| `REDIS_URL` | localhost | Redis DSN |
| `NOTIFCO_POLL_INTERVAL` | `60s` | base interval; bounds detection lag |
| `NOTIFCO_HOT_INTERVAL` | `15s` | for courses with a full watched section |
| `NOTIFCO_RATE_LIMIT` | `120` | req/min ceiling |
| `NOTIFCO_COOLDOWN` | `30m` | suppresses repeat alerts on a flapping section |
| `NOTIFCO_MAX_ATTEMPTS` | `6` | then dead-letter |
| `METRICS_ADDR` | `:9090` | `/metrics` and `/healthz` |

## Metrics

`notifco_polls_total`, `notifco_alert_latency_seconds`, `notifco_active_users`,
`notifco_active_subscriptions`, plus `notifco_api_requests_total`,
`notifco_rate_limit_wait_seconds`, `notifco_transitions_total`,
`notifco_notifications_total`, `notifco_queue_depth`, `notifco_poll_cycle_seconds`.

**On alert latency, honestly:** the upstream API exposes no "last updated" field on a
class, so the instant a seat actually opened is **unknowable**.
`notifco_alert_latency_seconds` measures **detected → sent**, which is the part notifco
controls. Detection lag is *bounded* by the poll interval and reported separately as
`notifco_poll_interval_seconds`. The honest end-to-end figure is
`poll interval + alert latency`, and nobody can do better than that against this API.

`/metrics` is unauthenticated and bound to localhost in docker-compose. Do not expose it.

## Deployment

[deploy/oracle/README.md](./deploy/oracle/README.md) — full walkthrough for a $0 Oracle
Cloud Always Free ARM VM, including the iptables gotcha, systemd unit, and backups.

Cost comparison and the free-tier traps worth knowing (Render spins down; Neon's free
Postgres runs out of compute hours for an always-on bot) are in that doc.

## How notifco differs from UWFlow

| | UWFlow | notifco |
|---|---|---|
| Cadence | daily cron, ~4 h sweep | continuous, 60 s |
| Scope | 4 terms × every course | only watched courses |
| Worst-case alert lag | ~28 h | ~60 s + seconds |
| "Was full" test | old vs **new** capacity (buggy) | old vs **old** capacity |
| Last-seen state | the serving table itself | Redis, decoupled |
| Detection | welded to the importer's `UPDATE` | standalone, pollable at any rate |
| Delivery retry | **none** — logged and skipped | exp backoff + jitter, dead-letter |
| Idempotency | `seen_at` flag | `UNIQUE(sub_id, transition_id)` |
| Send location | **inside** the DB transaction | outside it |
| Backlog on restart | never drained | reclaimed on boot |
| Re-notify control | none | cooldown + optional notify-once |
| Fan-out | serial, one SMTP dial each | pooled workers, 17k/sec |

Credit where due: UWFlow's staging-table bulk merge and its statement-level trigger are
genuinely elegant, and its `LISTEN/NOTIFY` doorbell is a good idea that notifco keeps in
spirit (an in-process wake-up) while using a real outbox as the source of truth.

## Data sources and scope

Reads exactly one upstream: the public, self-serve
[UW Open Data API v3](https://openapi.data.uwaterloo.ca/api-docs/), whose stated scope is
publicly visible data only.

**Nothing behind a login is touched.** No Quest scraping, no SourceAPI, no student
credentials, no Shibboleth. The only user data stored is a Discord user ID and the
sections someone chose to watch. Seat figures are best-effort and may be stale — every
seat count notifco shows carries that disclaimer, because the UW OpenData project asks
integrators to surface it. **Quest is authoritative.**

UWFlow is MIT licensed and was read as a reference only. No code, SQL, or templates were
copied; this is an independent implementation. See [NOTES.md §7](./NOTES.md).

## Limitations

- **~120 distinct watched courses at a 60 s interval.** There is no bulk enrollment
  endpoint, so cost scales with distinct watched courses. Raise `NOTIFCO_RATE_LIMIT`
  toward the observed ~409/min, or widen the interval.
- **No waitlist awareness.** Upstream exposes no waitlist or reserved-capacity field, so
  a section can report seats a given student cannot actually take.
- **Single poller.** The differ and notifier are safe to run concurrently; the poller is
  not, and would need a distributed lock to scale horizontally.
- **429 handling is untested against reality.** I never managed to trigger one, so
  whether upstream sends `Retry-After` is unconfirmed. Both paths are implemented.
- **Discord's rate limit dominates large fan-out.** Nothing notifco can do about that.

## Project layout

```
cmd/notifco/            entrypoint: bot | poller | all | poll | migrate
internal/
  config/               env parsing, role-aware validation
  source/               CourseSource interface + helpers
    uwapi/              real client: token bucket, backoff, upstream quirks
    mock/               fixtures + scriptable timeline, with testdata/
  poller/               scheduler, priority queue, worker pool, adaptive interval
  differ/               transition rule + Redis Lua CAS
  notifier/             outbox worker, retries, dead-letter
  discord/              slash commands + delivery transport
  store/                Postgres queries, outbox, embedded migrations/
  metrics/              Prometheus collectors
test/integration/       end-to-end on mock source, real PG + Redis
test/load/              10k-subscription benchmark (build tag `load`)
deploy/oracle/          $0 deployment guide
```

## License

MIT — see [LICENSE](./LICENSE).
