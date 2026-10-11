# UWFlow Investigation Notes

Reference: `github.com/UWFlow/uwflow` @ `48ac04b`, read-only. All line references are
to that commit. Nothing here is copied into notifco; this is a behavioural analysis.

---

## 0. Summary of what matters for notifco

UWFlow's "notify me when a seat opens" is a **by-product of its nightly catalog
importer**, not a purpose-built watcher. The seat-open test is five lines of SQL in a
Postgres statement-level trigger. The whole mechanism is elegant and cheap, but its
alert latency is bounded by the import cadence, which is **once per day**. For a bot
whose entire value is "tell me fast", that is the thing to beat.

The single biggest discovery is an endpoint UWFlow does not use:
`GET /v3/ClassSchedules/{termCode}` returns **every class in a term in one request**.
UWFlow instead issues one request per course (`ClassSchedules/{term}/{courseId}`),
which is why its import takes hours. This changes the design completely.

---

## 1. Where enrollment/seat data comes from

### Base URL and auth

| Item | Value |
|---|---|
| Base URL | `https://openapi.data.uwaterloo.ca/v3` |
| Auth | API key in a request header |
| Header name | `X-Api-Key` (UWFlow uses this casing; the OpenAPI spec declares `x-api-key`) |
| Env var in UWFlow | `UW_API_KEY_V3` |
| Client timeout | 30s (`ApiTimeout`, `flow/importer/uw/api/client.go`) |

Source: `flow/importer/uw/api/client.go` (`BaseUrlv3`, `Getv3`, `req.Header.Add("X-Api-Key", ...)`).

Verified live by me:

```
GET https://openapi.data.uwaterloo.ca/v3/Terms/current            -> HTTP 401
GET .../v3/Terms/current  with  x-api-key: invalid                -> HTTP 401
GET https://openapi.data.uwaterloo.ca/swagger/v1/swagger.json     -> HTTP 200 (112 KB)
```

The spec is public and unauthenticated; all data endpoints require a key. I do not
have a key, so notifco codes against an interface with a mock (see DESIGN.md §9).

### Endpoints UWFlow actually calls

| Endpoint | Used in | Purpose |
|---|---|---|
| `Courses/{termCode}` | `parts/course/fetch.go:fetchCoursesByTerm` | all courses in a term |
| `ClassSchedules/{termCode}/{courseId}` | `parts/course/fetch.go:fetchClass` | sections **for one course** |
| `ImportantDates` | `parts/term/fetch.go:fetchAll` | term start/end dates |

Note on `ImportantDates`: UWFlow deliberately avoids the `Terms` endpoint because it
snaps dates to month boundaries (`parts/term/struct.go` package comment), so lecture
start/end days are unavailable there.

### Endpoints that exist but UWFlow does NOT use

Extracted from the live OpenAPI spec:

```
/v3/ClassSchedules/{termCode}                          <-- whole term, ONE request
/v3/ClassSchedules/{termCode}/{courseId}               <-- what UWFlow uses
/v3/ClassSchedules/{termCode}/{subject}/{catalogNumber} <-- by "CS 341", no courseId lookup
/v3/Courses/{termCode}
/v3/Courses/{termCode}/{subject}
/v3/Courses/{termCode}/{subject}/{catalogNumber}
/v3/Terms,  /v3/Terms/current,  /v3/Terms/{code}
/v3/ImportantDates,  /v3/ImportantDates/{year}
```

`ClassSchedules/{termCode}` alone collapses UWFlow's multi-thousand-request crawl into
a single call. `ClassSchedules/{termCode}/{subject}/{catalogNumber}` is the natural fit
for a Discord bot, because a user types `CS 341`, not an opaque `courseId`.

### Response shape (enrollment data)

`Class` schema from the OpenAPI spec. The two fields that matter are
`maxEnrollmentCapacity` and `enrolledStudents`:

```
courseId                  string?     maxEnrollmentCapacity  integer   <-- capacity
courseOfferNumber         integer     enrolledStudents       integer   <-- taken
sessionCode               string?     enrollConsentCode      string?
classSection              integer     enrollConsentDescription string?
termCode                  string?     dropConsentCode        string?
classNumber               integer     dropConsentDescription string?
courseComponent           string?     scheduleData           array?   -> ClassSchedule[]
associatedClassCode       integer     instructorData         array?   -> ClassInstructor[]
```

