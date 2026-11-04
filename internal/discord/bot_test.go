package discord

import (
	"strings"
	"testing"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

func TestIsUnwatchAll(t *testing.T) {
	for _, in := range []string{"all", "ALL", " All ", "aLl"} {
		if !isUnwatchAll(in) {
			t.Errorf("isUnwatchAll(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "al", "all1", "CS 341", "allx", "a ll"} {
		if isUnwatchAll(in) {
			t.Errorf("isUnwatchAll(%q) = true, want false", in)
		}
	}
	// The keyword must never be a valid course code, or it would shadow one.
	if _, _, err := source.SplitCourseCode(unwatchAllKeyword); err == nil {
		t.Errorf("%q parses as a course code; it would shadow a real course", unwatchAllKeyword)
	}
}

func TestUnwatchCommandMentionsAll(t *testing.T) {
	for _, c := range commands() {
		if c.Name != "unwatch" {
			continue
		}
		if !strings.Contains(c.Description, "all") {
			t.Errorf("unwatch description does not mention all: %q", c.Description)
		}
		if len(c.Description) > 100 || len(c.Options[0].Description) > 100 {
			t.Errorf("descriptions exceed Discord's 100-character limit")
		}
		return
	}
	t.Fatal("no unwatch command registered")
}

func TestWatchlistFooterMentionsUnwatchAll(t *testing.T) {
	items := []store.WatchItem{{Section: sec("LEC", "LEC 001", 100, 100)}}
	desc := watchlistView(items, map[string]string{"1269": "Fall 2026"}, 0).embed.Description
	if !strings.Contains(desc, "/unwatch all") {
		t.Errorf("watchlist footer does not mention /unwatch all:\n%s", desc)
	}
}
