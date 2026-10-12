package discord

import (
	"strconv"
	"strings"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// A /search row is the section line followed by its schedule:
//
//	`LEC 001` ▰▰▰▰▰▰▱▱ `76/92` · `5986`
//	    ↳ 8:30 AM - 9:20 AM · Monday, Wednesday
//
// The number is the 4-digit class number, the one entered on Quest.

const (
	scheduleArrow = "↳"
	// scheduleIndent is a "tab" of space before the arrow: four no-break spaces
	// (U+00A0), the usual width of a tab.
	//
	// No-break spaces rather than ordinary ones because HTML rendering collapses
	// runs of ordinary spaces and does not collapse these. That is the only
	// protection they give. They are still Unicode whitespace, so code that
	// trims strings (Go's TrimSpace, JavaScript's trim) strips them like any
	// other space. Whether Discord trims or collapses an embed line's leading
	// whitespace cannot be tested from here, so this is a best guess. If the
	// indent does not show, this constant is the only thing to change.
	scheduleIndent = "\u00a0\u00a0\u00a0\u00a0"
	// maxSlotsShown caps schedule lines per section, so one oddly scheduled
	// section (ECE 105 lectures meet at three different times) cannot crowd out
	// the rest of the page.
	maxSlotsShown = 4
)

// scheduleLine formats one indented schedule line.
func scheduleLine(text string) string {
	return scheduleIndent + scheduleArrow + " " + text
}

var dayNames = [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// daysInWords spells out the days a meeting happens on: "Monday, Wednesday".
func daysInWords(m source.Meeting) string {
	var names []string
	for d := 0; d < 7; d++ {
		if m.Meets(d) {
			names = append(names, dayNames[d])
		}
	}
	return strings.Join(names, ", ")
}

// meetingLine renders one slot: "↳ 8:30 AM - 9:20 AM · Monday, Wednesday",
// indented. A slot with a time but no days (malformed upstream flags) shows just
// the time rather than a dangling separator.
func meetingLine(m source.Meeting) string {
	text := source.FormatClock(m.Start) + " - " + source.FormatClock(m.End)
	if days := daysInWords(m); days != "" {
		text += " · " + days
	}
	return scheduleLine(text)
}

// scheduleLines returns the bullet lines for a section.
//
// A section with no scheduled time says so instead of showing nothing, because
// a missing line reads as "meets never": 13 of 218 real sections surveyed had no
// times at all.
func scheduleLines(s store.Section) []string {
	if len(s.Meetings) == 0 {
		if s.IsOnline {
			return []string{scheduleLine("Online")}
		}
		return []string{scheduleLine("Time TBA")}
	}
	shown, hidden := s.Meetings, 0
	if len(shown) > maxSlotsShown {
		shown, hidden = shown[:maxSlotsShown], len(shown)-maxSlotsShown
	}
	lines := make([]string, 0, len(shown)+1)
	for _, m := range shown {
		lines = append(lines, meetingLine(m))
	}
	if hidden > 0 {
		lines = append(lines, scheduleLine("+"+plural(hidden, "more time")))
	}
	return lines
}

// searchSectionLine is a section's line in /search results: the shared head,
// then the class number where the seats text used to be.
//
//	`LEC 001` ▰▰▰▰▰▰▱▱ `76/92` · `5986`
//
// A full section keeps a plain "Full" after the number, so it still reads as
// full at a glance. The class number is in code formatting so it stands out and
// is easy to copy.
func searchSectionLine(s store.Section) string {
	line := rowHead(s)
	if s.ClassNumber > 0 {
		line += " · `" + strconv.Itoa(s.ClassNumber) + "`"
	}
	if s.SeatsOpen() <= 0 {
		line += " · Full"
	}
	return line + onlineSuffix(s)
}

// searchRow is the section line with its schedule directly beneath it.
func searchRow(s store.Section) string {
	return searchSectionLine(s) + "\n" + strings.Join(scheduleLines(s), "\n")
}
