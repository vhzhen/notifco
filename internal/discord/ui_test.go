package discord

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/vhzhen/notifco/internal/store"
)

func sec(component, name string, enrolled, capacity int) store.Section {
	return store.Section{
		TermCode: "1269", CourseCode: "math135", CourseTitle: "Algebra for Honours Mathematics",
		SectionName: name, Component: component,
		Enrolled: enrolled, Capacity: capacity, ObservedAt: time.Unix(1_790_000_000, 0),
	}
}

// manySections builds n sections of one course across LEC, TUT and LAB.
func manySections(n int) []store.Section {
	comps := []string{"LEC", "TUT", "LAB"}
	out := make([]store.Section, 0, n)
	for i := 0; i < n; i++ {
		c := comps[i%3]
		out = append(out, sec(c, fmt.Sprintf("%s %03d", c, i+1), 90+i%11, 100))
	}
	return out
}

// --- headers ---------------------------------------------------------------

// TestCourseHeaderExactFormat pins the format requested for the UI.
func TestCourseHeaderExactFormat(t *testing.T) {
	got := courseHeader("math135", "Algebra for Honours Mathematics", "Fall 2026")
	want := "MATH 135 - Algebra for Honours Mathematics [Fall 2026]"
	if got != want {
		t.Fatalf("header = %q\n want    %q", got, want)
	}
}

