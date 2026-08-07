package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
)

// Slack, over Socket Mode.
//
// The gateway dials out and holds the one live connection; the worker speaks
// only over the Web API. That is the same split Discord has, for the same
// reason — a second live connection would deliver every inbound event twice —
// and it is most of why Socket Mode was chosen over the Events API. See
// adr/001-slack-socket-mode.
//
// Everything here calls the same Dispatcher the Discord and HTTP edges call.
// Nothing in this file talks to Temporal or SQLite on its own; if it looks like
// it needs to, the behaviour belongs on the dispatcher where all three edges
// share it.

// Slack is the Socket Mode adapter.
type Slack struct {
	api  *slack.Client
	sock *socketmode.Client
	disp *Dispatcher
	log  *slog.Logger

	// botToken is held as well as handed to the client because uploads are
	// fetched with a plain HTTP GET: Slack serves file bytes from a URL that
	// needs the same bearer credential, and slack-go exposes no method for it.
	botToken string

	router *claude.Router

	// botUserID identifies us in message payloads: it is how our own messages
	// are ignored and how a mention is recognised. Slack does not put the
	// display name in the payload, and it changes whenever somebody renames the
	// app, so the ID is the only stable handle.
	botUserID string

	stop  context.CancelFunc
	done  chan struct{}
	close sync.Once
}

// NewSlack builds the adapter. botToken authorises the Web API calls;
// appToken opens the socket. Both are required — a bot token on its own can
// speak but cannot listen, which is a bot that silently ignores everybody.
func NewSlack(botToken, appToken string, disp *Dispatcher, log *slog.Logger) (*Slack, error) {
	if botToken == "" {
		return nil, fmt.Errorf("slack: bot token is empty")
	}
	if appToken == "" {
		return nil, fmt.Errorf("slack: app-level token is empty; socket mode cannot connect without one")
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		// Worth catching at startup: the two tokens are easy to swap in a .env,
		// and the failure otherwise arrives as an opaque handshake error on a
		// connection that then retries forever.
		return nil, fmt.Errorf("slack: app-level token should start with xapp-; the bot token (xoxb-) goes in the other field")
	}

	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	return &Slack{
		api:      api,
		sock:     socketmode.New(api),
		disp:     disp,
		log:      log,
		botToken: botToken,
		done:     make(chan struct{}),
	}, nil
}

// SetRouter enables agent routing for messages in unbound channels.
func (s *Slack) SetRouter(r *claude.Router) { s.router = r }

// Start connects and begins consuming events.
//
// Unlike Discord there are no commands to register: Slack slash commands are
// declared in the app manifest by whoever installs the app, not created over
// the API at runtime. docs/usage.md carries the manifest to paste in.
func (s *Slack) Start(ctx context.Context) error {
	auth, err := s.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth: %w", err)
	}
	s.botUserID = auth.UserID

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = cancel

	go func() {
		// RunContext reconnects on its own and only returns when the context
		// ends or the credentials are refused.
		if err := s.sock.RunContext(runCtx); err != nil && runCtx.Err() == nil {
			s.log.Error("slack socket mode stopped", "error", err)
		}
	}()
	go s.loop(runCtx)

	s.log.Info("slack adapter connected", "user", auth.User, "team", auth.Team, "bot_user_id", auth.UserID)
	return nil
}

// Close disconnects. Safe to call more than once; the gateway defers it on a
// path that can also be reached by an error return.
func (s *Slack) Close() error {
	s.close.Do(func() {
		if s.stop != nil {
			s.stop()
		}
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			s.log.Warn("slack event loop did not stop in time; leaving it")
		}
	})
	return nil
}

func (s *Slack) loop(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-s.sock.Events:
			if !ok {
				return
			}
			s.dispatchEvent(ctx, evt)
		}
	}
}

