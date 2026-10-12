// Package uwapi implements source.CourseSource against the UW Open Data API v3.
package uwapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/vhzhen/notifco/internal/source"
)

const (
	// requestTimeout caps a single upstream call. The slowest observed real
	// response was the 7.1 MB catalog at 0.6s, so this is generous.
	requestTimeout = 30 * time.Second

	maxAttempts   = 5
	backoffBase   = time.Second
	backoffCap    = 60 * time.Second
	backoffJitter = 0.2

	// capacitySentinel marks placeholder classes. Upstream uses absurd
	// capacities for administrative rows that are not real sections.
	capacitySentinel = 9999
)

// Observer receives per-request outcomes for metrics. Nil is fine.
type Observer interface {
	ObserveRequest(endpoint string, status int, dur time.Duration)
	ObserveRateLimitWait(d time.Duration)
}

// Client is a concurrency-safe UW Open Data API v3 client.
//
// All callers share one rate limiter, so the configured ceiling holds no matter
// how many poller workers are running.
//
// The limiter is adaptive. The documented upstream cap is 120 req/min, but the
// docs call the limit experimental and send no rate-limit headers, so a client
// configured above the documented figure is betting on observed behaviour. To
// make that bet safe rather than reckless, a 429 halves the effective rate on
// the spot and sustained success walks it back up toward the configured value.
// If UW tightens enforcement, notifco self-corrects instead of hammering.
type Client struct {
	base    string
	key     string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger
	obs     Observer

	// configured is the rate ceiling from config, in requests per second.
	configured float64

	mu       sync.Mutex
	current  float64 // effective rate, <= configured
	okStreak int     // consecutive successes since the last throttle

	// termNames caches term codes to names ("1269" -> "Fall 2026"). They never
	// change, so entries live for the process lifetime.
	termMu    sync.Mutex
	termNames map[source.TermCode]string

	// backoffBase is the retry backoff base. Overridable so tests do not have
	// to sleep through real exponential delays.
	backoffBase time.Duration
}

// Adaptive limiter tuning.
const (
	// throttleBackoffFactor is applied to the effective rate on each 429.
	throttleBackoffFactor = 0.5
	// recoveryFactor is applied when a success streak is reached.
	recoveryFactor = 1.5
	// recoveryStreak is how many consecutive successes earn a rate increase.
	recoveryStreak = 200
	// minRatePerMin floors the adaptive reduction so the poller never stalls.
	minRatePerMin = 30.0
)

// Options configures a Client.
type Options struct {
	BaseURL string
	APIKey  string
	// RequestsPerMinute is the self-imposed ceiling. The documented upstream
	// limit is 120/min. Measured headroom is ~409/min with no 429s, but the UW
	// docs call the limit experimental and return no rate-limit headers, so we
	// stay at the documented figure by default and treat the rest as margin.
	RequestsPerMinute int
	Logger            *slog.Logger
	Observer          Observer
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("uwapi: API key is required")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = "https://openapi.data.uwaterloo.ca/v3"
	}
	if opts.RequestsPerMinute <= 0 {
		opts.RequestsPerMinute = 120
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	// Smooth the budget across the minute rather than allowing a full-minute
	// burst: a burst of the whole allowance followed by silence is far more
	// likely to trip an undocumented limiter than a steady trickle.
	perSec := float64(opts.RequestsPerMinute) / 60.0
	burst := opts.RequestsPerMinute / 12
	if burst < 1 {
		burst = 1
	}

	return &Client{
		base:        strings.TrimRight(opts.BaseURL, "/"),
		key:         opts.APIKey,
		http:        &http.Client{Timeout: requestTimeout},
		limiter:     rate.NewLimiter(rate.Limit(perSec), burst),
		log:         opts.Logger,
		obs:         opts.Observer,
		configured:  perSec,
		current:     perSec,
		backoffBase: backoffBase,
	}, nil
}

