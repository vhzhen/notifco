package discord

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

func tsec(name, comp string, class, enrolled, capacity int, ms ...source.Meeting) store.Section {
	return store.Section{
		TermCode: "1269", CourseCode: "cs341", CourseTitle: "Algorithms",
		SectionName: name, Component: comp, ClassNumber: class,
		Enrolled: enrolled, Capacity: capacity, Meetings: ms,
	}
}

func mt(days, start, end string) source.Meeting {
	return source.Meeting{Days: days, Start: start, End: end}
}

// realCS341 mirrors internal/source/mock/testdata/classschedules_1269_CS_341.json,
// the CS 341 response captured from the live API on 2026-10-11. It is checked
// against that file by TestRealCS341MatchesTheFixture, so it cannot drift.
func realCS341() []store.Section {
	return []store.Section{
		tsec("LEC 001", "LEC", 6003, 83, 100, mt("NYNYNNN", "13:00", "14:20")),
		tsec("LEC 002", "LEC", 6546, 99, 100, mt("NYNYNNN", "11:30", "12:50")),
		tsec("LEC 003", "LEC", 6547, 86, 100, mt("NYNYNNN", "10:00", "11:20")),
		tsec("LAB 101", "LAB", 6358, 99, 100, mt("NNNNYNN", "10:30", "11:20")),
		tsec("LAB 102", "LAB", 6438, 83, 100, mt("NNNNYNN", "11:30", "12:20")),
		tsec("LAB 103", "LAB", 5934, 86, 100, mt("NNNNYNN", "13:30", "14:20")),
		tsec("TST 201", "TST", 6549, 268, 300, mt("YNNNNNN", "18:00", "19:50")),
	}
}

// TestRealCS341MatchesTheFixture keeps the hand-written realCS341 honest: it
// must agree with the captured API response on every class number, enrolment
// and meeting. (An earlier version of this fixture was typed from memory and
// had wrong times, which is exactly what this guards against.)
func TestRealCS341MatchesTheFixture(t *testing.T) {
	raw, err := os.ReadFile("../source/mock/testdata/classschedules_1269_CS_341.json")
	if err != nil {
		t.Fatal(err)
	}
	var api []struct {
		ClassNumber int `json:"classNumber"`
		Enrolled    int `json:"enrolledStudents"`
		Capacity    int `json:"maxEnrollmentCapacity"`
		Schedule    []struct {
			Start string `json:"classMeetingStartTime"`
			End   string `json:"classMeetingEndTime"`
			Week  string `json:"classMeetingWeekPatternCode"`
			Day   string `json:"classMeetingDayPatternCode"`
		} `json:"scheduleData"`
	}
	if err := json.Unmarshal(raw, &api); err != nil {
		t.Fatal(err)
	}
	byClass := map[int]store.Section{}
	for _, s := range realCS341() {
		byClass[s.ClassNumber] = s
	}
	if len(byClass) != len(api) {
		t.Fatalf("realCS341 has %d sections, fixture has %d", len(byClass), len(api))
	}
	for _, a := range api {
		s, ok := byClass[a.ClassNumber]
		if !ok {
			t.Errorf("class %d is in the fixture but not in realCS341", a.ClassNumber)
			continue
		}
		if s.Enrolled != a.Enrolled || s.Capacity != a.Capacity {
			t.Errorf("class %d: %d/%d, fixture says %d/%d", a.ClassNumber, s.Enrolled, s.Capacity, a.Enrolled, a.Capacity)
		}
		var want []source.Meeting
		for _, m := range a.Schedule {
			if mm, ok := source.MeetingFromAPI(m.Start, m.End, m.Week, m.Day); ok {
				want = append(want, mm)
			}
		}
		want = source.NormalizeMeetings(want)
		if !reflect.DeepEqual(s.Meetings, want) {
			t.Errorf("class %d: meetings %+v, fixture says %+v", a.ClassNumber, s.Meetings, want)
		}
	}
}

// --- the requested format ------------------------------------------------------