// dispatchEvent acknowledges and then hands the work off.
//
// The acknowledgement comes first and the handling runs in its own goroutine,
// always. Slack expects an ack within three seconds and retries what it does
// not get one for, so doing the work first turns one slow request into several
// identical ones. Everything downstream is keyed for idempotency anyway, but a
// retry that is merely harmless still costs a queue slot and a person's
// patience.
func (s *Slack) dispatchEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnected:
		s.log.Info("slack socket connected")

	case socketmode.EventTypeInvalidAuth:
		// Retrying will not fix a rejected credential, and saying so once is
		// more use than a reconnect loop with no explanation.
		s.log.Error("slack refused the app-level token; check the socket-mode credential")

	case socketmode.EventTypeEventsAPI:
		ev, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		s.ack(evt)
		go s.onEventsAPI(ctx, ev)

	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			return
		}
		s.ack(evt)
		go s.onSlashCommand(ctx, cmd)

	case socketmode.EventTypeInteractive:
		cb, ok := evt.Data.(slack.InteractionCallback)
		if !ok {
			return
		}
		s.ack(evt)
		go s.onInteraction(ctx, cb)
	}
}

func (s *Slack) ack(evt socketmode.Event) {
	if evt.Request == nil {
		return
	}
	if err := s.sock.Ack(*evt.Request); err != nil {
		s.log.Warn("failed to acknowledge a slack event", "type", evt.Type, "error", err)
	}
}

func (s *Slack) onEventsAPI(ctx context.Context, ev slackevents.EventsAPIEvent) {
	// Only message events. A mention in a channel the app is in arrives twice —
	// once as message, once as app_mention — and handling both would queue every
	// mentioned request two times. The message carries everything app_mention
	// does, so it is the one kept, and the mention is recognised by scanning for
	// our own user ID in the text.
	inner, ok := ev.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	s.onMessage(ctx, inner)
}

// onMessage turns a message in a bound channel into a queued request.
func (s *Slack) onMessage(ctx context.Context, m *slackevents.MessageEvent) {
	// Our own messages, other bots, and the edited/deleted/joined varieties.
	// file_share is kept because it is how a message with an upload arrives;
	// everything else with a subtype is housekeeping, not a request.
	if m.BotID != "" || m.User == "" || m.User == s.botUserID {
		return
	}
	if m.SubType != "" && m.SubType != "file_share" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// A message in a thread runs as that thread's own conversation; one in the
	// channel itself is the agent's default conversation. The binding is always
	// on the channel — a Slack thread is not a channel and cannot be bound.
	conversationID, err := slackConversation(m.ThreadTimeStamp)
	if err != nil {
		s.log.Warn("ignoring a slack message with an unreadable thread id",
			"channel", m.Channel, "thread", m.ThreadTimeStamp, "error", err)
		return
	}

	agent, err := s.disp.Registry().ByChannel(ctx, core.FormatChannelRef(core.PlatformSlack, m.Channel))
	if err != nil {
		// No binding. Either the router picks an agent, or this is not ours —
		// an unbound channel stays silent rather than guessing.
		s.routeUnbound(ctx, m)
		return
	}

	text := strings.TrimSpace(m.Text)
	mentioned, stripped := s.stripMention(text)
	if mentioned {
		text = stripped
	}
	// The same gate Discord applies, for the same reason: a bound channel would
	// otherwise treat everybody's chatter as billable requests. And the same
	// exemption — a thread the agent is already talking in is an ongoing
	// conversation, and demanding a mention on every line there would push
	// people back to the channel, where a fresh mention starts a *new* thread
	// and loses the context they were building.
	if agent.RequireMention && !mentioned && !s.conversationLive(ctx, agent.ID, conversationID) {
		return
	}
	if text == "" && len(messageFiles(m)) == 0 {
		if mentioned {
			s.say(ctx, core.SlackOrigin(m.Channel, m.ThreadTimeStamp), "👋 What would you like me to do?")
		}
		return
	}

	prompt, staged, err := s.attachFiles(ctx, agent.ID, text, messageFiles(m))
	if err != nil {
		s.say(ctx, core.SlackOrigin(m.Channel, m.ThreadTimeStamp), "⚠️ "+err.Error())
		return
	}

	// Where the reply and the rest of this exchange go. Normally wherever the
	// message was; for a reply-in-thread agent in the channel itself, a thread
	// hanging off this message — which then becomes this exchange's own
	// conversation. Slack needs no API call to open one: replying with a
	// thread timestamp *is* opening it, which is the one place this edge is
	// simpler than Discord's.
	replyThread := m.ThreadTimeStamp
	if agent.ReplyInThread && conversationID == "" {
		if conv, err := slackConversation(m.TimeStamp); err == nil {
			conversationID, replyThread = conv, m.TimeStamp
		}
	}

	// Channel and timestamp together are unique, and Slack redelivers what it
	// thinks went unacknowledged, so this makes a redelivery a no-op rather
	// than a second agent run.
	sub, err := s.disp.SubmitIn(ctx, agent.ID, conversationID, prompt,
		core.SlackOrigin(m.Channel, replyThread), "slack:"+m.Channel+":"+m.TimeStamp, staged)
	if err != nil {
		s.log.Error("failed to queue slack message", "channel", m.Channel, "error", err)
		s.say(ctx, core.SlackOrigin(m.Channel, replyThread), "⚠️ Could not queue that request: "+err.Error())
		return
	}
	if sub.Duplicate {
		return
	}

	// Only when it has to wait. Acknowledging every message would double the
	// bot's chatter for the common case where it starts immediately.
	if sub.QueuePosition > 0 {
		s.say(ctx, core.SlackOrigin(m.Channel, replyThread),
			fmt.Sprintf("⏳ Queued (%d ahead) — turn #%d", sub.QueuePosition, sub.TurnID))
	}
}

