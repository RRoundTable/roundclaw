package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
)

// statusTailLines is how much recent transcript /status shows. Enough to see
// what the agent is doing without pushing past Discord's message limit.
const statusTailLines = 12

// Discord is the Discord inbound adapter. It owns the single gateway websocket
// connection; the worker sends replies over REST only.
type Discord struct {
	session *discordgo.Session
	disp    *Dispatcher
	log     *slog.Logger
	guildID string
	// router is nil when routing is disabled, which is the default. Unbound
	// channels are then simply ignored.
	router *claude.Router

	// admin turns natural-language management requests in adminChannel into
	// registry changes. Nil (and adminChannel empty) when not configured.
	admin        *claude.Admin
	adminChannel string

	registered []*discordgo.ApplicationCommand
}

// SetRouter enables routing of messages in channels bound to no agent.
func (d *Discord) SetRouter(r *claude.Router) { d.router = r }

// SetAdmin enables natural-language management in channelID.
func (d *Discord) SetAdmin(a *claude.Admin, channelID string) {
	d.admin = a
	d.adminChannel = channelID
}

// NewDiscord builds the adapter. token is the bot token; guildID scopes slash
// command registration (empty registers globally, which Discord propagates
// slowly).
func NewDiscord(token, guildID string, disp *Dispatcher, log *slog.Logger) (*Discord, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	// Message content is a privileged intent; without it every message body
	// arrives empty and the bot silently does nothing.
	session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	return &Discord{session: session, disp: disp, log: log, guildID: guildID}, nil
}

// Start connects and registers slash commands.
func (d *Discord) Start(ctx context.Context) error {
	d.session.AddHandler(d.onMessage)
	d.session.AddHandler(d.onInteraction)

	if err := d.session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	if err := d.registerCommands(); err != nil {
		d.session.Close()
		return err
	}
	d.log.Info("discord adapter connected", "user", d.session.State.User.Username)
	return nil
}

// Close disconnects.
//
// It deliberately leaves the slash commands registered. Deleting them here
// would race a replacement instance during a restart: compose stops the old
// container and starts the new one, and if the old one's deletes land after the
// new one's registration, the guild is left missing commands with nothing in
// the logs to say why. Registration is an idempotent bulk overwrite instead, so
// a restart converges no matter which order the calls arrive in.
//
// The cost is that a fully stopped roundclaw leaves commands visible that fail
// when used. That is a clearer failure than a command silently disappearing.
func (d *Discord) Close() error {
	return d.session.Close()
}

// scheduleOption names a schedule, with autocomplete over the registered ones.
func scheduleOption() *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type:         discordgo.ApplicationCommandOptionString,
		Name:         "schedule",
		Description:  "Which schedule",
		Required:     true,
		Autocomplete: true,
	}
}

// agentOption is the shared "which agent?" argument.
//
// It is optional on most commands: omitting it uses the channel's bound agent,
// and naming one addresses that agent from any channel. Autocomplete is on so
// nobody has to remember agent IDs.
func agentOption(required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type:         discordgo.ApplicationCommandOptionString,
		Name:         "agent",
		Description:  "Which agent to talk to (defaults to this channel's agent)",
		Required:     required,
		Autocomplete: true,
	}
}

