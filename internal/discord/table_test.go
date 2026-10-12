package discord

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripANSI removes styling, leaving what a client without ANSI support shows.
func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

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

// tableLines renders sections and returns the plain-text lines of the code
// block (ANSI stripped, fences removed).
func tableLines(t *testing.T, sections []store.Section) []string {
	t.Helper()
	sections = sortSections(sections)
	l := newTableLayout(sections)
	block, _, _ := renderTablePage(tableRows(sections, l), l, 0)
	plain := stripANSI(block)
	plain = strings.TrimPrefix(plain, "```ansi\n")
	plain = strings.TrimSuffix(plain, "```")
	return strings.Split(strings.TrimRight(plain, "\n"), "\n")
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

// --- the five requested columns ----------------------------------------------

func TestTableHasTheFiveRequestedColumnsInOrder(t *testing.T) {
	lines := tableLines(t, realCS341())
	head := strings.Fields(lines[0])
	// "Th" etc. follow Date, so only check the first five tokens.
	want := []string{"Section", "Class", "Enrolled", "Time", "Date"}
	for i, w := range want {
		if head[i] != w {
			t.Fatalf("header column %d = %q, want %q (header: %q)", i, head[i], w, lines[0])
		}
	}
}

func TestTableRowContent(t *testing.T) {
	lines := tableLines(t, realCS341())
	var lec1 string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "LEC 001") {
			lec1 = ln
		}
	}
	if lec1 == "" {
		t.Fatalf("no LEC 001 row:\n%s", strings.Join(lines, "\n"))
	}
	for _, want := range []string{"6003", "83/100", "1:00 PM", "2:20 PM", "M T W Th F S Su"} {
		if !strings.Contains(lec1, want) {
			t.Errorf("LEC 001 row missing %q: %q", want, lec1)
		}
	}
	// The Class column is the 4-digit class number, not the section number.
	if strings.Contains(lec1, "6003") == false {
		t.Error("class number not shown")
	}
}

func TestTimeFormatMatchesRequestedStyle(t *testing.T) {
	// Requested: "8:30 AM - 9:50 AM".
	sections := []store.Section{tsec("LEC 001", "LEC", 1111, 1, 2, mt("YNYNYNN", "08:30", "09:50"))}
	row := strings.Join(tableLines(t, sections), "\n")
	if !strings.Contains(row, "8:30 AM - 9:50 AM") {
		t.Errorf("time not rendered as 8:30 AM - 9:50 AM:\n%s", row)
	}
}

// --- alignment -----------------------------------------------------------------

// TestColumnsAlignOnEveryRow is the core requirement: columns standardised.
// Each header word must start at the same offset as the matching cell in every
// row, even with data chosen to break alignment: a 4-digit enrolment, two-digit
// hours next to one-digit hours, and a three-digit section next to LEC 001.
func TestColumnsAlignOnEveryRow(t *testing.T) {
	sections := append(realCS341(),
		tsec("LEC 081", "LEC", 7001, 1477, 1920, mt("YNYNYNN", "10:30", "11:20")), // 4-digit enrolment, 2-digit hours
		tsec("TUT 101", "TUT", 12345, 5, 9, mt("NNYNNNN", "12:00", "12:50")),      // 5-digit class number
	)
	lines := tableLines(t, sections)

	header := lines[0]
	offsetOf := func(word string) int { return strings.Index(header, word) }
	dateOff := offsetOf("Date")
	enrOff, classOff := offsetOf("Enrolled"), offsetOf("Class")

	for _, ln := range lines[2:] { // skip header and rule
		if strings.TrimSpace(ln) == "" {
			continue
		}
		// Date labels start exactly under "Date".
		if !strings.HasPrefix(ln[dateOff:], "M T W Th F S Su") {
			t.Errorf("Date column misaligned (want it at offset %d): %q", dateOff, ln)
		}
		// Class numbers and Enrolled begin under their headings.
		if strings.TrimSpace(ln[classOff:enrOff]) == "" {
			t.Errorf("Class column empty or misaligned: %q", ln)
		}
	}

	// Hours are right-aligned inside the Time cell, so the dash and both AM/PM
	// markers must sit at one offset on every single-slot row.
	dash, firstMeridiem, secondMeridiem := map[int]bool{}, map[int]bool{}, map[int]bool{}
	mre := regexp.MustCompile(`[AP]M`)
	for _, ln := range lines[2:] {
		if !strings.Contains(ln, " - ") {
			continue
		}
		dash[strings.Index(ln, " - ")] = true
		if loc := mre.FindAllStringIndex(ln, -1); len(loc) == 2 {
			firstMeridiem[loc[0][0]] = true
			secondMeridiem[loc[1][0]] = true
		}
	}
	for name, set := range map[string]map[int]bool{"dash": dash, "first AM/PM": firstMeridiem, "second AM/PM": secondMeridiem} {
		if len(set) != 1 {
			t.Errorf("%s sits at %d different offsets %v, want 1", name, len(set), set)
		}
	}

	// All rows with a Date have the same visible width up to the Date column.
	widths := map[int]bool{}
	for _, ln := range lines[2:] {
		if i := strings.Index(ln, "M T W"); i >= 0 {
			widths[i] = true
		}
	}
	if len(widths) != 1 {
		t.Errorf("Date column starts at %d different offsets %v, want exactly 1", len(widths), widths)
	}
}

