package discord

import (
	"strings"
	"testing"

	"github.com/vhzhen/notifco/internal/store"
)

func TestUnwatchAllCommandRegistered(t *testing.T) {
	var found bool
	for _, c := range commands() {
		switch c.Name {
		case "unwatchall":
			found = true
			if len(c.Options) != 0 {
				t.Errorf("/unwatchall should take no options, has %d", len(c.Options))
			}
			if c.Description == "" || len(c.Description) > 100 {
				t.Errorf("/unwatchall description empty or over Discord's 100-character limit: %q", c.Description)
			}
		case "unwatch":
			if strings.Contains(strings.ToLower(c.Description), "all") {
				t.Errorf("/unwatch description still mentions the old all keyword: %q", c.Description)
			}
		}
	}
	if !found {
		t.Fatal("no unwatchall command registered")
	}
}

func TestUnwatchAllView(t *testing.T) {
	if e := unwatchAllView(0, 0).embed; !strings.Contains(e.Description, "weren't watching anything") {
		t.Errorf("empty result not explained: %+v", e)
	}
	e := unwatchAllView(3, 2).embed
	if !strings.Contains(e.Description, "3 sections") || !strings.Contains(e.Description, "2 courses") {
		t.Errorf("counts missing from confirmation: %q", e.Description)
	}
	if e := unwatchAllView(1, 1).embed; !strings.Contains(e.Description, "**1 section**") ||
		!strings.Contains(e.Description, "1 course") || strings.Contains(e.Description, "1 courses") {
		t.Errorf("singular counts wrong: %q", e.Description)
	}
}

func TestWatchlistFooterMentionsUnwatchAll(t *testing.T) {
	items := []store.WatchItem{{Section: sec("LEC", "LEC 001", 100, 100)}}
	desc := watchlistView(items, map[string]string{"1269": "Fall 2026"}, 0).embed.Description
	if !strings.Contains(desc, "/unwatchall") {
		t.Errorf("watchlist footer does not mention /unwatchall:\n%s", desc)
	}
}