There is **no "seats available" field and no waitlist field**. Availability must be
derived: `enrolledStudents < maxEnrollmentCapacity`. There is also no `lastUpdated`
timestamp on the class, so you cannot tell how stale the upstream figure is, and you
cannot do conditional requests per section.

`courseComponent` is `LEC` / `TUT` / `LAB` / `TST` etc. `classSection` is an integer
(`1`), which UWFlow zero-pads to 3 digits and joins to produce the human section name
`LEC 001` (`parts/course/convert.go:convertSection`).

Caveats UWFlow encodes, worth copying as behaviour:

- `maxEnrollmentCapacity >= 9999` means a dummy/placeholder class — skip it
  (`convert.go:convertSection`).
- `courseComponent == nil` — skip, section is unusable.
- Cross-listed courses (e.g. `SOC 327` / `LS 327`) share one `courseId`;
  `ClassSchedules/{term}/{courseId}` returns sections for **all** of them, distinguished
  by `courseOfferNumber` (`fetch.go` `courseOffer` struct and the `offerToCode` map).
  Fetching per-courseId naively double-imports sections.
- A `(classNumber, termId)` pair is the real unique key for a section. Duplicates appear
  in API responses and UWFlow drops them client-side because the DB constraint would
  abort the whole batch (`convert.go:convertAll`, `seenSections`).
- Section number with `8` as the middle digit (`X8X`) indicates an online section
  (`convert.go`, `IsOnline: sectionNumber[1] == '8'`).
- Many `(course, term)` combinations legitimately 404; UWFlow swallows 404s from
  `fetchClass` and continues (`fetch.go:fetchClass`).

### Rate limits

- **120 requests/minute**, published. Confirmed from two sources: UWFlow's own crontab
  comment ("API rate limiting at 120 req/min") and the UW API's published rate-limit
  documentation.
- UWFlow self-throttles to ~109 req/min with a fixed `550ms` sleep between class
  requests (`classRequestDelay`, `parts/course/fetch.go`), explicitly leaving headroom
  for the course-list calls that share the quota.
- On HTTP 429 the client retries up to `maxRetries = 5`, honouring `Retry-After` when
  present, else waiting `retryMaxDelay + 1s` = **61 seconds** (`retryDelay` in
  `api/client.go`).
- Note: the constants `retryBaseDelay` and the exponential-backoff-with-jitter scheme
  described in `retryDelay`'s doc comment are **not actually implemented** — the
  function ignores `attempt` and returns a flat 61s. The comment lies about the code.
- All non-429 errors (including 5xx) fail immediately with no retry.
- I could not observe `X-RateLimit-*` or `Retry-After` headers, because a 401 response
  carries none. Treat header-based limit discovery as unverified.

---

## 2. How and how often they poll

**Whole catalog, once a day, sequentially.** Not per-section, not on demand.

From `flow/importer/uw/crontab`:

```
20 6 * * * /app/uw hourly    # 06:20 UTC daily
30 05 * * * /app/uw vacuum   # 05:30 UTC daily
```

The action is named `hourly` but is **scheduled daily**. The crontab's own comment
concedes the run takes "several hours due to API rate limiting". Cron is the scheduler;
there is no in-process loop and no queue.

Request volume per run (`parts/course/import.go:ImportAll`):

- 4 terms are imported each run: `PreviousTermId(2)`, `PreviousTermId(1)`,
  `CurrentTermId()`, `NextTermId()` (`common/util/term.go`).
- `fetchCourses`: 1 request per term = **4 requests**.
- `fetchAll`: 1 request per unique `courseId` **per term** = `len(uniqueCourses) × 4`.

With roughly 6–7k unique courses that is ~25–28k sequential requests at 550ms each,
i.e. **~4 hours of wall clock**, which matches the 360-minute Sentry `maxRuntime` for
the `hourly` monitor (`importer/uw/main.go`, `monitors`).

Scheduling / batching / backoff specifics:

- **Scheduling**: OS cron inside the container. The crontab is the single source of
  truth; `importer/uw/cron/cron.go` re-parses it so the Sentry Cron monitor cannot drift
  from what actually runs. Good practice, worth keeping in spirit.
