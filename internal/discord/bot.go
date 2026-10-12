// Package discord implements the Discord bot: slash commands and delivery.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// staleness is the disclaimer appended to every seat figure.
//
// The UW OpenData project explicitly asks integrators to inform users of the
// inherent risks of building on the API (NOTES.md §7), and seat counts are a
// snapshot that can be minutes old. Quest is authoritative.
const disclaimer = "-# Seat counts are a best-effort snapshot and may be stale. " +
	"Verify on Quest before relying on them."

// CourseEnsurer fetches a course's sections on demand, so a user can watch a
// course nobody has subscribed to yet.
type CourseEnsurer interface {
	EnsureCourse(ctx context.Context, term source.TermCode, courseCode string) ([]source.SectionState, error)
}

// Options configures the Bot.
type Options struct {
	Token   string
	AppID   string
	GuildID string // empty = register commands globally
	Store   *store.Store
	Ensurer CourseEnsurer
	Logger  *slog.Logger
	// Term is the term commands operate on.
	Term source.TermCode
}

// Bot wires discordgo to the store.
type Bot struct {
	opts    Options
	log     *slog.Logger
	session *discordgo.Session

	// ready distinguishes the GuildCreate burst emitted during the initial
	// connect from genuine joins that happen afterwards.
	ready atomic.Bool
	// appID is resolved once the session is open.
	appID string
}

// New creates a Bot. It does not connect; call Run.
func New(opts Options) (*Bot, error) {
	if opts.Token == "" {
		return nil, errors.New("discord: token is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s, err := discordgo.New("Bot " + opts.Token)
	if err != nil {
		return nil, fmt.Errorf("discord: new session: %w", err)
	}
	// Only the guild intent is needed to receive interactions. No message
	// content intent, no member scraping: notifco reads nothing from chat.
	s.Identify.Intents = discordgo.IntentsGuilds

	return &Bot{opts: opts, log: opts.Logger, session: s}, nil
}

// Session exposes the underlying session, used by the delivery transport.
func (b *Bot) Session() *discordgo.Session { return b.session }

// commands is the slash command set.
func commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "search",
			Description: "Search UW courses and see live seat counts",
			Options: []*discordgo.ApplicationCommandOption{{
				Name:         "code",
				Description:  "Course code or title, e.g. CS 341",
				Type:         discordgo.ApplicationCommandOptionString,
				Required:     true,
				Autocomplete: true,
			}},
		},
		{
			Name:        "watch",
			Description: "Get a DM when a full section has a seat open up",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:         "code",
					Description:  "Course code, e.g. CS 341",
					Type:         discordgo.ApplicationCommandOptionString,
					Required:     true,
					Autocomplete: true,
				},
				{
					Name:         "section",
					Description:  "Section, e.g. LEC 001 (omit to watch all sections)",
					Type:         discordgo.ApplicationCommandOptionString,
					Required:     false,
					Autocomplete: true,
				},
			},
		},
		{
			Name:        "unwatch",
			Description: "Stop watching a section or a whole course",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:         "code",
					Description:  "Course code, e.g. CS 341",
					Type:         discordgo.ApplicationCommandOptionString,
					Required:     true,
					Autocomplete: true,
				},
				{
					Name:        "section",
					Description: "Section, e.g. LEC 001 (omit to unwatch the whole course)",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
				},
			},
		},
		{
			Name:        "watchlist",
			Description: "Show the sections you are watching",
		},
	}
}

// Run connects, registers commands, and serves until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	b.session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		b.ready.Store(true)
		b.log.Info("discord connected", "user", r.User.Username, "guilds", len(r.Guilds))
	})
	b.session.AddHandler(b.handleInteraction)
	b.session.AddHandler(b.handleGuildCreate)

	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord: open: %w", err)
	}
	defer func() { _ = b.session.Close() }()

	appID := b.opts.AppID
	if appID == "" {
		appID = b.session.State.User.ID
	}
	b.appID = appID

	// Scope matters a lot here.
	//
	// Guild-scoped commands appear instantly but ONLY in that one guild, so a
	// bot invited elsewhere looks completely broken: the commands simply are
	// not there. That is a development convenience only.
	//
	// Global commands work in every server the bot is in, including ones it
	// joins later, which is what a publicly invitable bot needs. They can take
	// up to an hour to propagate, so new guilds additionally get an instant
	// guild-scoped copy via handleGuildCreate.
	if b.opts.GuildID != "" {
		registered, err := b.session.ApplicationCommandBulkOverwrite(appID, b.opts.GuildID, commands())
		if err != nil {
			return fmt.Errorf("discord: register guild commands: %w", err)
		}
		b.log.Warn("commands registered to a SINGLE guild only — "+
			"they will NOT appear in any other server. Unset DISCORD_GUILD_ID for a public bot.",
			"count", len(registered), "guild", b.opts.GuildID)
	} else {
		registered, err := b.session.ApplicationCommandBulkOverwrite(appID, "", commands())
		if err != nil {
			return fmt.Errorf("discord: register global commands: %w", err)
		}
		b.log.Info("slash commands registered globally",
			"count", len(registered),
			"note", "new servers may take up to an hour to see them; joins get an instant copy")
	}

	<-ctx.Done()
	return nil
}

