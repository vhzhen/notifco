# notifco

A Discord bot for University of Waterloo students: search courses, watch sections, and
get a DM the moment a full section has a seat open up.

> **Status: design phase.** This repo currently contains research and design only.
> No implementation yet. The full README (architecture diagram, setup, benchmarks)
> lands with the build.

## Documents

| File | What it is |
|---|---|
| [NOTES.md](./NOTES.md) | Investigation of [UWFlow](https://github.com/UWFlow/uwflow)'s existing seat-notification system: data source, polling, detection logic, schema, delivery, weaknesses, and licensing/permission analysis. |
| [DESIGN.md](./DESIGN.md) | The notifco design derived from those findings: architecture, interfaces, data model, retry/idempotency strategy, metrics, and test plan. |

## The short version

UWFlow already does "notify me when a course frees up", but it rides on a nightly
full-catalog importer, so a student learns about an open seat **up to ~28 hours later**.
notifco targets **under 60 seconds** by:

- Polling only watched sections, continuously, with an adaptive interval.
- Using `ClassSchedules/{termCode}` — one request returns an entire term, an endpoint
  UWFlow does not use and the reason its sweep takes ~4 hours.
- Detecting the full→open edge against last-seen state in Redis, with a corrected
  predicate (UWFlow's "was full" test misses the case where a department *raises*
  capacity on a full section).
- Delivering through a Postgres outbox with retries, backoff, and dead-lettering, and an
  idempotency key that holds across restarts.

## Data sources and scope

Reads exactly one upstream: the public, self-serve
[UW Open Data API v3](https://openapi.data.uwaterloo.ca/api-docs/index.html), whose stated
scope is publicly visible data only.

**Nothing behind a login is touched.** No Quest scraping, no SourceAPI, no student
credentials, no stored personal data beyond a Discord user ID and the sections a user
chose to watch. Seat figures are best-effort and may be stale — Quest is authoritative.

UWFlow is MIT licensed and was read as a reference only; no code, SQL, or templates were
copied. See NOTES.md §7.

## License

MIT — see [LICENSE](./LICENSE).