func TestCourseHeaderVariants(t *testing.T) {
	tests := []struct{ name, code, title, term, want string }{
		{"no title", "cs341", "", "Fall 2026", "CS 341 [Fall 2026]"},
		{"no term", "cs341", "Algorithms", "", "CS 341 - Algorithms"},
		{"neither", "cs341", "", "", "CS 341"},
		{"letter suffix", "math237l", "Calc Lab", "Winter 2027", "MATH 237L - Calc Lab [Winter 2027]"},
		{"whitespace title", "cs341", "   ", "Fall 2026", "CS 341 [Fall 2026]"},
		{"unparseable code", "zzz", "T", "Fall 2026", "ZZZ - T [Fall 2026]"},
	}
	for _, tc := range tests {
		if got := courseHeader(tc.code, tc.title, tc.term); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCourseHeaderKeepsTermWhenTruncating: a long title is trimmed, but the
// "[Fall 2026]" suffix is the part a student cannot afford to lose.
func TestCourseHeaderKeepsTermWhenTruncating(t *testing.T) {
	long := strings.Repeat("Advanced Topics in ", 40)
	got := courseHeader("cs341", long, "Fall 2026")
	if n := utf8.RuneCountInString(got); n > maxEmbedTitle {
		t.Fatalf("header is %d runes, exceeds Discord's %d limit", n, maxEmbedTitle)
	}
	if !strings.HasSuffix(got, " [Fall 2026]") {
		t.Errorf("term suffix lost when truncating: %q", got[len(got)-30:])
	}
	if !strings.HasPrefix(got, "CS 341 - ") {
		t.Errorf("course label lost: %q", got[:20])
	}
	if !strings.Contains(got, "…") {
		t.Error("truncated title should end in an ellipsis")
	}
}

func TestCourseHeaderPrefixedStaysWithinLimit(t *testing.T) {
	got := courseHeaderPrefixed("✅ ", "cs341", strings.Repeat("é", 400), "Fall 2026")
	if n := utf8.RuneCountInString(got); n > maxEmbedTitle {
		t.Fatalf("prefixed header is %d runes, limit %d", n, maxEmbedTitle)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a multi-byte character")
	}
}

func TestTruncateRunesNeverSplitsCharacters(t *testing.T) {
	for n := 0; n < 12; n++ {
		got := truncateRunes("日本語のコース名です", n)
		if !utf8.ValidString(got) {
			t.Fatalf("n=%d produced invalid UTF-8: %q", n, got)
		}
		if utf8.RuneCountInString(got) > n && n > 0 {
			t.Fatalf("n=%d produced %d runes", n, utf8.RuneCountInString(got))
		}
	}
}

// --- ordering and rows -------------------------------------------------------

func TestSortSectionsLecturesFirst(t *testing.T) {
	in := []store.Section{
		sec("TST", "TST 201", 1, 2), sec("LAB", "LAB 101", 1, 2),
		sec("TUT", "TUT 101", 1, 2), sec("LEC", "LEC 002", 1, 2), sec("LEC", "LEC 001", 1, 2),
	}
	got := sortSections(in)
	want := []string{"LEC 001", "LEC 002", "TUT 101", "LAB 101", "TST 201"}
	for i, w := range want {
		if got[i].SectionName != w {
			t.Errorf("position %d = %s, want %s", i, got[i].SectionName, w)
		}
	}
	if in[0].SectionName != "TST 201" {
		t.Error("sortSections mutated its input")
	}
}

func TestFillBarOnlyFullWhenActuallyFull(t *testing.T) {
	full := strings.Repeat("▰", barWidth)
	if got := fillBar(100, 100); got != full {
		t.Errorf("100/100 = %q, want a full bar", got)
	}
	if got := fillBar(105, 100); got != full {
		t.Errorf("over-enrolled = %q, want a full bar", got)
	}
	// The whole point of the bar: 99/100 must not look the same as 100/100.
	if got := fillBar(99, 100); got == full {
		t.Error("99/100 rendered as a completely full bar")
	}
	if got := fillBar(0, 100); got != strings.Repeat("▱", barWidth) {
		t.Errorf("0/100 = %q, want empty", got)
	}
	if got := fillBar(5, 0); utf8.RuneCountInString(got) != barWidth {
		t.Errorf("zero capacity gave %d cells", utf8.RuneCountInString(got))
	}
	for e := 0; e <= 120; e++ {
		if n := utf8.RuneCountInString(fillBar(e, 100)); n != barWidth {
			t.Fatalf("fillBar(%d,100) has %d cells, want %d", e, n, barWidth)
		}
	}
}

func TestSectionRowStates(t *testing.T) {
	open := sectionRow(sec("LEC", "LEC 002", 99, 100))
	for _, want := range []string{"`LEC 002`", "`99/100`", "1 seat open"} {
		if !strings.Contains(open, want) {
			t.Errorf("open row missing %q: %s", want, open)
		}
	}
	if strings.Contains(open, "1 seats") {
		t.Errorf("bad pluralisation: %s", open)
	}
	full := sectionRow(sec("LEC", "LEC 001", 100, 100))
	if !strings.Contains(full, "Full") {
		t.Errorf("full row wrong: %s", full)
	}
	// Status is carried by text and the fill bar, not coloured dots.
	for _, row := range []string{open, full} {
		for _, banned := range []string{"🟢", "🔴", "✅"} {
			if strings.Contains(row, banned) {
				t.Errorf("row contains %s, which was removed: %s", banned, row)
			}
		}
	}
	many := sectionRow(sec("LEC", "LEC 003", 80, 100))
	if !strings.Contains(many, "20 seats open") {
		t.Errorf("plural row wrong: %s", many)
	}
	online := sec("LEC", "LEC 081", 10, 100)
	online.IsOnline = true
	if !strings.Contains(sectionRow(online), "online") {
		t.Error("online marker missing")
	}
}

// --- pagination --------------------------------------------------------------

func TestClampPage(t *testing.T) {
	// Page numbers come from button IDs, so they may be stale or edited.
	for _, tc := range []struct{ page, pages, want int }{
		{-5, 3, 0}, {0, 3, 0}, {2, 3, 2}, {3, 3, 2}, {999, 3, 2}, {0, 1, 0},
	} {
		if got := clampPage(tc.page, tc.pages); got != tc.want {
			t.Errorf("clampPage(%d,%d) = %d, want %d", tc.page, tc.pages, got, tc.want)
		}
	}
}

func TestPageSizeIsTwenty(t *testing.T) {
	if pageSize != 20 {
		t.Fatalf("pageSize = %d, want 20", pageSize)
	}
	count := func(n int) int {
		rows := make([]pageRow, n)
		for i := range rows {
			rows[i] = pageRow{group: "Lectures", text: "row"}
		}
		return len(paginateRows(rows, pageSize))
	}
	for _, tc := range []struct{ n, want int }{
		{0, 1}, {1, 1}, {20, 1}, {21, 2}, {40, 2}, {41, 3}, {100, 5},
	} {
		if got := count(tc.n); got != tc.want {
			t.Errorf("%d rows -> %d pages, want %d", tc.n, got, tc.want)
		}
	}
}

// TestPaginationCoversEverySectionExactlyOnce: walking every page must show
// each section once, with none dropped or repeated.
func TestPaginationCoversEverySectionExactlyOnce(t *testing.T) {
	const n = 45
	sections := manySections(n)

	seen := map[string]int{}
	pages := 0
	for p := 0; ; p++ {
		v := searchView("math135", "Fall 2026", sections, p)
		if v.embed == nil {
			t.Fatal("nil embed")
		}
		total := len(paginateRows(sectionRows(sections), pageSize))
		pages = total
		// A section's own line starts with its backticked name. Bullet lines
		// start with the bullet, so a schedule line cannot be double counted.
		for _, line := range strings.Split(v.embed.Description, "\n") {
			for _, s := range sections {
				if strings.HasPrefix(line, "`"+s.SectionName+"`") {
					seen[s.SectionName]++
				}
			}
		}
		if p >= total-1 {
			break
		}
	}
	if pages != 3 {
		t.Fatalf("%d sections at %d per page should be 3 pages, got %d", n, pageSize, pages)
	}
	if len(seen) != n {
		t.Errorf("saw %d distinct sections across pages, want %d", len(seen), n)
	}
	for name, c := range seen {
		if c != 1 {
			t.Errorf("%s appeared on %d pages, want exactly 1", name, c)
		}
	}
}

func TestPagerButtonsAtEachPosition(t *testing.T) {
	buttons := func(page, pages int) (prev, ind, next discordgo.Button) {
		comps := pagerComponents("s", "math135", page, pages)
		row := comps[0].(discordgo.ActionsRow)
		return row.Components[0].(discordgo.Button), row.Components[1].(discordgo.Button), row.Components[2].(discordgo.Button)
	}

	prev, ind, next := buttons(0, 3)
	if !prev.Disabled || next.Disabled {
		t.Errorf("first page: prev.Disabled=%v next.Disabled=%v, want true/false", prev.Disabled, next.Disabled)
	}
	if ind.Label != "Page 1 / 3" || !ind.Disabled {
		t.Errorf("indicator = %q disabled=%v", ind.Label, ind.Disabled)
	}
	if next.CustomID != "pg:s:math135:1" {
		t.Errorf("next on page 0 targets %q, want page 1", next.CustomID)
	}

	prev, _, next = buttons(1, 3)
	if prev.Disabled || next.Disabled {
		t.Error("middle page should have both buttons enabled")
	}
	if prev.CustomID != "pg:s:math135:0" || next.CustomID != "pg:s:math135:2" {
		t.Errorf("middle targets prev=%q next=%q", prev.CustomID, next.CustomID)
	}

	prev, ind, next = buttons(2, 3)
	if prev.Disabled || !next.Disabled {
		t.Errorf("last page: prev.Disabled=%v next.Disabled=%v, want false/true", prev.Disabled, next.Disabled)
	}
	if ind.Label != "Page 3 / 3" {
		t.Errorf("indicator = %q", ind.Label)
	}
}

// TestSinglePageHasNoButtons: short courses should not show a pager at all.
func TestSinglePageHasNoButtons(t *testing.T) {
	v := searchView("cs341", "Fall 2026", manySections(5), 0)
	if v.components != nil {
		t.Errorf("a 5-section course got %d component rows, want none", len(v.components))
	}
	if v.embed.Footer != nil {
		t.Errorf("single page should have no footer, got %q", v.embed.Footer.Text)
	}
}

func TestPagerIDRoundTrip(t *testing.T) {
	tests := []struct {
		kind, key string
		page      int
	}{
		{"s", "math135", 0}, {"s", "cs341", 7}, {"w", "", 3}, {"s", "math237l", 12},
	}
	for _, tc := range tests {
		id := pagerID(tc.kind, tc.key, tc.page)
		kind, key, page, ok := parsePagerID(id)
		if !ok || kind != tc.kind || key != tc.key || page != tc.page {
			t.Errorf("round trip of %q -> (%q,%q,%d,%v), want (%q,%q,%d)",
				id, kind, key, page, ok, tc.kind, tc.key, tc.page)
		}
	}
}

func TestParsePagerIDRejectsGarbage(t *testing.T) {
	for _, id := range []string{
		"", "pg", "pg:s", "pg:noop", "xx:s:math135:1", "pg:s:math135:abc",
		"pg:s:math135:-1", "pg:s:math135:", "random-button",
	} {
		if _, _, _, ok := parsePagerID(id); ok {
			t.Errorf("parsePagerID(%q) accepted garbage", id)
		}
	}
}

// TestHeadingRepeatedAtTopOfPage: a page that starts mid-group must still say
// what group it belongs to.
func TestHeadingRepeatedAtTopOfPage(t *testing.T) {
	rows := make([]pageRow, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, pageRow{group: "Lectures", text: fmt.Sprintf("row %d", i)})
	}
	for p := 0; p < 2; p++ {
		desc, _, _ := renderPage(rows, p, 8)
		if !strings.HasPrefix(desc, "**Lectures**") {
			t.Errorf("page %d does not start with its heading: %q", p, desc[:30])
		}
	}
}

func TestGroupHeadingsAppearOncePerGroupOnAPage(t *testing.T) {
	rows := []pageRow{
		{group: "Lectures", text: "a"}, {group: "Lectures", text: "b"}, {group: "Tutorials", text: "c"}, {group: "Labs", text: "d"}, {group: "Labs", text: "e"},
	}
	desc, _, _ := renderPage(rows, 0, 8)
	for _, h := range []string{"**Lectures**", "**Tutorials**", "**Labs**"} {
		if c := strings.Count(desc, h); c != 1 {
			t.Errorf("%s appears %d times, want 1\n%s", h, c, desc)
		}
	}
}

// --- Discord limits -----------------------------------------------------------

// TestEmbedsRespectDiscordLimits: an over-limit payload makes Discord reject
// the entire message, so assert the documented limits on worst-case input.
func TestEmbedsRespectDiscordLimits(t *testing.T) {
	long := strings.Repeat("Interdisciplinary Perspectives on ", 30)
	var sections []store.Section
	for i := 0; i < 60; i++ {
		s := sec("LEC", fmt.Sprintf("LEC %03d", i), 99, 100)
		s.CourseTitle = long
		s.IsOnline = true
		sections = append(sections, s)
	}

	var views []view
	for p := 0; p < 6; p++ {
		views = append(views, searchView("math135", "Fall 2026", sections, p))
	}
	var items []store.WatchItem
	for _, s := range sections {
		items = append(items, store.WatchItem{Section: s})
	}
	for p := 0; p < 6; p++ {
		views = append(views, watchlistView(items, map[string]string{"1269": "Fall 2026"}, p))
	}
	views = append(views, watchConfirmView("math135", long, "Fall 2026", sections, 60, 0))
	views = append(views, matchesView(strings.Repeat("q", 500), "Fall 2026", []store.CourseHit{
		{CourseCode: "cs341", CourseTitle: long, Sections: 3, OpenSections: 1},
	}))

	for i, v := range views {
		e := v.embed
		if n := utf8.RuneCountInString(e.Title); n > maxEmbedTitle {
			t.Errorf("view %d: title %d runes > %d", i, n, maxEmbedTitle)
		}
		if n := utf8.RuneCountInString(e.Description); n > maxEmbedDesc {
			t.Errorf("view %d: description %d runes > %d", i, n, maxEmbedDesc)
		}
		total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
		if e.Footer != nil {
			total += utf8.RuneCountInString(e.Footer.Text)
		}
		if total > 6000 {
			t.Errorf("view %d: embed totals %d chars > Discord's 6000", i, total)
		}
		for _, row := range v.components {
			ar := row.(discordgo.ActionsRow)
			if len(ar.Components) > 5 {
				t.Errorf("view %d: %d buttons in a row, max 5", i, len(ar.Components))
			}
			for _, c := range ar.Components {
				if id := c.(discordgo.Button).CustomID; len(id) > maxCustomIDLen {
					t.Errorf("view %d: custom_id %q is %d bytes > %d", i, id, len(id), maxCustomIDLen)
				}
			}
		}
	}
}

func TestAutocompleteChoiceNamesFitDiscordLimit(t *testing.T) {
	label := courseLabel("math135") + " - " + strings.Repeat("Topics ", 40)
	if n := utf8.RuneCountInString(truncateRunes(label, maxChoiceName)); n > maxChoiceName {
		t.Fatalf("choice name %d runes > %d", n, maxChoiceName)
	}
}

// --- whole views ---------------------------------------------------------------

func TestSearchViewTitleAndColour(t *testing.T) {
	v := searchView("math135", "Fall 2026", []store.Section{sec("LEC", "LEC 001", 100, 100)}, 0)
	if v.embed.Title != "MATH 135 - Algebra for Honours Mathematics [Fall 2026]" {
		t.Errorf("title = %q", v.embed.Title)
	}
	if v.embed.Color != colorFull {
		t.Errorf("all-full course colour = %#x, want red", v.embed.Color)
	}

	v = searchView("math135", "Fall 2026", []store.Section{
		sec("LEC", "LEC 001", 100, 100), sec("LEC", "LEC 002", 99, 100),
	}, 0)
	if v.embed.Color != colorOpen {
		t.Errorf("course with a seat = %#x, want green", v.embed.Color)
	}
	if !strings.Contains(v.embed.Description, "**2 sections** · 1 open · 1 full") {
		t.Errorf("summary line wrong:\n%s", v.embed.Description)
	}
	// The watch hint should point at a FULL section, which is what people want.
	if !strings.Contains(v.embed.Description, "*Watch one: `/watch MATH 135 LEC 001`*") {
		t.Errorf("hint should suggest the full section:\n%s", v.embed.Description)
	}
	if v.embed.Timestamp == "" {
		t.Error("expected an as-of timestamp")
	}
}

func TestWatchlistViewEmptyAndGrouped(t *testing.T) {
	empty := watchlistView(nil, nil, 0)
	if empty.embed.Color != colorWarn || !strings.Contains(empty.embed.Description, "/watch") {
		t.Errorf("empty watchlist not helpful: %+v", empty.embed)
	}
	if empty.components != nil {
		t.Error("empty watchlist should have no buttons")
	}

	cs := sec("LEC", "LEC 001", 100, 100)
	cs.CourseCode, cs.CourseTitle = "cs341", "Algorithms"
	items := []store.WatchItem{
		{Section: sec("LEC", "LEC 002", 99, 100)},
		{Section: cs},
	}
	v := watchlistView(items, map[string]string{"1269": "Fall 2026"}, 0)
	d := v.embed.Description
	for _, want := range []string{
		"**CS 341 - Algorithms [Fall 2026]**",
		"**MATH 135 - Algebra for Honours Mathematics [Fall 2026]**",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("watchlist missing group heading %q:\n%s", want, d)
		}
	}
	if strings.Index(d, "CS 341") > strings.Index(d, "MATH 135") {
		t.Error("courses should be listed alphabetically")
	}
}

