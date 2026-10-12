# notifco

Get a DM the moment a full University of Waterloo course section has a seat open up.

[![Add to Discord](https://img.shields.io/badge/Add%20to%20Discord-5865F2?style=for-the-badge&logo=discord&logoColor=white)](https://discord.com/oauth2/authorize?client_id=1558968102463471638&scope=bot+applications.commands&permissions=3072)
[![CI](https://github.com/vhzhen/notifco/actions/workflows/ci.yml/badge.svg)](https://github.com/vhzhen/notifco/actions/workflows/ci.yml)
[![MIT](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)

## Quickstart

**[→ Add notifco to your server](https://discord.com/oauth2/authorize?client_id=1558968102463471638&scope=bot+applications.commands&permissions=3072)**

Then, in any channel:

```
/search MATH 135            every section with live seat counts
/watch MATH 135 LEC 002     get DMed when that section frees up
/watchlist                  what you're watching
/unwatch MATH 135           stop
```

Results are laid out like this, with **Previous / Next** buttons when a course has more sections than fit on one page (MATH 135 has 25):

> **MATH 135 - Algebra for Honours Mathematics [Fall 2026]**
> 25 sections · 🟢 23 open · 🔴 2 full
>
> **Lectures**
> 🟢 `LEC 002` ▰▰▰▰▰▰▰▱ `109/120` · **11 seats open**
> 🔴 `LEC 001` ▰▰▰▰▰▰▰▰ `120/120` · Full

Three things to know:

- **Turn on DMs from server members**, or the alert can't reach you. Server settings → Privacy Settings. If DMs are closed, notifco falls back to mentioning you in the channel without naming the course.
- **Alerts fire on full → open only.** Watching a section that already has seats stays quiet until it fills and frees again. That's the point: it tells you about *changes*.
- **Seat counts are a best-effort snapshot.** Quest is authoritative — verify before you rely on it.

No account, no signup. Replies are private to you.

## Why it exists

[UWFlow](https://github.com/UWFlow/uwflow) already has this feature, but it rides on a nightly importer: cron fires once a day and the sweep takes ~4 hours, so you hear about an open seat **up to ~28 hours later**. A seat that opens and gets retaken within a day often never gets reported.

notifco targets **under 60 seconds**.

It also fixes a bug in UWFlow's detection. UWFlow tests "was this section full?" by comparing the old enrollment count against the *new* capacity:

```sql
WHERE o.enrollment_total >= n.enrollment_capacity
```

So when a department **raises capacity** on a full section — 100/100 becomes 100/120, meaning twenty seats just appeared — `100 >= 120` is false and no alert fires. That's one of the most valuable alerts there is, silently dropped. notifco compares old-vs-old. There's [a test](./internal/differ/differ_test.go) asserting both that notifco fires and that UWFlow's predicate wouldn't.

## How it works

```
UW Open Data API v3  ──poll only watched courses, adaptive 15–60s──┐
                                                                   ▼
                        differ: fires on full→open edge, Redis CAS (atomic)
                                                                   │
                        outbox: Postgres, SKIP LOCKED, retries, dead-letter
                                                                   │
                                                          DM ──► you
```

One request returns **every section of a course**, so watching three sections of CS 341 costs one request, not three. Detection state lives in Redis; delivery is a Postgres outbox keyed `UNIQUE(subscription_id, transition_id)`, which makes it exactly-once across restarts and crashes.

Full design in [DESIGN.md](./DESIGN.md). The UWFlow teardown that informed it is in [NOTES.md](./NOTES.md). Live API measurements are in [VERIFIED.md](./VERIFIED.md).

## Numbers

Measured, not estimated. Reproduce with `make load-test`.

| | |
|---|---|
| 10,000 watchers on one opening | 616 ms to drain, **0 duplicates, 0 lost** |
| Throughput | 16,231 msg/sec |
| Detect a transition | 114 µs |
| With Discord's real ~50 req/s ceiling | ~28 s for 10k DMs |
| Tests | 65, all green under `-race` |
| Running footprint | ~48 MB total (bot + Postgres + Redis) |

Detection lag is bounded by the poll interval. End-to-end is **poll interval + a couple of seconds**; the upstream API exposes no "last updated" field, so the true seat-open instant is unknowable and notifco doesn't pretend otherwise.

## Self-hosting

MIT licensed and free to run. Two $0 paths:

- **[Google Cloud](./deploy/gcp/README.md)** — always-free `e2-micro`. This is the one actually deployed and verified end to end.
- **[Oracle Cloud](./deploy/oracle/README.md)** — always-free ARM VM, far more RAM.

Locally:

```sh
git clone https://github.com/vhzhen/notifco.git && cd notifco
cp .env.sample .env      # add DISCORD_TOKEN and DISCORD_APP_ID
docker compose up -d
```

Leave `UW_API_KEY` empty and it runs on captured API fixtures — that's how CI works, with no secrets. For live data, [register a key](https://openapi.data.uwaterloo.ca/api-docs/) (returned instantly, no email confirmation) and set `NOTIFCO_SOURCE=uwapi`.

```sh
make help          # all targets
make verify        # lint + build + race tests
make load-test     # the 10k benchmark
```

**On a host with ~1 GB RAM, don't build in place** — the Go compiler thrashes swap instead of finishing. Cross-compile instead (~8 s):

```sh
make dist-linux    # then copy dist/notifco-linux-amd64 to the host
docker compose -f docker-compose.yml -f docker-compose.prebuilt.yml up -d
```

Cloudflare and CloudFront can't host this: a Discord gateway bot holds a long-lived outbound WebSocket, so it needs a process that stays running, not a CDN or per-request serverless.

## Limits

- **Watched courses ≈ your rate limit**, at a 60 s interval. There's no bulk enrollment endpoint, so cost scales with distinct watched courses, and one request covers all sections of a course. The default 120 is UW's documented cap; ~266/min was measured sustained with no throttling, so raising it is viable. The client halves its own rate on a 429 and recovers on sustained success, so running above the documented figure degrades rather than breaks.
- **No waitlist awareness.** The API exposes no waitlist or reserved-capacity field, so a section can report seats you personally can't take.
- **Single poller.** The differ and notifier are safe to run concurrently; the poller would need a distributed lock.
- Discord's rate limit dominates very large fan-outs. Nothing notifco can do about that.

## Data and privacy

Reads exactly one upstream: the public [UW Open Data API v3](https://openapi.data.uwaterloo.ca/api-docs/), whose stated scope is publicly visible data only.

**Nothing behind a login is touched** — no Quest scraping, no credentials, no SourceAPI. The only thing stored about you is your Discord user ID and the sections you chose to watch. `/unwatch` removes the subscription and cancels any undelivered alert.

UWFlow is MIT licensed and was read as a reference only; no code, SQL, or templates were copied.

## Configuration

Environment variables only — see [.env.sample](./.env.sample) for the full list with comments. The ones you'll actually touch:

| Variable | Default | |
|---|---|---|
| `DISCORD_TOKEN` | — | required |
| `DISCORD_APP_ID` | — | required |
| `DISCORD_GUILD_ID` | — | set for instant command registration while developing |
| `UW_API_KEY` | — | empty ⇒ fixtures instead of live data |
| `NOTIFCO_POLL_INTERVAL` | `60s` | bounds detection lag |
| `NOTIFCO_RATE_LIMIT` | `120` | upstream req/min. UW documents 120; ~266/min measured sustained with no throttling. Raise it to watch more courses — the client halves its own rate on a 429 and recovers, so it self-corrects |
| `NOTIFCO_COOLDOWN` | `30m` | suppresses repeat alerts on a flapping section |

Prometheus metrics and `/healthz` on `:9090`. `/metrics` is unauthenticated by design — keep it on localhost.

## License

MIT — see [LICENSE](./LICENSE).
