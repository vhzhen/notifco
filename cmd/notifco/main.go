// Command notifco is a Discord bot that DMs University of Waterloo students
// when a watched course section goes from full to having an open seat.
//
// Roles:
//
//	notifco bot      slash commands and message delivery
//	notifco poller   upstream polling, transition detection, fan-out
//	notifco all      both in one process (what the free-tier deploy uses)
//	notifco poll     one-shot poll of every watched course, then exit
//	notifco migrate  apply database migrations and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vhzhen/notifco/internal/config"
	"github.com/vhzhen/notifco/internal/differ"
	"github.com/vhzhen/notifco/internal/discord"
	"github.com/vhzhen/notifco/internal/metrics"
	"github.com/vhzhen/notifco/internal/notifier"
	"github.com/vhzhen/notifco/internal/poller"
	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/source/mock"
	"github.com/vhzhen/notifco/internal/source/uwapi"
	"github.com/vhzhen/notifco/internal/store"
)

func main() {
	role := "all"
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	switch role {
	case "-h", "--help", "help":
		usage()
		return
	}

	if err := run(role); err != nil {
		fmt.Fprintf(os.Stderr, "notifco: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`notifco — DMs you when a full UW course section opens up

Usage:
  notifco [role]

Roles:
  all       bot + poller in one process (default)
  bot       slash commands and delivery only
  poller    polling and detection only
  poll      one-shot poll of watched courses, then exit
  migrate   apply database migrations, then exit

Configuration is entirely via environment variables; see .env.sample.
`)
}

func run(role string) error {
	// migrate and poll do not need Discord credentials.
	cfgRole := role
	switch role {
	case "migrate", "poll":
		cfgRole = "poller"
	}

	cfg, err := config.Load(cfgRole)
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if role == "migrate" {
		log.Info("migrations applied")
		return nil
	}
	log.Info("database ready")

	rdb, err := openRedis(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()
	log.Info("redis ready")

	m := metrics.New()

	src, err := buildSource(cfg, log, m)
	if err != nil {
		return err
	}

	term, err := src.CurrentTerm(ctx)
	if err != nil {
		return fmt.Errorf("resolve current term: %w", err)
	}
	log.Info("notifco starting",
		"role", role, "term", term, "config", cfg.Redacted())

	d := differ.New(rdb)

	// The transport is only wired when Discord is in play. The poller alone
	// still enqueues; the rows simply wait for a bot process to drain them.
	var transport notifier.Transport = noopTransport{log: log}
	var bot *discord.Bot

	notif := notifier.New(notifier.Options{
		Store:       st,
		Transport:   transport,
		Metrics:     m,
		Logger:      log,
		Workers:     cfg.NotifyWorkers,
		MaxAttempts: cfg.MaxAttempts,
		Cooldown:    cfg.Cooldown,
	})

	pol := poller.New(poller.Options{
		Source:       src,
		Store:        st,
		Differ:       d,
		Enqueuer:     notif,
		Metrics:      m,
		Logger:       log,
		BaseInterval: cfg.PollInterval,
		HotInterval:  cfg.HotInterval,
		MaxInterval:  cfg.MaxInterval,
		HotMax:       cfg.HotMax,
		Workers:      cfg.PollWorkers,
	})

	if role == "poll" {
		if err := pol.RunOnce(ctx); err != nil {
			return err
		}
		n, err := notif.DrainOnce(ctx)
		if err != nil {
			return err
		}
		log.Info("one-shot poll complete", "delivered", n)
		return nil
	}

	if role == "bot" || role == "all" {
		bot, err = discord.New(discord.Options{
			Token:   cfg.DiscordToken,
			AppID:   cfg.DiscordAppID,
			GuildID: cfg.DiscordGuildID,
			Store:   st,
			Ensurer: pol,
			Namer:   src,
			Logger:  log,
			Term:    term,
		})
		if err != nil {
			return err
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// Metrics and health endpoint.
	srv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           m.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	if bot != nil {
		// Connect first so the session is live, then point the notifier at it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bot.Run(ctx); err != nil {
				errCh <- err
			}
		}()
		// Give the gateway a moment to establish before delivering.
		time.Sleep(2 * time.Second)
		notif.SetTransport(discord.NewTransport(bot.Session(), st, src, log))

		// Seed the catalog so /search works immediately.
		go func() {
			if err := pol.SeedCatalog(ctx, term); err != nil {
				log.Warn("catalog seed failed", "err", err)
			}
		}()
	}

	// Only drain the outbox when there is a real transport. In poller-only mode
	// rows stay 'pending' for a bot process to deliver, rather than burning
	// retry attempts against a transport that cannot work.
	if bot != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := notif.Run(ctx); err != nil {
				errCh <- err
			}
		}()
	} else {
		log.Info("poller-only mode: notifications will queue for a bot process to deliver")
	}

	if role == "poller" || role == "all" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pol.Run(ctx); err != nil {
				errCh <- err
			}
		}()
	}

	// Wait for a signal or a fatal error.
	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case runErr = <-errCh:
		log.Error("fatal error", "err", runErr)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
	log.Info("notifco stopped")
	return runErr
}

func buildSource(cfg *config.Config, log *slog.Logger, m *metrics.Metrics) (source.CourseSource, error) {
	if cfg.SourceMock() {
		log.Warn("using MOCK course source: no UW_API_KEY set, data is fixtures not live")
		return mock.New()
	}
	return uwapi.New(uwapi.Options{
		BaseURL:           cfg.UWAPIBase,
		APIKey:            cfg.UWAPIKey,
		RequestsPerMinute: cfg.RateLimit,
		Logger:            log,
		Observer:          m,
	})
}

func openRedis(ctx context.Context, url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	c := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return c, nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// noopTransport stands in when no Discord session exists (poller-only mode).
// Rows stay pending so a bot process can deliver them later.
type noopTransport struct{ log *slog.Logger }

func (n noopTransport) Deliver(_ context.Context, d store.Delivery) (string, error) {
	return "", fmt.Errorf("no Discord transport in this process (notification %d queued)", d.ID)
}
