// Package source abstracts the upstream course-data provider.
//
// Everything above this package depends only on CourseSource, so the real UW
// API and the fixture-backed mock are interchangeable. That is what lets the
// whole system be tested, and CI be green, without an API key.
//
// Shape of the upstream, measured 2026-10-11 (see VERIFIED.md):
//
//   - Courses/{term} returns the whole catalog in one request (~7.1 MB).
//   - ClassSchedules/{term} returns ONLY an array of courseId strings. It has
//     no enrollment data, so there is no bulk enrollment endpoint.
//   - ClassSchedules/{term}/{subject}/{catalog} returns every section of one
//     course, with enrollment. This is the unit the poller schedules on: one
//     request covers all sections of a course.
package source

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TermCode is a UW term identifier, e.g. "1269" for Fall 2026.
// The encoding is (year-1900)*10 + season, where season is 1, 5, or 9.
type TermCode string

// SectionState is one section's enrollment at a point in time.
//
// Identity is (TermCode, ClassNumber): class numbers are unique within a term
// and are the only stable key the upstream offers.
type SectionState struct {
	TermCode    TermCode
	ClassNumber int
	CourseCode  string // normalised, e.g. "cs341"
	CourseTitle string
	SectionName string // display form, e.g. "LEC 001"
	Component   string // LEC | TUT | LAB | TST | ...
	Capacity    int    // maxEnrollmentCapacity
	Enrolled    int    // enrolledStudents
	IsOnline    bool
	ObservedAt  time.Time // set by us; the upstream exposes no timestamp
}

// IsFull reports whether the section has no free seats.
func (s SectionState) IsFull() bool { return s.Enrolled >= s.Capacity }

// SeatsOpen returns the number of free seats, never negative. Upstream
// occasionally reports Enrolled > Capacity (over-enrollment by the registrar).
func (s SectionState) SeatsOpen() int {
	if n := s.Capacity - s.Enrolled; n > 0 {
		return n
	}
	return 0
}

// Key returns the stable identity of the section.
func (s SectionState) Key() SectionKey {
	return SectionKey{TermCode: s.TermCode, ClassNumber: s.ClassNumber}
}

// SectionKey is the unique identity of a section.
type SectionKey struct {
	TermCode    TermCode
	ClassNumber int
}

func (k SectionKey) String() string {
	return fmt.Sprintf("%s:%d", k.TermCode, k.ClassNumber)
}

// Course is a catalog entry, used to power search.
type Course struct {
	TermCode    TermCode
	CourseCode  string // "cs341"
	CourseID    string // upstream opaque id, e.g. "004392"
	Subject     string // "CS"
	Catalog     string // "341"
	Title       string
	Description string
}

// CourseSource is the upstream contract.
//
// Implementations must be safe for concurrent use: the poller calls
// CourseSections from several workers at once.
type CourseSource interface {
	// CurrentTerm returns the term in progress.
	CurrentTerm(ctx context.Context) (TermCode, error)

	// Courses returns a term's full catalog. One upstream request.
	Courses(ctx context.Context, term TermCode) ([]Course, error)

	// ScheduledCourseIDs returns the courseIds that actually have classes in a
	// term. One upstream request. Bounds the polling universe: for term 1269,
	// 2179 of 7295 catalog courseIds have schedules.
	ScheduledCourseIDs(ctx context.Context, term TermCode) ([]string, error)

	// CourseSections returns every section of one course, with enrollment.
	// A course with no sections in that term yields an empty slice, not an error.
	CourseSections(ctx context.Context, term TermCode, subject, catalog string) ([]SectionState, error)

	// CourseInfo returns one course's catalog entry, including its title.
	// ClassSchedules carries no title, so this is where titles come from.
	// Returns (nil, nil) when the course does not exist in that term.
	CourseInfo(ctx context.Context, term TermCode, subject, catalog string) (*Course, error)

	// TermName returns the human name of a term, e.g. "Fall 2026".
	TermName(ctx context.Context, term TermCode) (string, error)
}

// FallbackTermName derives "Fall 2026" from a term code like "1269" without
// any network call. Used when the API is unreachable, so the UI never shows a
// bare number. Format: (year-1900)*10 + season, where season is 1 (Winter),
// 5 (Spring) or 9 (Fall).
func FallbackTermName(term TermCode) string {
	s := string(term)
	if len(s) != 4 {
		return s
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return s
	}
	season := map[int]string{1: "Winter", 5: "Spring", 9: "Fall"}[n%10]
	if season == "" {
		return s
	}
	return fmt.Sprintf("%s %d", season, 1900+n/10)
}

// NormalizeCourseCode turns user input into the canonical internal form:
// "CS 341", "cs-341", "CS341" all become "cs341".
func NormalizeCourseCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
		// Spaces, hyphens and anything else are dropped.
	}
	return b.String()
}

// SplitCourseCode splits a normalised code into subject and catalog number,
// which is what the upstream per-course endpoint takes. "cs341" -> ("CS", "341").
//
// The split is at the first digit: UW subjects are alphabetic and catalog
// numbers start with a digit, possibly with a trailing letter ("MATH237L").
func SplitCourseCode(code string) (subject, catalog string, err error) {
	norm := NormalizeCourseCode(code)
	idx := -1
	for i, r := range norm {
		if r >= '0' && r <= '9' {
			idx = i
			break
		}
	}
	if idx <= 0 || idx == len(norm) {
		return "", "", fmt.Errorf("not a course code: %q", code)
	}
	return strings.ToUpper(norm[:idx]), strings.ToUpper(norm[idx:]), nil
}

// SectionNameFrom builds the display name for a section from its component and
// section number, matching how UW presents them: "LEC 001", "LAB 101".
//
// classSection is the real section number as an integer (1, 101, 201), not a
// sequential index, so it is zero-padded to three digits.
func SectionNameFrom(component string, classSection int) string {
	return fmt.Sprintf("%s %03d", strings.ToUpper(component), classSection)
}

// IsOnlineSection reports whether a section number denotes an online offering.
// UW encodes this as an 8 in the tens position of the 3-digit number, e.g. 081.
func IsOnlineSection(classSection int) bool {
	name := fmt.Sprintf("%03d", classSection)
	return len(name) >= 2 && name[len(name)-2] == '8'
}

// MatchSection reports whether user input refers to the given section.
// Accepts "LEC 001", "lec001", "001", or "1".
func MatchSection(input, sectionName string, classSection int) bool {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r == ' ' || r == '-' || r == '_' {
				continue
			}
			if r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	in := norm(input)
	if in == "" {
		return false
	}
	if in == norm(sectionName) {
		return true
	}
	// Bare numbers: "001" and "1" both match section 1.
	if in == fmt.Sprintf("%03d", classSection) || in == fmt.Sprintf("%d", classSection) {
		return true
	}
	return false
}
