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

// indent is the "tab" before the arrow: four no-break spaces (U+00A0).
const indent = "\u00a0\u00a0\u00a0\u00a0"

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
	want := indent + "↳ 8:30 AM - 9:20 AM · Monday, Wednesday"
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
	if got != indent+"↳ 8:30 AM - 9:20 AM" {
		t.Errorf("got %q, want the time alone", got)
	}
}

func TestTimeUsesTwelveHourClockWithoutPadding(t *testing.T) {
	for _, tc := range []struct{ start, end, want string }{
		{"08:30", "09:50", indent + "↳ 8:30 AM - 9:50 AM · Monday"},
		{"10:30", "11:20", indent + "↳ 10:30 AM - 11:20 AM · Monday"},
		{"11:30", "12:50", indent + "↳ 11:30 AM - 12:50 PM · Monday"},
		{"13:00", "14:20", indent + "↳ 1:00 PM - 2:20 PM · Monday"},
		{"18:00", "19:50", indent + "↳ 6:00 PM - 7:50 PM · Monday"},
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
	if lines[0] != "`LEC 001` ▰▰▰▰▰▰▱▱ `76/92` · `5986`" {
		t.Errorf("section line = %q", lines[0])
	}
	if lines[1] != indent+"↳ 8:30 AM - 9:20 AM · Monday, Wednesday" {
		t.Errorf("second line = %q", lines[1])
	}
}

// TestSearchSectionLineShowsTheClassNumberNotSeatsText: the number replaces the
// "N seats open" text, and everything else on the line is as before.
func TestSearchSectionLineShowsTheClassNumberNotSeatsText(t *testing.T) {
	s := tsec("LEC 001", "LEC", 5986, 76, 92, mt("YNYNNNN", "08:30", "09:20"))
	line := strings.Split(searchRow(s), "\n")[0]

	if !strings.Contains(line, "`5986`") {
		t.Errorf("class number missing: %q", line)
	}
	if strings.Contains(line, "seat") {
		t.Errorf("the seats text was meant to be replaced: %q", line)
	}
	for _, want := range []string{"`LEC 001`", "▰", "▱", "`76/92`"} {
		if !strings.Contains(line, want) {
			t.Errorf("section line lost %q: %q", want, line)
		}
	}
	// It sits where the seats text was: after the counts, behind the separator.
	if i, j := strings.Index(line, "`76/92`"), strings.Index(line, "`5986`"); !(i >= 0 && j > i) || !strings.Contains(line[i:j], " · ") {
		t.Errorf("class number is not after the counts behind a separator: %q", line)
	}
}

// TestFullSectionStillSaysFullAndHasItsNumber: the class number matters most on
// a full section (it is the one people watch), so it must never be dropped.
func TestFullSectionStillSaysFullAndHasItsNumber(t *testing.T) {
	full := searchSectionLine(tsec("LEC 001", "LEC", 6003, 100, 100))
	if !strings.Contains(full, "`6003`") {
		t.Errorf("a full section lost its class number: %q", full)
	}
	if !strings.HasSuffix(full, " · Full") {
		t.Errorf("a full section should still say Full: %q", full)
	}
	open := searchSectionLine(tsec("LEC 002", "LEC", 6546, 99, 100))
	if strings.Contains(open, "Full") {
		t.Errorf("an open section must not say Full: %q", open)
	}
}

func TestSearchSectionLineWithoutAClassNumber(t *testing.T) {
	// A zero class number means the data is missing; showing `0` would be a lie.
	line := searchSectionLine(tsec("LEC 001", "LEC", 0, 5, 10))
	if strings.Contains(line, "`0`") {
		t.Errorf("missing class number rendered as 0: %q", line)
	}
}

func TestSearchSectionLineKeepsOnlineMarker(t *testing.T) {
	s := tsec("LEC 081", "LEC", 7001, 5, 10)
	s.IsOnline = true
	if line := searchSectionLine(s); !strings.HasSuffix(line, "· 🌐 online") {
		t.Errorf("online marker lost: %q", line)
	}
}

// --- the indented arrow ------------------------------------------------------------

// TestArrowIsIndentedByATab pins the requested marker: an arrow, preceded by a
// tab-width of space, and nothing like the old bullet.
func TestArrowIsIndentedByATab(t *testing.T) {
	line := meetingLine(mt("YNYNNNN", "08:30", "09:20"))
	if !strings.HasPrefix(line, indent+"↳ ") {
		t.Fatalf("line does not start with an indent then the arrow: %q", line)
	}
	if got := strings.Count(line[:strings.Index(line, "↳")], "\u00a0"); got != 4 {
		t.Errorf("indent is %d no-break spaces, want 4 (one tab)", got)
	}
	if strings.Contains(line, "•") {
		t.Errorf("the old bullet is still present: %q", line)
	}
}

// TestIndentUsesNoBreakSpaces pins the design choice only. It does NOT show the
// indent survives in Discord, which cannot be tested from here. No-break spaces
// resist HTML whitespace collapsing, but they are still Unicode whitespace, so a
// renderer that trims strings would strip them as readily as ordinary spaces.
// (An earlier version of this test asserted otherwise and failed: Go's
// TrimSpace removes U+00A0.)
func TestIndentUsesNoBreakSpaces(t *testing.T) {
	line := meetingLine(mt("YNYNNNN", "08:30", "09:20"))
	lead := line[:strings.Index(line, "↳")]
	if strings.TrimLeft(lead, "\u00a0") != "" {
		t.Errorf("indent contains something other than no-break spaces: %q", lead)
	}
	if n := len([]rune(lead)); n != 4 {
		t.Errorf("indent is %d characters, want 4 (one tab)", n)
	}
}

func TestSearchViewHasNoSeatsTextAndNoBullet(t *testing.T) {
	d := searchView("cs341", "Fall 2026", realCS341(), 0).embed.Description
	for _, banned := range []string{"seats open", "seat open", "•"} {
		if strings.Contains(d, banned) {
			t.Errorf("search output still contains %q:\n%s", banned, d)
		}
	}
	if n := strings.Count(d, "↳"); n != len(realCS341()) {
		t.Errorf("%d arrows, want one per section (%d)", n, len(realCS341()))
	}
}

// TestWatchlistAndWatchRowsAreUnchanged: only /search got the class number, so
// the other views keep the seats text, which is their only availability cue.
func TestWatchlistAndWatchRowsAreUnchanged(t *testing.T) {
	s := sec("LEC", "LEC 002", 99, 100)
	s.ClassNumber = 6546
	if row := sectionRow(s); !strings.Contains(row, "1 seat open") || strings.Contains(row, "6546") {
		t.Errorf("sectionRow changed: %q", row)
	}
	wl := watchlistView([]store.WatchItem{{Section: s}}, map[string]string{"1269": "Fall 2026"}, 0).embed.Description
	if !strings.Contains(wl, "1 seat open") {
		t.Errorf("watchlist lost its seats text:\n%s", wl)
	}
	wc := watchConfirmView("math135", "Algebra", "Fall 2026", []store.Section{s}, 1, 0).embed.Description
	if !strings.Contains(wc, "1 seat open") {
		t.Errorf("watch confirmation lost its seats text:\n%s", wc)
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

	if got := scheduleLines(tba); len(got) != 1 || got[0] != indent+"↳ Time TBA" {
		t.Errorf("TBA section: %q", got)
	}
	if got := scheduleLines(online); len(got) != 1 || got[0] != indent+"↳ Online" {
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
		indent + "↳ 9:30 AM - 10:20 AM · Monday, Thursday",
		indent + "↳ 12:30 PM - 1:20 PM · Tuesday",
		indent + "↳ 1:30 PM - 2:20 PM · Tuesday",
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
	if got[len(got)-1] != indent+"↳ +4 more times" {
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
		"LEC 001": indent + "↳ 1:00 PM - 2:20 PM · Tuesday, Thursday",
		"LEC 002": indent + "↳ 11:30 AM - 12:50 PM · Tuesday, Thursday",
		"LAB 101": indent + "↳ 10:30 AM - 11:20 AM · Friday",
		"TST 201": indent + "↳ 6:00 PM - 7:50 PM · Monday",
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
		"`LEC 001`": indent + "↳ 1:00 PM - 2:20 PM · Tuesday, Thursday",
		"`LEC 003`": indent + "↳ 10:00 AM - 11:20 AM · Tuesday, Thursday",
		"`LAB 102`": indent + "↳ 11:30 AM - 12:20 PM · Friday",
		"`TST 201`": indent + "↳ 6:00 PM - 7:50 PM · Monday",
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
		if strings.HasPrefix(ln, "`") && strings.Contains(ln, "▰") {
			rows++
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], indent+scheduleArrow+" ") {
				t.Errorf("section line %q has no arrow line beneath it", ln)
			}
		}
	}
	if rows != len(realCS341()) {
		t.Errorf("found %d section lines, want %d", rows, len(realCS341()))
	}
}
