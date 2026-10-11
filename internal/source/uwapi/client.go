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
type Client struct {
	base    string
	key     string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger
	obs     Observer
}

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
	// burst: a burst of 120 followed by silence is far more likely to trip an
	// undocumented limiter than a steady trickle.
	perSec := float64(opts.RequestsPerMinute) / 60.0
	burst := opts.RequestsPerMinute / 12
	if burst < 1 {
		burst = 1
	}

	return &Client{
		base:    strings.TrimRight(opts.BaseURL, "/"),
		key:     opts.APIKey,
		http:    &http.Client{Timeout: requestTimeout},
		limiter: rate.NewLimiter(rate.Limit(perSec), burst),
		log:     opts.Logger,
		obs:     opts.Observer,
	}, nil
}

// get issues a GET and decodes JSON into dst, retrying on 429 and 5xx.
func (c *Client) get(ctx context.Context, endpoint string, dst any) error {
	url := c.base + "/" + strings.TrimLeft(endpoint, "/")

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
			return nil
		}
		lastErr = err

		// 404 is handled by callers that expect it; never retry it.
		if status == http.StatusNotFound {
			return err
		}
		// Only 429 and 5xx are worth retrying. Everything else (401, 400) is
		// a permanent problem that retrying cannot fix.
		retryable := status == http.StatusTooManyRequests || status >= 500 || status == 0
		if !retryable || attempt == maxAttempts-1 {
			return err
		}

		delay := backoff(attempt, retryAfter)
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
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > backoffCap {
			return backoffCap
		}
		return retryAfter
	}
	d := float64(backoffBase) * math.Pow(2, float64(attempt))
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
