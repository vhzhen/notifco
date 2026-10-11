package mock

import (
	"context"
	"testing"

	"github.com/vhzhen/notifco/internal/source"
)

func TestNewLoadsRealFixtures(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	term, err := s.CurrentTerm(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if term != "1269" {
		t.Errorf("CurrentTerm = %q, want 1269", term)
	}

	courses, err := s.Courses(ctx, term)
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) == 0 {
		t.Fatal("no courses loaded from fixtures")
	}

	ids, err := s.ScheduledCourseIDs(ctx, term)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("no courseIds loaded from fixtures")
	}
}

// TestCS341FixtureMatchesLiveShape pins the real captured response. These are
// the actual enrollment numbers observed on 2026-10-11.
func TestCS341FixtureMatchesLiveShape(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	sections, err := s.CourseSections(context.Background(), "1269", "CS", "341")
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 7 {
		t.Fatalf("got %d sections, want 7 (real CS 341 response)", len(sections))
	}

	byName := make(map[string]source.SectionState)
	for _, sec := range sections {
		byName[sec.SectionName] = sec
	}

	// Two sections were at 99/100 when captured: the realistic near-full case.
	for _, name := range []string{"LEC 002", "LAB 101"} {
		sec, ok := byName[name]
		if !ok {
			t.Fatalf("missing section %s; have %v", name, keys(byName))
		}
		if sec.Enrolled != 99 || sec.Capacity != 100 {
			t.Errorf("%s = %d/%d, want 99/100", name, sec.Enrolled, sec.Capacity)
		}
		if sec.SeatsOpen() != 1 {
			t.Errorf("%s SeatsOpen = %d, want 1", name, sec.SeatsOpen())
		}
	}

	// The test slot is the large one.
	if tst, ok := byName["TST 201"]; ok {
		if tst.Enrolled != 268 || tst.Capacity != 300 {
			t.Errorf("TST 201 = %d/%d, want 268/300", tst.Enrolled, tst.Capacity)
		}
	} else {
		t.Error("missing TST 201")
	}
}

func keys(m map[string]source.SectionState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestScriptAdvancesPerCall(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	mk := func(enrolled int) []source.SectionState {
		return []source.SectionState{{
			TermCode: "1269", ClassNumber: 1, CourseCode: "cs100",
			SectionName: "LEC 001", Enrolled: enrolled, Capacity: 100,
		}}
	}
	s.Script("cs100", mk(100), mk(99), mk(50))

	want := []int{100, 99, 50, 50, 50} // last step repeats
	for i, w := range want {
		got, err := s.CourseSections(ctx, "1269", "CS", "100")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("call %d: got %d sections", i, len(got))
		}
		if got[0].Enrolled != w {
			t.Errorf("call %d: Enrolled = %d, want %d", i, got[0].Enrolled, w)
		}
	}
	if n := s.Calls("cs100"); n != len(want) {
		t.Errorf("Calls = %d, want %d", n, len(want))
	}
}

func TestFailNextInjectsErrors(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.FailNext(2)

	for i := 0; i < 2; i++ {
		if _, err := s.CourseSections(ctx, "1269", "CS", "341"); err == nil {
			t.Fatalf("call %d: expected injected failure", i)
		}
	}
	if _, err := s.CourseSections(ctx, "1269", "CS", "341"); err != nil {
		t.Fatalf("third call should succeed, got %v", err)
	}
}

func TestUnknownCourseReturnsEmpty(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.CourseSections(context.Background(), "1269", "ZZZ", "999")
	if err != nil {
		t.Fatalf("unknown course should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d sections for unknown course, want 0", len(got))
	}
}