func TestEnrolledSlashesLineUp(t *testing.T) {
	sections := []store.Section{
		tsec("LEC 001", "LEC", 1001, 5, 100, mt("YNNNNNN", "09:00", "09:50")),
		tsec("LEC 002", "LEC", 1002, 83, 100, mt("YNNNNNN", "09:00", "09:50")),
		tsec("LEC 003", "LEC", 1003, 1477, 1920, mt("YNNNNNN", "09:00", "09:50")),
	}
	slash := map[int]bool{}
	for _, ln := range tableLines(t, sections)[2:] {
		if i := strings.Index(ln, "/"); i >= 0 {
			slash[i] = true
		}
	}
	if len(slash) != 1 {
		t.Errorf("slashes at %d different offsets: %v", len(slash), slash)
	}
}

// TestColumnWidthsDoNotShiftBetweenPages: widths come from the whole course, so
// paging forward must not reflow the table.
func TestColumnWidthsDoNotShiftBetweenPages(t *testing.T) {
	var sections []store.Section
	for i := 0; i < 30; i++ {
		enrolled, capacity := 90, 100
		if i == 25 { // the wide row lands on the LAST page only
			enrolled, capacity = 1477, 1920
		}
		sections = append(sections, tsec(fmt.Sprintf("LEC %03d", i+1), "LEC", 1000+i, enrolled, capacity,
			mt("YNYNYNN", "08:30", "09:20")))
	}
	sections = sortSections(sections)
	l := newTableLayout(sections)
	rows := tableRows(sections, l)

	first, _, pages := renderTablePage(rows, l, 0)
	last, _, _ := renderTablePage(rows, l, pages-1)
	if pages < 2 {
		t.Fatalf("expected multiple pages, got %d", pages)
	}
	h1 := strings.Split(stripANSI(first), "\n")[1]
	h2 := strings.Split(stripANSI(last), "\n")[1]
	if h1 != h2 {
		t.Errorf("header differs between pages, so the table reflowed:\n%q\n%q", h1, h2)
	}
}

func TestTableFitsWithinWidthLimit(t *testing.T) {
	// Worst realistic content: 4-digit enrolment AND two-digit hours.
	sections := []store.Section{
		tsec("LEC 081", "LEC", 7001, 1477, 1920, mt("YNYNYNN", "10:30", "11:20")),
		tsec("TST 201", "TST", 7002, 268, 300, mt("YNNNNNN", "18:00", "19:50")),
	}
	for _, ln := range tableLines(t, sections) {
		if n := utf8.RuneCountInString(ln); n > maxTableWidth {
			t.Errorf("row is %d wide > %d, so Discord would wrap it: %q", n, maxTableWidth, ln)
		}
	}
}

// --- bold -----------------------------------------------------------------------

// TestMeetingDaysAreBold checks the styling itself, not just the text: exactly
// the days a section meets are wrapped in bold, and no others.
func TestMeetingDaysAreBold(t *testing.T) {
	cases := []struct {
		days string
		bold []string
	}{
		{"YNYNYNN", []string{"M", "W", "F"}},
		{"NYNYNNN", []string{"T", "Th"}},
		{"NNNNNYY", []string{"S", "Su"}},
		{"YYYYYYY", []string{"M", "T", "W", "Th", "F", "S", "Su"}},
		{"NNNNNNN", nil},
	}
	for _, tc := range cases {
		cell := dateCell(mt(tc.days, "08:30", "09:20"))
		var got []string
		for _, m := range regexp.MustCompile("\x1b\\[1m([A-Za-z]+)\x1b\\[0m").FindAllStringSubmatch(cell, -1) {
			got = append(got, m[1])
		}
		if strings.Join(got, ",") != strings.Join(tc.bold, ",") {
			t.Errorf("days %s: bold = %v, want %v", tc.days, got, tc.bold)
		}
		// All seven labels are always shown, bold or not.
		if plain := stripANSI(cell); plain != "M T W Th F S Su" {
			t.Errorf("days %s: visible text = %q, want all seven labels", tc.days, plain)
		}
	}
}

