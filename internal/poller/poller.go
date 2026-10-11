// Package poller schedules upstream polling of watched courses.
//
// Scheduling unit is the COURSE, not the section. One upstream request returns
// every section of a course (verified against the live API), so a user watching
// LEC 001, LAB 101 and TST 201 of CS 341 costs one request rather than three.
// Deduplicating subscriptions down to distinct courses is what makes 10k
// subscriptions cheap.
//
// There is no bulk enrollment endpoint: ClassSchedules/{term} returns only
// courseId strings. See VERIFIED.md §2. So cost scales with distinct watched
// courses, and at the documented 120 req/min a 60s interval supports ~120 of
// them.
package poller

import (
	"container/heap"
	"context"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/vhzhen/notifco/internal/differ"
	"github.com/vhzhen/notifco/internal/metrics"
	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// Tier describes why a course is being polled.
type Tier string

const (
	// TierBase is the normal watched-course sweep.
	TierBase Tier = "base"
	// TierHot is the tighter interval for courses with full, watched sections.
	TierHot Tier = "hot"
)

// job is one scheduled poll of one course.
type job struct {
	term       source.TermCode
	courseCode string
	subject    string
	catalog    string

	tier     Tier
	watchers int

	dueAt time.Time
	// noChange counts consecutive polls with no enrollment movement. Drives
	// the adaptive widening.
	noChange int

	index int // heap position
}

// priority orders jobs: earlier due first, then more watchers.
type priorityQueue []*job

func (q priorityQueue) Len() int { return len(q) }

func (q priorityQueue) Less(i, j int) bool {
	if !q[i].dueAt.Equal(q[j].dueAt) {
		return q[i].dueAt.Before(q[j].dueAt)
	}
	return q[i].watchers > q[j].watchers
}

func (q priorityQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *priorityQueue) Push(x any) {
	j := x.(*job)
	j.index = len(*q)
	*q = append(*q, j)
}

func (q *priorityQueue) Pop() any {
	old := *q
	n := len(old)
	j := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return j
}

// Enqueuer receives transitions for fan-out. The notifier implements it.
type Enqueuer interface {
	Enqueue(ctx context.Context, t differ.Transition) error
}

// Options configures a Poller.
type Options struct {
	Source   source.CourseSource
	Store    *store.Store
	Differ   *differ.Differ
	Enqueuer Enqueuer
	Metrics  *metrics.Metrics
	Logger   *slog.Logger

	BaseInterval time.Duration
	HotInterval  time.Duration
	MaxInterval  time.Duration
	HotMax       int
	Workers      int

	// RefreshEvery controls how often the watched-course list is reloaded from
	// Postgres, picking up new /watch commands.
	RefreshEvery time.Duration
}

// Poller continuously polls watched courses and feeds transitions onward.
type Poller struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	queue priorityQueue
	known map[string]*job // "term:course" -> job
}

// New builds a Poller, filling in defaults.
func New(opts Options) *Poller {
	if opts.BaseInterval <= 0 {
		opts.BaseInterval = 60 * time.Second
	}
	if opts.HotInterval <= 0 {
		opts.HotInterval = 15 * time.Second
	}
	if opts.MaxInterval <= 0 {
		opts.MaxInterval = 32 * time.Minute
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.HotMax <= 0 {
		opts.HotMax = 50
	}
	if opts.RefreshEvery <= 0 {
		opts.RefreshEvery = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Poller{
		opts:  opts,
		log:   opts.Logger,
		known: make(map[string]*job),
	}
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	if p.opts.Metrics != nil {
		p.opts.Metrics.PollIntervalBound.Set(p.opts.BaseInterval.Seconds())
	}

	// Load the work list before starting workers so the first tick has jobs.
	if err := p.refresh(ctx); err != nil {
		p.log.Warn("initial watched-course refresh failed", "err", err)
	}

	jobs := make(chan *job)
	var wg sync.WaitGroup
	for i := 0; i < p.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx, jobs)
		}()
	}

	refresh := time.NewTicker(p.opts.RefreshEvery)
	defer refresh.Stop()
	// Tick well under the hot interval so due jobs dispatch promptly.
	dispatch := time.NewTicker(time.Second)
	defer dispatch.Stop()

	defer func() {
		close(jobs)
		wg.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refresh.C:
			if err := p.refresh(ctx); err != nil {
				p.log.Warn("watched-course refresh failed", "err", err)
			}
		case <-dispatch.C:
			p.dispatchDue(ctx, jobs)
		}
	}
}

