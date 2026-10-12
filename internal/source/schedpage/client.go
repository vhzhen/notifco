package schedpage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/vhzhen/notifco/internal/source"
)

// DefaultBaseURL is UW's public Schedule of Classes lookup.
const DefaultBaseURL = "https://classes.uwaterloo.ca/cgi-bin/cgiwrap/infocour/salook.pl"

const (
	// requestTimeout bounds one upstream fetch. Real responses take ~50 ms.
	requestTimeout = 8 * time.Second
	// maxBodyBytes caps a response. Real pages are 4-13 KB; the largest seen was
	// well under 100 KB. Anything near this is not the page we expect.
	maxBodyBytes = 1 << 20
	// maxRedirects, and they may not leave the original host.
	maxRedirects = 3

	// DefaultTTL is how long a successful lookup is reused. Reserve notes change
	// a few times a term, not a few times an hour.
	DefaultTTL = 6 * time.Hour
	// DefaultFailTTL is how long a failure is remembered, so a struggling site is
	// not retried on every /search.
	DefaultFailTTL = 5 * time.Minute

	// requestsPerSecond is the ceiling on upstream requests, shared by everyone.
	// A person clicking around the site makes about this many.
	requestsPerSecond = 1
	// maxInflight sheds load: past this many outstanding fetches a new one is
	// refused immediately instead of queueing behind the rate limiter.
	maxInflight = 6
	// maxCacheEntries bounds memory. One entry per (term, course).
	maxCacheEntries = 2000

	userAgent = "notifco/0.1 (+https://github.com/vhzhen/notifco)"
)

var (
	// ErrBusy is returned when too many fetches are already outstanding.
	ErrBusy = errors.New("schedpage: too many lookups in flight")
	// ErrLayoutChanged means the page mentions reserves but none could be read,
	// so the markup has probably changed and notes would be silently missed.
	ErrLayoutChanged = errors.New("schedpage: page layout not recognised")
	// ErrInvalidCourse is returned for input that could not be a course code.
	ErrInvalidCourse = errors.New("schedpage: not a valid subject and catalog number")

	subjectRE = regexp.MustCompile(`^[A-Z]{2,8}$`)
	catalogRE = regexp.MustCompile(`^[0-9]{3}[A-Z]?$`)
	termRE    = regexp.MustCompile(`^[0-9]{4}$`)
)

// globalSignIntermediate is the GlobalSign "GCC R46 OV TLS CA 2025" intermediate
// certificate (public, expires 2029-06-23).
//
// classes.uwaterloo.ca issues its leaf from this CA but SENDS a different,
// older intermediate, so the chain the server presents cannot be verified.
// Browsers and macOS quietly fetch the missing certificate; Linux (the VM and
// the Alpine container) does not, and every request failed certificate
// verification. The server's chain is simply misconfigured.
//
// Fix: trust this one public intermediate, for THIS client only. Verification
// stays on. It is added to the system roots, never replacing them, and the
// client can only talk to the one UW host (redirects off it are refused). When
// the certificate expires this fails closed: notes show as unavailable. The
// proper fix is for UW to serve the right chain.
//
//go:embed certs/globalsign-gcc-r46-ov-tls-ca-2025.pem
var globalSignIntermediate []byte

// tlsConfig returns system roots plus the embedded intermediate.
func tlsConfig() *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(globalSignIntermediate)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// Options configures a Client. Zero values take the defaults.
type Options struct {
	BaseURL string
	TTL     time.Duration
	FailTTL time.Duration
	Logger  *slog.Logger
	// HTTPClient, if set, replaces the default (used by tests).
	HTTPClient *http.Client
	// Now, if set, replaces time.Now (used by tests to expire the cache).
	Now func() time.Time
	// RatePerSecond overrides the request ceiling (used by tests).
	RatePerSecond float64
}

type cacheEntry struct {
	notes   map[int][]string
	err     error
	expires time.Time
}

// call is one in-flight fetch that any number of callers can wait on.
type call struct {
	done  chan struct{}
	notes map[int][]string
	err   error
}

// Client fetches reserve notes. Safe for concurrent use.
type Client struct {
	base    string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger
	ttl     time.Duration
	failTTL time.Duration
	now     func() time.Time

	mu       sync.Mutex
	cache    map[string]cacheEntry
	inflight map[string]*call
}

// New builds a Client.
func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.FailTTL <= 0 {
		opts.FailTTL = DefaultFailTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	rps := opts.RatePerSecond
	if rps <= 0 {
		rps = requestsPerSecond
	}

	hc := opts.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tlsConfig()
		hc = &http.Client{Timeout: requestTimeout, Transport: tr}
	}
	// Never follow a redirect off the original host: a redirect is the one way
	// a request this client builds could end up somewhere it did not choose.
	base, _ := url.Parse(opts.BaseURL)
	hc2 := *hc
	hc2.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("schedpage: too many redirects")
		}
		if base != nil && req.URL.Host != base.Host {
			return fmt.Errorf("schedpage: refusing redirect to %s", req.URL.Host)
		}
		return nil
	}

	return &Client{
		base:     opts.BaseURL,
		http:     &hc2,
		limiter:  rate.NewLimiter(rate.Limit(rps), 2),
		log:      opts.Logger,
		ttl:      opts.TTL,
		failTTL:  opts.FailTTL,
		now:      opts.Now,
		cache:    make(map[string]cacheEntry),
		inflight: make(map[string]*call),
	}
}

