package source

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseClock(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		// The exact shape the live API returns: the date part is meaningless.
		{"2026-10-12T08:30:00", "08:30", true},
		{"2026-10-12T16:00:00", "16:00", true},
		{"1900-01-01T00:05:00", "00:05", true},
		{"2026-10-12T08:30", "08:30", true},
		{"13:45:00", "13:45", true},
		{"13:45", "13:45", true},
		{"  08:30:00 ", "08:30", true},
		{"", "", false},
		{"   ", "", false},
		{"not a time", "", false},
		{"25:99", "", false},
	}
	for _, tc := range tests {
		got, ok := ParseClock(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ParseClock(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestFormatClock(t *testing.T) {
	tests := []struct{ in, want string }{
		{"08:30", "8:30 AM"},
		{"09:50", "9:50 AM"},
		{"10:00", "10:00 AM"},
		{"11:20", "11:20 AM"},
		{"12:00", "12:00 PM"}, // noon is PM
		{"12:30", "12:30 PM"},
		{"13:00", "1:00 PM"},
		{"16:00", "4:00 PM"},
		{"19:50", "7:50 PM"},
		{"00:05", "12:05 AM"}, // midnight hour is AM
		{"23:59", "11:59 PM"},
		{"", ""},
		{"garbage", ""},
		{"24:00", ""},
	}
	for _, tc := range tests {
		if got := FormatClock(tc.in); got != tc.want {
			t.Errorf("FormatClock(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDaysFromPatternDisambiguates is the reason the seven-flag form is
// preferred: in the text form "T" versus "Th" and "S" versus "Su" are only
// distinguishable by looking ahead.
func TestDaysFromPatternDisambiguates(t *testing.T) {
	tests := []struct{ in, want string }{
		{"MWF", "YNYNYNN"},
		{"TTh", "NYNYNNN"},
		{"T", "NYNNNNN"},
		{"Th", "NNNYNNN"},
		{"S", "NNNNNYN"},
		{"Su", "NNNNNNY"},
		{"SSu", "NNNNNYY"},
		{"MTWThF", "YYYYYNN"},
		{"ThF", "NNNYYNN"},
		{"", "NNNNNNN"},
		{"xyz", "NNNNNNN"},
	}
	for _, tc := range tests {
		if got := DaysFromPattern(tc.in); got != tc.want {
			t.Errorf("DaysFromPattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMeetingMeets(t *testing.T) {
	m := Meeting{Days: "YNYNYNN", Start: "08:30", End: "09:20"}
	want := []bool{true, false, true, false, true, false, false}
	for d, w := range want {
		if got := m.Meets(d); got != w {
			t.Errorf("Meets(%d) = %v, want %v", d, got, w)
		}
	}
	for _, bad := range []int{-1, 7, 99} {
		if m.Meets(bad) {
			t.Errorf("Meets(%d) should be false", bad)
		}
	}
	if (Meeting{Days: "YN"}).Meets(0) {
		t.Error("a malformed day string must not report any meeting day")
	}
}

// TestMeetingFromAPIRealShapes uses values copied from real responses.
func TestMeetingFromAPIRealShapes(t *testing.T) {
	m, ok := MeetingFromAPI("2026-10-12T08:30:00", "2026-10-12T09:20:00", "YNYNYNN", "MWF")
	if !ok || m != (Meeting{Days: "YNYNYNN", Start: "08:30", End: "09:20"}) {
		t.Fatalf("got (%+v, %v)", m, ok)
	}

	// Week flags missing: fall back to the text form so the right days are bold.
	m, ok = MeetingFromAPI("2026-10-12T13:00:00", "2026-10-12T14:20:00", "", "TTh")
	if !ok || m.Days != "NYNYNNN" {
		t.Fatalf("fallback to text pattern: got (%+v, %v), want days NYNYNNN", m, ok)
	}
	// Malformed flags also fall back.
	m, _ = MeetingFromAPI("2026-10-12T13:00:00", "2026-10-12T14:20:00", "YYY", "MWF")
	if m.Days != "YNYNYNN" {
		t.Errorf("malformed flags: days = %q, want fallback YNYNYNN", m.Days)
	}
	// Lowercase flags from a hypothetical upstream change still work.
	m, _ = MeetingFromAPI("2026-10-12T13:00:00", "2026-10-12T14:20:00", "ynynynn", "")
	if m.Days != "YNYNYNN" {
		t.Errorf("lowercase flags: days = %q", m.Days)
	}
}

// TestMeetingFromAPIRejectsUnusableSlots: 13 of 218 surveyed real sections had
// no times at all; those must be reported as having no slot, not given an
// invented one.
func TestMeetingFromAPIRejectsUnusableSlots(t *testing.T) {
	tests := []struct{ name, start, end string }{
		{"both empty", "", ""},
		{"start missing", "", "2026-10-12T09:20:00"},
		{"end missing", "2026-10-12T08:30:00", ""},
		{"zero length", "2026-10-12T08:30:00", "2026-10-12T08:30:00"},
		{"garbage", "soon", "later"},
	}
	for _, tc := range tests {
		if m, ok := MeetingFromAPI(tc.start, tc.end, "NNNNNNN", ""); ok {
			t.Errorf("%s: accepted as %+v", tc.name, m)
		}
	}
}

func TestNormalizeMeetingsMergesSameTimeRows(t *testing.T) {
	// UW sometimes lists Monday and Wednesday as two rows of the same time. A
	// table must show that as ONE line meeting on both days.
	in := []Meeting{
		{Days: "YNNNNNN", Start: "19:00", End: "20:50"},
		{Days: "NNNYNNN", Start: "19:00", End: "20:50"},
	}
	got := NormalizeMeetings(in)
	want := []Meeting{{Days: "YNNYNNN", Start: "19:00", End: "20:50"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNormalizeMeetingsDedupesAndSorts(t *testing.T) {
	// Real shape: CS 341 TST 201 is listed twice with an identical pattern.
	dup := Meeting{Days: "YNNNNNN", Start: "18:00", End: "19:50"}
	if got := NormalizeMeetings([]Meeting{dup, dup}); len(got) != 1 {
		t.Errorf("identical duplicates -> %d slots, want 1", len(got))
	}

	// Real shape: ECE 105 has lectures at three different times.
	in := []Meeting{
		{Days: "NYNNNNN", Start: "13:30", End: "14:20"},
		{Days: "YNNYNNN", Start: "09:30", End: "10:20"},
		{Days: "NYNNNNN", Start: "12:30", End: "13:20"},
	}
	got := NormalizeMeetings(in)
	if len(got) != 3 {
		t.Fatalf("got %d slots, want 3 distinct times kept", len(got))
	}
	for i, wantStart := range []string{"09:30", "12:30", "13:30"} {
		if got[i].Start != wantStart {
			t.Errorf("slot %d starts %s, want %s (sorted by start)", i, got[i].Start, wantStart)
		}
	}
}

func TestNormalizeMeetingsDropsInvalidAndNeverNil(t *testing.T) {
	got := NormalizeMeetings([]Meeting{
		{Days: "YNYNYNN", Start: "", End: "09:20"},
		{Days: "YNYNYNN", Start: "08:30", End: "08:30"},
		{Days: "YNYNYNN", Start: "bad", End: "09:20"},
	})
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty non-nil slice", got)
	}
	// Non-nil matters: nil would marshal as JSON null instead of [].
	b, _ := json.Marshal(got)
	if string(b) != "[]" {
		t.Errorf("marshals as %s, want []", b)
	}
	if out := NormalizeMeetings(nil); out == nil {
		t.Error("NormalizeMeetings(nil) returned nil")
	}
}

func TestNormalizeMeetingsIgnoresMalformedDayFlags(t *testing.T) {
	got := NormalizeMeetings([]Meeting{{Days: "garbage", Start: "08:30", End: "09:20"}})
	if len(got) != 1 || got[0].Days != NoDays {
		t.Fatalf("got %+v, want one slot with no days (time is still real)", got)
	}
}

func TestMeetingJSONRoundTrip(t *testing.T) {
	in := []Meeting{{Days: "YNYNYNN", Start: "08:30", End: "09:20"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"d":"YNYNYNN","s":"08:30","e":"09:20"}]`; string(b) != want {
		t.Errorf("wire format = %s, want %s", b, want)
	}
	var out []Meeting
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Errorf("round trip = (%+v, %v)", out, err)
	}
}
