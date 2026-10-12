package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/vhzhen/notifco/internal/source"
	"github.com/vhzhen/notifco/internal/store"
)

// CourseEnsurer fetches a course's sections on demand, so a user can watch a
// course nobody has subscribed to yet.
type CourseEnsurer interface {
	EnsureCourse(ctx context.Context, term source.TermCode, courseCode string) ([]source.SectionState, error)
}

// TermNamer resolves a term code to its display name ("1269" -> "Fall 2026").
// The UW API source implements it with a cache; implementations should fall
// back to a locally derived name rather than fail.
type TermNamer interface {
	TermName(ctx context.Context, term source.TermCode) (string, error)
}

// Options configures the Bot.
type Options struct {
	Token   string
	AppID   string
	GuildID string // empty = register commands globally
	Store   *store.Store
	Ensurer CourseEnsurer
	Namer   TermNamer
	Logger  *slog.Logger
	// Term is the term commands operate on.
	Term source.TermCode
}

// Bot wires discordgo to the store.
type Bot struct {
	opts    Options
	log     *slog.Logger
	session *discordgo.Session

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

// termName resolves a term to its display name, never failing: a bare
// "1269" in the UI is worse than a locally derived "Fall 2026".
func (b *Bot) termName(ctx context.Context, term source.TermCode) string {
	if b.opts.Namer != nil {
		if n, err := b.opts.Namer.TermName(ctx, term); err == nil && strings.TrimSpace(n) != "" {
			return n
		}
	}
	return source.FallbackTermName(term)
}

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
	// joins later, which is what a publicly invitable bot needs. Changes to the
	// global set can take up to an hour to propagate, but a guild joining after
	// that is immediate.
	//
	// Do NOT also register guild-scoped copies: Discord lists a guild command
	// and a global command of the same name separately, so every command would
	// appear twice in the picker.
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
		b.log.Info("slash commands registered globally", "count", len(registered))
	}

	<-ctx.Done()
	return nil
}

// handleGuildCreate logs guild joins. It deliberately does NOT register
// commands.
//
// An earlier version registered a guild-scoped copy of the command set here,
// intending to make commands available instantly instead of waiting for global
// propagation. That was wrong twice over:
//
//   - Discord does not merge a guild command with a global command of the same
//     name. It lists both, so every command appeared TWICE in the picker.
//   - It solved a problem that does not exist. The "up to an hour" delay applies
//     to propagating *changes* to the global command set. Once a global command
//     exists, a guild the bot joins later sees it immediately, because global
//     commands belong to the application rather than to any guild.
//
// Global registration alone is correct for a public bot.
func (b *Bot) handleGuildCreate(_ *discordgo.Session, g *discordgo.GuildCreate) {
	b.log.Info("joined guild", "guild", g.ID, "name", g.Name)
}

func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.handleCommand(ctx, s, i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.handleAutocomplete(ctx, s, i)
	case discordgo.InteractionMessageComponent:
		b.handleComponent(ctx, s, i)
	}
}

// safely runs fn, converting a panic into an error. discordgo dispatches each
// interaction on its own goroutine, so an unrecovered panic would take the
// whole process down.
func (b *Bot) safely(what string, fn func() (view, error)) (v view, err error) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("panic handling interaction", "what", what, "panic", r)
			err = errors.New("internal error")
		}
	}()
	return fn()
}

func errorView(err error) view {
	return noticeView(colorFull, "Something went wrong",
		"Try again in a moment. If it keeps happening, the UW API may be down.\n\n*"+truncateRunes(err.Error(), 200)+"*")
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

	v, err := b.safely(data.Name, func() (view, error) {
		switch data.Name {
		case "search":
			return b.cmdSearch(ctx, data)
		case "watch":
			return b.cmdWatch(ctx, i, data)
		case "unwatch":
			return b.cmdUnwatch(ctx, i, data)
		case "watchlist":
			return b.cmdWatchlist(ctx, i)
		default:
			return noticeView(colorWarn, "Unknown command", "I don't know that one."), nil
		}
	})
	if err != nil {
		b.log.Error("command failed", "command", data.Name, "err", err)
		v = errorView(err)
	}

	// Components must be sent as an explicit empty slice to clear them; a nil
	// pointer would leave the previous message's buttons in place.
	embeds := []*discordgo.MessageEmbed{v.embed}
	comps := v.components
	if comps == nil {
		comps = []discordgo.MessageComponent{}
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &embeds,
		Components: &comps,
	}); err != nil {
		b.log.Error("respond failed", "command", data.Name, "err", err)
	}
}

