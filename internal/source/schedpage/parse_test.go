package schedpage

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func parseFixture(t *testing.T, name string) *Page {
	t.Helper()
	f, err := os.Open("testdata/" + name + ".html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return p
}

func parseString(t *testing.T, s string) *Page {
	t.Helper()
	p, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

// --- real pages, checked against counts taken independently ---------------------

// TestRealPagesMatchTheirOwnReserveCounts: for every captured page, the number
// of reserve rows the parser extracts equals the number of times the literal
// "Reserve:" appears in the raw file. The raw count is made with strings.Count,
// not with the parser, so this cannot pass by the parser agreeing with itself.
func TestRealPagesMatchTheirOwnReserveCounts(t *testing.T) {
	for _, name := range []string{"AFM101", "PSYCH101", "CS135", "CS341", "ECE105", "MATH135", "grad_CS686", "empty_ZZZ999"} {
		raw, err := os.ReadFile("testdata/" + name + ".html")
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Count(string(raw), "Reserve:")
		p := parseString(t, string(raw))
		if p.Parsed != want {
			t.Errorf("%s: parsed %d reserve rows, page contains %d", name, p.Parsed, want)
		}
		if p.Unattached != 0 {
			t.Errorf("%s: %d reserve rows were not attached to any section", name, p.Unattached)
		}
		if p.Drift() {
			t.Errorf("%s: reports layout drift on a known-good page", name)
		}
	}
}

func TestSectionCountsOnRealPages(t *testing.T) {
	for name, want := range map[string]int{
		"AFM101": 6, "PSYCH101": 6, "CS135": 25, "CS341": 7, "ECE105": 13, "MATH135": 25, "grad_CS686": 3, "empty_ZZZ999": 0,
	} {
		if got := parseFixture(t, name).Sections; got != want {
			t.Errorf("%s: %d sections, want %d", name, got, want)
		}
	}
}

// TestUserExamples pins the two notes quoted in the request.
func TestUserExamples(t *testing.T) {
	cs135 := parseFixture(t, "CS135")
	if got := cs135.Reserves[6112]; !reflect.DeepEqual(got, []string{"1A AFM students", "Year 1 students"}) {
		t.Errorf("CS 135 class 6112 = %q", got)
	}
	afm := parseFixture(t, "AFM101")
	if got := afm.Reserves[3234]; !reflect.DeepEqual(got, []string{"Year 1 Math Students"}) {
		t.Errorf("AFM 101 class 3234 = %q", got)
	}
}

// TestMultipleReservesKeepPageOrder: PSYCH 101 sections carry four.
func TestMultipleReservesKeepPageOrder(t *testing.T) {
	p := parseFixture(t, "PSYCH101")
	want := []string{"REC Year 1 Students", "Medical Science students", "Year 1 Arts students", "Science Year 1 students"}
	if got := p.Reserves[3153]; !reflect.DeepEqual(got, want) {
		t.Errorf("class 3153 = %q, want %q", got, want)
	}
	if got := p.Reserves[3737]; !reflect.DeepEqual(got, []string{"SDS students", "Arts BA Adult/PTS Students"}) {
		t.Errorf("class 3737 = %q", got)
	}
}

// TestReservesAttachToTheSectionAbove: a note belongs to the section row it
// sits under, so two sections with identical notes must not bleed into a third.
func TestReservesAttachToTheSectionAbove(t *testing.T) {
	p := parseFixture(t, "ECE105")
	// LEC 001 (4730) and LEC 003 (5052) have one note; LEC 002 (4731) has two.
	if len(p.Reserves[4730]) != 1 || len(p.Reserves[4731]) != 2 || len(p.Reserves[5052]) != 1 {
		t.Errorf("counts: 4730=%d 4731=%d 5052=%d, want 1/2/1", len(p.Reserves[4730]), len(p.Reserves[4731]), len(p.Reserves[5052]))
	}
	for class, notes := range p.Reserves {
		if class < 4000 || class > 5400 {
			t.Errorf("implausible class number %d attached to %q", class, notes)
		}
	}
}

func TestGradPageParses(t *testing.T) {
	p := parseFixture(t, "grad_CS686")
	if p.Sections == 0 || p.Parsed != 4 {
		t.Errorf("grad page: sections=%d parsed=%d", p.Sections, p.Parsed)
	}
}

func TestNoResultsPageIsEmptyNotAnError(t *testing.T) {
	p := parseFixture(t, "empty_ZZZ999")
	if p.Sections != 0 || len(p.Reserves) != 0 || p.Drift() {
		t.Errorf("%+v", p)
	}
}

// --- layout drift ------------------------------------------------------------------

// TestDriftDetectedWhenMarkupChanges: this is the safeguard against silently
// losing every note. It fired for real during development, when a parser bug
// produced zero sections on every page while "Reserve:" still appeared in the
// text.
func TestDriftDetectedWhenMarkupChanges(t *testing.T) {
	// Same words, different structure: no table rows at all.
	p := parseString(t, `<div>3234 LEC 081</div><p>Reserve: Year 1 Math Students</p>`)
	if p.Parsed != 0 {
		t.Fatalf("parsed %d from markup with no rows", p.Parsed)
	}
	if p.Mentions != 1 || !p.Drift() {
		t.Errorf("mentions=%d drift=%v, want the change reported", p.Mentions, p.Drift())
	}
}

func TestNoReservesAnywhereIsNotDrift(t *testing.T) {
	p := parseFixture(t, "empty_ZZZ999")
	if p.Drift() {
		t.Error("a page that never mentions reserves must not be flagged")
	}
}

// --- tolerance and hostile input ------------------------------------------------------

func TestMalformedHTMLNeverPanics(t *testing.T) {
	cases := []string{
		"", " ", "<", "<<<<>>>>", "<table>", "<tr><td>", "</td></tr></table>",
		"<table><tr><td>3234</td><td>LEC 001", "<TR><TD>Reserve:", "\x00\x01\x02", "<tr>" + strings.Repeat("<td>x", 5000),
		strings.Repeat("<table>", 3000), "<TD>3234 </TD><TD>LEC 001 </TD>", "Reserve: Reserve: Reserve:",
		"<tr><td colspan=999999999>Reserve: x</td></tr>", "\xff\xfe\xfd",
	}
	for i, c := range cases {
		p, err := Parse(strings.NewReader(c))
		if err != nil || p == nil {
			t.Errorf("case %d: (%v, %v)", i, p, err)
		}
	}
}

func TestReserveBeforeAnySectionIsCountedNotAttached(t *testing.T) {
	p := parseString(t, `<table><tr><td>Reserve: Orphan</td></tr></table>`)
	if p.Unattached != 1 || len(p.Reserves) != 0 {
		t.Errorf("unattached=%d reserves=%v", p.Unattached, p.Reserves)
	}
}

func TestScriptAndStyleTextIsIgnored(t *testing.T) {
	p := parseString(t, `<script>var x="Reserve: from script";</script><style>.a{content:"Reserve: css"}</style>`+
		`<table><tr><td>3234 </td><td>LEC 081 </td></tr><tr><td><i>Reserve: Real</i></td></tr></table>`)
	if got := p.Reserves[3234]; !reflect.DeepEqual(got, []string{"Real"}) {
		t.Errorf("got %q, want only the table's note", got)
	}
	if p.Mentions != 1 {
		t.Errorf("mentions=%d: text inside script/style must not count", p.Mentions)
	}
}

func TestRepeatedIdenticalNoteIsKeptOnce(t *testing.T) {
	p := parseString(t, `<table><tr><td>3234 </td><td>LEC 081 </td></tr>`+
		`<tr><td>Reserve: Year 1</td></tr><tr><td>Reserve: Year 1</td></tr></table>`)
	if got := p.Reserves[3234]; len(got) != 1 {
		t.Errorf("duplicate kept: %q", got)
	}
}

func TestLabelMatchingIsCaseAndSpaceTolerant(t *testing.T) {
	p := parseString(t, `<table><tr><td>3234 </td><td>LEC 081 </td></tr>`+
		`<tr><td>RESERVE :   Spaced   Out  Note</td></tr><tr><td>reserve:Lower</td></tr></table>`)
	if got := p.Reserves[3234]; !reflect.DeepEqual(got, []string{"Spaced Out Note", "Lower"}) {
		t.Errorf("got %q", got)
	}
}

func TestOnlyRowsThatStartWithTheLabelCount(t *testing.T) {
	p := parseString(t, `<table><tr><td>3234 </td><td>LEC 081 </td></tr>`+
		`<tr><td>Notes: Reserve: this is a course note, not a reserve row</td></tr></table>`)
	if p.Parsed != 0 {
		t.Errorf("a row that merely contains the word was parsed: %v", p.Reserves)
	}
}

// The caps below are asserted against LITERAL numbers, not against the package
// constants. A test that compares the code to its own constant passes however
// the constant is changed; an earlier version of this test did exactly that and
// a mutation run showed it could not fail.

func TestAHugeNoteIsCutToAReasonableLength(t *testing.T) {
	p := parseString(t, `<table><tr><td>3234 </td><td>LEC 081 </td></tr>`+
		`<tr><td>Reserve: `+strings.Repeat("A", 100000)+`</td></tr></table>`)
	notes := p.Reserves[3234]
	if len(notes) != 1 {
		t.Fatalf("got %d notes, want 1", len(notes))
	}
	n := utf8.RuneCountInString(notes[0])
	if n > 200 {
		t.Errorf("a %d-rune note survived; real ones are a few dozen characters", n)
	}
	if n < 100 {
		t.Errorf("a %d-rune note: the cap is cutting far too aggressively", n)
	}
	if !strings.HasSuffix(notes[0], "…") {
		t.Errorf("a truncated note should end in an ellipsis: %q", notes[0][len(notes[0])-5:])
	}
}

func TestARealisticNoteIsNeverTruncated(t *testing.T) {
	// The longest real note seen across the captured pages.
	for _, name := range []string{"PSYCH101", "CS135", "ECE105", "AFM101", "MATH135"} {
		for _, notes := range parseFixture(t, name).Reserves {
			for _, n := range notes {
				if strings.HasSuffix(n, "…") {
					t.Errorf("%s: a real note was truncated: %q", name, n)
				}
			}
		}
	}
}

func TestTooManyNotesOnOneSectionAreCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<table><tr><td>3234 </td><td>LEC 081 </td></tr>`)
	for i := 0; i < 200; i++ {
		sb.WriteString(fmt.Sprintf(`<tr><td>Reserve: distinct note %d</td></tr>`, i))
	}
	sb.WriteString(`</table>`)
	p := parseString(t, sb.String())
	if n := len(p.Reserves[3234]); n > 20 {
		t.Errorf("%d notes kept on one section; real data tops out at 4", n)
	}
	if n := len(p.Reserves[3234]); n < 4 {
		t.Errorf("only %d notes kept; PSYCH 101 sections really have 4", n)
	}
}

func TestControlCharactersAndBadUTF8AreScrubbed(t *testing.T) {
	p := parseString(t, "<table><tr><td>3234 </td><td>LEC 081 </td></tr>"+
		"<tr><td>Reserve: Year\x00 1\x1b[31m\x07 \xff\xfestudents</td></tr></table>")
	for _, n := range p.Reserves[3234] {
		if !utf8.ValidString(n) {
			t.Errorf("invalid UTF-8 survived: %q", n)
		}
		for _, r := range n {
			if r < 0x20 || r == 0x7f {
				t.Errorf("control character %U survived in %q", r, n)
			}
		}
	}
}

func TestNonBreakingSpacesBecomeSpaces(t *testing.T) {
	p := parseString(t, "<table><tr><td>3234 </td><td>LEC 081 </td></tr><tr><td><i>Reserve:\u00a0Year\u00a01&nbsp;Math</i></td></tr></table>")
	if got := p.Reserves[3234]; !reflect.DeepEqual(got, []string{"Year 1 Math"}) {
		t.Errorf("got %q", got)
	}
}

func TestEmptyReserveLabelIsIgnored(t *testing.T) {
	p := parseString(t, `<table><tr><td>3234 </td><td>LEC 081 </td></tr><tr><td>Reserve:   </td></tr></table>`)
	if p.Parsed != 0 || len(p.Reserves) != 0 {
		t.Errorf("an empty note was recorded: %+v", p)
	}
}

func TestClassNumberPatternIsStrict(t *testing.T) {
	// 2-digit, 6-digit, and non-numeric first cells are not section rows.
	for _, first := range []string{"12", "123456", "12a4", "-123", "1 23"} {
		p := parseString(t, `<table><tr><td>`+first+`</td><td>LEC 001</td></tr><tr><td>Reserve: x</td></tr></table>`)
		if p.Sections != 0 {
			t.Errorf("%q was accepted as a class number", first)
		}
	}
}

func TestTruncateNeverSplitsARune(t *testing.T) {
	for n := 1; n < 8; n++ {
		if got := truncate("日本語のコース名です", n); !utf8.ValidString(got) {
			t.Fatalf("n=%d: invalid UTF-8 %q", n, got)
		}
	}
}