func (d *Discord) registerCommands() error {
	commands := []*discordgo.ApplicationCommand{
		{
			// The direct-invocation path: call any agent from anywhere, without
			// needing the channel to be bound to it.
			Name:        "ask",
			Description: "Send a request to a specific agent",
			Options: []*discordgo.ApplicationCommandOption{
				agentOption(true),
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "prompt",
					Description: "What you want the agent to do",
					Required:    true,
				},
				{
					// Saved into the agent's workspace and named in the prompt,
					// so the agent opens it with Read rather than receiving its
					// contents inline.
					Type:        discordgo.ApplicationCommandOptionAttachment,
					Name:        "file",
					Description: "A file for the agent to read",
				},
			},
		},
		{
			Name:        "agents",
			Description: "List the agents you can call and what each one is for",
		},
		{
			// Natural-language management: "create an agent called pr-bot",
			// "schedule dev to report at 9am". roundclaw validates and applies it.
			Name:        "admin",
			Description: "Manage roundclaw in natural language (agents, schedules, workflows)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "request",
					Description: "What to do — e.g. create an agent called pr-bot for reviews",
					Required:    true,
				},
			},
		},
		{
			// Management. Creating and editing open a form rather than taking
			// flat options: an agent has more fields than a slash command reads
			// comfortably, and tools and channels are free text.
			Name:        "agent",
			Description: "Create, edit or delete an agent",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "create",
					Description: "Open a form to create a new agent",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "edit",
					Description: "Open a form pre-filled with an agent's definition",
					Options:     []*discordgo.ApplicationCommandOption{agentOption(true)},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "show",
					Description: "Show an agent's full definition",
					Options:     []*discordgo.ApplicationCommandOption{agentOption(true)},
				},
				{
					// Enabled does not fit the five inputs a modal allows, and
					// taking an agent out of service is common enough to deserve
					// its own command anyway.
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "enable",
					Description: "Let an agent accept requests again",
					Options:     []*discordgo.ApplicationCommandOption{agentOption(true)},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "disable",
					Description: "Stop an agent accepting requests, keeping its conversation",
					Options:     []*discordgo.ApplicationCommandOption{agentOption(true)},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "delete",
					Description: "Delete an agent's definition (its workspace and conversation are kept)",
					Options:     []*discordgo.ApplicationCommandOption{agentOption(true)},
				},
			},
		},
		{
			Name:        "schedule",
			Description: "Run an agent on a recurring schedule",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "create",
					Description: "Open a form to schedule recurring work for an agent",
					Options:     []*discordgo.ApplicationCommandOption{agentOption(true)},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "list",
					Description: "List schedules with their next run times",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "show",
					Description: "Show one schedule in full",
					Options:     []*discordgo.ApplicationCommandOption{scheduleOption()},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "pause",
					Description: "Stop a schedule firing, keeping its definition",
					Options:     []*discordgo.ApplicationCommandOption{scheduleOption()},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "resume",
					Description: "Let a schedule fire again",
					Options:     []*discordgo.ApplicationCommandOption{scheduleOption()},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "delete",
					Description: "Delete a schedule and its trigger",
					Options:     []*discordgo.ApplicationCommandOption{scheduleOption()},
				},
			},
		},
		{
			Name:        "status",
			Description: "Show what an agent is doing right now",
			Options:     []*discordgo.ApplicationCommandOption{agentOption(false)},
		},
		{
			// The other half of /status: SQLite says what the agent has done,
			// this says whether its workflow is actually alive and whether an
			// activity is quietly retrying.
			Name:        "workflow",
			Description: "Show an agent's Temporal execution — alive, waiting, or retrying",
			Options:     []*discordgo.ApplicationCommandOption{agentOption(false)},
		},
		{
			Name:        "stop",
			Description: "Stop the current turn and drop anything queued behind it",
			Options:     []*discordgo.ApplicationCommandOption{agentOption(false)},
		},
		{
			Name:        "steer",
			Description: "Interrupt the current turn and redirect the agent, keeping context",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "instruction",
					Description: "What the agent should do instead",
					Required:    true,
				},
				agentOption(false),
			},
		},
	}

	// Discord enforces this before the interaction reaches us, so an ordinary
	// member cannot stop another person's turn or spend tokens. It is not a
	// substitute for authorisation inside roundclaw, but it is free and it
	// applies immediately.
	permission := d.disp.Config().Discord.CommandPermissionBits()
	for _, cmd := range commands {
		cmd.DefaultMemberPermissions = permission
	}

	// One atomic overwrite rather than a create per command: it is idempotent,
	// it removes commands this build no longer defines, and it cannot leave the
	// guild half-updated if the process dies partway through.
	created, err := d.session.ApplicationCommandBulkOverwrite(d.session.State.User.ID, d.guildID, commands)
	if err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}
	d.registered = created
	return nil
}

