// Package store is the Postgres persistence layer.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vhzhen/notifco/internal/source"
)

//go:embed all:migrations
var migrationFS embed.FS

// Store wraps a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and verifies the connection.
//
// maxConns caps the pool. Each Postgres backend costs memory, so on a 1 GB
// free-tier VM this wants to be small (4-8) rather than the default.
func Open(ctx context.Context, dsn string, maxConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	if maxConns <= 0 {
		maxConns = 16
	}
	cfg.MaxConns = int32(maxConns)
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate applies every embedded migration in filename order. Migrations are
// idempotent (IF NOT EXISTS throughout), so this is safe to run on every boot.
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		sqlBytes, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		if _, err := s.pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	return nil
}

// --- users ------------------------------------------------------------------

// User is a Discord user known to notifco.
type User struct {
	ID                int64
	DiscordUserID     string
	DMChannelID       string
	DMsBlockedAt      *time.Time
	FallbackGuildID   string
	FallbackChannelID string
}

// UpsertUser returns the user's internal id, creating the row if needed.
func (s *Store) UpsertUser(ctx context.Context, discordUserID string) (int64, error) {
	const q = `
INSERT INTO users (discord_user_id) VALUES ($1)
ON CONFLICT (discord_user_id) DO UPDATE SET discord_user_id = EXCLUDED.discord_user_id
RETURNING id`
	var id int64
	if err := s.pool.QueryRow(ctx, q, discordUserID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: upsert user: %w", err)
	}
	return id, nil
}

// SetFallbackChannel records where to ping a user whose DMs are closed.
func (s *Store) SetFallbackChannel(ctx context.Context, userID int64, guildID, channelID string) error {
	const q = `UPDATE users SET fallback_guild_id = $2, fallback_channel_id = $3 WHERE id = $1`
	_, err := s.pool.Exec(ctx, q, userID, guildID, channelID)
	if err != nil {
		return fmt.Errorf("store: set fallback channel: %w", err)
	}
	return nil
}

// MarkDMsBlocked records that Discord refused a DM to this user.
func (s *Store) MarkDMsBlocked(ctx context.Context, userID int64) error {
	const q = `UPDATE users SET dms_blocked_at = NOW() WHERE id = $1`
	_, err := s.pool.Exec(ctx, q, userID)
	return err
}

// CacheDMChannel stores a resolved DM channel id.
func (s *Store) CacheDMChannel(ctx context.Context, userID int64, channelID string) error {
	const q = `UPDATE users SET dm_channel_id = $2, dms_blocked_at = NULL WHERE id = $1`
	_, err := s.pool.Exec(ctx, q, userID, channelID)
	return err
}

// --- sections ---------------------------------------------------------------

// UpsertSections writes observed sections, returning how many rows changed.
//
// This both seeds search and keeps displayed seat counts fresh. It deliberately
// does NOT drive notification: detection is the differ's job against Redis, so
// the write path here is decoupled from alerting.
func (s *Store) UpsertSections(ctx context.Context, states []source.SectionState) error {
	if len(states) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	const q = `
INSERT INTO sections (term_code, class_number, course_code, course_title,
                      section_name, component, capacity, enrolled, is_online, observed_at, meetings)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)
ON CONFLICT (term_code, class_number) DO UPDATE SET
  course_code  = EXCLUDED.course_code,
  course_title = CASE WHEN EXCLUDED.course_title <> '' THEN EXCLUDED.course_title
                      ELSE sections.course_title END,
  section_name = EXCLUDED.section_name,
  component    = EXCLUDED.component,
  capacity     = EXCLUDED.capacity,
  enrolled     = EXCLUDED.enrolled,
  is_online    = EXCLUDED.is_online,
  observed_at  = EXCLUDED.observed_at,
  meetings     = EXCLUDED.meetings`

	for _, st := range states {
		observed := st.ObservedAt
		if observed.IsZero() {
			observed = time.Now().UTC()
		}
		meetings := st.Meetings
		if meetings == nil {
			meetings = []source.Meeting{} // marshal as [], never null
		}
		mj, err := json.Marshal(meetings)
		if err != nil {
			return fmt.Errorf("store: marshal meetings for class %d: %w", st.ClassNumber, err)
		}
		batch.Queue(q, st.TermCode, st.ClassNumber, st.CourseCode, st.CourseTitle,
			st.SectionName, st.Component, st.Capacity, st.Enrolled, st.IsOnline, observed, string(mj))
	}

	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	for range states {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("store: upsert sections: %w", err)
		}
	}
	return nil
}