// messageFiles is the uploads on a message.
//
// They are read from the nested Message rather than the event's own fields:
// slack-go models the message body once, and its unmarshaller fills that in
// from the top level for an ordinary message and from the nested object for an
// edited one. Reading the event directly would find files on neither.
func messageFiles(m *slackevents.MessageEvent) []slack.File {
	if m.Message == nil {
		return nil
	}
	return m.Message.Files
}

// slackConversation turns a Slack thread timestamp into a conversation ID.
//
// The timestamp is used as-is except for the dot, which becomes a dash: a
// conversation ID ends up in a Temporal workflow ID and in a directory name
// under the agent's workspace, and keeping it to digits and dashes means
// neither has to care. An empty timestamp is the channel itself, which is the
// agent's default conversation.
//
// The shape is checked rather than assumed. Slack has only ever sent
// "seconds.microseconds", but this value is concatenated into a filesystem
// path, and a validation that exists is worth more than one that is currently
// unnecessary.
func slackConversation(threadTS string) (string, error) {
	if threadTS == "" {
		return "", nil
	}
	for _, r := range threadTS {
		if (r < '0' || r > '9') && r != '.' {
			return "", fmt.Errorf("slack timestamp %q contains %q", threadTS, r)
		}
	}
	return strings.ReplaceAll(threadTS, ".", "-"), nil
}

// conversationLive reports whether the agent already has a turn in this
// conversation — an ongoing thread rather than an untouched one.
func (s *Slack) conversationLive(ctx context.Context, agentID, conversationID string) bool {
	if conversationID == "" {
		return false
	}
	st, err := s.disp.Store(ctx, agentID)
	if err != nil {
		return false
	}
	turns, err := st.RecentTurnsIn(ctx, conversationID, 1)
	return err == nil && len(turns) > 0
}

// stripMention reports whether the bot was addressed, and returns the message
// with the mention removed.
//
// Slack renders a mention as <@U0123ABCD> in the text. The ID is matched rather
// than any rendered name, which is not in the payload and changes whenever the
// app is renamed.
func (s *Slack) stripMention(text string) (bool, string) {
	if s.botUserID == "" {
		return false, text
	}
	form := "<@" + s.botUserID + ">"
	if !strings.Contains(text, form) {
		return false, text
	}
	text = strings.ReplaceAll(text, form, " ")
	return true, strings.TrimSpace(strings.Join(strings.Fields(text), " "))
}

