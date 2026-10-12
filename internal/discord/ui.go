package discord

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// This file is the presentation layer. Everything here is a pure function from
// data to Discord payloads, with no I/O, so formatting and pagination can be
// tested without a Discord connection.

const (
	colorOpen    = 0x57F287 // green: at least one seat somewhere
	colorFull    = 0xED4245 // red: everything full
	colorNeutral = 0x5865F2 // blurple: lists and confirmations
	colorWarn    = 0xFEE75C // yellow: nothing found

	// pageSize is the MOST sections shown on one page. A page may hold fewer:
	// see descBudget.
	pageSize = 20
	// descBudget is how many characters of rows (plus their group headings) fit
	// on one page. Discord rejects a description over 4096 characters, and the
	// summary line and hint around the rows take a few hundred, so rows get
	// the rest. Without this, a watchlist of 20 different courses with long
	// titles overflowed and the overflow was silently cut off, dropping rows.
	descBudget = 3400
	// barWidth is the number of cells in the fill bar.
	barWidth = 8

	// Discord's hard limits. Exceeding any of them makes the API reject the
	// whole message, so they are enforced here and asserted in tests.
	maxEmbedTitle  = 256
	maxEmbedDesc   = 4096
	maxChoiceName  = 100
	maxCustomIDLen = 100
)

// view is a renderable message: one embed plus optional pager buttons.
type view struct {
	embed      *discordgo.MessageEmbed
	components []discordgo.MessageComponent
}

// --- headers ----------------------------------------------------------------

// courseLabel turns a normalised code into display form: "math135" -> "MATH 135".
func courseLabel(code string) string {
	subject, catalog, err := source.SplitCourseCode(code)
	if err != nil {
		return strings.ToUpper(code)
	}
	return subject + " " + catalog
}

// courseHeader builds "MATH 135 - Algebra for Honours Mathematics [Fall 2026]".
// The title and term are each optional; a missing title degrades to
// "MATH 135 [Fall 2026]" rather than showing an empty dash.
func courseHeader(code, title, termName string) string {
	return courseHeaderPrefixed("", code, title, termName)
}

// courseHeaderPrefixed is courseHeader with a leading prefix (an emoji, say),
// keeping the whole result within Discord's 256-character embed title limit.
//
// When it must shorten, it trims the TITLE, never the "[Fall 2026]" suffix:
// the term is the part a student cannot afford to lose.
func courseHeaderPrefixed(prefix, code, title, termName string) string {
	head := prefix + courseLabel(code)
	suffix := ""
	if termName != "" {
		suffix = " [" + termName + "]"
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return truncateRunes(head+suffix, maxEmbedTitle)
	}
	room := maxEmbedTitle - utf8.RuneCountInString(head+" - "+suffix)
	if room < 1 {
		return truncateRunes(head+suffix, maxEmbedTitle)
	}
	return head + " - " + truncateRunes(title, room) + suffix
}

// truncateRunes shortens s to at most n runes, ending in an ellipsis if cut.
// Rune-based, so it never splits a multi-byte character.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}

// --- sections ---------------------------------------------------------------

// componentRank orders section types the way a student thinks about them:
// lectures first, then tutorials, labs, and tests last.
func componentRank(component string) int {
	switch strings.ToUpper(component) {
	case "LEC":
		return 0
	case "TUT":
		return 1
	case "SEM":
		return 2
	case "LAB":
		return 3
	case "TST":
		return 4
	default:
		return 5
	}
}

func componentTitle(component string) string {
	switch strings.ToUpper(component) {
	case "LEC":
		return "Lectures"
	case "TUT":
		return "Tutorials"
	case "SEM":
		return "Seminars"
	case "LAB":
		return "Labs"
	case "TST":
		return "Tests"
	case "PRJ":
		return "Projects"
	case "":
		return "Other"
	default:
		return strings.ToUpper(component)
	}
}

// sortSections orders by component rank, then section name. Stable and
// non-mutating.
func sortSections(in []store.Section) []store.Section {
	out := append([]store.Section(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := componentRank(out[i].Component), componentRank(out[j].Component)
		if ri != rj {
			return ri < rj
		}
		return out[i].SectionName < out[j].SectionName
	})
	return out
}