// throttled halves the effective request rate, floored, and logs the change.
// Called when upstream returns 429.
func (c *Client) throttled() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.okStreak = 0

	floor := minRatePerMin / 60.0
	next := c.current * throttleBackoffFactor
	if next < floor {
		next = floor
	}
	if next == c.current {
		return
	}
	c.current = next
	c.limiter.SetLimit(rate.Limit(next))
	c.log.Warn("upstream throttled us; reducing request rate",
		"new_rate_per_min", int(next*60), "configured_per_min", int(c.configured*60))
}

// succeeded records a success and, after a long enough clean streak, walks the
// effective rate back toward the configured ceiling.
func (c *Client) succeeded() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.current >= c.configured {
		return // already at the ceiling; nothing to recover
	}
	c.okStreak++
	if c.okStreak < recoveryStreak {
		return
	}
	c.okStreak = 0
	next := c.current * recoveryFactor
	if next > c.configured {
		next = c.configured
	}
	c.current = next
	c.limiter.SetLimit(rate.Limit(next))
	c.log.Info("no recent throttling; raising request rate",
		"new_rate_per_min", int(next*60), "configured_per_min", int(c.configured*60))
}

// RatePerMinute reports the effective rate, for tests and diagnostics.
func (c *Client) RatePerMinute() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int(c.current * 60)
}

// get issues a GET and decodes JSON into dst, retrying on 429 and 5xx.
func (c *Client) get(ctx context.Context, endpoint string, dst any) error {
	url := c.base + "/" + strings.TrimLeft(endpoint, "/")

	// Reduce the sustained rate at most once per logical request. Without this,
	// a single throttled call would halve the rate once per retry attempt and
	// slam straight to the floor.
	reducedThisCall := false

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Rate limit before every attempt, including retries.
		waitStart := time.Now()
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("uwapi: rate limiter: %w", err)
		}
		if waited := time.Since(waitStart); waited > time.Millisecond && c.obs != nil {
			c.obs.ObserveRateLimitWait(waited)
		}

		status, retryAfter, err := c.do(ctx, url, endpoint, dst)
		if err == nil {
			c.succeeded()
			return nil
		}
		lastErr = err

		// 404 is handled by callers that expect it; never retry it, and treat
		// it as a healthy response for rate-adaptation purposes.
		if status == http.StatusNotFound {
			c.succeeded()
			return err
		}
		// Upstream is rate limiting us: reduce our own sustained rate, not just
		// this attempt.
		if status == http.StatusTooManyRequests && !reducedThisCall {
			c.throttled()
			reducedThisCall = true
		}
		// Only 429 and 5xx are worth retrying. Everything else (401, 400) is
		// a permanent problem that retrying cannot fix.
		retryable := status == http.StatusTooManyRequests || status >= 500 || status == 0
		if !retryable || attempt == maxAttempts-1 {
			return err
		}

		delay := c.backoff(attempt, retryAfter)
		c.log.Warn("uwapi retrying",
			"endpoint", endpoint, "status", status, "attempt", attempt+1,
			"max", maxAttempts, "delay", delay.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

// StatusError carries an upstream HTTP status so callers can branch on it
// without matching substrings of an error message.
type StatusError struct {
	Endpoint string
	Status   int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("uwapi: %s: status %d: %s", e.Endpoint, e.Status, e.Body)
}

// IsNotFound reports whether err is an upstream 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// do performs one attempt. It returns the HTTP status (0 on transport error)
// and any Retry-After hint.
func (c *Client) do(ctx context.Context, url, endpoint string, dst any) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("uwapi: build request: %w", err)
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	// Identify honestly so UW can see who is calling.
	req.Header.Set("User-Agent", "notifco/0.1 (+https://github.com/vhzhen/notifco)")

	start := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		if c.obs != nil {
			c.obs.ObserveRequest(endpoint, 0, time.Since(start))
		}
		// The URL is included by http.Client errors but never the key, which
		// is header-only and never logged.
		return 0, 0, fmt.Errorf("uwapi: %s: %w", endpoint, err)
	}
	defer res.Body.Close()

	if c.obs != nil {
		c.obs.ObserveRequest(endpoint, res.StatusCode, time.Since(start))
	}

	if res.StatusCode >= 400 {
		// Drain a bounded amount for the error message; never log the key.
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return res.StatusCode, parseRetryAfter(res.Header.Get("Retry-After")),
			&StatusError{
				Endpoint: endpoint,
				Status:   res.StatusCode,
				Body:     strings.TrimSpace(string(snippet)),
			}
	}

	if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
		return res.StatusCode, 0, fmt.Errorf("uwapi: %s: decode: %w", endpoint, err)
	}
	return res.StatusCode, 0, nil
}

