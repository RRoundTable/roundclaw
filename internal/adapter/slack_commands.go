package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/roundtable/roundclaw/internal/core"
)

// Slash commands.
//
// The set matches Discord's, because a capability that exists in one chat tool
// and not the other splits the fleet into two systems sharing a queue. How they
// are *typed* differs, and has to: Slack gives a command one free-text string,
// where Discord gives named, typed, autocompleted options. So the subcommand
// and its arguments are parsed out of that string here, and anything that would
// have been a picker is a name typed in full.
//
//	/ask        <agent> <prompt>
//	/agents
//	/agent      create | edit <id> | show <id> | enable <id> | disable <id> | delete <id>
//	/schedule   create <agent> | list | show <id> | pause <id> | resume <id> | delete <id>
//	/proposals
//	/status     [agent]
//	/workflow   [agent]
//	/stop       [agent]
//	/steer      <instruction>
//
// Slack cannot pre-filter who invokes a command the way Discord's
// default_member_permissions does, so roundclaw's allow-list is the only gate
// on this edge rather than the second of two. slack.allowed_users is what a
// deployment that cares about who spends tokens has to set.

func (s *Slack) onSlashCommand(ctx context.Context, cmd slack.SlashCommand) {
	name := strings.TrimPrefix(cmd.Command, "/")

	if !s.disp.Config().Slack.PermitsCommand(name, cmd.UserID) {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID,
			"⛔ You are not on this bot's allow-list. Ask an administrator to add your Slack user ID.")
		s.log.Info("refused a slack command from an unlisted caller",
			"command", name, "user", cmd.UserID)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	switch name {
	case "agents":
		// Needs no target and must work in an unbound channel, since that is
		// exactly where somebody is trying to find out what to call.
		s.handleAgents(ctx, cmd)
		return
	case "proposals":
		// Fleet-wide: a queue of changes waiting on a person, not something one
		// agent owns, so it takes no agent argument either.
		s.handleProposals(ctx, cmd)
		return
	case "agent":
		s.handleAgentCommand(ctx, cmd)
		return
	case "schedule":
		s.handleScheduleCommand(ctx, cmd)
		return
	}

	// The rest act on an agent: the one named, or the channel's.
	//
	// Always on that agent's default conversation, never on a thread. Slack
	// does not tell a slash command which thread it was typed in — the payload
	// has no thread timestamp at all, unlike a message — so a command run in a
	// thread cannot be honoured there, and guessing would stop the wrong line
	// of work. Thread-scoped stop and steer are message shortcuts instead
	// (slack_shortcuts.go), which do carry the thread.
	arg := strings.TrimSpace(cmd.Text)
	const conversationID = ""

	switch name {
	case "ask":
		// /ask always names its agent, and always targets that agent's default
		// conversation regardless of where it was typed — same as Discord.
		agentID, prompt := cutWord(arg)
		if agentID == "" || prompt == "" {
			s.ephemeral(ctx, cmd.ChannelID, cmd.UserID,
				"Usage: `/ask <agent> <what you want done>`. Run `/agents` to see the options.")
			return
		}
		s.handleAsk(ctx, cmd, agentID, prompt)

	case "status", "workflow", "stop", "steer":
		explicit := ""
		instruction := ""
		if name == "steer" {
			// A steer is all instruction: an agent name in front of free text
			// could not be told from the first word of the instruction, so
			// steering targets the channel's agent.
			instruction = arg
			if instruction == "" {
				s.ephemeral(ctx, cmd.ChannelID, cmd.UserID,
					"Usage: `/steer <what the agent should do instead>`.")
				return
			}
		} else {
			explicit = arg
		}

		agent, err := ResolveAgent(ctx, s.disp.Registry(), explicit,
			core.FormatChannelRef(core.PlatformSlack, cmd.ChannelID))
		if err != nil {
			s.ephemeral(ctx, cmd.ChannelID, cmd.UserID,
				"⚠️ "+err.Error()+"\nRun `/agents` to see what is available.")
			return
		}

		switch name {
		case "status":
			s.handleStatus(ctx, cmd, agent.ID)
		case "workflow":
			s.handleWorkflow(ctx, cmd, agent.ID)
		case "stop":
			s.handleStop(ctx, cmd, agent.ID, conversationID)
		case "steer":
			s.handleSteer(ctx, cmd, agent.ID, conversationID, instruction)
		}

	default:
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "Unknown command `/"+name+"`.")
	}
}