func TestAlertEmbedContent(t *testing.T) {
	d := store.Delivery{
		CourseCode: "math135", CourseTitle: "Algebra for Honours Mathematics",
		SectionName: "LEC 002", TermCode: "1269",
		Capacity: 100, Enrolled: 99, SeatsOpen: 1, DetectedAt: time.Unix(1_790_000_000, 0),
	}
	e := alertEmbed(d, "Fall 2026")
	if want := "🔔 MATH 135 - Algebra for Honours Mathematics [Fall 2026]"; e.Title != want {
		t.Errorf("title = %q, want %q", e.Title, want)
	}
	if e.Color != colorOpen {
		t.Errorf("alert colour = %#x, want green", e.Color)
	}
	if len(e.Fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(e.Fields))
	}
	if !strings.Contains(e.Description, "LEC 002") {
		t.Errorf("description should name the section: %q", e.Description)
	}
	// A transition with SeatsOpen unset must never claim zero seats opened.
	d.SeatsOpen = 0
	if got := alertEmbed(d, "Fall 2026").Fields[1].Value; got != "**1**" {
		t.Errorf("seats field = %q, want **1** (an alert implies at least one seat)", got)
	}
}

func TestPlural(t *testing.T) {
	if plural(1, "section") != "1 section" || plural(0, "section") != "0 sections" || plural(3, "section") != "3 sections" {
		t.Error("plural is wrong")
	}
}