// TestEveryBoldIsClosed: an unclosed bold would style the rest of the block.
func TestEveryBoldIsClosed(t *testing.T) {
	for _, ln := range tableLines2(t, realCS341()) {
		if strings.Count(ln, "\x1b[1m") != strings.Count(ln, "\x1b[0m") {
			t.Errorf("unbalanced bold/reset on a line: %q", ln)
		}
	}
}

// tableLines2 is tableLines without stripping, for style assertions.
func tableLines2(t *testing.T, sections []store.Section) []string {
	t.Helper()
	sections = sortSections(sections)
	l := newTableLayout(sections)
	block, _, _ := renderTablePage(tableRows(sections, l), l, 0)
	return strings.Split(block, "\n")
}

// --- awkward data -------------------------------------------------------------------

// TestNoTimeShowsTBAOrOnline: 13 of 218 surveyed real sections had no times.
// They must say so, not show seven plain day labels that read as "never meets".
func TestNoTimeShowsTBAOrOnline(t *testing.T) {
	tba := tsec("LEC 001", "LEC", 1001, 5, 10)
	online := tsec("LEC 081", "LEC", 1002, 5, 10)
	online.IsOnline = true

	lines := tableLines(t, []store.Section{tba, online})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "TBA") {
		t.Errorf("a section with no times should say TBA:\n%s", joined)
	}
	if !strings.Contains(joined, "Online") {
		t.Errorf("an online section with no times should say Online:\n%s", joined)
	}
	for _, ln := range lines[2:] {
		if strings.Contains(ln, "M T W") {
			t.Errorf("a section with no times must not render day labels: %q", ln)
		}
	}
}

// TestMultipleSlotsUseContinuationLines: ECE 105 lectures really do meet at
// three different times, which one line cannot show honestly.
func TestMultipleSlotsUseContinuationLines(t *testing.T) {
	sections := []store.Section{tsec("LEC 001", "LEC", 4321, 50, 100,
		mt("YNNYNNN", "09:30", "10:20"),
		mt("NYNNNNN", "12:30", "13:20"),
		mt("NYNNNNN", "13:30", "14:20"),
	)}
	lines := tableLines(t, sections)[2:]
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (one per distinct slot):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[0], "LEC 001") || !strings.Contains(lines[0], "4321") {
		t.Errorf("first line should carry the section identity: %q", lines[0])
	}
	for i, ln := range lines[1:] {
		if strings.HasPrefix(ln, "LEC") || strings.Contains(ln, "4321") || strings.Contains(ln, "50/100") {
			t.Errorf("continuation line %d repeats the identity columns: %q", i+1, ln)
		}
		if !strings.Contains(ln, "PM") {
			t.Errorf("continuation line %d has no time: %q", i+1, ln)
		}
	}
	// Continuation lines keep the same column alignment.
	d0 := strings.Index(lines[0], "M T W")
	for i, ln := range lines[1:] {
		if d := strings.Index(ln, "M T W"); d != d0 {
			t.Errorf("continuation line %d Date at offset %d, first line at %d", i+1, d, d0)
		}
	}
}

func TestManySlotsAreCapped(t *testing.T) {
	var ms []source.Meeting
	for h := 8; h < 16; h++ {
		ms = append(ms, mt("YNNNNNN", fmt.Sprintf("%02d:00", h), fmt.Sprintf("%02d:50", h)))
	}
	lines := tableLines(t, []store.Section{tsec("LEC 001", "LEC", 1111, 1, 2, ms...)})[2:]
	if len(lines) != maxSlotsShown+1 {
		t.Fatalf("8 slots -> %d lines, want %d (cap + a '+N more' line)", len(lines), maxSlotsShown+1)
	}
	if !strings.Contains(lines[len(lines)-1], "+4 more") {
		t.Errorf("last line should say how many were hidden: %q", lines[len(lines)-1])
	}
}

