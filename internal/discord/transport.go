package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/vhzhen/notifco/internal/notifier"
	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// Discord API error codes we branch on.
const (
	errCodeCannotDM     = 50007 // "Cannot send messages to this user"
	errCodeUnknownUser  = 10013
	errCodeMissingAcces = 50001
)

// Transport delivers notifications over Discord, preferring a DM and falling
// back to a channel mention when the user's DMs are closed.
type Transport struct {
	session *discordgo.Session
	store   *store.Store
	namer   TermNamer
	log     *slog.Logger
}

// NewTransport builds a Transport. namer may be nil, in which case term names
// are derived locally from the term code.
func NewTransport(s *discordgo.Session, st *store.Store, namer TermNamer, log *slog.Logger) *Transport {
	if log == nil {
		log = slog.Default()
	}
	return &Transport{session: s, store: st, namer: namer, log: log}
}

// termName resolves a term for display, never failing.
func (t *Transport) termName(ctx context.Context, term string) string {
	if t.namer != nil {
		if n, err := t.namer.TermName(ctx, source.TermCode(term)); err == nil && strings.TrimSpace(n) != "" {
			return n
		}
	}
	return source.FallbackTermName(source.TermCode(term))
}

// Deliver implements notifier.Transport.
func (t *Transport) Deliver(ctx context.Context, d store.Delivery) (string, error) {
	embed := alertEmbed(d, t.termName(ctx, d.TermCode))

	// Try a DM first. A cached channel id skips the create-DM round trip.
	channelID := d.DMChannelID
	if channelID == "" {
		ch, err := t.session.UserChannelCreate(d.DiscordUserID)
		if err != nil {
			if code := discordErrCode(err); code == errCodeCannotDM {
				return t.fallback(ctx, d, err)
			}
			if wrapped := classify(err); wrapped != nil {
				return "", wrapped
			}
			return "", fmt.Errorf("discord: create DM channel: %w", err)
		}
		channelID = ch.ID
		// Caching is an optimisation; a failure here must not fail delivery.
		if err := t.cacheByDiscordID(ctx, d.DiscordUserID, channelID); err != nil {
			t.log.Debug("cache dm channel failed", "err", err)
		}
	}

	_, err := t.session.ChannelMessageSendEmbed(channelID, embed)
	if err == nil {
		return "dm", nil
	}
	if code := discordErrCode(err); code == errCodeCannotDM {
		return t.fallback(ctx, d, err)
	}
	if wrapped := classify(err); wrapped != nil {
		return "", wrapped
	}
	return "", fmt.Errorf("discord: send DM: %w", err)
}

// fallback pings the user in their configured channel.
//
// The message deliberately does NOT name the course: a user whose DMs are
// closed should not have their watchlist leaked into a public channel.
func (t *Transport) fallback(ctx context.Context, d store.Delivery, dmErr error) (string, error) {
	if err := t.markBlocked(ctx, d.DiscordUserID); err != nil {
		t.log.Debug("mark dms blocked failed", "err", err)
	}
	if d.FallbackChannelID == "" {
		return "", &notifier.Permanent{Err: fmt.Errorf(
			"discord: DMs closed for user %s and no fallback channel configured: %w",
			d.DiscordUserID, dmErr)}
	}

	msg := fmt.Sprintf(
		"<@%s> a section you are watching just opened up. "+
			"I could not DM you — enable DMs from server members to get details privately. "+
			"Run `/watchlist` to see current seat counts.",
		d.DiscordUserID)

	if _, err := t.session.ChannelMessageSend(d.FallbackChannelID, msg); err != nil {
		if wrapped := classify(err); wrapped != nil {
			return "", wrapped
		}
		return "", fmt.Errorf("discord: fallback ping: %w", err)
	}
	return "channel", nil
}

func (t *Transport) cacheByDiscordID(ctx context.Context, discordUserID, channelID string) error {
	_, err := t.store.Pool().Exec(ctx,
		`UPDATE users SET dm_channel_id = $2, dms_blocked_at = NULL WHERE discord_user_id = $1`,
		discordUserID, channelID)
	return err
}

func (t *Transport) markBlocked(ctx context.Context, discordUserID string) error {
	_, err := t.store.Pool().Exec(ctx,
		`UPDATE users SET dms_blocked_at = NOW(), dm_channel_id = NULL WHERE discord_user_id = $1`,
		discordUserID)
	return err
}

// classify maps a Discord error to a notifier error type so the outbox knows
// whether to retry, throttle, or dead-letter.
func classify(err error) error {
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) {
		return &notifier.Throttled{Err: err, RetryAfter: rl.RetryAfter}
	}

	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		if restErr.Response != nil && restErr.Response.StatusCode == 429 {
			return &notifier.Throttled{Err: err, RetryAfter: 2 * time.Second}
		}
		if restErr.Message != nil {
			switch restErr.Message.Code {
			case errCodeUnknownUser:
				return &notifier.Permanent{Err: err}
			case errCodeMissingAcces:
				// We cannot post there, and that will not change by itself.
				return &notifier.Permanent{Err: err}
			}
		}
		// 4xx other than rate limiting will not fix itself.
		if restErr.Response != nil &&
			restErr.Response.StatusCode >= 400 && restErr.Response.StatusCode < 500 {
			return &notifier.Permanent{Err: err}
		}
	}
	// Unknown or 5xx: let the caller treat it as transient and retry.
	return nil
}

func discordErrCode(err error) int {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Message != nil {
		return restErr.Message.Code
	}
	return 0
}
