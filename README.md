# notifco

A Discord bot for University of Waterloo students: search courses, watch sections, and
get a DM the moment a full section has a seat open up.

> **Status: design phase, API verified.** Research and design only — no implementation
> yet. The UW Open Data API has been registered against and measured, so the design now
> rests on hard numbers rather than assumptions. Full README (architecture diagram,
> setup, benchmarks) lands with the build.

## Documents

| File | What it is |
|---|---|
| [NOTES.md](./NOTES.md) | Investigation of [UWFlow](https://github.com/UWFlow/uwflow)'s existing seat-notification system: data source, polling, detection logic, schema, delivery, weaknesses, licensing and permission analysis. |
| [DESIGN.md](./DESIGN.md) | The notifco design: architecture, interfaces, data model, retry/idempotency strategy, metrics, test plan. |
| [VERIFIED.md](./VERIFIED.md) | Live measurements against the real API, and the design assumption they disproved. |

## The problem

UWFlow already does "notify me when a course frees up", but it rides on a nightly
full-catalog importer: cron fires once a day, the sweep takes ~4 hours, so a student
learns about an open seat **up to ~28 hours later**. A seat that opens and is retaken
within a day is often never reported at all.

notifco targets **under 60 seconds**.

## How

- **Poll only watched courses**, continuously, with an adaptive interval — instead of
  UWFlow's 4 terms × every course, watched or not.
- **One request covers every section of a course**, so watching `LEC 001` + `LAB 101` +
  `TST 201` of CS 341 costs one request, not three.
- **Correct the full→open test.** UWFlow compares the old enrollment count against the
  *new* capacity, so when a department raises capacity on a full section — 100/100 →
  100/120, one of the most valuable alerts — no notification fires. notifco compares
  old-vs-old.
- **Last-seen state in Redis**, decoupled from the catalog, with an atomic Lua CAS that
  bumps a per-section epoch. That epoch gives a restart-stable `transition_id`.
- **Deliver through a Postgres outbox** with `FOR UPDATE SKIP LOCKED`, retries with
  jittered backoff, and dead-lettering. Idempotency is enforced by a unique constraint on
  `(subscription_id, transition_id)`, so losing Redis can never cause a duplicate DM.
  UWFlow has no retry at all: a failed send is logged and skipped until the next day.

## What the live API actually allows

Measured 2026-10-11 with a registered key ([VERIFIED.md](./VERIFIED.md)):

| | |
|---|---|
| Catalog | `Courses/{term}` — 7,894 offerings, 7.1 MB, **one** request, 0.6 s |
| Enrollment | **per-course only — no bulk endpoint exists** |
| Polling universe | 2,179 of 7,295 courseIds have classes in a term |
| Full-term sweep | 2,179 requests ≈ 18 min at the documented rate |
| 60 s target supports | ~120 distinct watched courses |
| Rate limit | 120/min documented; 150 requests in 22 s (~409/min) observed with zero 429s |

The honest correction: I had designed around `ClassSchedules/{termCode}` returning an
entire term's enrollment in one request. It returns only an array of courseId strings.
There is no bulk enrollment endpoint, so UWFlow's per-course requests were the only
option available — their fixable cost is scope and serialism, not requests per course.
See [VERIFIED.md §2](./VERIFIED.md).

## Data sources and scope

Reads exactly one upstream: the public, self-serve
[UW Open Data API v3](https://openapi.data.uwaterloo.ca/api-docs/), whose stated scope is
publicly visible data only.

**Nothing behind a login is touched.** No Quest scraping, no SourceAPI, no student
credentials. The only user data stored is a Discord user ID and the sections a user chose
to watch. Seat figures are best-effort and may be stale — Quest is authoritative.

UWFlow is MIT licensed and was read as a reference only; no code, SQL, or templates were
copied. See [NOTES.md §7](./NOTES.md).

## Local setup

```sh
cp .env.sample .env     # fill in DISCORD_TOKEN; UW_API_KEY optional
```

Leave `UW_API_KEY` empty to run against the mock source backed by real captured responses
in `internal/source/testdata/`. That is how CI runs — no key required.

To use live data, register a key (returned immediately, no email confirmation):

```sh
curl --request POST \
  --url https://openapi.data.uwaterloo.ca/v3/account/register \
  --header 'content-type: application/x-www-form-urlencoded' \
  --data 'email=YOU@example.com&project=notifco&uri=https%3A%2F%2Fgithub.com%2Fvhzhen%2Fnotifco'
```

Then set `UW_API_KEY` and `NOTIFCO_SOURCE=uwapi`.

## License

MIT — see [LICENSE](./LICENSE).
