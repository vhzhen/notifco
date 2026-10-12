package discord

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// This file renders the /search section table:
//
//	Section  Class  Enrolled   Time                 Date
//	LEC 001  6003    83/100    1:00 PM -  2:20 PM   M  T  W  Th F  S  Su
//
// with the days a section actually meets in bold.
//
// Bold inside a monospace table needs an ANSI code block: markdown bold does
// not render inside a code fence, and without the fence columns cannot align.
// Discord renders ANSI on desktop and web. Its mobile apps are reported to
// ignore it (shown as plain monospace), in which case the table still lines up
// but the bold, and so which days meet, is not visible.

const (
	ansiBold  = "\x1b[1m"
	ansiReset = "\x1b[0m"

	// maxTableWidth is the widest the table may be before gaps shrink. Discord
	// wraps long lines inside a code block rather than scrolling, and a wrapped
	// row wrecks the columns.
	maxTableWidth = 60
	// maxSlotsShown caps continuation lines for a section with many distinct
	// meeting times, so one oddly scheduled section cannot dominate a page.
	maxSlotsShown = 4
)

var tableHeaders = struct{ section, class, enrolled, time, date string }{
	"Section", "Class", "Enrolled", "Time", "Date",
}

// tableLayout holds the column widths. They are computed once over EVERY
// section of the course rather than per page, so the columns do not shift as
// the user pages through.
type tableLayout struct {
	sectionW, classW int
	enrolledW        int // whole Enrolled column
	enrNumW, capNumW int // digits either side of the slash, so slashes line up
	startW, endW     int // widths of the formatted start and end times
	timeW, dateW     int
	// gaps are the spaces between adjacent columns: Section|Class, Class|
	// Enrolled, Enrolled|Time, Time|Date.
	gaps [4]int
}

// dateText is the visible Date column with no styling: "M T W Th F S Su".
func dateText() string { return strings.Join(source.DayLabels[:], " ") }

// slotsOf returns the meetings to show for a section, capped.
func slotsOf(s store.Section) (shown []source.Meeting, hidden int) {
	if len(s.Meetings) > maxSlotsShown {
		return s.Meetings[:maxSlotsShown], len(s.Meetings) - maxSlotsShown
	}
	return s.Meetings, 0
}

// timeLabel is what the Time column says when a section has no meeting.
func timeLabel(s store.Section) string {
	if s.IsOnline {
		return "Online"
	}
	return "TBA"
}

func newTableLayout(sections []store.Section) tableLayout {
	l := tableLayout{
		sectionW: utf8.RuneCountInString(tableHeaders.section),
		classW:   utf8.RuneCountInString(tableHeaders.class),
		dateW:    utf8.RuneCountInString(dateText()),
	}
	timeW := utf8.RuneCountInString(tableHeaders.time)

	for _, s := range sections {
		l.sectionW = max(l.sectionW, utf8.RuneCountInString(safeCell(s.SectionName)))
		l.classW = max(l.classW, len(strconv.Itoa(s.ClassNumber)))
		l.enrNumW = max(l.enrNumW, len(strconv.Itoa(s.Enrolled)))
		l.capNumW = max(l.capNumW, len(strconv.Itoa(s.Capacity)))

		shown, hidden := slotsOf(s)
		for _, m := range shown {
			l.startW = max(l.startW, len(source.FormatClock(m.Start)))
			l.endW = max(l.endW, len(source.FormatClock(m.End)))
		}
		if len(shown) == 0 {
			timeW = max(timeW, utf8.RuneCountInString(timeLabel(s)))
		}
		if hidden > 0 {
			timeW = max(timeW, utf8.RuneCountInString(moreLabel(hidden)))
		}
	}
	if l.startW > 0 {
		timeW = max(timeW, l.startW+3+l.endW) // 3 for " - "
	}
	l.timeW = timeW
	l.enrolledW = max(utf8.RuneCountInString(tableHeaders.enrolled), l.enrNumW+1+l.capNumW)

	// Two-space gaps read best. If the table would be wide enough to wrap,
	// shrink them one at a time starting from the left. Time|Date goes last
	// because "2:20 PM M T W" with a single space reads as one run of text.
	l.gaps = [4]int{2, 2, 2, 2}
	for i := 0; i < len(l.gaps) && l.width() > maxTableWidth; i++ {
		l.gaps[i] = 1
	}
	return l
}

// width is the visible width of a full row.
func (l tableLayout) width() int {
	g := l.gaps[0] + l.gaps[1] + l.gaps[2] + l.gaps[3]
	return l.sectionW + l.classW + l.enrolledW + l.timeW + l.dateW + g
}