// TestNoButtonIDEverEscapesTheValidRange: even a disabled button's ID should
// name a real page, never -1 or one past the end.
func TestNoButtonIDEverEscapesTheValidRange(t *testing.T) {
	for pages := 2; pages <= 6; pages++ {
		for page := 0; page < pages; page++ {
			row := pagerComponents("s", "math135", page, pages)[0].(discordgo.ActionsRow)
			seen := map[string]bool{}
			for _, c := range row.Components {
				id := c.(discordgo.Button).CustomID
				if seen[id] {
					t.Errorf("pages=%d page=%d: duplicate custom_id %q (Discord requires unique IDs)", pages, page, id)
				}
				seen[id] = true
				if id == "pg:noop" {
					continue
				}
				_, _, target, ok := parsePagerID(id)
				if !ok || target < 0 || target >= pages {
					t.Errorf("pages=%d page=%d: id %q targets %d, outside [0,%d)", pages, page, id, target, pages)
				}
			}
		}
	}
}

// TestNoFooterOrStatusEmojiAnywhere guards the requested removals across every
// view, including multi-page ones: no "best-effort" footer, no coloured status
// dots, no checkmark.
func TestNoFooterOrStatusEmojiAnywhere(t *testing.T) {
	sections := manySections(21)
	var items []store.WatchItem
	for _, s := range sections {
		items = append(items, store.WatchItem{Section: s})
	}
	cs := store.Delivery{
		CourseCode: "math135", CourseTitle: "Algebra", SectionName: "LEC 002",
		Capacity: 100, Enrolled: 99, SeatsOpen: 1, DetectedAt: time.Unix(1_790_000_000, 0),
	}

	views := map[string]*discordgo.MessageEmbed{
		"alert": alertEmbed(cs, "Fall 2026"),
		"watch": watchConfirmView("math135", "Algebra", "Fall 2026", sections, 3, 1).embed,
		"matches": matchesView("alg", "Fall 2026", []store.CourseHit{
			{CourseCode: "math135", CourseTitle: "Algebra", Sections: 3, OpenSections: 2},
		}).embed,
	}
	for p := 0; p < 3; p++ {
		views[fmt.Sprintf("search p%d", p)] = searchView("math135", "Fall 2026", sections, p).embed
		views[fmt.Sprintf("watchlist p%d", p)] = watchlistView(items, map[string]string{"1269": "Fall 2026"}, p).embed
	}

	for name, e := range views {
		if e.Footer != nil {
			t.Errorf("%s: has a footer %q, which was removed", name, e.Footer.Text)
		}
		all := e.Title + "\n" + e.Description
		for _, f := range e.Fields {
			all += "\n" + f.Name + f.Value
		}
		for _, banned := range []string{"🟢", "🔴", "✅", "Best-effort", "best-effort", "snapshot"} {
			if strings.Contains(all, banned) {
				t.Errorf("%s: still contains %q", name, banned)
			}
		}
	}
}