// handleGuildCreate gives a newly joined guild an immediate guild-scoped copy of
// the command set, so users can run commands the moment they invite the bot
// instead of waiting for global propagation.
//
// Only genuine joins are handled. discordgo also emits GuildCreate for every
// existing guild during the initial connect, and registering for all of them on
// each boot would mean one API call per guild at startup.
func (b *Bot) handleGuildCreate(_ *discordgo.Session, g *discordgo.GuildCreate) {
	if !b.ready.Load() || b.opts.GuildID != "" || b.appID == "" {
		return
	}
	if _, err := b.session.ApplicationCommandBulkOverwrite(b.appID, g.ID, commands()); err != nil {
		// Not fatal: global registration still covers this guild shortly.
		b.log.Warn("could not fast-register commands for new guild",
			"guild", g.ID, "name", g.Name, "err", err)
		return
	}
	b.log.Info("joined guild; commands available immediately",
		"guild", g.ID, "name", g.Name)
}

func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.handleCommand(ctx, s, i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.handleAutocomplete(ctx, s, i)
	}
}

func (b *Bot) handleCommand(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()

	// Defer immediately: Discord allows only 3 seconds to acknowledge, and a
	// cold course fetch can exceed that.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		b.log.Error("defer failed", "command", data.Name, "err", err)
		return
	}

	var msg string
	var err error
	// discordgo dispatches each interaction on its own goroutine, so an
	// unrecovered panic here would take the whole process down.
	func() {
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("panic handling command", "command", data.Name, "panic", r)
				err = fmt.Errorf("internal error")
			}
		}()
		switch data.Name {
		case "search":
			msg, err = b.cmdSearch(ctx, data)
		case "watch":
			msg, err = b.cmdWatch(ctx, i, data)
		case "unwatch":
			msg, err = b.cmdUnwatch(ctx, i, data)
		case "watchlist":
			msg, err = b.cmdWatchlist(ctx, i)
		default:
			msg = "Unknown command."
		}
	}()
	if err != nil {
		b.log.Error("command failed", "command", data.Name, "err", err)
		msg = "Something went wrong: " + err.Error()
	}
	if msg == "" {
		msg = "No response."
	}
	msg = clampMessage(msg)

	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &msg,
	}); err != nil {
		b.log.Error("respond failed", "command", data.Name, "err", err)
	}
}

func optString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return o.StringValue()
		}
	}
	return ""
}

func (b *Bot) cmdSearch(ctx context.Context, data discordgo.ApplicationCommandInteractionData) (string, error) {
	query := optString(data.Options, "code")
	if strings.TrimSpace(query) == "" {
		return "Give me a course code, e.g. `/search CS 341`.", nil
	}

	// If the query looks like a full course code, fetch it on demand so search
	// works even for a course nobody has watched yet.
	if _, _, err := source.SplitCourseCode(query); err == nil && b.opts.Ensurer != nil {
		if _, err := b.opts.Ensurer.EnsureCourse(ctx, b.opts.Term, query); err != nil {
			b.log.Warn("ensure course failed", "query", query, "err", err)
		}
	}

	code := source.NormalizeCourseCode(query)
	sections, err := b.opts.Store.SectionsByCourse(ctx, string(b.opts.Term), code)
	if err != nil {
		return "", err
	}
	if len(sections) > 0 {
		return renderSections(code, sections), nil
	}

	hits, err := b.opts.Store.SearchCourses(ctx, string(b.opts.Term), query, 10)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No course matched %q for term %s.", query, b.opts.Term), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "**Courses matching %q**\n", query)
	for _, h := range hits {
		fmt.Fprintf(&sb, "- `%s` %s — %d section(s), %d with seats\n",
			strings.ToUpper(h.CourseCode), h.CourseTitle, h.Sections, h.OpenSections)
	}
	sb.WriteString("\nUse `/search <exact code>` for section detail.\n")
	sb.WriteString(disclaimer)
	return sb.String(), nil
}