// RunOnce polls every watched course a single time. Used by tests and by the
// one-shot `poll` subcommand.
func (p *Poller) RunOnce(ctx context.Context) error {
	if err := p.refresh(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	type todo struct {
		j    *job
		tier Tier
	}
	work := make([]todo, 0, len(p.known))
	for _, j := range p.known {
		work = append(work, todo{j: j, tier: j.tier})
	}
	p.mu.Unlock()

	for _, w := range work {
		if err := p.pollCourse(ctx, w.j, w.tier); err != nil {
			p.log.Warn("poll failed", "course", w.j.courseCode, "err", err)
		}
	}
	return nil
}

// refresh reloads the watched-course work list from Postgres.
func (p *Poller) refresh(ctx context.Context) error {
	watched, err := p.opts.Store.WatchedCourses(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	seen := make(map[string]struct{}, len(watched))
	hotBudget := p.opts.HotMax

	for _, w := range watched {
		key := w.TermCode + ":" + w.CourseCode
		seen[key] = struct{}{}

		// A course is hot when it has a watched section sitting at capacity:
		// that is precisely the section a seat could open in.
		tier := TierBase
		if w.FullSections > 0 && hotBudget > 0 {
			tier = TierHot
			hotBudget--
		}

		if j, ok := p.known[key]; ok {
			j.watchers = w.Watchers
			j.tier = tier
			continue
		}

		subject, catalog, err := source.SplitCourseCode(w.CourseCode)
		if err != nil {
			p.log.Warn("skipping unparseable course code", "code", w.CourseCode, "err", err)
			continue
		}
		j := &job{
			term:       source.TermCode(w.TermCode),
			courseCode: w.CourseCode,
			subject:    subject,
			catalog:    catalog,
			tier:       tier,
			watchers:   w.Watchers,
			// Stagger first polls so a restart does not fire every request at once.
			dueAt: now.Add(time.Duration(rand.Int63n(int64(time.Second)))),
		}
		p.known[key] = j
		heap.Push(&p.queue, j)
	}

	// Drop courses nobody watches any more.
	for key, j := range p.known {
		if _, still := seen[key]; !still {
			if j.index >= 0 && j.index < len(p.queue) && p.queue[j.index] == j {
				heap.Remove(&p.queue, j.index)
			}
			delete(p.known, key)
		}
	}

	if p.opts.Metrics != nil {
		if users, subs, err := p.opts.Store.Counts(ctx); err == nil {
			p.opts.Metrics.ActiveUsers.Set(float64(users))
			p.opts.Metrics.ActiveSubscriptions.Set(float64(subs))
		}
		if depth, err := p.opts.Store.QueueDepth(ctx); err == nil {
			p.opts.Metrics.QueueDepth.Set(float64(depth))
		}
	}
	return nil
}

// dispatchDue sends every job whose time has come to the worker pool.
func (p *Poller) dispatchDue(ctx context.Context, jobs chan<- *job) {
	now := time.Now()
	for {
		p.mu.Lock()
		if p.queue.Len() == 0 || p.queue[0].dueAt.After(now) {
			p.mu.Unlock()
			return
		}
		j := heap.Pop(&p.queue).(*job)
		j.index = -1
		p.mu.Unlock()

		select {
		case jobs <- j:
		case <-ctx.Done():
			return
		}
	}
}

func (p *Poller) worker(ctx context.Context, jobs <-chan *job) {
	for j := range jobs {
		if ctx.Err() != nil {
			return
		}
		// tier is mutated by refresh under p.mu; snapshot it once.
		p.mu.Lock()
		tier := j.tier
		p.mu.Unlock()

		if err := p.pollCourse(ctx, j, tier); err != nil {
			p.log.Warn("poll failed", "course", j.courseCode, "tier", tier, "err", err)
		}
		p.reschedule(j)
	}
}

// pollCourse fetches one course's sections, records them, and fans out any
// transitions.
//
// noChange is owned by whichever worker currently holds the job. A job is only
// ever dispatched to one worker at a time (dispatchDue pops it off the heap,
// reschedule pushes it back), so it needs no lock.
func (p *Poller) pollCourse(ctx context.Context, j *job, tier Tier) error {
	start := time.Now()
	states, err := p.opts.Source.CourseSections(ctx, j.term, j.subject, j.catalog)
	if p.opts.Metrics != nil {
		p.opts.Metrics.PollCycle.Observe(time.Since(start).Seconds())
	}
	if err != nil {
		p.mark(tier, "error")
		return err
	}
	p.mark(tier, "ok")

	if len(states) == 0 {
		j.noChange++
		return nil
	}

	// Detect transitions BEFORE writing to Postgres. The differ compares
	// against Redis, so ordering here is independent of the catalog write, but
	// doing detection first keeps alert latency off the DB write path.
	transitions, derr := p.opts.Differ.ObserveAll(ctx, states)

	changed := len(transitions) > 0
	if err := p.opts.Store.UpsertSections(ctx, states); err != nil {
		p.log.Warn("upsert sections failed", "course", j.courseCode, "err", err)
	}

	for _, t := range transitions {
		if p.opts.Metrics != nil {
			p.opts.Metrics.TransitionsTotal.Inc()
		}
		p.log.Info("seat opened",
			"course", t.Section.CourseCode, "section", t.Section.SectionName,
			"seats", t.Section.SeatsOpen(), "capacity", t.Section.Capacity,
			"transition", t.ID)
		if p.opts.Enqueuer != nil {
			if err := p.opts.Enqueuer.Enqueue(ctx, t); err != nil {
				p.log.Error("enqueue failed", "transition", t.ID, "err", err)
			}
		}
	}

	if changed {
		j.noChange = 0
	} else {
		j.noChange++
	}
	return derr
}

func (p *Poller) mark(tier Tier, result string) {
	if p.opts.Metrics != nil {
		p.opts.Metrics.PollsTotal.WithLabelValues(string(tier), result).Inc()
	}
}

// reschedule computes the next due time and returns the job to the queue.
//
// Adaptive interval: widen exponentially while nothing changes, snap back to
// base the moment something does, and clamp hot courses to the tight interval.
// Jitter prevents all courses realigning into a synchronised burst.
//
// tier and watchers are mutated by refresh under p.mu, so they are read inside
// the lock here rather than before taking it.
func (p *Poller) reschedule(j *job) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Only requeue if the job is still watched.
	if _, ok := p.known[string(j.term)+":"+j.courseCode]; !ok {
		return
	}
	j.dueAt = time.Now().Add(p.intervalLocked(j.tier, j.noChange))
	heap.Push(&p.queue, j)
}

// intervalLocked computes the next interval. Callers must hold p.mu, or pass
// values not shared with refresh.
func (p *Poller) intervalLocked(tier Tier, noChange int) time.Duration {
	base := p.opts.BaseInterval
	// Widen on quiet courses, capped at 5 doublings.
	exp := noChange
	if exp > 5 {
		exp = 5
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(exp)))

	if tier == TierHot && d > p.opts.HotInterval {
		d = p.opts.HotInterval
	}
	if d > p.opts.MaxInterval {
		d = p.opts.MaxInterval
	}
	if min := p.opts.HotInterval; d < min {
		d = min
	}

	// +/-10% jitter.
	jitter := 1 + 0.1*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}

