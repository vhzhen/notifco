// Package config loads notifco's configuration from the environment.
//
// There is no config file. Everything comes from env vars so the same binary
// runs unchanged in docker-compose, on a VM under systemd, and in CI.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// Discord
	DiscordToken string
	DiscordAppID string
	// GuildID, when set, registers slash commands to a single guild instead of
	// globally. Guild commands appear instantly; global ones take up to an hour
	// to propagate, which makes development painful.
	DiscordGuildID string

	// Upstream data source
	Source    string // "uwapi" | "mock"
	UWAPIKey  string
	UWAPIBase string

	// Storage
	DatabaseURL string
	RedisURL    string

	// Poller
	PollInterval time.Duration
	HotInterval  time.Duration
	HotMax       int
	PollWorkers  int
	RateLimit    int // upstream requests per minute
	MaxInterval  time.Duration

	// Notifier
	NotifyWorkers int
	Cooldown      time.Duration
	MaxAttempts   int

	// Ops
	MetricsAddr string
	LogLevel    string
}

// SourceMock reports whether the mock upstream is in use.
func (c *Config) SourceMock() bool { return c.Source == "mock" }

// Load reads configuration from the environment and validates it for the given
// role ("bot", "poller", or "all"). Validation is role-aware because the poller
// does not need a Discord token and the bot does not need an API key.
func Load(role string) (*Config, error) {
	c := &Config{
		DiscordToken:   os.Getenv("DISCORD_TOKEN"),
		DiscordAppID:   os.Getenv("DISCORD_APP_ID"),
		DiscordGuildID: os.Getenv("DISCORD_GUILD_ID"),

		Source:    strings.ToLower(envDefault("NOTIFCO_SOURCE", "")),
		UWAPIKey:  os.Getenv("UW_API_KEY"),
		UWAPIBase: envDefault("UW_API_BASE", "https://openapi.data.uwaterloo.ca/v3"),

		DatabaseURL: envDefault("DATABASE_URL",
			"postgres://notifco:notifco@localhost:5432/notifco?sslmode=disable"),
		RedisURL: envDefault("REDIS_URL", "redis://localhost:6379/0"),

		MetricsAddr: envDefault("METRICS_ADDR", ":9090"),
		LogLevel:    envDefault("LOG_LEVEL", "info"),
	}

	var err error
	if c.PollInterval, err = envDuration("NOTIFCO_POLL_INTERVAL", 60*time.Second); err != nil {
		return nil, err
	}
	if c.HotInterval, err = envDuration("NOTIFCO_HOT_INTERVAL", 15*time.Second); err != nil {
		return nil, err
	}
	if c.MaxInterval, err = envDuration("NOTIFCO_MAX_INTERVAL", 32*time.Minute); err != nil {
		return nil, err
	}
	if c.Cooldown, err = envDuration("NOTIFCO_COOLDOWN", 30*time.Minute); err != nil {
		return nil, err
	}
	if c.HotMax, err = envInt("NOTIFCO_HOT_MAX", 50); err != nil {
		return nil, err
	}
	if c.PollWorkers, err = envInt("NOTIFCO_POLL_WORKERS", 4); err != nil {
		return nil, err
	}
	// Default to the documented 120 req/min. Measured headroom is ~409 req/min
	// (see VERIFIED.md), but the UW docs call the limit experimental and send no
	// rate-limit headers, so the extra is treated as safety margin, not budget.
	if c.RateLimit, err = envInt("NOTIFCO_RATE_LIMIT", 120); err != nil {
		return nil, err
	}
	if c.NotifyWorkers, err = envInt("NOTIFCO_NOTIFY_WORKERS", 8); err != nil {
		return nil, err
	}
	if c.MaxAttempts, err = envInt("NOTIFCO_MAX_ATTEMPTS", 6); err != nil {
		return nil, err
	}

	// Choosing the source: an explicit NOTIFCO_SOURCE wins. Otherwise infer from
	// whether a key is present, so the repo runs out of the box with no setup.
	if c.Source == "" {
		if c.UWAPIKey != "" {
			c.Source = "uwapi"
		} else {
			c.Source = "mock"
		}
	}
	if c.Source != "uwapi" && c.Source != "mock" {
		return nil, fmt.Errorf("NOTIFCO_SOURCE must be \"uwapi\" or \"mock\", got %q", c.Source)
	}
	if c.Source == "uwapi" && c.UWAPIKey == "" {
		return nil, errors.New("NOTIFCO_SOURCE=uwapi but UW_API_KEY is empty")
	}

	if err := c.validate(role); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate(role string) error {
	switch role {
	case "bot", "all":
		if c.DiscordToken == "" {
			return errors.New("DISCORD_TOKEN is required to run the bot")
		}
		if c.DiscordAppID == "" {
			return errors.New("DISCORD_APP_ID is required to register slash commands")
		}
	case "poller":
		// The poller never talks to Discord.
	default:
		return fmt.Errorf("unknown role %q (want bot, poller, or all)", role)
	}

	if c.PollInterval <= 0 {
		return errors.New("NOTIFCO_POLL_INTERVAL must be positive")
	}
	if c.HotInterval <= 0 {
		return errors.New("NOTIFCO_HOT_INTERVAL must be positive")
	}
	if c.HotInterval > c.PollInterval {
		return fmt.Errorf("NOTIFCO_HOT_INTERVAL (%s) must not exceed NOTIFCO_POLL_INTERVAL (%s)",
			c.HotInterval, c.PollInterval)
	}
	if c.RateLimit <= 0 {
		return errors.New("NOTIFCO_RATE_LIMIT must be positive")
	}
	if c.PollWorkers <= 0 || c.NotifyWorkers <= 0 {
		return errors.New("worker counts must be positive")
	}
	if c.MaxAttempts <= 0 {
		return errors.New("NOTIFCO_MAX_ATTEMPTS must be positive")
	}
	return nil
}

// Redacted returns a loggable summary with no secrets in it.
func (c *Config) Redacted() string {
	return fmt.Sprintf(
		"source=%s key=%s poll=%s hot=%s workers=%d/%d rate=%d/min cooldown=%s attempts=%d",
		c.Source, present(c.UWAPIKey), c.PollInterval, c.HotInterval,
		c.PollWorkers, c.NotifyWorkers, c.RateLimit, c.Cooldown, c.MaxAttempts,
	)
}

func present(s string) string {
	if s == "" {
		return "absent"
	}
	return "set"
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (try 60s, 5m)", key, v)
	}
	return d, nil
}