func renderSections(code string, sections []store.Section) string {
	var sb strings.Builder
	title := sections[0].CourseTitle
	if title != "" {
		fmt.Fprintf(&sb, "**%s — %s**\n", strings.ToUpper(code), title)
	} else {
		fmt.Fprintf(&sb, "**%s**\n", strings.ToUpper(code))
	}
	var newest time.Time
	for _, s := range sections {
		state := "FULL"
		if s.SeatsOpen() > 0 {
			state = fmt.Sprintf("%d seat(s) open", s.SeatsOpen())
		}
		online := ""
		if s.IsOnline {
			online = " · online"
		}
		fmt.Fprintf(&sb, "- `%s` %d/%d — %s%s\n",
			s.SectionName, s.Enrolled, s.Capacity, state, online)
		if s.ObservedAt.After(newest) {
			newest = s.ObservedAt
		}
	}
	fmt.Fprintf(&sb, "\nAs of <t:%d:R>. Watch one with `/watch %s <section>`.\n",
		newest.Unix(), strings.ToUpper(code))
	sb.WriteString(disclaimer)
	return sb.String()
}

func (b *Bot) cmdWatch(
	ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData,
) (string, error) {
	code := optString(data.Options, "code")
	sectionInput := optString(data.Options, "section")

	if _, _, err := source.SplitCourseCode(code); err != nil {
		return fmt.Sprintf("%q is not a course code. Try `CS 341`.", code), nil
	}

	// Fetch on demand so a brand-new course can be watched.
	if b.opts.Ensurer != nil {
		if _, err := b.opts.Ensurer.EnsureCourse(ctx, b.opts.Term, code); err != nil {
			b.log.Warn("ensure course failed", "code", code, "err", err)
		}
	}

	norm := source.NormalizeCourseCode(code)
	sections, err := b.opts.Store.SectionsByCourse(ctx, string(b.opts.Term), norm)
	if err != nil {
		return "", err
	}
	if len(sections) == 0 {
		return fmt.Sprintf("No sections found for `%s` in term %s.",
			strings.ToUpper(norm), b.opts.Term), nil
	}

	userID, err := b.opts.Store.UpsertUser(ctx, interactionUserID(i))
	if err != nil {
		return "", err
	}
	// Remember where to ping if DMs turn out to be closed.
	if i.GuildID != "" && i.ChannelID != "" {
		if err := b.opts.Store.SetFallbackChannel(ctx, userID, i.GuildID, i.ChannelID); err != nil {
			b.log.Warn("set fallback channel failed", "err", err)
		}
	}

	// No section given: watch all of them.
	targets := sections
	if strings.TrimSpace(sectionInput) != "" {
		sec, err := b.opts.Store.FindSection(ctx, string(b.opts.Term), norm, sectionInput)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Sprintf("No section %q in `%s`. Known: %s",
					sectionInput, strings.ToUpper(norm), sectionNames(sections)), nil
			}
			return "", err
		}
		targets = []store.Section{*sec}
	}

	var added, already int
	for _, t := range targets {
		isNew, err := b.opts.Store.Subscribe(ctx, userID, t.ID)
		if err != nil {
			return "", err
		}
		if isNew {
			added++
		} else {
			already++
		}
	}

	var sb strings.Builder
	if added > 0 {
		fmt.Fprintf(&sb, "Watching **%d** section(s) of `%s`.\n", added, strings.ToUpper(norm))
	}
	if already > 0 {
		fmt.Fprintf(&sb, "Already watching %d section(s).\n", already)
	}
	for _, t := range targets {
		state := "FULL — you will be DMed when a seat opens"
		if t.SeatsOpen() > 0 {
			state = fmt.Sprintf("%d seat(s) open right now", t.SeatsOpen())
		}
		fmt.Fprintf(&sb, "- `%s` %d/%d — %s\n", t.SectionName, t.Enrolled, t.Capacity, state)
	}
	sb.WriteString("\nI only alert on a full → open transition, so an already-open " +
		"section will not notify until it fills and frees again.\n")
	sb.WriteString(disclaimer)
	return sb.String(), nil
}

func sectionNames(sections []store.Section) string {
	names := make([]string, 0, len(sections))
	for _, s := range sections {
		names = append(names, "`"+s.SectionName+"`")
	}
	return strings.Join(names, ", ")
}

