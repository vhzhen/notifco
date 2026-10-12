# Verified API Measurements

Everything here was measured against the live UW Open Data API v3 on **2026-10-11** with
a real registered key. This supersedes any inference in NOTES.md or DESIGN.md that
disagrees with it. The headline item is that **my central design assumption was wrong**;
see §2.

Key registered via the documented flow:

```
POST /v3/account/register   email=<redacted>  project=notifco  uri=https://github.com/vhzhen/notifco
-> HTTP 200 {"message":"Registration successful, your API key is <redacted>", ...}
```

No email confirmation step was required — the key worked on the very next request. (The
UWFlow README describes a two-step register-then-confirm flow; that is no longer
accurate, or no longer enforced.) Auth header is `x-api-key`.

---

## 1. Current term

```
GET /v3/Terms/current  ->  200
{"termCode":"1269","name":"Fall 2026","nameShort":"2026 F",
 "termBeginDate":"2026-09-01T00:00:00","termEndDate":"2026-12-31T00:00:00",
 "sixtyPercentCompleteDate":null,"associatedAcademicYear":0}
```

Confirms UWFlow's term-id arithmetic (`(year-1900)*10 + season`): Fall 2026 = `1269`.
Also confirms their complaint in `parts/term/struct.go` — `termBeginDate`/`termEndDate`
are snapped to month boundaries (Sep 1 / Dec 31), useless for real lecture dates, which
is why they use `ImportantDates` instead.

---

## 2. `ClassSchedules/{termCode}` is an INDEX, not a bulk data dump

**This invalidates the primary optimisation in DESIGN.md §4.**

```
GET /v3/ClassSchedules/1269  ->  200   19,612 bytes   0.18 s
```

I assumed this returned every `Class` object for the term. It does not. It returns a flat
JSON **array of 2,179 six-digit courseId strings**:

```json
["013912","015796","003845","013368","013864","009328", ...]
```

All 2,179 entries are 6 characters. No enrollment data whatsoever. The OpenAPI spec was
telling the truth with `{"type":"array","items":{"type":"string"}}` — I read that as the
spec being sloppily typed, since the sibling endpoints return objects. It was literal.

**Consequence**: there is no bulk enrollment endpoint. Enrollment can only be read
per-course. UWFlow's per-course approach was not a missed optimisation — it is the only
option the API offers. My NOTES.md §6 claim that their ~4-hour sweep was "self-inflicted"
was wrong on this point; the fixable part of their cost is concurrency and scope, not
request count per course.

What the endpoint *is* good for: it cheaply enumerates which courses actually have
classes in a term. Only **2,179 of 7,295** distinct courseIds in the catalog have
schedules in 1269, so it bounds the polling universe at 2,179 rather than 7,295, for one
request.

---

## 3. Per-course enrollment — both forms work

### By subject + catalog number (no courseId lookup needed)

```
GET /v3/ClassSchedules/1269/CS/341  ->  200   5,884 bytes   0.77 s
```

7 sections, real enrollment:

| classNumber | component | classSection | enrolled / capacity |
|---|---|---|---|
| 6003 | LEC | 1 | 83 / 100 |
| 6546 | LEC | 2 | 99 / 100 |
| 6547 | LEC | 3 | 86 / 100 |
| 6358 | LAB | 101 | 99 / 100 |
| 6438 | LAB | 102 | 83 / 100 |
| 5934 | LAB | 103 | 86 / 100 |
| 6549 | TST | 201 | 268 / 300 |

This form is ideal for the bot: a user types `CS 341` and it maps straight to a request,
with no courseId resolution step. UWFlow does not use it.

### By courseId

```
GET /v3/ClassSchedules/1269/013912  ->  200   783 bytes   0.34 s
```

Also returns full enrollment. Needed only for cross-listed handling, where one courseId
spans several course codes distinguished by `courseOfferNumber`.

### Important batching property

**One request returns every section of a course.** A user watching `LEC 001`, `LAB 101`,
and `TST 201` of CS 341 costs **one** request, not three. The poller must therefore
deduplicate watched sections down to distinct `(term, course)` pairs. This is the single
most important efficiency fact for sizing the poller.

Also note `classSection` is the real section number as an integer (`1`, `101`, `201`),
not a sequential index, so UWFlow's zero-pad-to-3 to build `LEC 001` / `LAB 101` is
correct.

---

## 4. Catalog: one request for everything

