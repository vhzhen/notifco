package discord

import (
	"strings"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// A /search row is the section line followed by its schedule:
//
//	`LEC 001` ▰▰▰▰▰▰▱▱ `76/92` · **16 seats open**
//	• 8:30 AM - 9:20 AM · Monday, Wednesday
//
// The bullet is a literal "•" rather than a markdown list marker. A "-" or "*"
// list item can swallow the plain line that follows it as a continuation, which
// would pull the next section's row into this one's bullet.

const (
	scheduleBullet = "•"
	// maxSlotsShown caps schedule lines per section, so one oddly scheduled
	// section (ECE 105 lectures meet at three different times) cannot crowd out
	// the rest of the page.
	maxSlotsShown = 4
)

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

// meetingLine renders one slot: "• 8:30 AM - 9:20 AM · Monday, Wednesday".
// A slot with a time but no days (malformed upstream flags) shows just the time
// rather than a dangling separator.
func meetingLine(m source.Meeting) string {
	line := scheduleBullet + " " + source.FormatClock(m.Start) + " - " + source.FormatClock(m.End)
	if days := daysInWords(m); days != "" {
		line += " · " + days
	}
	return line
}

// scheduleLines returns the bullet lines for a section.
//
// A section with no scheduled time says so instead of showing nothing, because
// a missing line reads as "meets never": 13 of 218 real sections surveyed had no
// times at all.
func scheduleLines(s store.Section) []string {
	if len(s.Meetings) == 0 {
		if s.IsOnline {
			return []string{scheduleBullet + " Online"}
		}
		return []string{scheduleBullet + " Time TBA"}
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
		lines = append(lines, scheduleBullet+" +"+plural(hidden, "more time"))
	}
	return lines
}

// searchRow is the section line with its schedule directly beneath it.
func searchRow(s store.Section) string {
	return sectionRow(s) + "\n" + strings.Join(scheduleLines(s), "\n")
}
