package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// --- rendering ----------------------------------------------------------------------

// TestReserveLineFormat pins the requested shape: its own indented arrow line,
// reading "Reserve: <note>".
func TestReserveLineFormat(t *testing.T) {
	s := tsec("LEC 001", "LEC", 6112, 90, 120, mt("YNYNYNN", "08:30", "09:20"))
	s.Reserves = []string{"1A AFM students", "Year 1 students"}

	lines := strings.Split(searchRow(s), "\n")
	want := []string{
		"`LEC 001` ▰▰▰▰▰▰▱▱ `90/120` · `6112`",
		indent + "↳ 8:30 AM - 9:20 AM · Monday, Wednesday, Friday",
		indent + "↳ Reserve: 1A AFM students",
		indent + "↳ Reserve: Year 1 students",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n got  %q\n want %q", i, lines[i], want[i])
		}
	}
}

func TestReserveLinesComeAfterTheScheduleNotBefore(t *testing.T) {
	s := tsec("LEC 001", "LEC", 1111, 1, 2, mt("YNNNNNN", "09:00", "09:50"))
	s.Reserves = []string{"Year 1 Math Students"}
	lines := strings.Split(searchRow(s), "\n")
	var sched, res int = -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "9:00 AM") {
			sched = i
		}
		if strings.Contains(ln, "Reserve:") {
			res = i
		}
	}
	if sched < 0 || res < 0 || res <= sched {
		t.Errorf("schedule at %d, reserve at %d; the reserve must follow the schedule:\n%s", sched, res, strings.Join(lines, "\n"))
	}
}

func TestNoReservesMeansNoReserveLine(t *testing.T) {
	s := tsec("LEC 001", "LEC", 1111, 1, 2, mt("YNNNNNN", "09:00", "09:50"))
	if strings.Contains(searchRow(s), "Reserve") {
		t.Errorf("a section without reserves shows one:\n%s", searchRow(s))
	}
	if got := reserveLines(s); got != nil {
		t.Errorf("reserveLines = %q, want nil", got)
	}
}

func TestEachReserveGetsItsOwnIndentedArrowLine(t *testing.T) {
	s := tsec("LEC 001", "LEC", 3153, 1, 2)
	s.Reserves = []string{"REC Year 1 Students", "Medical Science students", "Year 1 Arts students", "Science Year 1 students"}
	got := reserveLines(s)
	if len(got) != 4 {
		t.Fatalf("got %d lines for 4 reserves: %q", len(got), got)
	}
	for _, ln := range got {
		if !strings.HasPrefix(ln, indent+"↳ Reserve: ") {
			t.Errorf("line is not an indented arrow reserve line: %q", ln)
		}
	}
}

func TestManyReservesAreCapped(t *testing.T) {
	s := tsec("LEC 001", "LEC", 1, 1, 2)
	for i := 0; i < 9; i++ {
		s.Reserves = append(s.Reserves, fmt.Sprintf("group %d", i))
	}
	got := reserveLines(s)
	if len(got) != 5 {
		t.Fatalf("9 reserves -> %d lines, want 4 plus a count: %q", len(got), got)
	}
	if got[4] != indent+"↳ +5 more reserves" {
		t.Errorf("overflow line = %q", got[4])
	}
	one := tsec("LEC 001", "LEC", 1, 1, 2)
	one.Reserves = make([]string, 5)
	for i := range one.Reserves {
		one.Reserves[i] = fmt.Sprintf("g%d", i)
	}
	if got := reserveLines(one); got[len(got)-1] != indent+"↳ +1 more reserve" {
		t.Errorf("singular overflow = %q", got[len(got)-1])
	}
}

// --- untrusted text ---------------------------------------------------------------------

// TestReserveTextCannotInjectMarkdownOrMentions: the notes come from a
// third-party web page and are printed into an embed.
func TestReserveTextCannotInjectMarkdownOrMentions(t *testing.T) {
	evil := []string{
		"[click here](https://evil.example/phish)",
		"<@123456789012345678> <@&987654321098765432> @everyone @here",
		"**bold** __under__ ~~strike~~ ||spoiler|| `code` *it*",
		"> quote\n# heading\n- list",
		"<https://evil.example> <#1234>",
	}
	for _, e := range evil {
		s := tsec("LEC 001", "LEC", 1, 1, 2)
		s.Reserves = []string{e}
		line := reserveLines(s)[0]

		// Every markdown/mention-significant character must be backslash-escaped.
		for i, r := range line {
			if strings.ContainsRune("*_~`|<>[]()#@", r) && !strings.HasPrefix(line[:i], indent) {
				if i == 0 || line[i-1] != '\\' {
					t.Errorf("%q: unescaped %q at byte %d of %q", e, r, i, line)
				}
			}
		}
		if strings.Contains(line, "\n") {
			t.Errorf("%q: a newline survived into a single arrow line: %q", e, line)
		}
	}
}