func (b *Bot) cmdUnwatch(
	ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData,
) (string, error) {
	code := optString(data.Options, "code")
	sectionInput := optString(data.Options, "section")
	norm := source.NormalizeCourseCode(code)

	userID, err := b.opts.Store.UpsertUser(ctx, interactionUserID(i))
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(sectionInput) == "" {
		n, err := b.opts.Store.UnsubscribeCourse(ctx, userID, string(b.opts.Term), norm)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return fmt.Sprintf("You were not watching any section of `%s`.",
				strings.ToUpper(norm)), nil
		}
		return fmt.Sprintf("Stopped watching **%d** section(s) of `%s`.",
			n, strings.ToUpper(norm)), nil
	}

	sec, err := b.opts.Store.FindSection(ctx, string(b.opts.Term), norm, sectionInput)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Sprintf("No section %q in `%s`.", sectionInput, strings.ToUpper(norm)), nil
		}
		return "", err
	}
	ok, err := b.opts.Store.Unsubscribe(ctx, userID, sec.ID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("You were not watching `%s` `%s`.",
			strings.ToUpper(norm), sec.SectionName), nil
	}
	return fmt.Sprintf("Stopped watching `%s` `%s`.",
		strings.ToUpper(norm), sec.SectionName), nil
}

func (b *Bot) cmdWatchlist(ctx context.Context, i *discordgo.InteractionCreate) (string, error) {
	userID, err := b.opts.Store.UpsertUser(ctx, interactionUserID(i))
	if err != nil {
		return "", err
	}
	items, err := b.opts.Store.Watchlist(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "You are not watching anything. Add one with `/watch CS 341 LEC 001`.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "**Your watchlist** (%d section(s))\n", len(items))
	var newest time.Time
	// Cap the listing so a heavy watcher's reply still fits Discord's limit.
	const maxListed = 40
	for idx, it := range items {
		s := it.Section
		if idx == maxListed {
			fmt.Fprintf(&sb, "- … and %d more\n", len(items)-maxListed)
			break
		}
		state := "FULL"
		if s.SeatsOpen() > 0 {
			state = fmt.Sprintf("%d open", s.SeatsOpen())
		}
		fmt.Fprintf(&sb, "- `%s` `%s` %d/%d — %s\n",
			strings.ToUpper(s.CourseCode), s.SectionName, s.Enrolled, s.Capacity, state)
		if s.ObservedAt.After(newest) {
			newest = s.ObservedAt
		}
	}
	fmt.Fprintf(&sb, "\nAs of <t:%d:R>.\n", newest.Unix())
	sb.WriteString(disclaimer)
	return sb.String(), nil
}

// handleAutocomplete suggests course codes and section names.
func (b *Bot) handleAutocomplete(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	var choices []*discordgo.ApplicationCommandOptionChoice

	var focused, codeVal string
	for _, o := range data.Options {
		if o.Focused {
			focused = o.Name
		}
		if o.Name == "code" {
			codeVal = o.StringValue()
		}
	}

	switch focused {
	case "code":
		if hits, err := b.opts.Store.SearchCourses(ctx, string(b.opts.Term), codeVal, 25); err == nil {
			for _, h := range hits {
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
					Name:  strings.ToUpper(h.CourseCode),
					Value: strings.ToUpper(h.CourseCode),
				})
			}
		}
	case "section":
		norm := source.NormalizeCourseCode(codeVal)
		if sections, err := b.opts.Store.SectionsByCourse(ctx, string(b.opts.Term), norm); err == nil {
			for _, sec := range sections {
				label := fmt.Sprintf("%s (%d/%d)", sec.SectionName, sec.Enrolled, sec.Capacity)
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
					Name:  label,
					Value: sec.SectionName,
				})
				if len(choices) >= 25 {
					break
				}
			}
		}
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		b.log.Debug("autocomplete respond failed", "err", err)
	}
}

// discordMaxMessage is Discord's hard limit on message content length.
// Exceeding it makes the API reject the whole message, so every reply this
// package builds is clamped.
const discordMaxMessage = 2000

// clampMessage truncates to Discord's limit on a line boundary where possible.
func clampMessage(s string) string {
	if len(s) <= discordMaxMessage {
		return s
	}
	const notice = "\n… truncated. Narrow the query or use `/watchlist` for your own watches."
	cut := discordMaxMessage - len(notice)
	if cut < 0 {
		return s[:discordMaxMessage]
	}
	trimmed := s[:cut]
	// Prefer cutting at the last newline so a line is not left half-rendered.
	if idx := strings.LastIndexByte(trimmed, '\n'); idx > cut/2 {
		trimmed = trimmed[:idx]
	}
	return trimmed + notice
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