// onMessage turns a plain channel message into a queued request.
func (d *Discord) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}

	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A message in a thread binds through the thread's parent channel and runs
	// as that thread's own conversation; a message in a plain channel is the
	// agent's default conversation.
	agentChannel, conversationID := d.conversation(m.ChannelID)

	// Admin, not a normal agent: the configured admin channel, or an admin thread
	// that /admin opened. Both manage roundclaw in natural language rather than
	// running a turn; a thread keeps the exchange in context.
	if d.admin != nil && (agentChannel == d.adminChannel || d.isAdminThread(m.ChannelID)) {
		go d.handleAdmin(m, text)
		return
	}

	agent, err := d.disp.Registry().ByChannel(ctx, agentChannel)
	if err != nil {
		// No binding. Either the router picks an agent, or the message is not
		// ours — an unbound channel stays silent rather than guessing.
		d.routeUnbound(m, text)
		return
	}

	// A mention gates a bound channel, or the bot would answer everyone's
	// chatter. But a thread the agent is already talking in is an ongoing
	// conversation of its own: requiring a mention on every line there would
	// break the back-and-forth — and drive the user back to the channel, where a
	// fresh mention spawns a *new* thread and a new session, losing the context.
	// So the gate applies to plain channels and to threads the agent has not yet
	// engaged, never to a live thread.
	mentioned, stripped := d.stripMention(m, text)
	if mentioned {
		text = stripped
	}
	if agent.RequireMention && !mentioned && !d.conversationLive(ctx, agent.ID, conversationID) {
		return
	}
	if text == "" {
		// A bare mention with nothing after it. Prompting is friendlier than
		// sending an empty request to the agent.
		if mentioned {
			d.reply(m.ChannelID, "👋 What would you like me to do?")
		}
		return
	}

	prompt, err := d.attachFiles(ctx, agent.ID, text, m.Attachments)
	if err != nil {
		d.reply(m.ChannelID, "⚠️ "+err.Error())
		return
	}

	// Where the reply and the rest of this exchange go. Normally the channel
	// the message arrived in; for a reply-in-thread agent, a fresh thread spun
	// off that message, which then becomes this exchange's own conversation.
	replyChannel := m.ChannelID
	if agent.ReplyInThread && conversationID == "" {
		if threadID, ok := d.startThread(m, text); ok {
			conversationID = threadID
			replyChannel = threadID
		}
	}

	// The Discord message ID is a natural idempotency key: a gateway
	// reconnection can redeliver the same message, and this makes that a no-op.
	sub, submitErr := d.disp.SubmitIn(ctx, agent.ID, conversationID, prompt, core.DiscordOrigin(replyChannel, m.ID), "discord:"+m.ID)
	err = submitErr
	if err != nil {
		d.log.Error("failed to queue discord message", "channel", m.ChannelID, "error", err)
		d.reply(replyChannel, "⚠️ Could not queue that request: "+err.Error())
		return
	}
	if sub.Duplicate {
		return
	}

	// Show a "typing…" indicator in the reply channel while the turn runs, so a
	// slow agent looks busy rather than silent.
	go d.showTyping(replyChannel, agent.ID, sub.TurnID)

	// Only acknowledge when the request has to wait. Acknowledging every
	// message would double the bot's chatter for the common case where it can
	// start immediately.
	if sub.QueuePosition > 0 {
		d.reply(replyChannel, fmt.Sprintf("⏳ Queued (%d ahead) — turn #%d", sub.QueuePosition, sub.TurnID))
	}
}

// showTyping keeps Discord's "typing…" indicator alive in a channel while a turn
// runs. Discord clears the indicator after ~10s, so it is re-sent on a shorter
// interval; the turn's status is polled more often so the indicator stops
// promptly once the reply lands. A hard deadline guards against a stuck turn
// leaking the goroutine.
func (d *Discord) showTyping(channelID, agentID string, turnID int64) {
	st, err := d.disp.Store(context.Background(), agentID)
	if err != nil {
		return
	}
	_ = d.session.ChannelTyping(channelID)
	lastTyped := time.Now()

	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	deadline := time.After(40 * time.Minute)
	for {
		select {
		case <-deadline:
			return
		case <-poll.C:
			turn, err := st.GetTurn(context.Background(), turnID)
			if err != nil || turn.Status != core.TurnRunning {
				return
			}
			if time.Since(lastTyped) >= 8*time.Second {
				_ = d.session.ChannelTyping(channelID)
				lastTyped = time.Now()
			}
		}
	}
}

// conversationLive reports whether the agent already has a turn in this
// conversation — an ongoing thread rather than a plain channel or an untouched
// one. Such a conversation continues without a fresh mention. The turn row is
// written at submit time, so the reply-in-thread agent's own first turn already
// marks the thread live for the user's next line.
func (d *Discord) conversationLive(ctx context.Context, agentID, conversationID string) bool {
	if conversationID == "" {
		return false
	}
	st, err := d.disp.Store(ctx, agentID)
	if err != nil {
		return false
	}
	turns, err := st.RecentTurnsIn(ctx, conversationID, 1)
	return err == nil && len(turns) > 0
}