// handleComponent serves pager button clicks by re-rendering the requested page
// in place. The target page is carried in the button's custom ID, so no
// per-message state is stored anywhere.
func (b *Bot) handleComponent(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	kind, key, page, ok := parsePagerID(id)
	if !ok {
		// The disabled page indicator, or an ID from a future version. Just
		// acknowledge so Discord does not show "interaction failed".
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredMessageUpdate,
		})
		return
	}

	v, err := b.safely("pager:"+kind, func() (view, error) {
		switch kind {
		case "s":
			return b.searchPage(ctx, key, page)
		case "w":
			return b.watchlistPage(ctx, interactionUserID(i), page)
		default:
			return view{}, fmt.Errorf("unknown pager %q", kind)
		}
	})
	if err != nil {
		b.log.Error("pager failed", "id", id, "err", err)
		v = errorView(err)
	}

	comps := v.components
	if comps == nil {
		comps = []discordgo.MessageComponent{}
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{v.embed},
			Components: comps,
		},
	}); err != nil {
		b.log.Error("pager respond failed", "id", id, "err", err)
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

// --- /search ----------------------------------------------------------------

func (b *Bot) cmdSearch(ctx context.Context, data discordgo.ApplicationCommandInteractionData) (view, error) {
	query := optString(data.Options, "code")
	if strings.TrimSpace(query) == "" {
		return noticeView(colorWarn, "What should I search for?",
			"Give me a course code, e.g. `/search CS 341`."), nil
	}

	// If the query looks like a full course code, fetch it on demand so search
	// works even for a course nobody has watched yet.
	if _, _, err := source.SplitCourseCode(query); err == nil && b.opts.Ensurer != nil {
		if _, err := b.opts.Ensurer.EnsureCourse(ctx, b.opts.Term, query); err != nil {
			b.log.Warn("ensure course failed", "query", query, "err", err)
		}
	}

	code := source.NormalizeCourseCode(query)
	if v, err := b.searchPage(ctx, code, 0); err != nil {
		return view{}, err
	} else if v.embed != nil {
		return v, nil
	}

	hits, err := b.opts.Store.SearchCourses(ctx, string(b.opts.Term), query, 10)
	if err != nil {
		return view{}, err
	}
	if len(hits) == 0 {
		return noticeView(colorWarn, "No course found",
			fmt.Sprintf("Nothing matched “%s” for %s.\n\nCheck the code, e.g. `/search MATH 135`.",
				truncateRunes(query, 60), b.termName(ctx, b.opts.Term))), nil
	}
	return matchesView(query, b.termName(ctx, b.opts.Term), hits), nil
}

// searchPage renders one page of a course's sections. It returns a zero view
// (nil embed) when the course has no sections, so callers can fall back to a
// text search.
func (b *Bot) searchPage(ctx context.Context, code string, page int) (view, error) {
	code = source.NormalizeCourseCode(code)
	sections, err := b.opts.Store.SectionsByCourse(ctx, string(b.opts.Term), code)
	if err != nil {
		return view{}, err
	}
	if len(sections) == 0 {
		return view{}, nil
	}
	return searchView(code, b.termName(ctx, b.opts.Term), sections, page), nil
}

// --- /watch -----------------------------------------------------------------

func (b *Bot) cmdWatch(
	ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData,
) (view, error) {
	code := optString(data.Options, "code")
	sectionInput := optString(data.Options, "section")

	if _, _, err := source.SplitCourseCode(code); err != nil {
		return noticeView(colorWarn, "That doesn't look like a course code",
			fmt.Sprintf("“%s” isn't a course code. Try `CS 341`.", truncateRunes(code, 60))), nil
	}

	// Fetch on demand so a brand-new course can be watched.
	if b.opts.Ensurer != nil {
		if _, err := b.opts.Ensurer.EnsureCourse(ctx, b.opts.Term, code); err != nil {
			b.log.Warn("ensure course failed", "code", code, "err", err)
		}
	}

	norm := source.NormalizeCourseCode(code)
	termName := b.termName(ctx, b.opts.Term)
	sections, err := b.opts.Store.SectionsByCourse(ctx, string(b.opts.Term), norm)
	if err != nil {
		return view{}, err
	}
	if len(sections) == 0 {
		return noticeView(colorWarn, "No sections found",
			fmt.Sprintf("`%s` has no scheduled sections in %s.", courseLabel(norm), termName)), nil
	}

	userID, err := b.opts.Store.UpsertUser(ctx, interactionUserID(i))
	if err != nil {
		return view{}, err
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
				return noticeView(colorWarn, "No such section",
					fmt.Sprintf("`%s` has no section “%s”.\n\n**Sections:** %s",
						courseLabel(norm), truncateRunes(sectionInput, 40), sectionNames(sections))), nil
			}
			return view{}, err
		}
		targets = []store.Section{*sec}
	}

	var added, already int
	for _, t := range targets {
		isNew, err := b.opts.Store.Subscribe(ctx, userID, t.ID)
		if err != nil {
			return view{}, err
		}
		if isNew {
			added++
		} else {
			already++
		}
	}
	return watchConfirmView(norm, sections[0].CourseTitle, termName, targets, added, already), nil
}