// UpsertCourseTitles records course titles so search can show them.
func (s *Store) UpsertCourseTitles(ctx context.Context, courses []source.Course) error {
	if len(courses) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	const q = `UPDATE sections SET course_title = $3
               WHERE course_code = $1 AND term_code = $2 AND course_title = ''`
	for _, c := range courses {
		batch.Queue(q, c.CourseCode, c.TermCode, c.Title)
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	for range courses {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("store: upsert course titles: %w", err)
		}
	}
	return nil
}

// Section is a stored section row.
type Section struct {
	ID          int64
	TermCode    string
	ClassNumber int
	CourseCode  string
	CourseTitle string
	SectionName string
	Component   string
	Capacity    int
	Enrolled    int
	IsOnline    bool
	// Meetings are the weekly slots; empty means online or TBA.
	Meetings   []source.Meeting
	ObservedAt time.Time
	// Reserves are "Reserve:" notes such as "Year 1 Math Students". They are
	// NOT stored and NOT in the Open Data API: the bot fills them in per request
	// from UW's public schedule page, so this is empty unless that succeeded.
	Reserves []string
}

// SeatsOpen returns free seats, never negative.
func (s Section) SeatsOpen() int {
	if n := s.Capacity - s.Enrolled; n > 0 {
		return n
	}
	return 0
}

const sectionCols = `id, term_code, class_number, course_code, course_title,
                     section_name, component, capacity, enrolled, is_online, observed_at, meetings`