// handleAdmin turns a natural-language management message into a registry
// change. It runs its own credentialed planner (like the router) to propose a
// structured action, then roundclaw validates and executes it — the LLM never
// touches the API or a token. Runs in its own goroutine: it makes an LLM call
// and must not block Discord's event loop.
func (d *Discord) handleAdmin(m *discordgo.MessageCreate, text string) {
	// An admin thread is a public thread, and a message carries no slash-command
	// permission gate, so enforce the same allow-list here: without it, anyone
	// who can see the thread could create or reconfigure agents.
	if !d.permittedAdminMessage(m) {
		return
	}
	// A leading mention is stripped so "@bot create an agent…" and "create an
	// agent…" read the same; a mention is not required in the admin channel.
	if _, stripped := d.stripMention(m, text); stripped != "" {
		text = stripped
	}
	if strings.TrimSpace(text) == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = d.session.ChannelTyping(m.ChannelID)

	// In an admin thread, the earlier exchange is context so a follow-up like
	// "make it 10am instead" resolves against what was just done.
	history := ""
	if d.isAdminThread(m.ChannelID) {
		history = d.adminThreadHistory(m.ChannelID, m.ID)
	}
	d.reply(m.ChannelID, d.runAdmin(ctx, text, m.ChannelID, history))
}

// handleAdminCommand is the /admin slash command. It opens an admin *thread* and
// runs the first request there, so the operator can keep managing agents and
// schedules in one contextual conversation. Permission-gated like the other
// management commands.
func (d *Discord) handleAdminCommand(i *discordgo.InteractionCreate, request string) {
	if d.admin == nil {
		d.respondNow(i, "⚠️ Natural-language admin is not configured (no credential).")
		return
	}
	if strings.TrimSpace(request) == "" {
		d.respondNow(i, "⚠️ Tell me what to do, e.g. `create an agent called pr-bot`.")
		return
	}
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	threadID, ok := d.startAdminThread(i.ChannelID, request)
	if !ok {
		// No thread (e.g. already inside one, or missing permission): answer once.
		d.followUp(i, d.runAdmin(ctx, request, i.ChannelID, ""))
		return
	}
	d.followUp(i, "🛠️ Opened an admin thread — continue there: <#"+threadID+">")
	_ = d.session.ChannelTyping(threadID)
	d.reply(threadID, "> "+request+"\n\n"+d.runAdmin(ctx, request, threadID, ""))
}

// runAdmin plans and executes one management request, returning the result text.
// Shared by the admin channel, admin threads and the /admin command.
func (d *Discord) runAdmin(ctx context.Context, request, currentChannelID, history string) string {
	// Give the planner the full current state — agents with their settings,
	// schedules, and workflows — so it can answer questions and resolve
	// references instead of inventing an answer.
	agents, err := d.disp.Registry().List(ctx)
	if err != nil {
		return "⚠️ Could not read the registry: " + err.Error()
	}
	var agentLines strings.Builder
	for _, a := range agents {
		fmt.Fprintf(&agentLines,
			"- %s: %s | permission=%s require_mention=%v reply_in_thread=%v channels=[%s] enabled=%v\n",
			a.ID, a.Description, noneIfEmpty(a.PermissionMode), a.RequireMention, a.ReplyInThread,
			strings.Join(a.DiscordChannels, ","), a.Enabled)
	}

	var scheduleLines strings.Builder
	if schedules, err := d.disp.ListSchedules(ctx); err == nil {
		for _, s := range schedules {
			fmt.Fprintf(&scheduleLines, "- %s: agent %s, cron %q (%s)\n", s.ID, s.AgentID, s.Cron, s.Timezone)
		}
	}

	var workflowLines strings.Builder
	if wfs, err := d.disp.Registry().ListWorkflows(ctx); err == nil {
		for _, w := range wfs {
			fmt.Fprintf(&workflowLines, "- %s: %s (%d steps, channel=%s)\n",
				w.ID, w.Description, len(w.Steps), noneIfEmpty(w.ChannelID))
		}
	}

	action, err := d.admin.Plan(ctx, request, claude.AdminContext{
		CurrentChannelID: currentChannelID,
		Agents:           agentLines.String(),
		Schedules:        scheduleLines.String(),
		Workflows:        workflowLines.String(),
		History:          history,
	})
	if err != nil {
		d.log.Warn("admin planning failed", "channel", currentChannelID, "error", err)
		return "⚠️ I could not work out what to do: " + err.Error()
	}
	result, err := d.disp.ExecuteAdmin(ctx, action)
	if err != nil {
		return "⚠️ " + err.Error()
	}
	return result
}

// permittedAdminMessage applies the same allow-list the /admin slash command
// gets, to a message in the admin channel or an admin thread. When commands are
// unrestricted it lets everyone through, exactly as the slash gate does.
func (d *Discord) permittedAdminMessage(m *discordgo.MessageCreate) bool {
	cfg := d.disp.Config().Discord
	if !cfg.CommandsRestricted() {
		return true
	}
	var roles []string
	var userID string
	if m.Member != nil {
		roles = m.Member.Roles
	}
	if m.Author != nil {
		userID = m.Author.ID
	}
	return cfg.PermitsCommand("admin", userID, roles)
}