```
GET /v3/Courses/1269  ->  200   7,134,495 bytes (7.1 MB)   0.60 s
```

- **7,894** course offerings, **7,295** distinct courseIds, **215** distinct subjects.
- Fields: `courseId, courseOfferNumber, termCode, termName, associatedAcademicCareer,`
  `associatedAcademicGroupCode, associatedAcademicOrgCode, subjectCode, catalogNumber,`
  `title, descriptionAbbreviated, description, gradingBasis, courseComponentCode,`
  `enrollConsentCode, enrollConsentDescription, dropConsentCode, dropConsentDescription,`
  `requirementsDescription`

7.1 MB in 0.6 s is entirely practical to pull once daily to seed `/search`. Course
*metadata* is bulk-available; only *enrollment* is not.

---

## 5. Rate limits — the published figure is not what is enforced

Measured: **150 sequential requests in 22 s (~409 req/min). Zero 429s.**

I stopped there rather than hunt for the true ceiling; the point was whether 120/min is a
hard wall, and it is not. This matters because UWFlow paces itself at ~109 req/min with a
fixed 550 ms sleep, which appears to be roughly 4× more conservative than necessary.

**No rate-limit headers are returned.** Full header set on a 200:

```
content-type, date, server: Microsoft-IIS/10.0, set-cookie: ARRAffinity...,
strict-transport-security, x-powered-by: ASP.NET
```

No `X-RateLimit-*`, no `Retry-After`, nothing. A client cannot discover its remaining
quota and must self-pace and react to 429s blind.

**Unverified**: I never triggered a 429, so I could not confirm whether it carries
`Retry-After`. The backoff must handle both cases. UWFlow's client reads `Retry-After`
and falls back to a flat 61 s.

### Design stance

Default to the **documented 120 req/min**, not the ~409 observed. The docs say the limit
is per-key and explicitly "experimenting with an implementation", so observed headroom
today is not a contract. Make it configurable, and treat the measured headroom as safety
margin rather than budget to spend.

---

## 6. Corrected polling budget

A full enrollment sweep of an entire term is **2,179 requests** (§2), one per course with
classes:

| Pacing | Full-term sweep | Watched-only, 60 s target |
|---|---|---|
| 120 req/min (documented, default) | ~18 min | ~120 distinct courses |
| 400 req/min (observed, not a contract) | ~5.5 min | ~400 distinct courses |

Compare UWFlow: ~25–28k requests and ~4 hours, because it sweeps 4 terms × every course
unconditionally and serially. Polling only watched courses, at one request per course
regardless of section count, is what delivers the latency win — not a bulk endpoint,
which does not exist.

This lands almost exactly on the risk I flagged in DESIGN.md §13: "if the bulk endpoint
is unusable then the 60 s target holds only up to roughly 100 distinct watched courses."
Measured answer: ~120 at the documented rate.

---

## 7. Fixtures captured

Real responses saved to `internal/source/testdata/` for the mock source and tests:

| File | Contents |
|---|---|
| `classschedules_1269_CS_341.json` | 7 real CS 341 sections with enrollment (full response) |
| `terms_current.json` | `Terms/current` response |
| `courses_1269_slice.json` | 40 CS/MATH/STAT/ECE courses (7.1 MB trimmed to be committable) |
| `classschedules_1269_courseids_slice.json` | First 50 of the 2,179 courseIds |

Verified key-free before commit.

---

## 8. What changes in the design

| DESIGN.md said | Corrected |
|---|---|
| Tier 1 = one bulk `ClassSchedules/{term}` sweep covering all watched sections | **No bulk enrollment endpoint exists.** Poll per course. |
| Poll cost independent of subscription count | Cost scales with **distinct watched courses** (not sections) |
| `NOTIFCO_POLL_STRATEGY=per-course` is the fallback | per-course is now the **only** strategy; the flag is removed |
| 120 req/min is a hard ceiling | ~409/min observed; keep 120 as a polite self-imposed default |
| `ClassSchedules/{term}` is the sweep | Repurposed: a 1-request index of which courses have classes |

Unchanged and still correct: the differ's full→open rule and the capacity-increase bug
fix (NOTES.md §3), Redis last-seen state, the Postgres outbox with retries and
dead-lettering, the `(subscription_id, transition_id)` idempotency key, grouping one
request per course, and the adaptive interval.

---

## 9. Independent audit of the implementation