// backoff returns the delay before the next attempt. A server-provided
// Retry-After always wins; otherwise exponential with jitter.
//
// Note: we never observed a 429 in testing, so whether upstream sends
// Retry-After is unconfirmed. Both paths are handled.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > backoffCap {
			return backoffCap
		}
		return retryAfter
	}
	base := c.backoffBase
	if base <= 0 {
		base = backoffBase
	}
	d := float64(base) * math.Pow(2, float64(attempt))
	if d > float64(backoffCap) {
		d = float64(backoffCap)
	}
	// Jitter spreads concurrent retries instead of realigning them.
	jitter := 1 + backoffJitter*(2*rand.Float64()-1)
	return time.Duration(d * jitter)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// --- wire types -------------------------------------------------------------

type apiTerm struct {
	TermCode string `json:"termCode"`
	Name     string `json:"name"`
}

type apiCourse struct {
	CourseID          string `json:"courseId"`
	CourseOfferNumber int    `json:"courseOfferNumber"`
	TermCode          string `json:"termCode"`
	TermName          string `json:"termName"`
	SubjectCode       string `json:"subjectCode"`
	CatalogNumber     string `json:"catalogNumber"`
	Title             string `json:"title"`
	Description       string `json:"description"`
}

type apiClass struct {
	CourseID          string  `json:"courseId"`
	CourseOfferNumber int     `json:"courseOfferNumber"`
	TermCode          string  `json:"termCode"`
	ClassNumber       int     `json:"classNumber"`
	CourseComponent   *string `json:"courseComponent"`
	ClassSection      int     `json:"classSection"`
	MaxEnrollmentCap  int     `json:"maxEnrollmentCapacity"`
	EnrolledStudents  int     `json:"enrolledStudents"`
}

// --- CourseSource -----------------------------------------------------------

// CurrentTerm returns the term in progress.
func (c *Client) CurrentTerm(ctx context.Context) (source.TermCode, error) {
	var t apiTerm
	if err := c.get(ctx, "Terms/current", &t); err != nil {
		return "", err
	}
	if t.TermCode == "" {
		return "", errors.New("uwapi: Terms/current returned no termCode")
	}
	return source.TermCode(t.TermCode), nil
}

// Courses returns a term's catalog. One request, ~7.1 MB for ~7894 offerings.
func (c *Client) Courses(ctx context.Context, term source.TermCode) ([]source.Course, error) {
	var raw []apiCourse
	if err := c.get(ctx, "Courses/"+string(term), &raw); err != nil {
		return nil, err
	}

	out := make([]source.Course, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		if r.SubjectCode == "" || r.CatalogNumber == "" {
			continue
		}
		code := source.NormalizeCourseCode(r.SubjectCode + r.CatalogNumber)
		// Cross-listed codes repeat per offer number; keep one row per code.
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, source.Course{
			TermCode:    term,
			CourseCode:  code,
			CourseID:    r.CourseID,
			Subject:     strings.ToUpper(r.SubjectCode),
			Catalog:     strings.ToUpper(r.CatalogNumber),
			Title:       r.Title,
			Description: r.Description,
		})
	}
	return out, nil
}