// adminThreadMarker prefixes an admin thread's name. It is how a later message
// in the thread is recognised as admin without any in-memory state, so it
// survives a gateway restart.
const adminThreadMarker = "🛠️ admin"

func (d *Discord) startAdminThread(channelID, request string) (string, bool) {
	// A message already in a thread cannot spawn another; run one-shot instead.
	if d.isAdminThread(channelID) {
		return "", false
	}
	th, err := d.session.ThreadStartComplex(channelID, &discordgo.ThreadStart{
		Name:                adminThreadName(request),
		Type:                discordgo.ChannelTypeGuildPublicThread,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		d.log.Warn("could not open an admin thread; answering once", "channel", channelID, "error", err)
		return "", false
	}
	return th.ID, true
}

func adminThreadName(request string) string {
	name := adminThreadMarker + ": " + strings.TrimSpace(strings.SplitN(request, "\n", 2)[0])
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	return name
}

// isAdminThread reports whether channelID is a thread opened by /admin.
func (d *Discord) isAdminThread(channelID string) bool {
	ch, err := d.session.State.Channel(channelID)
	if err != nil {
		ch, err = d.session.Channel(channelID)
	}
	return err == nil && ch != nil && ch.IsThread() && strings.HasPrefix(ch.Name, adminThreadMarker)
}

// adminThreadHistory formats the exchange before beforeID as planner context,
// oldest first. Best-effort: no history simply means less context.
func (d *Discord) adminThreadHistory(threadID, beforeID string) string {
	msgs, err := d.session.ChannelMessages(threadID, 20, beforeID, "", "")
	if err != nil {
		return ""
	}
	var b strings.Builder
	for i := len(msgs) - 1; i >= 0; i-- { // ChannelMessages is newest-first
		mm := msgs[i]
		content := strings.TrimSpace(mm.Content)
		if content == "" {
			continue
		}
		who := "operator"
		if mm.Author != nil && mm.Author.Bot {
			who = "admin"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, content)
	}
	return b.String()
}

// startThread spins a Discord thread off a message so a reply-in-thread agent's
// answer, and the exchange that follows, live in it. The thread's ID doubles as
// the conversation ID — each thread is its own session and workspace. On any
// failure it returns false and the caller falls back to replying in the channel.
func (d *Discord) startThread(m *discordgo.MessageCreate, request string) (string, bool) {
	th, err := d.session.MessageThreadStart(m.ChannelID, m.ID, threadName(request), 1440)
	if err != nil {
		d.log.Warn("could not start a thread; replying in the channel",
			"channel", m.ChannelID, "error", err)
		return "", false
	}
	return th.ID, true
}

// threadName derives a thread title from the request: its first line, capped to
// Discord's length limit by runes so a multi-byte character is never split.
func threadName(request string) string {
	name := strings.TrimSpace(strings.SplitN(request, "\n", 2)[0])
	if name == "" {
		name = "request"
	}
	if r := []rune(name); len(r) > 90 {
		name = string(r[:90])
	}
	return name
}

// conversation resolves where a message or command landed into the channel that
// carries the agent binding and the conversation it belongs to.
//
// A Discord thread is a Claude session of its own: messages in it run in an
// isolated workspace and against a separate Claude conversation, so two threads
// under the same channel never share context or step on each other's files. The
// binding, though, lives on the parent channel — a thread is not bound to an
// agent directly — so the two IDs have to be told apart.
//
// A plain channel is the agent's default conversation (empty ID): it is what
// /ask, schedules and webhooks all use, and what existed before threads did.
func (d *Discord) conversation(channelID string) (agentChannel, conversationID string) {
	ch, err := d.session.State.Channel(channelID)
	if err != nil {
		// Not cached — fetch it. A thread the bot has not seen an event for yet
		// still has to be recognised as a thread, or its first message would be
		// mistaken for the parent channel's default conversation.
		ch, err = d.session.Channel(channelID)
	}
	if err != nil || ch == nil || !ch.IsThread() {
		return channelID, ""
	}
	return ch.ParentID, channelID
}

// stripMention reports whether the bot was addressed, and returns the message
// with the mention removed.
//
// Discord delivers a mention as <@id> or <@!id> in the content, and separately
// in Mentions. The IDs are checked rather than the rendered text because the
// display name is not in the payload and changes whenever someone renames the
// bot or gives it a per-server nickname.
func (d *Discord) stripMention(m *discordgo.MessageCreate, text string) (bool, string) {
	self := d.session.State.User
	if self == nil {
		return false, text
	}

	mentioned := false
	for _, u := range m.Mentions {
		if u.ID == self.ID {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return false, text
	}

	for _, form := range []string{"<@" + self.ID + ">", "<@!" + self.ID + ">"} {
		text = strings.ReplaceAll(text, form, " ")
	}
	return true, strings.TrimSpace(strings.Join(strings.Fields(text), " "))
}

// routeUnbound handles a message in a channel bound to no agent.
//
// Routing runs in its own goroutine: it is an LLM call, and blocking Discord's
// event handler on it would stall every other message the bot is receiving.
func (d *Discord) routeUnbound(m *discordgo.MessageCreate, text string) {
	if d.router == nil || !d.disp.Config().Router.RoutesChannel(m.ChannelID) {
		return
	}

	go func() {
		cfg := d.disp.Config()
		summaries := make([]claude.AgentSummary, 0, len(cfg.AllAgents()))
		for _, a := range cfg.AllAgents() {
			summaries = append(summaries, claude.AgentSummary{ID: a.ID, Description: a.Description})
		}

		ctx, cancel := context.WithTimeout(context.Background(), cfg.Router.Timeout+15*time.Second)
		defer cancel()

		decision, err := d.router.Route(ctx, text, summaries)
		if err != nil {
			// A router failure must stay silent. Unbound channels carry
			// ordinary conversation, and an error reply to every message would
			// be worse than doing nothing.
			d.log.Warn("routing failed", "channel", m.ChannelID, "error", err)
			return
		}

		switch decision.Action {
		case claude.RouteIgnore:
			return
		case claude.RouteClarify:
			d.reply(m.ChannelID, "🤔 I am not sure which agent should take that. Try `/ask agent:<name> prompt:…`, or `/agents` to see the options.")
			return
		}

		sub, err := d.disp.Submit(ctx, decision.AgentID, text,
			core.DiscordOrigin(m.ChannelID, m.ID), "discord:"+m.ID)
		if err != nil {
			d.log.Error("failed to queue routed message", "agent", decision.AgentID, "error", err)
			return
		}
		// Routing is a guess, so it says which agent it picked. Otherwise a
		// wrong choice is invisible until the answer comes back off-target.
		d.reply(m.ChannelID, fmt.Sprintf("📨 Routed to `%s` — turn #%d.", decision.AgentID, sub.TurnID))
	}()
}

func (d *Discord) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommandAutocomplete:
		d.handleAutocomplete(i)
		return
	case discordgo.InteractionModalSubmit:
		d.handleAgentForm(i)
		return
	case discordgo.InteractionApplicationCommand:
	default:
		return
	}

	data := i.ApplicationCommandData()

	if !d.permitted(i, data.Name) {
		d.respondNow(i, "⛔ You are not on this bot's allow-list. Ask an administrator to add your role.")
		d.log.Info("refused a command from an unlisted caller",
			"command", data.Name, "user", interactionUser(i))
		return
	}

	if data.Name == "agent" {
		d.handleAgentCommand(i, data)
		return
	}
	if data.Name == "schedule" {
		d.handleScheduleCommand(i, data)
		return
	}

	// /agents needs no target and must work in an unbound channel, since that
	// is exactly where someone is trying to find out what to call.
	if data.Name == "agents" {
		d.handleAgents(i)
		return
	}

	if data.Name == "admin" {
		d.handleAdminCommand(i, optionString(data.Options, "request"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// In a thread the binding is on the parent channel, and control commands act
	// on that thread's conversation. /ask, by contrast, always targets the
	// agent's default session regardless of where it is typed.
	agentChannel, conversationID := d.conversation(i.ChannelID)

	agent, err := ResolveAgent(ctx, d.disp.Registry(), optionString(data.Options, "agent"), agentChannel)
	if err != nil {
		d.respondNow(i, "⚠️ "+err.Error()+"\nRun `/agents` to see what is available.")
		return
	}

	switch data.Name {
	case "ask":
		d.handleAsk(i, agent.ID, optionString(data.Options, "prompt"), discordAttachments(data))
	case "status":
		d.handleStatus(i, agent.ID)
	case "workflow":
		d.handleWorkflow(i, agent.ID)
	case "stop":
		d.handleStop(i, agent.ID, conversationID)
	case "steer":
		d.handleSteer(i, agent.ID, conversationID, optionString(data.Options, "instruction"))
	}
}

// permitted applies roundclaw's own allow-list on top of Discord's permission
// gate. Discord can express "Manage Server"; it cannot express "these three
// people may spend tokens".
func (d *Discord) permitted(i *discordgo.InteractionCreate, command string) bool {
	cfg := d.disp.Config().Discord
	if !cfg.CommandsRestricted() {
		return true
	}

	var userID string
	var roles []string
	if i.Member != nil {
		roles = i.Member.Roles
		if i.Member.User != nil {
			userID = i.Member.User.ID
		}
	}
	if userID == "" && i.User != nil {
		// A direct message carries no member, and so no roles: only an
		// explicitly listed user can act from one.
		userID = i.User.ID
	}
	return cfg.PermitsCommand(command, userID, roles)
}

// handleAutocomplete offers agent IDs as the user types. It reads only static
// config, so it comfortably fits inside Discord's interaction deadline.
func (d *Discord) handleAutocomplete(i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	// Subcommands nest their options one level down.
	opts := data.Options
	if len(opts) == 1 && opts[0].Type == discordgo.ApplicationCommandOptionSubCommand {
		opts = opts[0].Options
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	choices, err := d.autocompleteChoices(ctx, opts)
	if err != nil {
		d.log.Warn("autocomplete could not read the registry", "error", err)
		return
	}

	if err := d.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		d.log.Warn("failed to answer autocomplete", "error", err)
	}
}

// autocompleteChoices offers agents or schedules depending on which option is
// being typed into. Discord caps the list at 25.
func (d *Discord) autocompleteChoices(
	ctx context.Context,
	opts []*discordgo.ApplicationCommandInteractionDataOption,
) ([]*discordgo.ApplicationCommandOptionChoice, error) {
	var choices []*discordgo.ApplicationCommandOptionChoice

	if typed, ok := focusedOption(opts, "schedule"); ok {
		schedules, err := d.disp.Registry().ListSchedules(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range schedules {
			if typed != "" && !strings.Contains(strings.ToLower(s.ID), typed) {
				continue
			}
			label := s.ID + " — " + s.AgentID + " · " + s.Cron
			if len(label) > 100 {
				label = label[:97] + "..."
			}
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: label, Value: s.ID})
			if len(choices) == 25 {
				break
			}
		}
		return choices, nil
	}

	typed, _ := focusedOption(opts, "agent")
	agents, err := d.disp.Registry().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if typed != "" && !strings.Contains(strings.ToLower(a.ID), typed) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: a.Label(), Value: a.ID})
		if len(choices) == 25 {
			break
		}
	}
	return choices, nil
}

