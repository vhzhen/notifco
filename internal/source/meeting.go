package source

import (
	"sort"
	"strings"
	"time"
)

// Meeting is one weekly meeting slot of a section: which days, and when.
//
// Shapes below are taken from real UW responses, not assumed:
//
//	classMeetingStartTime        "2026-10-12T08:30:00"  (the DATE part is
//	                             meaningless; only the clock time is real)
//	classMeetingWeekPatternCode  "YNYNYNN"              seven flags, Monday
//	                             first, so this is Mon/Wed/Fri
//	classMeetingDayPatternCode   "MWF"                  the same thing as text
//
// The seven-flag form is used because the text form is ambiguous to parse
// ("T" versus "Th"), and it is what the table renders from.
type Meeting struct {
	Days  string `json:"d"` // seven Y/N flags, Monday first
	Start string `json:"s"` // 24-hour "15:04"
	End   string `json:"e"` // 24-hour "15:04"
}

// NoDays is the all-No pattern.
const NoDays = "NNNNNNN"

// Meets reports whether the meeting happens on day (0 = Monday .. 6 = Sunday).
func (m Meeting) Meets(day int) bool {
	return day >= 0 && day < 7 && len(m.Days) == 7 && m.Days[day] == 'Y'
}

// ParseClock extracts "15:04" from the formats UW uses. The date portion of a
// full timestamp is ignored. ok is false for empty or unparseable input, which
// is how sections with no scheduled time (online, TBA) arrive.
func ParseClock(raw string) (clock string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "15:04:05", "15:04"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Format("15:04"), true
		}
	}
	return "", false
}

// FormatClock renders "13:30" as "1:30 PM" and "00:05" as "12:05 AM". Returns
// "" for anything that is not a valid 24-hour clock time.
func FormatClock(hhmm string) string {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return ""
	}
	return t.Format("3:04 PM")
}

// DaysFromPattern converts the text form ("MWF", "TTh", "S") into seven flags.
// Used only as a fallback when the flag form is missing. "Th" and "Su" are
// matched before "T" and "S", which is the whole reason the text form is the
// second choice.
func DaysFromPattern(code string) string {
	flags := []byte(NoDays)
	for i := 0; i < len(code); {
		switch {
		case strings.HasPrefix(code[i:], "Th"):
			flags[3] = 'Y'
			i += 2
		case strings.HasPrefix(code[i:], "Su"):
			flags[6] = 'Y'
			i += 2
		default:
			switch code[i] {
			case 'M':
				flags[0] = 'Y'
			case 'T':
				flags[1] = 'Y'
			case 'W':
				flags[2] = 'Y'
			case 'F':
				flags[4] = 'Y'
			case 'S':
				flags[5] = 'Y'
			}
			i++
		}
	}
	return string(flags)
}

// validFlags reports whether s is exactly seven Y/N characters.
func validFlags(s string) bool {
	if len(s) != 7 {
		return false
	}
	for i := 0; i < 7; i++ {
		if s[i] != 'Y' && s[i] != 'N' {
			return false
		}
	}
	return true
}

// MeetingFromAPI builds a Meeting from the four raw UW fields. ok is false when
// the slot has no usable time, so the caller can treat the section as TBA
// rather than invent one.
func MeetingFromAPI(start, end, weekPattern, dayPattern string) (Meeting, bool) {
	s, ok1 := ParseClock(start)
	e, ok2 := ParseClock(end)
	// A zero-length slot (both ends identical) is a placeholder, not a class.
	if !ok1 || !ok2 || s == e {
		return Meeting{}, false
	}
	days := strings.ToUpper(strings.TrimSpace(weekPattern))
	if !validFlags(days) {
		// Flag form missing or malformed: fall back to the text form, so the
		// right days are bold instead of none.
		days = DaysFromPattern(strings.TrimSpace(dayPattern))
	}
	return Meeting{Days: days, Start: s, End: e}, true
}

// NormalizeMeetings cleans a section's raw meetings for display and storage:
//
//   - drops slots without a valid time
//   - merges slots that share the same start and end, OR-ing their days. UW
//     sometimes lists Monday and Wednesday as two rows of the same time, and a
//     table shows that as one line meeting on both days
//   - removes exact duplicates (a midterm listed once per date, say)
//   - sorts by start time, so the earliest slot is first
//
// Never returns nil, so it round-trips through JSON as [] rather than null.
func NormalizeMeetings(in []Meeting) []Meeting {
	type key struct{ start, end string }
	merged := make(map[key][]byte)
	var order []key

	for _, m := range in {
		if FormatClock(m.Start) == "" || FormatClock(m.End) == "" || m.Start == m.End {
			continue
		}
		k := key{m.Start, m.End}
		flags, seen := merged[k]
		if !seen {
			flags = []byte(NoDays)
			order = append(order, k)
		}
		if validFlags(m.Days) {
			for i := 0; i < 7; i++ {
				if m.Days[i] == 'Y' {
					flags[i] = 'Y'
				}
			}
		}
		merged[k] = flags
	}

	out := make([]Meeting, 0, len(order))
	for _, k := range order {
		out = append(out, Meeting{Days: string(merged[k]), Start: k.start, End: k.end})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End < out[j].End
	})
	return out
}