// Reserves returns the reserve notes for every section of a course, keyed by
// class number. A course with no reserves yields an empty map and no error.
//
// It never blocks the caller beyond ctx: the fetch itself runs detached, so if
// ctx expires the caller gets ctx.Err() but the fetch still completes and warms
// the cache for the next caller.
func (c *Client) Reserves(ctx context.Context, term source.TermCode, subject, catalog string) (map[int][]string, error) {
	subject = strings.ToUpper(strings.TrimSpace(subject))
	catalog = strings.ToUpper(strings.TrimSpace(catalog))
	if !termRE.MatchString(string(term)) || !subjectRE.MatchString(subject) || !catalogRE.MatchString(catalog) {
		return nil, ErrInvalidCourse
	}
	key := string(term) + "|" + subject + "|" + catalog

	c.mu.Lock()
	if e, ok := c.cache[key]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.notes, e.err
	}
	cl, running := c.inflight[key]
	if !running {
		if len(c.inflight) >= maxInflight {
			c.mu.Unlock()
			return nil, ErrBusy
		}
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		go c.fetch(key, string(term), subject, catalog, cl)
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
		return cl.notes, cl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetch runs one lookup, stores the outcome, and releases waiters. It uses its
// own context so that a caller giving up does not cancel work other callers, and
// the cache, still benefit from.
func (c *Client) fetch(key, term, subject, catalog string, cl *call) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout+2*time.Second)
	defer cancel()

	notes, err := c.lookup(ctx, term, subject, catalog)
	if err != nil {
		// Warn, not debug: this was invisible once, and it hid a broken TLS
		// chain. Failures are cached for FailTTL, so this is at most one line
		// per course per few minutes.
		c.log.Warn("schedule page lookup failed; reserve notes unavailable",
			"term", term, "subject", subject, "catalog", catalog, "err", err)
	}

	ttl := c.ttl
	if err != nil {
		ttl = c.failTTL
	}
	c.mu.Lock()
	if len(c.cache) >= maxCacheEntries {
		c.evictLocked()
	}
	c.cache[key] = cacheEntry{notes: notes, err: err, expires: c.now().Add(ttl)}
	delete(c.inflight, key)
	c.mu.Unlock()

	cl.notes, cl.err = notes, err
	close(cl.done)
}

// evictLocked drops expired entries, and if that frees nothing, an arbitrary
// quarter of the cache. Callers must hold c.mu.
func (c *Client) evictLocked() {
	now := c.now()
	for k, e := range c.cache {
		if !now.Before(e.expires) {
			delete(c.cache, k)
		}
	}
	if len(c.cache) < maxCacheEntries {
		return
	}
	drop := maxCacheEntries / 4
	for k := range c.cache {
		delete(c.cache, k)
		if drop--; drop <= 0 {
			break
		}
	}
}

// levelsFor decides which schedule levels to ask. Graduate courses (500+) live
// on the graduate page, and some 500-level courses are undergraduate, so those
// try graduate first and fall back to undergraduate. At most two requests.
func levelsFor(catalog string) []string {
	n := 0
	for _, r := range catalog {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	if n >= 500 {
		return []string{"grad", "under"}
	}
	return []string{"under"}
}

func (c *Client) lookup(ctx context.Context, term, subject, catalog string) (map[int][]string, error) {
	var lastErr error
	for _, level := range levelsFor(catalog) {
		page, err := c.get(ctx, level, term, subject, catalog)
		if err != nil {
			lastErr = err
			continue
		}
		if page.Drift() {
			c.log.Warn("schedule page mentions reserves but none were parsed; layout may have changed",
				"subject", subject, "catalog", catalog, "level", level, "mentions", page.Mentions)
			return nil, ErrLayoutChanged
		}
		if page.Sections > 0 {
			return page.Reserves, nil
		}
		// No sections at this level: try the next.
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return map[int][]string{}, nil
}

func (c *Client) get(ctx context.Context, level, term, subject, catalog string) (*Page, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("schedpage: rate limiter: %w", err)
	}
	// url.Values encodes every value, so nothing here can add or alter a query
	// parameter, whatever the inputs were.
	q := url.Values{}
	q.Set("level", level)
	q.Set("sess", term)
	q.Set("subject", subject)
	q.Set("cournum", catalog)
	u := c.base + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("schedpage: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("schedpage: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schedpage: status %d", res.StatusCode)
	}

	// Read one byte past the cap so an oversized body is detected, not silently
	// truncated into something that parses as a smaller page.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("schedpage: read: %w", err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("schedpage: response larger than %d bytes", maxBodyBytes)
	}
	return Parse(strings.NewReader(string(body)))
}

// CacheSize reports cached entries, for tests.
func (c *Client) CacheSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cache)
}