// fillBar draws enrolment as a bar. It only ever shows a completely full bar
// when the section really is full, so a 99/100 section reads as nearly full
// rather than identical to a 100/100 one.
func fillBar(enrolled, capacity int) string {
	if capacity <= 0 {
		return strings.Repeat("▱", barWidth)
	}
	filled := enrolled * barWidth / capacity
	switch {
	case enrolled >= capacity:
		filled = barWidth
	case filled >= barWidth:
		filled = barWidth - 1
	case filled < 0:
		filled = 0
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", barWidth-filled)
}

// seatsText is the human status: "Full", "1 seat open", "12 seats open".
func seatsText(s store.Section) string {
	switch n := s.SeatsOpen(); {
	case n <= 0:
		return "Full"
	case n == 1:
		return "**1 seat open**"
	default:
		return fmt.Sprintf("**%d seats open**", n)
	}
}

// sectionRow renders one section as a line, e.g.
//
//	`LEC 002` ▰▰▰▰▰▰▰▱ `99/100` · **1 seat open**
//
// Open versus full is carried by the text itself (bold "N seats open" against
// plain "Full") and by the fill bar, rather than by a coloured dot.
func sectionRow(s store.Section) string {
	online := ""
	if s.IsOnline {
		online = " · 🌐 online"
	}
	return fmt.Sprintf("`%s` %s `%d/%d` · %s%s",
		s.SectionName, fillBar(s.Enrolled, s.Capacity),
		s.Enrolled, s.Capacity, seatsText(s), online)
}

// --- pagination -------------------------------------------------------------

// pageRow is one line plus the heading it belongs under.
type pageRow struct {
	group string
	text  string
}

// clampPage forces page into [0, pages-1]. Page numbers arrive inside button
// IDs, so they are untrusted and may be stale or hand-edited.
func clampPage(page, pages int) int {
	if page >= pages {
		page = pages - 1
	}
	if page < 0 {
		page = 0
	}
	return page
}

// groupCost is the characters a group heading adds: the text plus bold markers
// and newlines.
func groupCost(group string) int {
	if group == "" {
		return 0
	}
	return utf8.RuneCountInString(group) + 6
}

// paginateRows splits rows into pages holding at most size rows AND at most
// descBudget characters. The split depends only on the rows, so it can be
// recomputed identically on every button click with no stored state.
//
// A group heading is charged to the first row of each page as well as to every
// group change, since a page that starts mid-group still prints its heading.
func paginateRows(rows []pageRow, size int) [][]pageRow {
	if len(rows) == 0 {
		return [][]pageRow{nil}
	}
	var (
		pages [][]pageRow
		cur   []pageRow
		used  int
		prev  string
	)
	for _, r := range rows {
		cost := utf8.RuneCountInString(r.text) + 1
		if len(cur) == 0 || r.group != prev {
			cost += groupCost(r.group)
		}
		// Always put at least one row on a page so an oversized row cannot
		// loop forever or produce an empty page.
		if len(cur) > 0 && (len(cur) >= size || used+cost > descBudget) {
			pages = append(pages, cur)
			cur, used, prev = nil, 0, ""
			cost = utf8.RuneCountInString(r.text) + 1 + groupCost(r.group)
		}
		cur = append(cur, r)
		used += cost
		prev = r.group
	}
	return append(pages, cur)
}

// renderPage returns the description for one page of rows, the page actually
// shown (after clamping), and the total page count.
//
// A group heading is emitted whenever the group changes, and always at the top
// of a page, so a page that starts mid-group still says what it belongs to.
func renderPage(rows []pageRow, page, size int) (desc string, cur, pages int) {
	all := paginateRows(rows, size)
	pages = len(all)
	cur = clampPage(page, pages)

	var sb strings.Builder
	prev := "\x00" // sentinel: no group yet
	for _, r := range all[cur] {
		if r.group != prev && r.group != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("**" + r.group + "**\n")
		}
		prev = r.group
		sb.WriteString(r.text + "\n")
	}
	return strings.TrimRight(sb.String(), "\n"), cur, pages
}

// pagerID builds a button custom ID. The TARGET page is encoded in the ID, so
// handling a click needs no server-side state: the button says where to go.
//
//	pg:s:math135:2   search results for math135, go to page 2
//	pg:w:1           watchlist, go to page 1
func pagerID(kind, key string, page int) string {
	if key == "" {
		return fmt.Sprintf("pg:%s:%d", kind, page)
	}
	return fmt.Sprintf("pg:%s:%s:%d", kind, key, page)
}