// sectionRows mirrors how searchView turns sections into rows, for tests that
// need to know the page count.
func sectionRows(sections []store.Section) []pageRow {
	sections = sortSections(sections)
	rows := make([]pageRow, 0, len(sections))
	for _, s := range sections {
		rows = append(rows, pageRow{group: componentTitle(s.Component), text: searchRow(s)})
	}
	return rows
}

// TestWatchlistNeverSilentlyDropsRows is the reason pages are also limited by
// size and not just row count. Twenty rows is fine for a search, whose headings
// are short ("Lectures"), but a watchlist heading is a whole course title, and
// 20 different courses with long titles overflowed Discord's 4096-character
// limit. The overflow used to be cut off silently, so rows vanished with no
// sign anything was missing.
func TestWatchlistNeverSilentlyDropsRows(t *testing.T) {
	for _, titleLen := range []int{40, 120, 200, 300} {
		var items []store.WatchItem
		for i := 0; i < 45; i++ {
			s := sec("LEC", "LEC 001", 99, 100)
			s.CourseCode = fmt.Sprintf("cs%d", 100+i)
			s.CourseTitle = strings.Repeat("x", titleLen)
			items = append(items, store.WatchItem{Section: s})
		}
		names := map[string]string{"1269": "Fall 2026"}

		seen := map[string]int{}
		for p := 0; p < 60; p++ {
			e := watchlistView(items, names, p).embed
			if n := utf8.RuneCountInString(e.Description); n > maxEmbedDesc {
				t.Fatalf("title=%d page %d: description %d runes > %d", titleLen, p, n, maxEmbedDesc)
			}
			// Whole-description truncation lands on exactly the limit. (An
			// ellipsis alone is not evidence: a long course title is legitimately
			// shortened with one inside its own heading.)
			if utf8.RuneCountInString(e.Description) >= maxEmbedDesc {
				t.Fatalf("title=%d page %d: description hit the %d limit, so rows were cut off",
					titleLen, p, maxEmbedDesc)
			}
			for _, it := range items {
				if strings.Contains(e.Description, "**"+courseHeader(it.Section.CourseCode, it.Section.CourseTitle, "Fall 2026")+"**") {
					seen[it.Section.CourseCode]++
				}
			}
			if len(watchlistView(items, names, p).components) > 0 {
				row := watchlistView(items, names, p).components[0].(discordgo.ActionsRow)
				if row.Components[2].(discordgo.Button).Disabled {
					break // last page reached
				}
			}
		}
		if len(seen) != len(items) {
			t.Errorf("title=%d: %d of %d courses visible across all pages", titleLen, len(seen), len(items))
		}
		for code, c := range seen {
			if c != 1 {
				t.Errorf("title=%d: %s shown on %d pages, want exactly 1", titleLen, code, c)
			}
		}
	}
}