// NextInterval exposes interval computation for tests.
func (p *Poller) NextInterval(tier Tier, noChange int) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.intervalLocked(tier, noChange)
}

// SeedCatalog populates the sections table for a term so /search and
// /watch work before anyone is subscribed.
//
// Strategy reflects what the API actually offers: the catalog comes in one
// request, but enrollment does not exist in bulk, so sections are only
// materialised for courses explicitly requested via EnsureCourse.
func (p *Poller) SeedCatalog(ctx context.Context, term source.TermCode) error {
	courses, err := p.opts.Source.Courses(ctx, term)
	if err != nil {
		return err
	}
	p.log.Info("catalog loaded", "term", term, "courses", len(courses))
	return p.opts.Store.UpsertCourseTitles(ctx, courses)
}

// EnsureCourse fetches and stores one course's sections on demand. Called by
// /watch and /search so a user can subscribe to a course nobody has
// watched before.
func (p *Poller) EnsureCourse(ctx context.Context, term source.TermCode, courseCode string) ([]source.SectionState, error) {
	subject, catalog, err := source.SplitCourseCode(courseCode)
	if err != nil {
		return nil, err
	}
	states, err := p.opts.Source.CourseSections(ctx, term, subject, catalog)
	if err != nil {
		return nil, err
	}
	if len(states) == 0 {
		return nil, nil
	}
	// Seed differ state so the first observation is never treated as a
	// transition for a section that was already open.
	if _, err := p.opts.Differ.ObserveAll(ctx, states); err != nil {
		p.log.Warn("seed differ state failed", "course", courseCode, "err", err)
	}
	if err := p.opts.Store.UpsertSections(ctx, states); err != nil {
		return nil, err
	}
	return states, nil
}