// TestScheduleLineFormat pins the exact requested shape:
//
//   - 8:30 AM - 9:20 AM · Monday, Wednesday
func TestScheduleLineFormat(t *testing.T) {
	got := meetingLine(mt("YNYNNNN", "08:30", "09:20"))
	want := "• 8:30 AM - 9:20 AM · Monday, Wednesday"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestDaysInWords(t *testing.T) {
	tests := []struct{ days, want string }{
		{"YNYNYNN", "Monday, Wednesday, Friday"},
		{"NYNYNNN", "Tuesday, Thursday"},
		{"NNNNYNN", "Friday"},
		{"YNNNNNN", "Monday"},
		{"NNNNNYY", "Saturday, Sunday"},
		{"YYYYYYY", "Monday, Tuesday, Wednesday, Thursday, Friday, Saturday, Sunday"},
		{"NNNNNNN", ""},
	}
	for _, tc := range tests {
		if got := daysInWords(mt(tc.days, "08:30", "09:20")); got != tc.want {
			t.Errorf("days %s = %q, want %q", tc.days, got, tc.want)
		}
	}
}

func TestMeetingLineWithoutDaysHasNoDanglingSeparator(t *testing.T) {
	got := meetingLine(mt("NNNNNNN", "08:30", "09:20"))
	if got != "• 8:30 AM - 9:20 AM" {
		t.Errorf("got %q, want the time alone", got)
	}
}

func TestTimeUsesTwelveHourClockWithoutPadding(t *testing.T) {
	for _, tc := range []struct{ start, end, want string }{
		{"08:30", "09:50", "• 8:30 AM - 9:50 AM · Monday"},
		{"10:30", "11:20", "• 10:30 AM - 11:20 AM · Monday"},
		{"11:30", "12:50", "• 11:30 AM - 12:50 PM · Monday"},
		{"13:00", "14:20", "• 1:00 PM - 2:20 PM · Monday"},
		{"18:00", "19:50", "• 6:00 PM - 7:50 PM · Monday"},
	} {
		if got := meetingLine(mt("YNNNNNN", tc.start, tc.end)); got != tc.want {
			t.Errorf("%s-%s: got %q, want %q", tc.start, tc.end, got, tc.want)
		}
	}
}

// --- placement under the section line ---------------------------------------------

// TestScheduleSitsDirectlyUnderTheSectionLine: the requested layout is the
// section line, then the bullet on the very next line.
func TestScheduleSitsDirectlyUnderTheSectionLine(t *testing.T) {
	s := tsec("LEC 001", "LEC", 5986, 76, 92, mt("YNYNNNN", "08:30", "09:20"))
	lines := strings.Split(searchRow(s), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want the section line plus one bullet:\n%s", len(lines), searchRow(s))
	}
	if !strings.HasPrefix(lines[0], "`LEC 001`") || !strings.Contains(lines[0], "`76/92`") || !strings.Contains(lines[0], "16 seats open") {
		t.Errorf("first line is not the usual section line: %q", lines[0])
	}
	if lines[1] != "• 8:30 AM - 9:20 AM · Monday, Wednesday" {
		t.Errorf("second line = %q", lines[1])
	}
}

// TestSearchRowKeepsTheEarlierLook: the section line must be unchanged from the
// layout the user asked to go back to (fill bar, counts, seats text).
func TestSearchRowKeepsTheEarlierLook(t *testing.T) {
	s := tsec("LEC 001", "LEC", 5986, 76, 92, mt("YNYNNNN", "08:30", "09:20"))
	first := strings.Split(searchRow(s), "\n")[0]
	if first != sectionRow(s) {
		t.Errorf("section line changed:\n got  %q\n want %q", first, sectionRow(s))
	}
	for _, want := range []string{"▰", "▱", "`76/92`", "**16 seats open**"} {
		if !strings.Contains(first, want) {
			t.Errorf("section line lost %q: %q", want, first)
		}
	}
}

func TestSearchViewIsNoLongerATable(t *testing.T) {
	d := searchView("cs341", "Fall 2026", realCS341(), 0).embed.Description
	for _, banned := range []string{"```", "\x1b", "Enrolled  Time", "Section Class", "────"} {
		if strings.Contains(d, banned) {
			t.Errorf("table remnant %q is still in the output:\n%s", banned, d)
		}
	}
	for _, want := range []string{"**Lectures**", "**Labs**", "**Tests**", "*Watch one:"} {
		if !strings.Contains(d, want) {
			t.Errorf("earlier-look element %q missing:\n%s", want, d)
		}
	}
}

// --- awkward data -------------------------------------------------------------------

func TestNoTimeSaysTBAOrOnline(t *testing.T) {
	tba := tsec("LEC 001", "LEC", 1001, 5, 10)
	online := tsec("LEC 081", "LEC", 1002, 5, 10)
	online.IsOnline = true

	if got := scheduleLines(tba); len(got) != 1 || got[0] != "• Time TBA" {
		t.Errorf("TBA section: %q", got)
	}
	if got := scheduleLines(online); len(got) != 1 || got[0] != "• Online" {
		t.Errorf("online section: %q", got)
	}
}

// TestSeveralTimesGetOneBulletEach: ECE 105 lectures really do meet at three
// different times.
func TestSeveralTimesGetOneBulletEach(t *testing.T) {
	s := tsec("LEC 001", "LEC", 4730, 146, 140,
		mt("YNNYNNN", "09:30", "10:20"),
		mt("NYNNNNN", "12:30", "13:20"),
		mt("NYNNNNN", "13:30", "14:20"),
	)
	got := scheduleLines(s)
	want := []string{
		"• 9:30 AM - 10:20 AM · Monday, Thursday",
		"• 12:30 PM - 1:20 PM · Tuesday",
		"• 1:30 PM - 2:20 PM · Tuesday",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestManyTimesAreCapped(t *testing.T) {
	var ms []source.Meeting
	for h := 8; h < 16; h++ {
		ms = append(ms, mt("YNNNNNN", fmt.Sprintf("%02d:00", h), fmt.Sprintf("%02d:50", h)))
	}
	got := scheduleLines(tsec("LEC 001", "LEC", 1111, 1, 2, ms...))
	if len(got) != maxSlotsShown+1 {
		t.Fatalf("8 times -> %d lines, want %d", len(got), maxSlotsShown+1)
	}
	if got[len(got)-1] != "• +4 more times" {
		t.Errorf("last line = %q, want a count of what was hidden", got[len(got)-1])
	}
}

// --- limits -------------------------------------------------------------------------

// TestSchedulesStayWithinDiscordLimits: every row now carries an extra line, so
// re-check that worst-case pages still fit and never silently lose sections.
func TestSchedulesStayWithinDiscordLimits(t *testing.T) {
	var sections []store.Section
	for i := 0; i < 80; i++ {
		var ms []source.Meeting
		for h := 0; h < 6; h++ { // more than maxSlotsShown, all seven days
			ms = append(ms, mt("YYYYYYY", fmt.Sprintf("%02d:00", 8+h), fmt.Sprintf("%02d:50", 8+h)))
		}
		sections = append(sections, tsec(fmt.Sprintf("LEC %03d", i+1), "LEC", 1000+i, 1477, 1920, ms...))
	}

	seen := map[string]int{}
	pages := len(paginateRows(sectionRows(sections), pageSize))
	for p := 0; p < pages; p++ {
		d := searchView("cs341", "Fall 2026", sections, p).embed.Description
		if n := len([]rune(d)); n >= maxEmbedDesc {
			t.Fatalf("page %d is %d runes, at or over the %d limit, so content was cut", p, n, maxEmbedDesc)
		}
		for _, line := range strings.Split(d, "\n") {
			for _, s := range sections {
				if strings.HasPrefix(line, "`"+s.SectionName+"`") {
					seen[s.SectionName]++
				}
			}
		}
	}
	if len(seen) != len(sections) {
		t.Errorf("only %d of %d sections visible across %d pages", len(seen), len(sections), pages)
	}
	for name, c := range seen {
		if c != 1 {
			t.Errorf("%s appeared %d times, want once", name, c)
		}
	}
	if pages < 2 {
		t.Errorf("80 sections with long schedules fit on %d page; the character budget is not being applied", pages)
	}
}

// TestRealCS341ScheduleLines checks rendered output against the captured live
// response, so the expected strings are real UW data and not my own typing.
func TestRealCS341ScheduleLines(t *testing.T) {
	want := map[string]string{
		"LEC 001": "• 1:00 PM - 2:20 PM · Tuesday, Thursday",
		"LEC 002": "• 11:30 AM - 12:50 PM · Tuesday, Thursday",
		"LAB 101": "• 10:30 AM - 11:20 AM · Friday",
		"TST 201": "• 6:00 PM - 7:50 PM · Monday",
	}
	for _, s := range realCS341() {
		w, ok := want[s.SectionName]
		if !ok {
			continue
		}
		got := scheduleLines(s)
		if len(got) != 1 || got[0] != w {
			t.Errorf("%s: %q, want [%q]", s.SectionName, got, w)
		}
	}
}

// TestSearchViewShowsAScheduleUnderEverySection is the end-to-end check on the
// actual embed: each section line is immediately followed by its bullet.
func TestSearchViewShowsAScheduleUnderEverySection(t *testing.T) {
	d := searchView("cs341", "Fall 2026", realCS341(), 0).embed.Description
	lines := strings.Split(d, "\n")

	want := map[string]string{
		"`LEC 001`": "• 1:00 PM - 2:20 PM · Tuesday, Thursday",
		"`LEC 003`": "• 10:00 AM - 11:20 AM · Tuesday, Thursday",
		"`LAB 102`": "• 11:30 AM - 12:20 PM · Friday",
		"`TST 201`": "• 6:00 PM - 7:50 PM · Monday",
	}
	found := 0
	for i, ln := range lines {
		for prefix, bullet := range want {
			if !strings.HasPrefix(ln, prefix) {
				continue
			}
			found++
			if i+1 >= len(lines) || lines[i+1] != bullet {
				next := "<end>"
				if i+1 < len(lines) {
					next = lines[i+1]
				}
				t.Errorf("%s is followed by %q, want %q", prefix, next, bullet)
			}
		}
	}
	if found != len(want) {
		t.Errorf("located %d of %d section lines in:\n%s", found, len(want), d)
	}

	// Every section line, without exception, is followed by a bullet.
	rows := 0
	for i, ln := range lines {
		if strings.HasPrefix(ln, "`") && strings.Contains(ln, "·") && strings.Contains(ln, "▰") {
			rows++
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], scheduleBullet+" ") {
				t.Errorf("section line %q has no bullet beneath it", ln)
			}
		}
	}
	if rows != len(realCS341()) {
		t.Errorf("found %d section lines, want %d", rows, len(realCS341()))
	}
}