- **Batching**: only on the DB side, and it is the strong part. Rows are streamed into
  `work.*_delta` staging tables with Postgres `COPY` (`tx.CopyFrom`), then applied with
  one bulk `UPDATE ... FROM delta` and one bulk `INSERT ... WHERE NOT EXISTS` inside a
  single transaction (`parts/course/insert.go`).
- **Concurrency**: none. Comment in `fetch.go`: "Requests are kept sequential (one at a
  time)". One section is fetched at a time, for the entire university.
- **Backoff**: only the flat 61s-on-429 described above. No circuit breaker, no adaptive
  interval, no prioritisation of watched sections.
- **Observability**: Sentry Crons check-ins wrap `hourly` and `vacuum` with
  `CheckInMargin: 30` and `maxRuntime` 360/30 min (`importer/uw/main.go:withMonitor`).

---

## 3. Seat-open detection, state storage, and duplicate suppression

### Detection

A **statement-level** `AFTER UPDATE` trigger on `course_section`, using Postgres
transition tables, in `hasura/migrations/default/1559740220527_init/up.sql:695-727`:

```sql
CREATE TRIGGER notify_enrolment_update
AFTER UPDATE ON course_section
REFERENCING NEW TABLE AS updated_table OLD TABLE AS old_table
FOR EACH STATEMENT EXECUTE PROCEDURE insert_course_vacated();
```

The whole seat-open test is this join inside `insert_course_vacated()`:

```sql
WITH vacated AS (
   SELECT n.id AS section_id, n.course_id, n.section_name
   FROM updated_table n
    JOIN old_table o ON n.id = o.id
   WHERE o.enrollment_total >= n.enrollment_capacity   -- was full
     AND n.enrollment_total <  n.enrollment_capacity   -- is now open
)
```

Then it joins `vacated` against `queue.section_subscribed` to get affected users,
aggregates section names per `(user, course)` with `array_agg`, and inserts one row per
`(user, course)` into `queue.section_vacated`.

Properties of this design:

- **Edge-triggered, not level-triggered.** A still-open section does not re-fire,
  because the `old_table` row must have been full. This is the correct primitive.
- **Fires once per statement, not per row.** Because the importer applies all sections
  in one bulk `UPDATE`, the trigger runs once for ~25k sections and produces a set.
  This is genuinely efficient.
- **Grouped per course, not per section.** A user watching three sections of CS 341 that
  all open simultaneously gets one row listing all three (hence the `oneVacatedText` vs
  `manyVacatedText` email templates in `email/format/template.go`).

### State storage

There is **no separate last-seen store**. The previous enrollment numbers *are* the
`course_section` row; the trigger's `old_table` is the pre-`UPDATE` image of that row.
State and serving data are the same rows.

Consequence: detection is strictly coupled to the write path. Only an `UPDATE` that goes
through that one statement can ever produce a notification. Nothing can observe a
transition that happens and reverts between two imports.

### Duplicate suppression

Three mechanisms, in different places:

1. **Subscription uniqueness**: `CONSTRAINT section_subscribed_unique UNIQUE(section_id, user_id)`
   on `queue.section_subscribed`. A user cannot double-subscribe, so cannot be
   double-counted in one fan-out.
2. **`seen_at` watermark**: every queue table has `seen_at TIMESTAMPTZ DEFAULT NULL`.
   Senders scan `WHERE seen_at IS NULL` and stamp `seen_at = NOW()` after a successful
   send (`email/process/info.go:writeQuery`, `email/process/process.go`). This is the
   actual dedup/idempotency mechanism.
3. **Per-course collapsing**: `array_agg` in the trigger, plus a separate pre-pass for
   subscription-confirmation mail that marks sibling sections of the same course as seen
   so a user gets one "you subscribed" email per course rather than per section
   (`email/process/scan.go:scanSubscribed`, `markQuery`).

**`queue.section_vacated` has no unique constraint.** Every distinct full→open
transition inserts a new row, so a section that oscillates full/open across successive
imports mails the user every time. Subscriptions are never auto-consumed — the user
stays subscribed until they manually unsubscribe (the email tells them to click the bell
icon). For a daily importer this is tolerable; at a 60s poll interval it would be a spam
cannon. notifco must handle this explicitly.

### A real bug in the detection predicate

```sql
WHERE o.enrollment_total >= n.enrollment_capacity   -- OLD total vs NEW capacity
```