// focusedOption reports what has been typed into a named option, and whether
// that option is present at all.
func focusedOption(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) (string, bool) {
	for _, o := range opts {
		if o.Name == name {
			return strings.ToLower(o.StringValue()), true
		}
	}
	return "", false
}

func (d *Discord) handleAgents(i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agents, err := d.disp.Registry().List(ctx)
	if err != nil {
		d.respondNow(i, "⚠️ Could not read the agent registry: "+err.Error())
		return
	}
	if len(agents) == 0 {
		d.respondNow(i, "No agents are registered yet. Create one with `POST /v1/agents`.")
		return
	}

	bound, _ := d.disp.Registry().ByChannel(ctx, i.ChannelID)

	var b strings.Builder
	b.WriteString("**Agents**\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "• `%s`", a.ID)
		if a.Description != "" {
			fmt.Fprintf(&b, " — %s", a.Description)
		}
		if bound.ID == a.ID {
			b.WriteString("  _(this channel)_")
		}
		if !a.Enabled {
			b.WriteString("  _(disabled)_")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nCall one with `/ask agent:<name> prompt:<...>`.")
	d.respondNow(i, b.String())
}

// handleAsk is the direct-invocation path. It defers first because signalling
// Temporal can outlast Discord's three-second interaction deadline.
func (d *Discord) handleAsk(i *discordgo.InteractionCreate, agentID, prompt string, files []*discordgo.MessageAttachment) {
	d.defer_(i)

	// Downloads get their own budget: a 25MB file over a slow link would
	// otherwise eat the whole request timeout and leave the user with a
	// timeout instead of a queued turn.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	prompt, err := d.attachFiles(ctx, agentID, prompt, files)
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}

	// The interaction ID is a natural idempotency key: Discord can redeliver an
	// interaction, and this makes that a no-op instead of a second agent run.
	sub, err := d.disp.Submit(ctx, agentID, prompt,
		core.DiscordOrigin(i.ChannelID, i.ID), "discord-ask:"+i.ID)
	if err != nil {
		d.followUp(i, "⚠️ Could not queue that: "+err.Error())
		return
	}

	msg := fmt.Sprintf("📨 Sent to `%s` — turn #%d.", agentID, sub.TurnID)
	if sub.QueuePosition > 0 {
		msg += fmt.Sprintf(" %d request(s) ahead of it.", sub.QueuePosition)
	}
	d.followUp(i, msg)
}

// optionString reads a string option by name, returning "" when absent.
func optionString(options []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, opt := range options {
		if opt.Name == name {
			return opt.StringValue()
		}
	}
	return ""
}

// handleStatus answers within Discord's three-second interaction budget by
// reading SQLite directly. No Temporal round trip, no LLM: this has to stay
// fast and available precisely when the agent is busy.
func (d *Discord) handleStatus(i *discordgo.InteractionCreate, agentID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	report, err := d.disp.Status(ctx, agentID, statusTailLines)
	if err != nil {
		d.respondNow(i, "⚠️ "+err.Error())
		return
	}
	d.respondNow(i, formatStatus(report))
}

// handleStop and handleSteer defer first: signalling Temporal can exceed the
// three-second interaction deadline, and a late response on an un-deferred
// interaction is discarded by Discord.
func (d *Discord) handleStop(i *discordgo.InteractionCreate, agentID, conversationID string) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Stops the conversation the command was typed in — the thread's turn, or
	// the default one in a plain channel — not every conversation the agent has.
	if err := d.disp.StopIn(ctx, agentID, conversationID, "stopped from discord"); err != nil {
		d.followUp(i, "⚠️ Could not stop: "+err.Error())
		return
	}
	d.followUp(i, "🛑 Stopping the current turn and clearing the queue.")
}