func TestEscapeMarkdown(t *testing.T) {
	for in, want := range map[string]string{
		"Year 1 Math Students": "Year 1 Math Students",
		"plain":                "plain",
		"a*b":                  `a\*b`,
		"[x](y)":               `\[x\]\(y\)`,
		"<@1>":                 `\<\@1\>`,
		"line1\nline2":         "line1 line2",
		"a\r\n\t b":            "a b",
		"x\x00y\x1bz":          "xyz",
		`back\slash`:           `back\\slash`,
		"日本語":                  "日本語",
		"":                     "",
	} {
		if got := escapeMarkdown(in); got != want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRealReserveNotesRenderUnchanged: escaping must not mangle the notes that
// actually exist. Slashes, apostrophes and digits all appear in real ones.
func TestRealReserveNotesRenderUnchanged(t *testing.T) {
	for _, note := range []string{
		"Year 1 Math Students", "1A AFM students", "St Jerome's students",
		"Arts BA Adult/PTS Students", "COMPE or ELE Yr 1 students", "REC Year 1 Students",
		"SDS students", "BASE students", "Science Year 1 students", "Medical Science students",
	} {
		if got := escapeMarkdown(note); got != note {
			t.Errorf("a real note was altered by escaping: %q -> %q", note, got)
		}
	}
}

// --- limits -----------------------------------------------------------------------------

// TestReserveLinesStayWithinDiscordLimits: reserves add up to six lines per
// section, so re-check the worst case still pages instead of overflowing.
func TestReserveLinesStayWithinDiscordLimits(t *testing.T) {
	var sections []store.Section
	for i := 0; i < 80; i++ {
		var ms []source.Meeting
		for h := 0; h < 6; h++ {
			ms = append(ms, mt("YYYYYYY", fmt.Sprintf("%02d:00", 8+h), fmt.Sprintf("%02d:50", 8+h)))
		}
		s := tsec(fmt.Sprintf("LEC %03d", i+1), "LEC", 1000+i, 1477, 1920, ms...)
		for r := 0; r < 9; r++ {
			s.Reserves = append(s.Reserves, strings.Repeat("Interdisciplinary ", 9)+fmt.Sprint(r))
		}
		sections = append(sections, s)
	}
	pages := len(paginateRows(sectionRows(sections), pageSize))
	seen := map[string]int{}
	for p := 0; p < pages; p++ {
		d := searchView("cs341", "Fall 2026", sections, p).embed.Description
		if n := utf8.RuneCountInString(d); n >= maxEmbedDesc {
			t.Fatalf("page %d is %d runes, at or over the limit %d: content was cut", p, n, maxEmbedDesc)
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
		t.Errorf("%d of %d sections visible across %d pages", len(seen), len(sections), pages)
	}
}

// --- the unavailable notice ------------------------------------------------------------------

func TestUnavailableNoticeAppearsOnlyWhenAsked(t *testing.T) {
	secs := realCS341()
	with := searchViewWith("cs341", "Fall 2026", secs, 0, viewOptions{NotesUnavailable: true}).embed.Description
	without := searchView("cs341", "Fall 2026", secs, 0).embed.Description
	if !strings.Contains(with, "Reserve notes are unavailable right now") {
		t.Errorf("the notice is missing when notes failed:\n%s", with)
	}
	if strings.Contains(without, "unavailable") {
		t.Errorf("the notice appears when notes did not fail:\n%s", without)
	}
}

// --- bot-level behaviour: /search must never break because of the notes ---------------------------

type fakeNotes struct {
	notes map[int][]string
	err   error
	delay time.Duration
	calls int
}

func (f *fakeNotes) Reserves(ctx context.Context, _ source.TermCode, subj, cat string) (map[int][]string, error) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.notes, f.err
}

func testBot(n NoteProvider) *Bot {
	return &Bot{opts: Options{Notes: n, Term: "1269"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestAttachReservesMatchesByClassNumber(t *testing.T) {
	b := testBot(&fakeNotes{notes: map[int][]string{6003: {"Year 1 students"}, 6549: {"A", "B"}}})
	in := realCS341()
	out, unavailable := b.attachReserves(context.Background(), "cs341", in, time.Second)

	if unavailable {
		t.Fatal("reported unavailable on success")
	}
	for _, s := range out {
		switch s.ClassNumber {
		case 6003:
			if len(s.Reserves) != 1 || s.Reserves[0] != "Year 1 students" {
				t.Errorf("6003: %q", s.Reserves)
			}
		case 6549:
			if len(s.Reserves) != 2 {
				t.Errorf("6549: %q", s.Reserves)
			}
		default:
			if len(s.Reserves) != 0 {
				t.Errorf("class %d got notes it should not have: %q", s.ClassNumber, s.Reserves)
			}
		}
	}
	for _, s := range in {
		if len(s.Reserves) != 0 {
			t.Fatal("attachReserves mutated the caller's slice")
		}
	}
}

func TestNoProviderMeansNoNoticeAndNoChange(t *testing.T) {
	b := testBot(nil)
	in := realCS341()
	out, unavailable := b.attachReserves(context.Background(), "cs341", in, time.Second)
	if unavailable {
		t.Error("with notes switched off there is nothing to apologise for")
	}
	if len(out) != len(in) {
		t.Errorf("sections changed: %d -> %d", len(in), len(out))
	}
}

// TestProviderFailureNeverBreaksSearch: errors, timeouts and a cancelled
// context all leave the sections intact and report notes as unavailable.
func TestProviderFailureNeverBreaksSearch(t *testing.T) {
	cases := map[string]*fakeNotes{
		"error":    {err: errors.New("site down")},
		"busy":     {err: errors.New("schedpage: too many lookups in flight")},
		"drifted":  {err: errors.New("schedpage: page layout not recognised")},
		"too slow": {delay: 2 * time.Second, notes: map[int][]string{6003: {"x"}}},
	}
	for name, f := range cases {
		b := testBot(f)
		in := realCS341()
		start := time.Now()
		out, unavailable := b.attachReserves(context.Background(), "cs341", in, 100*time.Millisecond)
		if time.Since(start) > time.Second {
			t.Errorf("%s: took %s despite a 100ms budget", name, time.Since(start))
		}
		if !unavailable {
			t.Errorf("%s: should report notes as unavailable", name)
		}
		if len(out) != len(in) {
			t.Errorf("%s: sections lost: %d -> %d", name, len(in), len(out))
		}
		for _, s := range out {
			if len(s.Reserves) != 0 {
				t.Errorf("%s: partial notes leaked through: %q", name, s.Reserves)
			}
		}
	}
}

// TestPagerWaitsLessThanSearch: a pager click has no deferral, and Discord needs
// an answer within 3 seconds.
func TestPagerWaitsLessThanSearchAndBothFitDiscordsDeadline(t *testing.T) {
	if notesWaitPager >= notesWaitSearch {
		t.Errorf("pager wait %s should be shorter than search wait %s", notesWaitPager, notesWaitSearch)
	}
	if notesWaitPager > time.Second {
		t.Errorf("pager wait %s is too long for a response that must land inside 3s", notesWaitPager)
	}
	if notesWaitSearch > 3*time.Second {
		t.Errorf("search wait %s is too long", notesWaitSearch)
	}
}

func TestRenderedSearchIncludesNotesEndToEnd(t *testing.T) {
	b := testBot(&fakeNotes{notes: map[int][]string{6003: {"1A AFM students", "Year 1 students"}}})
	secs, unavailable := b.attachReserves(context.Background(), "cs341", realCS341(), time.Second)
	d := searchViewWith("cs341", "Fall 2026", secs, 0, viewOptions{NotesUnavailable: unavailable}).embed.Description

	lines := strings.Split(d, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "`LEC 001`") {
			if i+3 >= len(lines) || lines[i+2] != indent+"↳ Reserve: 1A AFM students" || lines[i+3] != indent+"↳ Reserve: Year 1 students" {
				t.Errorf("reserves are not directly under LEC 001's schedule:\n%s", strings.Join(lines[i:min(i+5, len(lines))], "\n"))
			}
			return
		}
	}
	t.Error("LEC 001 not found")
}

// TestHostileNoteStaysOnOneLine asserts the structural guarantee directly: one
// reserve note is exactly one line, whatever it contains. The escaping test
// above could not establish this on its own, because escaping also defangs
// "# heading" and "- list", so it stayed green with line-break flattening
// switched off. Checking the line count is independent of the escaping layer.
func TestHostileNoteStaysOnOneLine(t *testing.T) {
	for _, evil := range []string{
		"a\nb", "a\r\nb", "a\rb", "a\u2028b", "a\u2029b", "a\vb\fb",
		"first\n# Heading\n> quote\n- item\n1. numbered\n```\ncode\n```",
		"x\n\n\n\ny", "\nleading", "trailing\n",
	} {
		s := tsec("LEC 001", "LEC", 1, 1, 2)
		s.Reserves = []string{evil}
		got := reserveLines(s)
		if len(got) != 1 {
			t.Errorf("%q produced %d lines: %q", evil, len(got), got)
			continue
		}
		if strings.ContainsAny(got[0], "\n\r\u2028\u2029") {
			t.Errorf("%q: line break survived in %q", evil, got[0])
		}
	}
}