"Was full" is tested using the **old total against the new capacity**. If capacity is
raised while the section is full — 100/100 becomes 100/120, which is exactly the
"department opened 20 more seats" case, one of the most valuable alerts — then
`100 >= 120` is false and **no notification fires**. The correct test is
`o.enrollment_total >= o.enrollment_capacity`. notifco uses old-vs-old (see DESIGN.md §5).

---

## 4. DB schema for subscriptions and notifications

All from `hasura/migrations/default/1559740220527_init/up.sql`. Abridged to the relevant
columns.

### The watched object

```sql
CREATE TABLE course_section (
  id SERIAL PRIMARY KEY,
  class_number INT NOT NULL,
  course_id INT NOT NULL REFERENCES course(id) ON DELETE CASCADE ON UPDATE CASCADE,
  term_id INT NOT NULL,
  section_name TEXT NOT NULL,          -- "LEC 001"
  enrollment_capacity INT NOT NULL,    -- doubles as last-seen state
  enrollment_total INT NOT NULL,       -- doubles as last-seen state
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT class_number_unique_to_term UNIQUE(class_number, term_id)
);
-- is_online BOOLEAN added later by migration 1759002620330
```

### Subscriptions

Note the subscription list lives in the `queue` schema and does double duty as the
confirmation-email queue:

```sql
CREATE TABLE queue.section_subscribed(
    id SERIAL PRIMARY KEY,
    user_id    INT NOT NULL REFERENCES "user"(id)         ON DELETE CASCADE ON UPDATE CASCADE,
    section_id INT NOT NULL REFERENCES course_section(id) ON DELETE CASCADE ON UPDATE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    seen_at    TIMESTAMPTZ DEFAULT NULL,   -- confirmation mail sent, NOT vacancy state
    CONSTRAINT section_subscribed_unique UNIQUE(section_id, user_id)
);
```

### Notification log

There is no durable notification log. `queue.section_vacated` is the outbox, and the
only record that a notification happened is `seen_at` being non-NULL:

```sql
CREATE TABLE queue.section_vacated(
    id SERIAL PRIMARY KEY,
    user_id       INT NOT NULL REFERENCES "user"(id)   ON DELETE CASCADE ON UPDATE CASCADE,
    course_id     INT NOT NULL REFERENCES course(id)   ON DELETE CASCADE ON UPDATE CASCADE,
    section_names TEXT[] NOT NULL,         -- denormalised, the section FK is lost here
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    seen_at       TIMESTAMPTZ DEFAULT NULL
);
```

No attempt counter, no error column, no delivery-status enum, no dead-letter state.
A row is either unsent (`NULL`) or sent. `section_names` is a denormalised text array, so
after the fact you cannot join a notification back to the section that caused it.

Subscribe/unsubscribe is not a backend endpoint at all: the frontend performs
`insert_queue_section_subscribed` / `delete_queue_section_subscribed` straight through
Hasura GraphQL (`frontend/src/graphql/mutations/SectionSubscription.tsx`), with the
Apollo cache keyed on `{section_id, user_id}` (`frontend/src/graphql/apollo.js`).

### Staging tables

`work.course_section_delta`, `work.course_delta`, `work.section_meeting_delta`, etc. are
`TRUNCATE`d at the start of each import, `COPY`-filled, then merged. These exist purely
so the merge can be one bulk statement — which is also what makes the one-statement
trigger possible.

---

## 5. Delivery and failure handling

### Transport

Email over SMTP, hardcoded to Google's SMTP (`email/smtp/send.go`, package doc says "via
the Google SMTP service"). `smtp.PlainAuth` + `smtp.SendMail`, one TCP connection per
message, no pooling. HTML body assembled by string concatenation of a `prologue`/
`epilogue` around `html/template` output (`email/format/template.go`). Credentials from
`SMTP_*` env vars; `smtp` package `init()` calls `log.Fatal` if any are missing.

### Wake-up path

Postgres `LISTEN`/`NOTIFY`, which is the nice part of the design:

```sql
CREATE FUNCTION sendmail_notify() RETURNS TRIGGER AS $$
    BEGIN PERFORM pg_notify('queue', TG_ARGV[0]); RETURN NULL; END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER notify_section_vacated AFTER INSERT ON queue.section_vacated
FOR EACH STATEMENT EXECUTE PROCEDURE sendmail_notify('section_vacated');
```