// parsePagerID is the inverse of pagerID. ok is false for anything that is not
// a well-formed pager ID, including the disabled page indicator.
func parsePagerID(id string) (kind, key string, page int, ok bool) {
	parts := strings.Split(id, ":")
	if len(parts) < 3 || parts[0] != "pg" {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n < 0 {
		return "", "", 0, false
	}
	kind = parts[1]
	if len(parts) > 3 {
		key = strings.Join(parts[2:len(parts)-1], ":")
	}
	return kind, key, n, true
}

// pagerComponents builds the Previous / indicator / Next row. Returns nil for a
// single page, so short lists carry no buttons at all.
func pagerComponents(kind, key string, page, pages int) []discordgo.MessageComponent {
	if pages <= 1 {
		return nil
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "◀ Previous",
				Style:    discordgo.SecondaryButton,
				CustomID: pagerID(kind, key, clampPage(page-1, pages)),
				Disabled: page <= 0,
			},
			discordgo.Button{
				Label:    fmt.Sprintf("Page %d / %d", page+1, pages),
				Style:    discordgo.SecondaryButton,
				CustomID: "pg:noop",
				Disabled: true,
			},
			discordgo.Button{
				Label:    "Next ▶",
				Style:    discordgo.PrimaryButton,
				CustomID: pagerID(kind, key, clampPage(page+1, pages)),
				Disabled: page >= pages-1,
			},
		}},
	}
}

// --- views ------------------------------------------------------------------

// searchView renders one course with its sections, paginated.
func searchView(code, termName string, sections []store.Section, page int) view {
	sections = sortSections(sections)
	title := ""
	if len(sections) > 0 {
		title = sections[0].CourseTitle
	}

	rows := make([]pageRow, 0, len(sections))
	open, full := 0, 0
	var newest time.Time
	for _, s := range sections {
		rows = append(rows, pageRow{group: componentTitle(s.Component), text: sectionRow(s)})
		if s.SeatsOpen() > 0 {
			open++
		} else {
			full++
		}
		if s.ObservedAt.After(newest) {
			newest = s.ObservedAt
		}
	}

	body, cur, pages := renderPage(rows, page, pageSize)

	var desc strings.Builder
	fmt.Fprintf(&desc, "**%s** · %d open · %d full\n\n", plural(len(sections), "section"), open, full)
	desc.WriteString(body)
	fmt.Fprintf(&desc, "\n\n*Watch one: `/watch %s %s`*", courseLabel(code), exampleSection(sections))

	color := colorFull
	if open > 0 {
		color = colorOpen
	}

	e := &discordgo.MessageEmbed{
		Title:       courseHeader(code, title, termName),
		Description: truncateRunes(desc.String(), maxEmbedDesc),
		Color:       color,
	}
	if !newest.IsZero() {
		e.Timestamp = newest.UTC().Format(time.RFC3339)
	}
	return view{embed: e, components: pagerComponents("s", source.NormalizeCourseCode(code), cur, pages)}
}

// exampleSection picks a sensible section for the /watch hint: a full one if
// there is one, since that is what people actually want alerts on.
func exampleSection(sections []store.Section) string {
	for _, s := range sections {
		if s.SeatsOpen() <= 0 {
			return s.SectionName
		}
	}
	if len(sections) > 0 {
		return sections[0].SectionName
	}
	return "LEC 001"
}

// watchlistView renders a user's subscriptions grouped by course, paginated.
// termNames maps a term code to its display name.
func watchlistView(items []store.WatchItem, termNames map[string]string, page int) view {
	if len(items) == 0 {
		return view{embed: &discordgo.MessageEmbed{
			Title:       "Your watchlist",
			Description: "You're not watching anything yet.\n\nTry `/search CS 341`, then `/watch CS 341 LEC 001`.",
			Color:       colorWarn,
		}}
	}

	sorted := append([]store.WatchItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].Section, sorted[j].Section
		if a.TermCode != b.TermCode {
			return a.TermCode > b.TermCode // newest term first
		}
		if a.CourseCode != b.CourseCode {
			return a.CourseCode < b.CourseCode
		}
		ra, rb := componentRank(a.Component), componentRank(b.Component)
		if ra != rb {
			return ra < rb
		}
		return a.SectionName < b.SectionName
	})

	rows := make([]pageRow, 0, len(sorted))
	var newest time.Time
	open := 0
	for _, it := range sorted {
		s := it.Section
		rows = append(rows, pageRow{
			group: courseHeader(s.CourseCode, s.CourseTitle, termNames[s.TermCode]),
			text:  sectionRow(s),
		})
		if s.SeatsOpen() > 0 {
			open++
		}
		if s.ObservedAt.After(newest) {
			newest = s.ObservedAt
		}
	}

	body, cur, pages := renderPage(rows, page, pageSize)
	desc := fmt.Sprintf("Watching **%s** · %d open now\n\n%s\n\n*Stop with `/unwatch <course> [section]`*",
		plural(len(items), "section"), open, body)

	e := &discordgo.MessageEmbed{
		Title:       "Your watchlist",
		Description: truncateRunes(desc, maxEmbedDesc),
		Color:       colorNeutral,
	}
	if !newest.IsZero() {
		e.Timestamp = newest.UTC().Format(time.RFC3339)
	}
	return view{embed: e, components: pagerComponents("w", "", cur, pages)}
}