func scanSections(rows pgx.Rows) ([]Section, error) {
	defer rows.Close()
	var out []Section
	for rows.Next() {
		var s Section
		var rawMeetings []byte
		if err := rows.Scan(&s.ID, &s.TermCode, &s.ClassNumber, &s.CourseCode,
			&s.CourseTitle, &s.SectionName, &s.Component, &s.Capacity,
			&s.Enrolled, &s.IsOnline, &s.ObservedAt, &rawMeetings); err != nil {
			return nil, err
		}
		// A malformed value must not take the whole listing down: the section
		// is still useful without its times, and shows as TBA.
		if len(rawMeetings) > 0 {
			if err := json.Unmarshal(rawMeetings, &s.Meetings); err != nil {
				s.Meetings = nil
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SectionsByCourse returns a course's sections for a term, ordered for display.
func (s *Store) SectionsByCourse(ctx context.Context, term, courseCode string) ([]Section, error) {
	q := `SELECT ` + sectionCols + ` FROM sections
          WHERE term_code = $1 AND course_code = $2
          ORDER BY component, section_name`
	rows, err := s.pool.Query(ctx, q, term, source.NormalizeCourseCode(courseCode))
	if err != nil {
		return nil, fmt.Errorf("store: sections by course: %w", err)
	}
	return scanSections(rows)
}

// SearchCourses finds distinct course codes matching a prefix or title substring.
type CourseHit struct {
	CourseCode   string
	CourseTitle  string
	Sections     int
	OpenSections int
}

// SearchCourses matches on course code prefix first, then title.
func (s *Store) SearchCourses(ctx context.Context, term, query string, limit int) ([]CourseHit, error) {
	norm := source.NormalizeCourseCode(query)
	// NormalizeCourseCode already strips everything but [a-z0-9], so norm is
	// safe in a LIKE prefix. Two guards matter:
	//   * an empty norm must NOT become `LIKE '%'`, which matches everything;
	//   * the raw query is escaped, so a user typing "%" does not wildcard.
	const q = `
SELECT course_code,
       MAX(course_title) AS course_title,
       COUNT(*)          AS sections,
       COUNT(*) FILTER (WHERE enrolled < capacity) AS open_sections
FROM sections
WHERE term_code = $1
  AND (
        ($2 <> '' AND course_code LIKE $2 || '%')
     OR ($3 <> '' AND course_title ILIKE '%' || $3 || '%')
      )
GROUP BY course_code
ORDER BY (course_code = $2) DESC, course_code
LIMIT $4`
	rows, err := s.pool.Query(ctx, q, term, norm, escapeLike(query), limit)
	if err != nil {
		return nil, fmt.Errorf("store: search courses: %w", err)
	}
	defer rows.Close()
	var out []CourseHit
	for rows.Next() {
		var h CourseHit
		if err := rows.Scan(&h.CourseCode, &h.CourseTitle, &h.Sections, &h.OpenSections); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// escapeLike neutralises LIKE/ILIKE wildcards in user input.
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FindSection resolves a (term, course, section) triple to a stored section.
func (s *Store) FindSection(ctx context.Context, term, courseCode, sectionInput string) (*Section, error) {
	sections, err := s.SectionsByCourse(ctx, term, courseCode)
	if err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, ErrNotFound
	}
	for _, sec := range sections {
		if source.MatchSection(sectionInput, sec.SectionName, trailingNumber(sec.SectionName)) {
			out := sec
			return &out, nil
		}
	}
	return nil, ErrNotFound
}

// trailingNumber extracts the numeric suffix of a section name such as
// "LEC 001" -> 1. Returns 0 when there is none.
//
// This is deliberately written without slicing a fixed offset: a stored
// section_name is not guaranteed to be at least three characters, and an
// unguarded slice here was reachable from user slash-command input.
func trailingNumber(sectionName string) int {
	end := len(sectionName)
	start := end
	for start > 0 {
		c := sectionName[start-1]
		if c < '0' || c > '9' {
			break
		}
		start--
	}
	if start == end {
		return 0
	}
	n, err := strconv.Atoi(sectionName[start:end])
	if err != nil {
		return 0
	}
	return n
}

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

// --- subscriptions ----------------------------------------------------------

// Subscribe creates or reactivates a subscription. Returns true if newly added.
func (s *Store) Subscribe(ctx context.Context, userID, sectionID int64) (bool, error) {
	// xmax is 0 for a genuine INSERT and non-zero when ON CONFLICT took the
	// UPDATE path, which is how we tell "new watch" from "already watching".
	const q = `
INSERT INTO subscriptions (user_id, section_id) VALUES ($1, $2)
ON CONFLICT (user_id, section_id) DO UPDATE
  SET active = TRUE, cooldown_until = NULL
RETURNING xmax = 0`
	var inserted bool
	if err := s.pool.QueryRow(ctx, q, userID, sectionID).Scan(&inserted); err != nil {
		return false, fmt.Errorf("store: subscribe: %w", err)
	}
	return inserted, nil
}

// Unsubscribe deactivates one subscription. Returns true if a row changed.
//
// Pending notifications for the subscription are dropped: a user who unwatches
// before delivery should not then receive the alert. Already-sent rows are kept
// so the notification log stays complete.
func (s *Store) Unsubscribe(ctx context.Context, userID, sectionID int64) (bool, error) {
	var changed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		const q = `UPDATE subscriptions SET active = FALSE
                   WHERE user_id = $1 AND section_id = $2 AND active
                   RETURNING id`
		var subID int64
		if err := tx.QueryRow(ctx, q, userID, sectionID).Scan(&subID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // was not subscribed; not an error
			}
			return err
		}
		changed = true
		_, err := tx.Exec(ctx,
			`DELETE FROM notifications WHERE subscription_id = $1 AND status = 'pending'`,
			subID)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: unsubscribe: %w", err)
	}
	return changed, nil
}

// UnsubscribeCourse deactivates every subscription a user has for a course.
func (s *Store) UnsubscribeCourse(ctx context.Context, userID int64, term, courseCode string) (int64, error) {
	var affected int64
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		const q = `
UPDATE subscriptions SET active = FALSE
WHERE user_id = $1 AND active AND section_id IN (
  SELECT id FROM sections WHERE term_code = $2 AND course_code = $3)
RETURNING id`
		rows, err := tx.Query(ctx, q, userID, term, source.NormalizeCourseCode(courseCode))
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		affected = int64(len(ids))
		if len(ids) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx,
			`DELETE FROM notifications WHERE subscription_id = ANY($1) AND status = 'pending'`,
			ids)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: unsubscribe course: %w", err)
	}
	return affected, nil
}

// WatchItem is one row of a user's watchlist.
type WatchItem struct {
	SubscriptionID int64
	Section        Section
}

// Watchlist returns a user's active subscriptions.
func (s *Store) Watchlist(ctx context.Context, userID int64) ([]WatchItem, error) {
	const q = `
SELECT sub.id, sec.id, sec.term_code, sec.class_number, sec.course_code,
       sec.course_title, sec.section_name, sec.component, sec.capacity,
       sec.enrolled, sec.is_online, sec.observed_at
FROM subscriptions sub
JOIN sections sec ON sec.id = sub.section_id
WHERE sub.user_id = $1 AND sub.active
ORDER BY sec.course_code, sec.section_name`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("store: watchlist: %w", err)
	}
	defer rows.Close()
	var out []WatchItem
	for rows.Next() {
		var w WatchItem
		if err := rows.Scan(&w.SubscriptionID, &w.Section.ID, &w.Section.TermCode,
			&w.Section.ClassNumber, &w.Section.CourseCode, &w.Section.CourseTitle,
			&w.Section.SectionName, &w.Section.Component, &w.Section.Capacity,
			&w.Section.Enrolled, &w.Section.IsOnline, &w.Section.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WatchedCourse is a course someone is watching, with scheduling hints.
type WatchedCourse struct {
	TermCode   string
	CourseCode string
	// Watchers is the number of active subscriptions across the course's sections.
	Watchers int
	// FullSections counts watched sections currently at capacity, which is what
	// makes a course "hot" and worth polling more often.
	FullSections int
}

// WatchedCourses returns the distinct courses with at least one active
// subscription. This is the poller's work list.
//
// Deduplicating to courses is the key efficiency property: one upstream request
// returns every section of a course, so a user watching three sections of
// CS 341 costs one request, not three.
func (s *Store) WatchedCourses(ctx context.Context) ([]WatchedCourse, error) {
	const q = `
SELECT sec.term_code,
       sec.course_code,
       COUNT(*)                                                    AS watchers,
       COUNT(*) FILTER (WHERE sec.enrolled >= sec.capacity)        AS full_sections
FROM subscriptions sub
JOIN sections sec ON sec.id = sub.section_id
WHERE sub.active
GROUP BY sec.term_code, sec.course_code
ORDER BY watchers DESC, sec.course_code`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: watched courses: %w", err)
	}
	defer rows.Close()
	var out []WatchedCourse
	for rows.Next() {
		var w WatchedCourse
		if err := rows.Scan(&w.TermCode, &w.CourseCode, &w.Watchers, &w.FullSections); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Counts reports active users and subscriptions, for metrics.
func (s *Store) Counts(ctx context.Context) (users, subs int64, err error) {
	const q = `
SELECT COUNT(DISTINCT user_id), COUNT(*) FROM subscriptions WHERE active`
	if err := s.pool.QueryRow(ctx, q).Scan(&users, &subs); err != nil {
		return 0, 0, fmt.Errorf("store: counts: %w", err)
	}
	return users, subs, nil
}