func (d *Discord) handleSteer(i *discordgo.InteractionCreate, agentID, conversationID, instruction string) {
	d.defer_(i)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A steer is a distinct turn, keyed by interaction ID so a Discord retry
	// cannot interrupt the agent twice.
	sub, err := d.disp.SteerIn(ctx, agentID, conversationID, instruction,
		core.DiscordOrigin(i.ChannelID, i.ID), "discord-steer:"+i.ID)
	if err != nil {
		d.followUp(i, "⚠️ Could not steer: "+err.Error())
		return
	}
	d.followUp(i, fmt.Sprintf("↪️ Redirecting — turn #%d. Context is preserved; the interrupted turn's unfinished work is not.", sub.TurnID))
}

func formatStatus(r StatusReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** — %s", r.AgentID, r.State)
	if r.CurrentTurn > 0 {
		fmt.Fprintf(&b, " (turn #%d)", r.CurrentTurn)
	}
	if r.QueueLength > 0 {
		fmt.Fprintf(&b, " · %d queued", r.QueueLength)
	}
	b.WriteString("\n")

	if len(r.Recent) == 0 {
		b.WriteString("_no activity recorded for the current turn_")
		return b.String()
	}

	b.WriteString("```\n")
	for _, e := range r.Recent {
		line := strings.ReplaceAll(e.Content, "```", "'''")
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		fmt.Fprintf(&b, "%-11s %s\n", e.Kind, line)
	}
	b.WriteString("```")

	out := b.String()
	if len(out) > 1900 {
		out = out[:1900] + "\n…```"
	}
	return out
}

func (d *Discord) reply(channelID, content string) {
	if _, err := d.session.ChannelMessageSend(channelID, content); err != nil {
		d.log.Warn("failed to send discord message", "channel", channelID, "error", err)
	}
}

func (d *Discord) respondNow(i *discordgo.InteractionCreate, content string) {
	err := d.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content},
	})
	if err != nil {
		d.log.Warn("failed to respond to interaction", "error", err)
	}
}

func (d *Discord) defer_(i *discordgo.InteractionCreate) {
	err := d.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		d.log.Warn("failed to defer interaction", "error", err)
	}
}

func (d *Discord) followUp(i *discordgo.InteractionCreate, content string) {
	if _, err := d.session.FollowupMessageCreate(i.Interaction, true,
		&discordgo.WebhookParams{Content: content}); err != nil {
		d.log.Warn("failed to send interaction follow-up", "error", err)
	}
}

// RESTSender returns a REST-only Discord client for the worker's delivery
// activity. It never opens a websocket, so it cannot double-consume events.
func RESTSender(token string) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create discord rest client: %w", err)
	}
	return s, nil
}