// TestUntrustedTextCannotBreakTheBlock: section names come from an external API
// and are printed inside a code fence.
func TestUntrustedTextCannotBreakTheBlock(t *testing.T) {
	evil := tsec("LEC `001```\n\x1b[31m", "LEC", 1111, 1, 2, mt("YNNNNNN", "09:00", "09:50"))
	sections := []store.Section{evil}
	l := newTableLayout(sections)
	block, _, _ := renderTablePage(tableRows(sections, l), l, 0)

	if n := strings.Count(block, "```"); n != 2 {
		t.Errorf("block contains %d fences, want exactly 2 (open + close): %q", n, block)
	}
	// The only ESC bytes allowed are our own bold/reset pairs.
	if got := len(ansiRE.FindAllString(block, -1)); got != strings.Count(block, "\x1b") {
		t.Errorf("a stray ESC byte survived from untrusted input: %q", block)
	}
	if strings.Contains(block, "\x1b[31m") {
		t.Error("untrusted input injected its own colour")
	}
	// A newline inside a name must not split the row into two.
	// Expected: opening fence, header, rule, exactly ONE data row, closing fence.
	if lines := strings.Split(strings.TrimRight(stripANSI(block), "\n"), "\n"); len(lines) != 5 {
		t.Errorf("name with a newline produced %d lines, want 5 (one data row): %q", len(lines), lines)
	}
}

func TestSafeCell(t *testing.T) {
	for in, want := range map[string]string{
		"LEC 001": "LEC 001", "a`b": "a'b", "x\ny": "xy", "a\x1b[31mb": "a[31mb", "\x00\x07": "", "日本": "日本",
	} {
		if got := safeCell(in); got != want {
			t.Errorf("safeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchViewUsesTheTable ties the pieces together at the embed level.
func TestSearchViewUsesTheTable(t *testing.T) {
	v := searchView("cs341", "Fall 2026", realCS341(), 0)
	d := v.embed.Description
	if !strings.Contains(d, "```ansi\n") {
		t.Fatalf("search results are not in an ansi block:\n%s", d)
	}
	if n := strings.Count(d, "```"); n != 2 {
		t.Errorf("%d fences, want 2", n)
	}
	// Padding exists to align columns, so compare content with runs of spaces
	// collapsed: "6:00 PM -  7:50 PM" is the same time as "6:00 PM - 7:50 PM".
	flat := strings.Join(strings.Fields(stripANSI(d)), " ")
	for _, want := range []string{"Section Class Enrolled Time Date", "6003", "6549", "268/300", "6:00 PM - 7:50 PM"} {
		if !strings.Contains(flat, want) {
			t.Errorf("embed missing %q", want)
		}
	}
	if !strings.Contains(d, "Watch one:") {
		t.Error("the Watch one tip should still be present")
	}
}

// TestTimeAndDateKeepTwoSpacesWhereTheyFit: "2:20 PM M T W Th" with a single
// space reads as one run of text, so that is the last gap to shrink.
func TestTimeAndDateKeepTwoSpacesWhereTheyFit(t *testing.T) {
	l := newTableLayout(realCS341())
	if l.width() > maxTableWidth {
		t.Fatalf("typical course is %d wide > %d", l.width(), maxTableWidth)
	}
	if l.gaps[3] != 2 {
		t.Errorf("Time|Date gap = %d, want 2 for a typical course (gaps %v)", l.gaps[3], l.gaps)
	}
	// Shrinking is leftmost-first: if any gap is reduced, the leftmost goes first.
	for i := 1; i < 4; i++ {
		if l.gaps[i] < l.gaps[i-1] && l.gaps[i-1] != 1 {
			t.Errorf("gap %d shrank before gap %d: %v", i, i-1, l.gaps)
		}
	}
}

func TestGapsShrinkLeftToRightUnderPressure(t *testing.T) {
	wide := []store.Section{tsec("LEC 081", "LEC", 70001, 1477, 1920, mt("YNYNYNN", "10:30", "11:20"))}
	l := newTableLayout(wide)
	if l.width() > maxTableWidth {
		t.Fatalf("even after shrinking, table is %d wide > %d (gaps %v)", l.width(), maxTableWidth, l.gaps)
	}
	if l.gaps[0] != 1 {
		t.Errorf("the leftmost gap should shrink first: %v", l.gaps)
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