// safeCell makes untrusted text safe to print inside the ansi code fence. Names
// come from an external API, and inside this block a backtick run could close
// the fence early while an ESC byte could start styling of its own. Control
// characters and newlines would also break the row alignment.
func safeCell(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
}

func moreLabel(n int) string { return fmt.Sprintf("+%d more", n) }

func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func padLeft(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return strings.Repeat(" ", w-n) + s
	}
	return s
}

// enrolledCell renders "83/100" so the slashes line up down the column.
func (l tableLayout) enrolledCell(s store.Section) string {
	return padLeft(strconv.Itoa(s.Enrolled), l.enrNumW) + "/" + padRight(strconv.Itoa(s.Capacity), l.capNumW)
}

// timeCell renders "8:30 AM - 9:50 AM", padding the hours so AM/PM and the
// dash line up whether or not an earlier row has a two-digit hour.
func (l tableLayout) timeCell(m source.Meeting) string {
	return padLeft(source.FormatClock(m.Start), l.startW) + " - " + padLeft(source.FormatClock(m.End), l.endW)
}

// dateCell renders all seven day labels, with the days the meeting actually
// happens on in bold. Every row has the same seven labels at the same columns;
// only the styling differs.
func dateCell(m source.Meeting) string {
	parts := make([]string, 7)
	for d := 0; d < 7; d++ {
		if m.Meets(d) {
			parts[d] = ansiBold + source.DayLabels[d] + ansiReset
		} else {
			parts[d] = source.DayLabels[d]
		}
	}
	return strings.Join(parts, " ")
}

// line joins the five cells with the layout's gaps and trims trailing spaces.
func (l tableLayout) line(section, class, enrolled, timeCell, date string) string {
	sp := func(n int) string { return strings.Repeat(" ", n) }
	return strings.TrimRight(
		padRight(section, l.sectionW)+sp(l.gaps[0])+
			padRight(class, l.classW)+sp(l.gaps[1])+
			padRight(enrolled, l.enrolledW)+sp(l.gaps[2])+
			padRight(timeCell, l.timeW)+sp(l.gaps[3])+
			date, " ")
}

// headerLines returns the header and the rule beneath it.
func (l tableLayout) headerLines() []string {
	head := l.line(tableHeaders.section, tableHeaders.class, tableHeaders.enrolled, tableHeaders.time, tableHeaders.date)
	return []string{head, strings.Repeat("─", l.width())}
}

// rowText renders one section: a single line normally, or one extra line per
// additional distinct meeting time, with the leading columns left blank.
func (l tableLayout) rowText(s store.Section) string {
	class := strconv.Itoa(s.ClassNumber)
	enrolled := l.enrolledCell(s)
	shown, hidden := slotsOf(s)

	if len(shown) == 0 {
		// No scheduled time: online or TBA. The Date column stays empty rather
		// than showing seven plain labels that would read as "meets never".
		return l.line(safeCell(s.SectionName), class, enrolled, timeLabel(s), "")
	}

	lines := make([]string, 0, len(shown)+1)
	for i, m := range shown {
		if i == 0 {
			lines = append(lines, l.line(safeCell(s.SectionName), class, enrolled, l.timeCell(m), dateCell(m)))
		} else {
			lines = append(lines, l.line("", "", "", l.timeCell(m), dateCell(m)))
		}
	}
	if hidden > 0 {
		lines = append(lines, l.line("", "", "", moreLabel(hidden), ""))
	}
	return strings.Join(lines, "\n")
}

// tableRows turns a course's sections into pageRows for pagination. comp is
// kept so a blank line can separate lectures from tutorials from labs.
func tableRows(sections []store.Section, l tableLayout) []pageRow {
	rows := make([]pageRow, 0, len(sections))
	for _, s := range sections {
		rows = append(rows, pageRow{comp: componentTitle(s.Component), text: l.rowText(s)})
	}
	return rows
}

// renderTablePage renders one page as an ansi code block with the header
// repeated, returning the page actually shown and the page count.
func renderTablePage(rows []pageRow, l tableLayout, page int) (block string, cur, pages int) {
	all := paginateRows(rows, pageSize)
	pages = len(all)
	cur = clampPage(page, pages)

	var sb strings.Builder
	sb.WriteString("```ansi\n")
	for _, h := range l.headerLines() {
		sb.WriteString(h + "\n")
	}
	prev := ""
	for i, r := range all[cur] {
		if i > 0 && r.comp != prev {
			sb.WriteString("\n") // blank line between component groups
		}
		sb.WriteString(r.text + "\n")
		prev = r.comp
	}
	sb.WriteString("```")
	return sb.String(), cur, pages
}
