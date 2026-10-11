package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsToMockWithoutKey(t *testing.T) {
	t.Setenv("UW_API_KEY", "")
	t.Setenv("NOTIFCO_SOURCE", "")

	c, err := Load("poller")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SourceMock() {
		t.Errorf("Source = %q, want mock when no API key is set", c.Source)
	}
}

func TestLoadInfersUwapiWithKey(t *testing.T) {
	t.Setenv("UW_API_KEY", "dummy-key")
	t.Setenv("NOTIFCO_SOURCE", "")

	c, err := Load("poller")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Source != "uwapi" {
		t.Errorf("Source = %q, want uwapi when a key is present", c.Source)
	}
}

func TestLoadRejectsUwapiWithoutKey(t *testing.T) {
	t.Setenv("UW_API_KEY", "")
	t.Setenv("NOTIFCO_SOURCE", "uwapi")

	if _, err := Load("poller"); err == nil {
		t.Fatal("expected an error when NOTIFCO_SOURCE=uwapi but no key is set")
	}
}

func TestLoadRejectsUnknownSource(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "wat")
	if _, err := Load("poller"); err == nil {
		t.Fatal("expected an error for an unknown source")
	}
}

func TestBotRoleRequiresDiscordCredentials(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "mock")
	t.Setenv("DISCORD_TOKEN", "")
	t.Setenv("DISCORD_APP_ID", "")

	if _, err := Load("bot"); err == nil {
		t.Fatal("bot role must require DISCORD_TOKEN")
	}

	t.Setenv("DISCORD_TOKEN", "tok")
	if _, err := Load("bot"); err == nil {
		t.Fatal("bot role must require DISCORD_APP_ID")
	}

	t.Setenv("DISCORD_APP_ID", "app")
	if _, err := Load("bot"); err != nil {
		t.Fatalf("bot role with both credentials should load: %v", err)
	}
}

func TestPollerRoleDoesNotRequireDiscord(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "mock")
	t.Setenv("DISCORD_TOKEN", "")
	t.Setenv("DISCORD_APP_ID", "")

	if _, err := Load("poller"); err != nil {
		t.Fatalf("poller role should not need Discord credentials: %v", err)
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "mock")
	if _, err := Load("frobnicate"); err == nil {
		t.Fatal("expected an error for an unknown role")
	}
}

func TestDurationAndIntParsing(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "mock")
	t.Setenv("NOTIFCO_POLL_INTERVAL", "90s")
	t.Setenv("NOTIFCO_HOT_INTERVAL", "10s")
	t.Setenv("NOTIFCO_RATE_LIMIT", "200")

	c, err := Load("poller")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PollInterval != 90*time.Second {
		t.Errorf("PollInterval = %s, want 90s", c.PollInterval)
	}
	if c.HotInterval != 10*time.Second {
		t.Errorf("HotInterval = %s, want 10s", c.HotInterval)
	}
	if c.RateLimit != 200 {
		t.Errorf("RateLimit = %d, want 200", c.RateLimit)
	}
}

func TestBadDurationRejected(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "mock")
	t.Setenv("NOTIFCO_POLL_INTERVAL", "ninety")
	if _, err := Load("poller"); err == nil {
		t.Fatal("expected an error for an unparseable duration")
	}
}

func TestHotIntervalMustNotExceedBase(t *testing.T) {
	t.Setenv("NOTIFCO_SOURCE", "mock")
	t.Setenv("NOTIFCO_POLL_INTERVAL", "30s")
	t.Setenv("NOTIFCO_HOT_INTERVAL", "60s")
	if _, err := Load("poller"); err == nil {
		t.Fatal("hot interval larger than base interval should be rejected")
	}
}

func TestRedactedHidesKey(t *testing.T) {
	t.Setenv("UW_API_KEY", "super-secret-value")
	t.Setenv("NOTIFCO_SOURCE", "uwapi")

	c, err := Load("poller")
	if err != nil {
		t.Fatal(err)
	}
	out := c.Redacted()
	if strings.Contains(out, "super-secret-value") {
		t.Fatalf("Redacted() leaked the API key: %q", out)
	}
	if !strings.Contains(out, "key=set") {
		t.Errorf("Redacted() should report key presence, got %q", out)
	}
}