func TestPaginateRowsRespectsBothLimits(t *testing.T) {
	// Many short rows: bounded by row count.
	short := make([]pageRow, 50)
	for i := range short {
		short[i] = pageRow{group: "g", text: "r"}
	}
	for i, pg := range paginateRows(short, 20) {
		if len(pg) > 20 {
			t.Errorf("page %d has %d rows, max 20", i, len(pg))
		}
	}
	// Few huge rows: bounded by character budget, never an empty page.
	huge := make([]pageRow, 10)
	for i := range huge {
		huge[i] = pageRow{group: fmt.Sprintf("g%d", i), text: strings.Repeat("y", 900)}
	}
	pages := paginateRows(huge, 20)
	if len(pages) < 2 {
		t.Fatalf("10 rows of 900 chars fit on %d page(s); the character budget is not applied", len(pages))
	}
	total := 0
	for i, pg := range pages {
		if len(pg) == 0 {
			t.Errorf("page %d is empty", i)
		}
		total += len(pg)
	}
	if total != len(huge) {
		t.Errorf("pages hold %d rows, want %d (rows were lost or duplicated)", total, len(huge))
	}
	// A single row larger than the budget still gets a page instead of looping.
	one := paginateRows([]pageRow{{group: "g", text: strings.Repeat("z", 5000)}}, 20)
	if len(one) != 1 || len(one[0]) != 1 {
		t.Errorf("oversized single row -> %d pages", len(one))
	}
}

// TestWatchHintFallsBackWhenNothingIsFull: with every section open, the tip
// still names a real section rather than a placeholder.
func TestWatchHintFallsBackWhenNothingIsFull(t *testing.T) {
	v := searchView("cs341", "Fall 2026", []store.Section{
		sec("LEC", "LEC 002", 50, 100), sec("LEC", "LEC 001", 60, 100),
	}, 0)
	if !strings.Contains(v.embed.Description, "/watch CS 341 LEC 001") {
		t.Errorf("expected a real section in the tip:\n%s", v.embed.Description)
	}
}

// TestWatchHintOnEveryPage: the tip belongs to the whole result, so a user who
// pages forward still sees it.
func TestWatchHintOnEveryPage(t *testing.T) {
	sections := manySections(45)
	for p := 0; p < 3; p++ {
		if d := searchView("math135", "Fall 2026", sections, p).embed.Description; !strings.Contains(d, "Watch one:") {
			t.Errorf("page %d lost the tip", p)
		}
	}
}