The `email` service holds a dedicated pooled connection on `LISTEN queue` and blocks in
`WaitForNotification`, dispatching on the payload string to one of four handlers
(`email/listen.go:listen`, `dispatch`). So once a row lands, delivery starts in
milliseconds. The latency is all upstream, in the daily poll.

### Failure handling — the weakest area

From `email/process/process.go`:

```go
for _, item := range items {
    msg, err := item.Message()
    if err != nil { log.Printf(...); continue }          // template error -> log, skip
    if err := smtp.Send(msg); err != nil { log.Printf(...); continue } // send error -> log, skip
    if _, err := tx.Exec(ctx, info.writeQuery, id); err != nil { log.Printf(...) } // mark error -> log only
}
```

- **No retry.** A failed send is logged and skipped. The row keeps `seen_at = NULL`, so
  it is retried only incidentally, the next time *any* `pg_notify('queue', ...)` fires
  for that source — which for vacancies means the next day's import.
- **No attempt cap or dead-lettering**, so a permanently bad address is re-attempted
  forever on every wake-up.
- **No backoff.** A sequential `for` loop with a fresh SMTP connection each iteration.
  10k recipients would be 10k serial dials inside one transaction.
- **Mark-failure is swallowed.** If `smtp.Send` succeeds but the `seen_at` `UPDATE` fails,
  the error is logged and the loop continues; the row stays `NULL` and **the email is
  sent again later**. At-least-once with a non-idempotent effect.
- **Send happens inside the DB transaction**, and `dispatch` errors abort `listen`
  entirely by returning (`main.go` just `log.Print`s and the process exits). One unknown
  payload or one transaction error takes the mailer down until restart.
- **Startup backlog is not drained.** `listen` only ever reacts to notifications; it does
  not scan for `seen_at IS NULL` rows on boot. Anything queued while the mailer was down
  waits for the next notification.
- No rate limiting toward the SMTP provider, so a large fan-out risks tripping Gmail's
  sending limits — with no retry to recover.

---

## 6. Weaknesses, latency bottlenecks, what notifco does better

### Latency budget, UWFlow

| Stage | Cost |
|---|---|
| Waiting for the next daily cron | 0–24 h |
| Position of the section in the sequential crawl | 0–4 h |
| Bulk merge + trigger | < 1 s |
| `pg_notify` → mailer wakes | ms |
| SMTP send | ~1 s |
| **Seat open → user informed** | **up to ~28 h, ~14 h average** |

A seat that opens and is retaken within a day is often never reported at all, because
both states fall between two snapshots. For course registration this is the difference
between a useful alert and a historical record.

### Ranked weaknesses

1. **Daily full-catalog crawl.** Dominates latency. Spends ~25k requests/day re-reading
   courses nobody watches, while the watched ones wait in line.
2. ~~**Per-course requests when a per-term bulk endpoint exists.**~~ **Retracted after
   live testing — see [VERIFIED.md](./VERIFIED.md) §2.** I assumed `ClassSchedules/{termCode}`
   returned an entire term's classes. It returns only an array of courseId strings with
   no enrollment data. **There is no bulk enrollment endpoint**, so UWFlow's per-course
   requests are the only option the API offers, not a missed optimisation. Their fixable
   cost is *scope* (4 terms × every course, watched or not) and *serialism*, not requests
   per course.
3. **No concurrency.** Strictly `time.Sleep(550ms)` serial. Measured against the live API,
   150 sequential requests completed in 22 s (~409 req/min) with zero 429s, so this pacing
   is roughly 4× more conservative than the service currently tolerates (VERIFIED.md §5).
4. **The capacity-increase bug** in §3 silently drops a whole class of real openings.
5. **No retry on delivery.** A transient SMTP blip loses the alert until tomorrow.
6. **Send inside the transaction, errors kill the listener.** Fragile and unbounded.
7. **No notification history.** `seen_at` only; no attempts, no errors, no latency data,
   and `section_names` loses the section FK.
8. **Unbounded re-notification.** No cooldown and no auto-unsubscribe; at a fast poll
   interval an oscillating section spams.
9. **Detection welded to the importer's write path.** Cannot poll hot sections more often
   without also rewriting the catalog.
10. **Flat 61s backoff on 429**, and `retryDelay`'s doc comment describes jittered
    exponential backoff that is not implemented. 5xx gets no retry at all.