// matchesView renders course search hits when the query was not an exact code.
func matchesView(query, termName string, hits []store.CourseHit) view {
	var sb strings.Builder
	for _, h := range hits {
		line := courseHeader(h.CourseCode, h.CourseTitle, "")
		fmt.Fprintf(&sb, "**%s**\n%s · %d with seats\n\n", line, plural(h.Sections, "section"), h.OpenSections)
	}
	sb.WriteString("*Use `/search <exact code>` for section detail*")
	title := fmt.Sprintf("Courses matching “%s”", truncateRunes(query, 80))
	if termName != "" {
		title += " [" + termName + "]"
	}
	return view{embed: &discordgo.MessageEmbed{
		Title:       truncateRunes(title, maxEmbedTitle),
		Description: truncateRunes(sb.String(), maxEmbedDesc),
		Color:       colorNeutral,
	}}
}

// noticeView is a single-embed message for confirmations and errors.
func noticeView(color int, title, desc string) view {
	return view{embed: &discordgo.MessageEmbed{
		Title:       truncateRunes(title, maxEmbedTitle),
		Description: truncateRunes(desc, maxEmbedDesc),
		Color:       color,
	}}
}

// watchConfirmView confirms a /watch, listing what is now being watched.
func watchConfirmView(code, title, termName string, targets []store.Section, added, already int) view {
	targets = sortSections(targets)
	const maxListed = 20

	var sb strings.Builder
	switch {
	case added > 0 && already > 0:
		fmt.Fprintf(&sb, "Now watching **%s**, and you were already watching %d.\n\n", plural(added, "new section"), already)
	case added > 0:
		fmt.Fprintf(&sb, "Now watching **%s**.\n\n", plural(added, "section"))
	default:
		fmt.Fprintf(&sb, "You were already watching **%s**.\n\n", plural(already, "section"))
	}
	for i, s := range targets {
		if i == maxListed {
			fmt.Fprintf(&sb, "…and %d more\n", len(targets)-maxListed)
			break
		}
		sb.WriteString(sectionRow(s) + "\n")
	}
	sb.WriteString("\nYou'll get a DM when a **full** section frees up. An already-open section stays quiet until it fills and frees again.")

	return view{embed: &discordgo.MessageEmbed{
		Title:       courseHeader(code, title, termName),
		Description: truncateRunes(sb.String(), maxEmbedDesc),
		Color:       colorOpen,
	}}
}

// alertEmbed is the DM sent when a seat opens.
func alertEmbed(d store.Delivery, termName string) *discordgo.MessageEmbed {
	capacity := d.Capacity
	enrolled := d.Enrolled
	seats := d.SeatsOpen
	if seats < 1 {
		seats = 1
	}
	return &discordgo.MessageEmbed{
		Title:       courseHeaderPrefixed("🔔 ", d.CourseCode, d.CourseTitle, termName),
		Description: fmt.Sprintf("A seat just opened in **%s**. Enroll on Quest now — seats go fast.", d.SectionName),
		Color:       colorOpen,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Section", Value: "`" + d.SectionName + "`", Inline: true},
			{Name: "Seats open", Value: fmt.Sprintf("**%d**", seats), Inline: true},
			{Name: "Enrolment", Value: fmt.Sprintf("%s `%d/%d`", fillBar(enrolled, capacity), enrolled, capacity), Inline: true},
		},
		Timestamp: d.DetectedAt.UTC().Format(time.RFC3339),
	}
}

// plural renders "1 section" / "3 sections".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