// ScheduledCourseIDs returns the courseIds with classes in a term.
//
// Despite the endpoint name, ClassSchedules/{term} returns ONLY an array of
// courseId strings — no enrollment data. See VERIFIED.md §2.
func (c *Client) ScheduledCourseIDs(ctx context.Context, term source.TermCode) ([]string, error) {
	var ids []string
	if err := c.get(ctx, "ClassSchedules/"+string(term), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// CourseInfo returns one course's catalog entry. Courses/{term}/{subject}/{catalog}
// carries the title and the term name in a single small response (~1 KB),
// versus the 7.1 MB full-catalog download.
func (c *Client) CourseInfo(
	ctx context.Context, term source.TermCode, subject, catalog string,
) (*source.Course, error) {
	endpoint := fmt.Sprintf("Courses/%s/%s/%s", term,
		strings.ToUpper(subject), strings.ToUpper(catalog))

	var raw []apiCourse
	if err := c.get(ctx, endpoint, &raw); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	r := raw[0]
	// Opportunistically learn the term name from the same response.
	if r.TermName != "" {
		c.rememberTermName(term, r.TermName)
	}
	return &source.Course{
		TermCode:    term,
		CourseCode:  source.NormalizeCourseCode(r.SubjectCode + r.CatalogNumber),
		CourseID:    r.CourseID,
		Subject:     strings.ToUpper(r.SubjectCode),
		Catalog:     strings.ToUpper(r.CatalogNumber),
		Title:       strings.TrimSpace(r.Title),
		Description: r.Description,
	}, nil
}

// TermName returns e.g. "Fall 2026" for term 1269. Term names never change, so
// results are cached for the life of the process. If the API is unreachable the
// name is derived locally from the term code rather than failing the UI.
func (c *Client) TermName(ctx context.Context, term source.TermCode) (string, error) {
	c.termMu.Lock()
	if n, ok := c.termNames[term]; ok {
		c.termMu.Unlock()
		return n, nil
	}
	c.termMu.Unlock()

	var t apiTerm
	if err := c.get(ctx, "Terms/"+string(term), &t); err != nil || strings.TrimSpace(t.Name) == "" {
		return source.FallbackTermName(term), err
	}
	c.rememberTermName(term, t.Name)
	return t.Name, nil
}

func (c *Client) rememberTermName(term source.TermCode, name string) {
	c.termMu.Lock()
	defer c.termMu.Unlock()
	if c.termNames == nil {
		c.termNames = make(map[source.TermCode]string)
	}
	c.termNames[term] = name
}

// CourseSections returns every section of one course, with enrollment.
// One request covers all sections, which is why the poller schedules per course.
func (c *Client) CourseSections(
	ctx context.Context, term source.TermCode, subject, catalog string,
) ([]source.SectionState, error) {
	endpoint := fmt.Sprintf("ClassSchedules/%s/%s/%s", term,
		strings.ToUpper(subject), strings.ToUpper(catalog))

	var raw []apiClass
	err := c.get(ctx, endpoint, &raw)
	if err != nil {
		// A course with no sections this term 404s. That is a normal empty
		// result, not a failure; UWFlow swallows these too.
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	code := source.NormalizeCourseCode(subject + catalog)
	now := time.Now().UTC()
	out := make([]source.SectionState, 0, len(raw))
	seen := make(map[int]struct{}, len(raw))

	for _, r := range raw {
		// Upstream quirks, all observed in real data:
		// placeholder capacities, missing components, and duplicate class
		// numbers within one response.
		if r.MaxEnrollmentCap >= capacitySentinel || r.MaxEnrollmentCap <= 0 {
			continue
		}
		if r.CourseComponent == nil || *r.CourseComponent == "" {
			continue
		}
		if _, dup := seen[r.ClassNumber]; dup {
			continue
		}
		seen[r.ClassNumber] = struct{}{}

		out = append(out, source.SectionState{
			TermCode:    term,
			ClassNumber: r.ClassNumber,
			CourseCode:  code,
			SectionName: source.SectionNameFrom(*r.CourseComponent, r.ClassSection),
			Component:   strings.ToUpper(*r.CourseComponent),
			Capacity:    r.MaxEnrollmentCap,
			Enrolled:    r.EnrolledStudents,
			IsOnline:    source.IsOnlineSection(r.ClassSection),
			ObservedAt:  now,
		})
	}
	return out, nil
}