func sectionNames(sections []store.Section) string {
	names := make([]string, 0, len(sections))
	for _, s := range sortSections(sections) {
		names = append(names, "`"+s.SectionName+"`")
	}
	return truncateRunes(strings.Join(names, ", "), 900)
}

// --- /unwatch ---------------------------------------------------------------

func (b *Bot) cmdUnwatch(
	ctx context.Context, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData,
) (view, error) {
	code := optString(data.Options, "code")
	sectionInput := optString(data.Options, "section")
	norm := source.NormalizeCourseCode(code)
	label := courseLabel(norm)

	userID, err := b.opts.Store.UpsertUser(ctx, interactionUserID(i))
	if err != nil {
		return view{}, err
	}

	if strings.TrimSpace(sectionInput) == "" {
		n, err := b.opts.Store.UnsubscribeCourse(ctx, userID, string(b.opts.Term), norm)
		if err != nil {
			return view{}, err
		}
		if n == 0 {
			return noticeView(colorWarn, "Nothing to remove",
				fmt.Sprintf("You weren't watching any section of `%s`.", label)), nil
		}
		return noticeView(colorNeutral, "🛑 Stopped watching "+label,
			fmt.Sprintf("Removed **%s** of `%s`. Any alert not yet sent has been cancelled.",
				plural(int(n), "section"), label)), nil
	}

	sec, err := b.opts.Store.FindSection(ctx, string(b.opts.Term), norm, sectionInput)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return noticeView(colorWarn, "No such section",
				fmt.Sprintf("`%s` has no section “%s”.", label, truncateRunes(sectionInput, 40))), nil
		}
		return view{}, err
	}
	ok, err := b.opts.Store.Unsubscribe(ctx, userID, sec.ID)
	if err != nil {
		return view{}, err
	}
	if !ok {
		return noticeView(colorWarn, "Nothing to remove",
			fmt.Sprintf("You weren't watching `%s` `%s`.", label, sec.SectionName)), nil
	}
	return noticeView(colorNeutral, "🛑 Stopped watching "+label,
		fmt.Sprintf("Removed `%s`. Any alert not yet sent has been cancelled.", sec.SectionName)), nil
}

// --- /watchlist -------------------------------------------------------------

func (b *Bot) cmdWatchlist(ctx context.Context, i *discordgo.InteractionCreate) (view, error) {
	return b.watchlistPage(ctx, interactionUserID(i), 0)
}

func (b *Bot) watchlistPage(ctx context.Context, discordUserID string, page int) (view, error) {
	userID, err := b.opts.Store.UpsertUser(ctx, discordUserID)
	if err != nil {
		return view{}, err
	}
	items, err := b.opts.Store.Watchlist(ctx, userID)
	if err != nil {
		return view{}, err
	}

	// Resolve each distinct term once. Normally that is a single term, but a
	// watch made before a term rollover can linger.
	names := make(map[string]string)
	for _, it := range items {
		t := it.Section.TermCode
		if _, done := names[t]; !done {
			names[t] = b.termName(ctx, source.TermCode(t))
		}
	}
	return watchlistView(items, names, page), nil
}

// --- autocomplete -----------------------------------------------------------

// handleAutocomplete suggests course codes (with titles) and section names.
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
				label := courseLabel(h.CourseCode)
				if h.CourseTitle != "" {
					label += " - " + h.CourseTitle
				}
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
					// The shown name carries the title; the submitted value
					// stays the bare code so the handlers keep working.
					Name:  truncateRunes(label, maxChoiceName),
					Value: strings.ToUpper(h.CourseCode),
				})
			}
		}
	case "section":
		norm := source.NormalizeCourseCode(codeVal)
		if sections, err := b.opts.Store.SectionsByCourse(ctx, string(b.opts.Term), norm); err == nil {
			for _, sec := range sortSections(sections) {
				icon := "🔴"
				if sec.SeatsOpen() > 0 {
					icon = "🟢"
				}
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
					Name:  truncateRunes(fmt.Sprintf("%s %s (%d/%d)", icon, sec.SectionName, sec.Enrolled, sec.Capacity), maxChoiceName),
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

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