After the first working version, the code was put through an independent audit
explicitly told **not** to trust the README. It found eight real defects, each
reproduced against live Postgres and Redis. All are fixed, and each has a
regression test in
[`test/integration/regression_test.go`](./test/integration/regression_test.go)
that fails without the fix.

| Severity | Defect | Fix |
|---|---|---|
| **Critical** | `transition_id` was `{term}:{class}:{epoch}` with the epoch from a Redis `INCR`, while the permanent dedup key is a Postgres unique constraint. Any loss of the Redis counter restarted it at 1, regenerating an ID already recorded, so `ON CONFLICT DO NOTHING` **silently discarded a real seat opening**. | ID now embeds the detection timestamp, collision-free across state loss; Redis also persists. |
| **Critical** | The epoch key's TTL was refreshed only when a transition fired, while state refreshed every observation. The two diverged, so a long-quiet section lost its epoch but kept its state — same collision, with no outage at all. | Both TTLs refresh in lockstep inside the Lua script. |
| **High** | `Claim` did `attempts+1` and `RetryWithoutAttempt` did `attempts-1`, netting zero, so a throttled notification retried **forever** and the `MaxAttempts` check was unreachable. | Separate `throttles` counter, capped at 20. |
| **High** | `ReclaimStale` ran only at boot and `Claim` only selects `pending`, so a worker killed mid-send stranded its row in `sending` **forever**. The original test passed only because it called `ReclaimStale` by hand. | Reclaim also runs every 30 s while live. |
| **High** | Data race on `job.tier` / `job.watchers` between the poller's refresh and its workers, proven under `-race`. Missed because the poller tests never started `Run`. | All reads moved under the mutex; workers snapshot `tier` once. |
| Medium | A `0/0` previous snapshot satisfied "was full", so a section *appearing* fired a spurious alert. | Both snapshots must have positive capacity. |
| Medium | Durations were passed to Postgres as Go strings (`"500µs"`), which `::interval` rejects. | `make_interval(secs => ...)`. |
| Medium | No guard against Discord's 2,000-character limit, so `/watchlist` broke permanently for heavy users. | Output clamped on a line boundary and capped. |
| Low | `FindSection` sliced the last three bytes of a section name unguarded — a panic reachable from slash input, and discordgo has no recover. | Numeric suffix parsed safely; panic recovery added to the handler. |
| Low | A bare `%` in search matched every course. | LIKE metacharacters escaped; empty prefix no longer matches all. |

What the audit tried hardest to break and could not: concurrent exactly-once
firing holds (64 goroutines produced one transition), cold-start suppression is
correct, there is no SQL injection anywhere (every query uses placeholders), the
API key cannot leak into logs or errors, `.env` is absent from all of git
history, and the rate limiter and backoff are correct.

Worth stating plainly: **the test suite was green before the audit too.** A green
suite proves that the tests and the failure modes don't intersect, not that the
code is right.

## 10. Deployment, verified on real hardware

Deployed to a Google Cloud always-free `e2-micro` (1 GB RAM, `us-central1`,
30 GB `pd-standard`) on 2026-10-11. Three findings worth recording:

1. **The Go compiler will not finish inside 1 GB.** The in-place Docker build sat
   at 83–93% iowait with the compiler holding ~395 MB RSS against 83 MB free,
   drove 626 MB into swap, had not completed after 10 minutes, and took the
   machine unreachable over SSH. Cross-compiling the same binary locally takes
   ~8 s and yields a 17 MB static executable. Hence `Dockerfile.prebuilt`.
2. **`gcloud compute instances reset` is a power cycle, not a reboot.** Running it
   moments after writing `/etc/systemd/system/*.service` truncated those files to
   0 bytes, and systemd reports a zero-length unit as **masked** — so autostart
   would silently never have run. It looked fine because Docker's
   `restart: unless-stopped` brought the containers back anyway.
3. **Ubuntu 26.04's minimal image has no `crontab`.** Backups use a systemd timer.

Measured footprint once running:

| Container | Using | Limit |
|---|---|---|
| notifco | 20 MB | 180 MB |
| postgres | 22 MB | 256 MB |
| redis | 6 MB | 80 MB |

~48 MB total against a 516 MB budget, ~530 MB host memory still available, swap
essentially untouched. The 1 GB ceiling only bites while compiling.