func (s *Slack) handleAgents(ctx context.Context, cmd slack.SlashCommand) {
	agents, err := s.disp.Registry().List(ctx)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not read the agent registry: "+err.Error())
		return
	}
	if len(agents) == 0 {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "No agents are registered yet. Create one with `/agent create`.")
		return
	}

	bound, _ := s.disp.Registry().ByChannel(ctx, core.FormatChannelRef(core.PlatformSlack, cmd.ChannelID))

	var b strings.Builder
	b.WriteString("*Agents*\n")
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
	b.WriteString("\nCall one with `/ask <agent> <prompt>`.")
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, b.String())
}

// handleAsk is the direct-invocation path.
func (s *Slack) handleAsk(ctx context.Context, cmd slack.SlashCommand, agentID, prompt string) {
	// The trigger ID is unique per invocation, so a Slack retry lands on the
	// original turn instead of starting a second one.
	sub, err := s.disp.SubmitIn(ctx, agentID, "", prompt,
		core.SlackOrigin(cmd.ChannelID, ""), "slack-ask:"+cmd.TriggerID, nil)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not queue that: "+err.Error())
		return
	}

	msg := fmt.Sprintf("📨 Sent to `%s` — turn #%d.", agentID, sub.TurnID)
	if sub.QueuePosition > 0 {
		msg += fmt.Sprintf(" %d request(s) ahead of it.", sub.QueuePosition)
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, msg)
}

// handleStatus reads SQLite directly. No Temporal round trip and no model:
// this has to stay fast and available precisely when the agent is wedged, which
// is when somebody asks.
func (s *Slack) handleStatus(ctx context.Context, cmd slack.SlashCommand, agentID string) {
	report, err := s.disp.Status(ctx, agentID, statusTailLines)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, slackMarkup(formatStatus(report)))
}

func (s *Slack) handleStop(ctx context.Context, cmd slack.SlashCommand, agentID, conversationID string) {
	// One conversation, never all of them. From a slash command that is always
	// the agent's default one; the thread-scoped version is the message
	// shortcut, which is the only interaction Slack tells us a thread from.
	if err := s.disp.StopIn(ctx, agentID, conversationID, "stopped from slack"); err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not stop: "+err.Error())
		return
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "🛑 Stopping the current turn and clearing the queue.")
}

func (s *Slack) handleSteer(ctx context.Context, cmd slack.SlashCommand, agentID, conversationID, instruction string) {
	sub, err := s.disp.SteerIn(ctx, agentID, conversationID, instruction,
		core.SlackOrigin(cmd.ChannelID, ""), "slack-steer:"+cmd.TriggerID, nil)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not steer: "+err.Error())
		return
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, fmt.Sprintf(
		"↪️ Redirecting — turn #%d. Context is preserved; the interrupted turn's unfinished work is not.", sub.TurnID))
}

// ephemeral answers the person who ran the command, visible to them alone.
//
// Every command answers this way, where Discord's read-only ones answer in the
// channel. Slack has no deferral: an interaction is acknowledged within three
// seconds or retried, and the answer arrives separately either way — so there
// is one path here rather than Discord's two, and making it ephemeral keeps a
// channel from filling with other people's status checks.
func (s *Slack) ephemeral(ctx context.Context, channelID, userID, text string) {
	for _, part := range chunk(text, core.SlackMaxMessage-10) {
		_, err := s.api.PostEphemeralContext(ctx, channelID, userID, slack.MsgOptionText(part, false))
		if err != nil {
			s.log.Warn("failed to send an ephemeral slack message",
				"channel", channelID, "user", userID, "error", err)
			return
		}
	}
}

// cutWord splits the first whitespace-separated word off a command's argument
// string, which is how a subcommand or an agent name is read out of the one
// free-text field Slack gives a slash command.
func cutWord(s string) (head, rest string) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}

// slackMarkup converts the Discord-flavoured formatting the shared formatters
// emit into Slack's.
//
// The two are close enough that keeping one set of formatters and translating
// at the edge is cheaper than a second copy of every report — and a second copy
// is how the two chat tools would start describing the same agent differently.
// Only bold differs in a way that matters: Slack reads *one* asterisk as bold
// and renders **two** literally.
func slackMarkup(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.Index(s, "**")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("*")
		s = s[i+2:]
	}
}
