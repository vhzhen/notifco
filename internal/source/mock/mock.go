// Package mock implements source.CourseSource without network access.
//
// Two uses:
//
//   - Running the bot with no API key. Fixtures are real captured responses
//     (see internal/source/testdata), so behaviour matches production shapes.
//   - Tests. Scripted lets a test drive an exact sequence of enrollment states,
//     which is how the differ and the end-to-end flow are exercised.
package mock

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vhzhen/notifco/internal/source"
)

//go:embed all:testdata
var fixtures embed.FS

// Source is a fixture-backed CourseSource. Safe for concurrent use.
type Source struct {
	mu       sync.RWMutex
	term     source.TermCode
	courses  []source.Course
	ids      []string
	sections map[string][]source.SectionState // course code -> sections

	// script, when non-empty, overrides sections per call. Each entry is
	// consumed in order; the last entry repeats once exhausted.
	script map[string][]scriptStep
	// calls counts CourseSections invocations per course code.
	calls map[string]int

	// failNext, when > 0, makes the next N CourseSections calls fail. Used to
	// prove the poller and notifier survive upstream errors.
	failNext int
}

type scriptStep struct {
	sections []source.SectionState
}

// New returns a Source seeded from the embedded real-response fixtures.
func New() (*Source, error) {
	s := &Source{
		term:     "1269",
		sections: make(map[string][]source.SectionState),
		script:   make(map[string][]scriptStep),
		calls:    make(map[string]int),
	}

	// Catalog slice: 40 real CS/MATH/STAT/ECE courses.
	var rawCourses []struct {
		CourseID      string `json:"courseId"`
		TermCode      string `json:"termCode"`
		SubjectCode   string `json:"subjectCode"`
		CatalogNumber string `json:"catalogNumber"`
		Title         string `json:"title"`
		Description   string `json:"description"`
	}
	if err := readJSON("testdata/courses_1269_slice.json", &rawCourses); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for _, c := range rawCourses {
		code := source.NormalizeCourseCode(c.SubjectCode + c.CatalogNumber)
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		s.courses = append(s.courses, source.Course{
			TermCode:    s.term,
			CourseCode:  code,
			CourseID:    c.CourseID,
			Subject:     strings.ToUpper(c.SubjectCode),
			Catalog:     strings.ToUpper(c.CatalogNumber),
			Title:       c.Title,
			Description: c.Description,
		})
	}

	if err := readJSON("testdata/classschedules_1269_courseids_slice.json", &s.ids); err != nil {
		return nil, err
	}

	// Real CS 341 sections, including two at 99/100.
	var rawClasses []struct {
		TermCode         string  `json:"termCode"`
		ClassNumber      int     `json:"classNumber"`
		CourseComponent  *string `json:"courseComponent"`
		ClassSection     int     `json:"classSection"`
		MaxEnrollmentCap int     `json:"maxEnrollmentCapacity"`
		EnrolledStudents int     `json:"enrolledStudents"`
	}
	if err := readJSON("testdata/classschedules_1269_CS_341.json", &rawClasses); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var cs341 []source.SectionState
	for _, r := range rawClasses {
		if r.CourseComponent == nil {
			continue
		}
		cs341 = append(cs341, source.SectionState{
			TermCode:    s.term,
			ClassNumber: r.ClassNumber,
			CourseCode:  "cs341",
			CourseTitle: "Algorithms",
			SectionName: source.SectionNameFrom(*r.CourseComponent, r.ClassSection),
			Component:   strings.ToUpper(*r.CourseComponent),
			Capacity:    r.MaxEnrollmentCap,
			Enrolled:    r.EnrolledStudents,
			IsOnline:    source.IsOnlineSection(r.ClassSection),
			ObservedAt:  now,
		})
	}
	s.sections["cs341"] = cs341
	return s, nil
}

func readJSON(name string, dst any) error {
	b, err := fixtures.ReadFile(name)
	if err != nil {
		return fmt.Errorf("mock: read fixture %s: %w", name, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("mock: parse fixture %s: %w", name, err)
	}
	return nil
}

// SetSections replaces the sections returned for a course.
func (s *Source) SetSections(code string, sections []source.SectionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sections[source.NormalizeCourseCode(code)] = sections
}

// Script queues an exact sequence of responses for a course. Each call to
// CourseSections consumes one step; the final step repeats thereafter.
func (s *Source) Script(code string, steps ...[]source.SectionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	norm := source.NormalizeCourseCode(code)
	s.script[norm] = nil
	for _, st := range steps {
		s.script[norm] = append(s.script[norm], scriptStep{sections: st})
	}
	s.calls[norm] = 0
}

// FailNext makes the next n CourseSections calls return an error.
func (s *Source) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// Calls reports how many times a course has been polled.
func (s *Source) Calls(code string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.calls[source.NormalizeCourseCode(code)]
}

// AddCourse registers a synthetic course, for tests that need many of them.
func (s *Source) AddCourse(c source.Course, sections []source.SectionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.courses = append(s.courses, c)
	s.sections[c.CourseCode] = sections
}

func (s *Source) CurrentTerm(context.Context) (source.TermCode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.term, nil
}

func (s *Source) Courses(_ context.Context, term source.TermCode) ([]source.Course, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]source.Course, 0, len(s.courses))
	for _, c := range s.courses {
		c.TermCode = term
		out = append(out, c)
	}
	return out, nil
}

func (s *Source) ScheduledCourseIDs(context.Context, source.TermCode) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.ids...), nil
}

func (s *Source) CourseSections(
	_ context.Context, term source.TermCode, subject, catalog string,
) ([]source.SectionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failNext > 0 {
		s.failNext--
		return nil, fmt.Errorf("mock: injected failure for %s %s", subject, catalog)
	}

	code := source.NormalizeCourseCode(subject + catalog)
	n := s.calls[code]
	s.calls[code] = n + 1

	var out []source.SectionState
	if steps, ok := s.script[code]; ok && len(steps) > 0 {
		if n >= len(steps) {
			n = len(steps) - 1 // last step repeats
		}
		out = steps[n].sections
	} else {
		out = s.sections[code]
	}

	// Copy so callers cannot mutate fixture state, and stamp observation time.
	now := time.Now().UTC()
	cp := make([]source.SectionState, len(out))
	for i, sec := range out {
		sec.TermCode = term
		sec.ObservedAt = now
		cp[i] = sec
	}
	return cp, nil
}