Verified after a graceful reboot: `notifco.service` journal shows
`Starting` → `Finished`, all three containers healthy, Discord gateway
reconnected with four commands registered, 7,891 courses loaded from the live
API, and the nightly backup timer scheduled.

---

## 11. Reserve notes: a second source, and why

Request: show "Reserve: Year 1 Students" style notes under a lecture in `/search`.

**They are not in the Open Data API.** The Class schema has sixteen fields
(courseId, courseOfferNumber, sessionCode, classSection, termCode, classNumber,
courseComponent, associatedClassCode, maxEnrollmentCapacity, enrolledStudents,
enrollConsentCode/Description, dropConsentCode/Description, scheduleData,
instructorData). None carries reserve text. The only free text is the consent
descriptions, which across 254 surveyed sections were only "No Consent Required"
and "Department Consent Required". The spec's only `notes` fields belong to exam
schedules and important dates. `Courses.requirementsDescription` is a course-level
prerequisite list, not a per-section reserve.

**They are on UW's public Schedule of Classes page.** For AFM 101:
`Reserve: Year 1 Math Students`, in an italic row directly beneath the section it
applies to, with the reserve's own capacity and enrolment alongside. Surveying 22
courses (225 sections): 132 reserve rows, and 29 sections with more than one
(PSYCH 101 sections carry four).

**Joining.** Class numbers on the page equal the API's `classNumber` (CS 341: the
same seven numbers), so notes attach to the API's sections by class number.

**What the site is.** An HTML page, not an API. Landing page HTTP 200, no login.
`/robots.txt` returns 404. No terms-of-use or disclaimer text on the lookup pages
(891 and 1840 characters of text checked). Response time about 50 ms. A nonexistent
course returns HTTP 200 with zero sections. Courses numbered 500 and above that are
graduate (CS 686) appear only at `level=grad`; undergraduate (`level=under`) returns
zero sections for them.

**Silence is not permission.** The project's rule is "nothing behind a login", and
that holds, but this source has no stated terms either way. So the integration is
built to be light and removable:

| Behaviour | Value |
|---|---|
| When it is contacted | only when someone runs `/search` for that course |
| Requests per course | 1 (a second only for 500+ courses that miss at the first level) |
| Global rate | 1 request per second, burst 2 |
| Cache | 6 hours for success, 5 minutes for failure |
| Concurrent identical lookups | coalesced into one request |
| Load shedding | refused outright beyond 6 lookups in flight |
| Cache size | bounded at 2000 entries |
| Response size | capped at 1 MiB; oversize is refused, not truncated |
| Redirects | at most 3, never off the original host |
| Inputs | validated against `^[A-Z]{2,8}$` and `^[0-9]{3}[A-Z]?$` before any URL is built |
| Identification | `User-Agent: notifco/0.1 (+https://github.com/vhzhen/notifco)` |
| Kill switch | `NOTIFCO_SCHEDULE_NOTES=false` |
| Mock source | always off, so CI and local runs never contact it |

**It can never break `/search`.** The lookup runs detached from the caller, so a
slow response still warms the cache. `/search` waits at most 2.5 s for it and a
Previous/Next click at most 0.7 s (Discord needs an answer within 3 s and a button
click cannot be deferred). Any failure, timeout, overload or layout change leaves
the sections as they were and adds "Reserve notes are unavailable right now" so a
missing line is not mistaken for "no reserves".

**Layout drift is detected, not assumed away.** If a page contains the text
"Reserve:" but zero reserve rows were parsed, that is reported as a layout change
instead of "no reserves". It fired for real during development: a parser bug
(returning after a `<tr>`, so the nested section table was never walked) found zero
sections on every page while the word still appeared 18 times in PSYCH 101.

**Untrusted text.** Notes come from a third-party page and are printed into an
embed. They are cleaned in the parser (control characters, invalid UTF-8,
non-breaking spaces, length capped at 160 characters, at most 12 per section) and
again at render time (markdown and mention characters backslash-escaped, line breaks
flattened so one note is always one line). All 15 distinct real notes across the
captured pages pass through unchanged.

**Reserve notes are never stored.** They are fetched per request and not written to
Postgres, so there is no stale copy to correct.

Fixtures: eight real pages in `internal/source/schedpage/testdata` (AFM 101,
PSYCH 101, CS 135, CS 341, ECE 105, MATH 135, graduate CS 686, and an empty result).
Their instructor and room cells are empty, so they carry no personal data.