11. **No startup reconciliation**, so mailer downtime silently delays the backlog.
12. **Fan-out is serial**, one SMTP dial per recipient, inside a transaction.

### What notifco does differently

| UWFlow | notifco |
|---|---|
| Daily cron, whole catalog | Continuous poller, **only watched courses**, adaptive interval |
| 4 terms × every course, watched or not | Current + next term, deduplicated to distinct **watched** courses (1 request covers all sections of a course) |
| Sequential, 550ms sleep (~109 req/min) | Worker pool + token bucket at the documented 120 req/min (~409 measured as available headroom) |
| `old.total >= new.capacity` | `old.total >= old.capacity && new.total < new.capacity` |
| Last-seen state = serving table | Last-seen state in Redis, independent of serving data |
| Detection inside importer's UPDATE | Standalone differ, pollable at any frequency |
| `seen_at` flag only | Notification log with attempts, status, error, latency |
| No retry | Queue with capped exponential backoff + jitter, dead-letter after N |
| At-least-once, non-idempotent | Idempotency key `(sub_id, transition_id)`, survives restart |
| No cooldown, manual unsubscribe | Per-subscription cooldown + optional auto-unsubscribe |
| Flat 61s backoff, no 5xx retry | `Retry-After`-aware jittered exponential backoff, 5xx retried |
| Reacts only to notifications | Reconciles unsent rows on boot |
| Latency unmeasured | Alert latency (seat open → message sent) as a first-class metric |

---

## 7. License and permission to use the data source

### UWFlow's license

**MIT**, `LICENSE`, "Copyright (c) 2025 UW Flow". MIT would permit reuse with attribution
and notice retention. Per the brief I am **not copying any code** — notifco is an
independent Go implementation of the ideas documented above. No UWFlow source, SQL, or
templates are carried over. The ideas worth adopting (edge-triggered full→open test,
`LISTEN/NOTIFY` wake-up, staging-table bulk merge) are reimplemented from this analysis.

### Is the data source allowed for this use?

Yes, with conditions. Findings from the UW OpenData project's own statements:

- **Scope is explicitly public data**: "The intent of data offered through this API is
  limited to publicly visible data only. Private data such as student specific
  information is out of the API's scope." Class capacity and enrolled counts are
  publicly visible schedule-of-classes data and sit inside that scope.
- **Access is self-serve.** Key registration is a plain `POST /v3/account/register` +
  `POST /v3/account/confirm`, no approval committee
  (`flow/importer/uw/README.md` documents the working invocation, noting the
  official instructions assume a VPN and that the activation email's command is broken).
- **License**: "There is no explicit license for version 3 of the API. At present, it is
  covered by existing University policy, including, but not exclusively Policy 46 -
  Information Management, and provided on an as-is, best case basis." So: no open data
  license grant, governed by UW policy, no warranty. Not a blocker for a free student
  tool, but it is **not** a guarantee of availability and should not be relied on for
  anything critical.
- **Disclaimer is requested**: "If building services upon this data, please inform your
  users of the inherent risks (as a best practice)." notifco therefore states in `/watch`
  output and the README that data is best-effort, may be stale, and is not authoritative
  — Quest is.
- **Rate limit 120 req/min** is published and must be respected. notifco enforces it
  client-side with a token bucket rather than trusting a sleep.

### Explicitly out of scope — nothing behind a login

- **UW SourceAPI**, the second, "deliberately gated" programme exposing authoritative
  **Quest** and WatIAM data, is **not used**. No credentials, no access, no attempt.
- **Quest itself is never touched.** No scraping, no session replay, no stored student
  credentials, no `idp.uwaterloo.ca` Shibboleth flow.
- notifco reads exactly one upstream: the keyed, public, self-serve Open Data API v3.
- No personal student data is ingested. The only user data stored is a Discord user ID
  and the sections they chose to watch.

### Operating conditions notifco adopts

1. Stay under 120 req/min globally, with headroom; one shared limiter across all workers.
2. Identify the client honestly via a descriptive `User-Agent` and a real project name on
   the key registration.
3. Honour `Retry-After`; back off on 429/5xx instead of hammering.
4. Poll only what users actually watch, and widen the interval when nothing is changing.
5. Show users a "data as of <timestamp>, not authoritative, verify on Quest" disclaimer.
6. Keep the API key out of the repo; env var only, never logged.
