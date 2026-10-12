package source

import "testing"

func TestNormalizeCourseCode(t *testing.T) {
	tests := []struct{ in, want string }{
		{"CS 341", "cs341"},
		{"cs341", "cs341"},
		{"CS-341", "cs341"},
		{"  cs 341  ", "cs341"},
		{"MATH 237", "math237"},
		{"co 250", "co250"},
		{"STAT230", "stat230"},
		{"MATH237L", "math237l"},
		{"CS/341", "cs341"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := NormalizeCourseCode(tc.in); got != tc.want {
			t.Errorf("NormalizeCourseCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitCourseCode(t *testing.T) {
	tests := []struct {
		in               string
		subject, catalog string
		wantErr          bool
	}{
		{in: "CS 341", subject: "CS", catalog: "341"},
		{in: "cs341", subject: "CS", catalog: "341"},
		{in: "MATH 237", subject: "MATH", catalog: "237"},
		{in: "co250", subject: "CO", catalog: "250"},
		{in: "math237l", subject: "MATH", catalog: "237L"},
		{in: "STAT 230", subject: "STAT", catalog: "230"},
		{in: "341", wantErr: true}, // no subject
		{in: "CS", wantErr: true},  // no number
		{in: "", wantErr: true},
		{in: "  ", wantErr: true},
	}
	for _, tc := range tests {
		subject, catalog, err := SplitCourseCode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SplitCourseCode(%q) = (%q,%q), want error", tc.in, subject, catalog)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitCourseCode(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if subject != tc.subject || catalog != tc.catalog {
			t.Errorf("SplitCourseCode(%q) = (%q,%q), want (%q,%q)",
				tc.in, subject, catalog, tc.subject, tc.catalog)
		}
	}
}

func TestSectionNameFrom(t *testing.T) {
	// classSection is the real section number, not a sequential index, so these
	// are the shapes seen in live CS 341 data.
	tests := []struct {
		component string
		section   int
		want      string
	}{
		{"LEC", 1, "LEC 001"},
		{"LEC", 2, "LEC 002"},
		{"LAB", 101, "LAB 101"},
		{"LAB", 103, "LAB 103"},
		{"TST", 201, "TST 201"},
		{"lec", 1, "LEC 001"},
		{"LEC", 81, "LEC 081"},
	}
	for _, tc := range tests {
		if got := SectionNameFrom(tc.component, tc.section); got != tc.want {
			t.Errorf("SectionNameFrom(%q,%d) = %q, want %q",
				tc.component, tc.section, got, tc.want)
		}
	}
}

func TestIsOnlineSection(t *testing.T) {
	// UW encodes online sections with an 8 in the tens position.
	tests := []struct {
		section int
		want    bool
	}{
		{1, false},   // 001
		{2, false},   // 002
		{81, true},   // 081
		{82, true},   // 082
		{101, false}, // 101
		{181, true},  // 181
		{201, false}, // 201
		{281, true},  // 281
	}
	for _, tc := range tests {
		if got := IsOnlineSection(tc.section); got != tc.want {
			t.Errorf("IsOnlineSection(%d) = %v, want %v", tc.section, got, tc.want)
		}
	}
}

func TestMatchSection(t *testing.T) {
	tests := []struct {
		input, sectionName string
		classSection       int
		want               bool
	}{
		{"LEC 001", "LEC 001", 1, true},
		{"lec 001", "LEC 001", 1, true},
		{"lec001", "LEC 001", 1, true},
		{"LEC-001", "LEC 001", 1, true},
		{"001", "LEC 001", 1, true},
		{"1", "LEC 001", 1, true},
		{"LAB 101", "LAB 101", 101, true},
		{"101", "LAB 101", 101, true},
		{"LEC 002", "LEC 001", 1, false},
		{"002", "LEC 001", 1, false},
		{"", "LEC 001", 1, false},
		{"LAB 001", "LEC 001", 1, false},
	}
	for _, tc := range tests {
		got := MatchSection(tc.input, tc.sectionName, tc.classSection)
		if got != tc.want {
			t.Errorf("MatchSection(%q, %q, %d) = %v, want %v",
				tc.input, tc.sectionName, tc.classSection, got, tc.want)
		}
	}
}

func TestSectionStateHelpers(t *testing.T) {
	tests := []struct {
		name      string
		enrolled  int
		capacity  int
		wantFull  bool
		wantSeats int
	}{
		{"full", 100, 100, true, 0},
		{"one seat", 99, 100, false, 1},
		{"empty", 0, 100, false, 100},
		{"over-enrolled", 105, 100, true, 0},
		{"zero capacity", 0, 0, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := SectionState{Enrolled: tc.enrolled, Capacity: tc.capacity}
			if got := s.IsFull(); got != tc.wantFull {
				t.Errorf("IsFull() = %v, want %v", got, tc.wantFull)
			}
			if got := s.SeatsOpen(); got != tc.wantSeats {
				t.Errorf("SeatsOpen() = %d, want %d", got, tc.wantSeats)
			}
		})
	}
}

func TestSectionKeyString(t *testing.T) {
	k := SectionKey{TermCode: "1269", ClassNumber: 6003}
	if got, want := k.String(), "1269:6003"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestFallbackTermName(t *testing.T) {
	tests := []struct {
		in   TermCode
		want string
	}{
		{"1261", "Winter 2026"},
		{"1265", "Spring 2026"},
		{"1269", "Fall 2026"},
		{"1271", "Winter 2027"},
		{"1199", "Fall 2019"},
		{"", ""},
		{"12", "12"},     // too short: show it rather than invent a name
		{"abcd", "abcd"}, // not numeric
		{"1267", "1267"}, // unknown season digit
	}
	for _, tc := range tests {
		if got := FallbackTermName(tc.in); got != tc.want {
			t.Errorf("FallbackTermName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