// routeUnbound handles a message in a channel bound to no agent.
//
// Already on its own goroutine — every event handler is — but it still checks
// the channel is one routing was switched on for. Unbound channels carry
// ordinary conversation, and an LLM call per message in them is a bill nobody
// asked for.
func (s *Slack) routeUnbound(ctx context.Context, m *slackevents.MessageEvent) {
	if s.router == nil || !s.disp.Config().Router.RoutesChannel(m.Channel) {
		return
	}

	text := strings.TrimSpace(m.Text)
	if _, stripped := s.stripMention(text); stripped != "" {
		text = stripped
	}
	if text == "" {
		return
	}

	cfg := s.disp.Config()
	summaries := make([]claude.AgentSummary, 0, len(cfg.AllAgents()))
	for _, a := range cfg.AllAgents() {
		summaries = append(summaries, claude.AgentSummary{ID: a.ID, Description: a.Description})
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Router.Timeout+15*time.Second)
	defer cancel()

	decision, err := s.router.Route(ctx, text, summaries)
	if err != nil {
		// Silence on failure, deliberately: an error reply to every message in
		// a chatty unbound channel is worse than doing nothing.
		s.log.Warn("routing failed", "channel", m.Channel, "error", err)
		return
	}

	switch decision.Action {
	case claude.RouteIgnore:
		return
	case claude.RouteClarify:
		s.say(ctx, core.SlackOrigin(m.Channel, m.ThreadTimeStamp),
			"🤔 I am not sure which agent should take that. Try `/ask <agent> <prompt>`, or `/agents` to see the options.")
		return
	}

	sub, err := s.disp.Submit(ctx, decision.AgentID, text,
		core.SlackOrigin(m.Channel, m.ThreadTimeStamp), "slack:"+m.Channel+":"+m.TimeStamp)
	if err != nil {
		s.log.Error("failed to queue routed slack message", "agent", decision.AgentID, "error", err)
		return
	}
	// Routing is a guess, so it says which agent it picked; otherwise a wrong
	// choice is invisible until the answer comes back off-target.
	s.say(ctx, core.SlackOrigin(m.Channel, m.ThreadTimeStamp),
		fmt.Sprintf("📨 Routed to `%s` — turn #%d.", decision.AgentID, sub.TurnID))
}

// say posts into a channel or thread, splitting to Slack's limit.
func (s *Slack) say(ctx context.Context, to core.Origin, text string) {
	for _, part := range chunk(text, core.SlackMaxMessage-10) {
		opts := []slack.MsgOption{slack.MsgOptionText(part, false)}
		if to.MessageID != "" {
			opts = append(opts, slack.MsgOptionTS(to.MessageID))
		}
		if _, _, err := s.api.PostMessageContext(ctx, to.ChannelID, opts...); err != nil {
			s.log.Warn("failed to send slack message", "channel", to.ChannelID, "error", err)
			return
		}
	}
}

// Sender exposes the live connection to the HTTP API, so an agent can speak
// outside a turn without a second connection being opened.
func (s *Slack) Sender() MessageSender { return slackSender{s} }

type slackSender struct{ s *Slack }

func (w slackSender) SendMessage(to core.Origin, text string) error {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if to.MessageID != "" {
		opts = append(opts, slack.MsgOptionTS(to.MessageID))
	}
	_, _, err := w.s.api.PostMessage(to.ChannelID, opts...)
	return err
}

// SendFiles uploads each file into the channel, with text as the comment on
// the first.
//
// Slack takes one file per call, unlike Discord's single multi-file message, so
// a set arrives as several uploads. The readers are closed here whatever
// happens: this is where ownership ends, and a failed upload would otherwise
// leak a descriptor per attempt.
func (w slackSender) SendFiles(to core.Origin, text string, files []OutFile) error {
	defer closeOutbound(files)

	for i, f := range files {
		params := slack.UploadFileParameters{
			Reader:          f.Body,
			Filename:        f.Name,
			Title:           f.Name,
			FileSize:        int(f.Size),
			Channel:         to.ChannelID,
			ThreadTimestamp: to.MessageID,
		}
		// The words go with the first upload; repeating them on each would say
		// the same thing three times for one report split across three files.
		if i == 0 {
			params.InitialComment = text
		}
		if _, err := w.s.api.UploadFile(params); err != nil {
			return fmt.Errorf("upload %s to slack: %w", f.Name, err)
		}
	}
	return nil
}

// SlackRESTSender returns a Web-API-only client for the worker's delivery
// activity. It opens no socket, so it cannot double-consume events.
func SlackRESTSender(botToken string) *SlackDelivery {
	return &SlackDelivery{api: slack.New(botToken)}
}

// SlackDelivery adapts slack-go to the narrow interface the delivery activity
// declares, so that package depends on roundclaw's own terms rather than on
// this library's option builders.
type SlackDelivery struct{ api *slack.Client }

func (d *SlackDelivery) PostMessage(ctx context.Context, channelID, threadTS, text string) error {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := d.api.PostMessageContext(ctx, channelID, opts...)
	return err
}
