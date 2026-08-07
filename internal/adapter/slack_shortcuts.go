package adapter

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"

	"github.com/roundtable/roundclaw/internal/core"
)

// Message shortcuts — how a thread is stopped or steered from Slack.
//
// This exists because a Slack slash command carries no thread. Its payload has
// a channel and a user and nothing that says which thread it was typed in,
// where a message carries thread_ts. So /stop and /steer can only ever act on
// an agent's default conversation, and on Discord — where a thread is a channel
// and the command knows it — they act on the thread.
//
// That would be a capability missing from one chat tool, which the Channels
// spec says cannot happen. A message shortcut closes it: it is invoked from a
// specific message's menu, so Slack does tell us the thread, and it is the
// native way to act on one. Same effect, different widget.
//
//	Stop this line of work    → stops the thread the message is in
//	Steer this line of work   → opens a box, then interrupts that thread
//
// Both are declared in the app manifest with these callback IDs; see
// docs/usage.md.

const (
	shortcutStop  = "stop_here"
	shortcutSteer = "steer_here"

	viewSteer = "steer-submit"

	fieldInstruction = "instruction"
)

// onMessageShortcut handles an action invoked from a message's menu.
func (s *Slack) onMessageShortcut(ctx context.Context, cb slack.InteractionCallback) {
	command := "stop"
	if cb.CallbackID == shortcutSteer {
		command = "steer"
	}
	if !s.disp.Config().Slack.PermitsCommand(command, cb.User.ID) {
		s.ephemeral(ctx, cb.Channel.ID, cb.User.ID,
			"⛔ You are not on this bot's allow-list. Ask an administrator to add your Slack user ID.")
		return
	}

	// The thread the message belongs to, or — when the shortcut is used on a
	// message that started one — that message itself. A message sitting in the
	// channel with no thread under it is the default conversation, which is the
	// same answer the slash command gives.
	threadTS := cb.Message.ThreadTimestamp
	if threadTS == "" && cb.Message.Timestamp != "" {
		// Only if that message actually heads a thread. Otherwise stopping from
		// a stray message in the channel would invent a conversation that has
		// never run and report success for stopping nothing.
		if cb.Message.ReplyCount > 0 {
			threadTS = cb.Message.Timestamp
		}
	}

	conversationID, err := slackConversation(threadTS)
	if err != nil {
		s.ephemeral(ctx, cb.Channel.ID, cb.User.ID, "⚠️ "+err.Error())
		return
	}

	agent, err := ResolveAgent(ctx, s.disp.Registry(), "",
		core.FormatChannelRef(core.PlatformSlack, cb.Channel.ID))
	if err != nil {
		s.ephemeral(ctx, cb.Channel.ID, cb.User.ID,
			"⚠️ "+err.Error()+"\nRun `/agents` to see what is available.")
		return
	}

	switch cb.CallbackID {
	case shortcutStop:
		if err := s.disp.StopIn(ctx, agent.ID, conversationID, "stopped from slack"); err != nil {
			s.ephemeral(ctx, cb.Channel.ID, cb.User.ID, "⚠️ Could not stop: "+err.Error())
			return
		}
		s.ephemeral(ctx, cb.Channel.ID, cb.User.ID, fmt.Sprintf(
			"🛑 Stopping `%s` here%s, and clearing what was queued behind it.",
			agent.ID, threadLabel(threadTS)))

	case shortcutSteer:
		// A steer needs words, and a shortcut carries none — so it opens a box.
		// The thread and the agent ride in the view's metadata, because a
		// submission is a separate interaction that remembers neither.
		view := slack.ModalViewRequest{
			Type:            slack.VTModal,
			Title:           plainText("Steer " + truncateRunes(agent.ID, 15)),
			Submit:          plainText("Send"),
			Close:           plainText("Cancel"),
			CallbackID:      viewSteer,
			PrivateMetadata: cb.Channel.ID + "\n" + agent.ID + "\n" + threadTS,
			Blocks: slack.Blocks{BlockSet: []slack.Block{
				textInput(fieldInstruction, "What should it do instead?",
					"the interrupted turn's unfinished work is lost; its context is not",
					"", true, 2000, true),
			}},
		}
		if _, err := s.api.OpenViewContext(ctx, cb.TriggerID, view); err != nil {
			s.log.Warn("failed to open the steer form", "error", err)
			s.ephemeral(ctx, cb.Channel.ID, cb.User.ID, "⚠️ Could not open the form: "+err.Error())
		}
	}
}

// handleSteerView applies a submitted steer.
func (s *Slack) handleSteerView(ctx context.Context, cb slack.InteractionCallback) {
	channelID, rest := splitMetadata(cb.View.PrivateMetadata)
	agentID, threadTS := splitMetadata(rest)

	instruction := viewFields(cb.View)[fieldInstruction]
	conversationID, err := slackConversation(threadTS)
	if err != nil {
		s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ "+err.Error())
		return
	}

	// Keyed on the view rather than the moment, so a Slack retry cannot
	// interrupt the agent twice with the same instruction.
	sub, err := s.disp.SteerIn(ctx, agentID, conversationID, instruction,
		core.SlackOrigin(channelID, threadTS), "slack-steer:"+cb.View.ID, nil)
	if err != nil {
		s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ Could not steer: "+err.Error())
		return
	}
	s.ephemeral(ctx, channelID, cb.User.ID, fmt.Sprintf(
		"↪️ Redirecting `%s`%s — turn #%d. Context is preserved; the interrupted turn's unfinished work is not.",
		agentID, threadLabel(threadTS), sub.TurnID))
}

func threadLabel(threadTS string) string {
	if threadTS == "" {
		return " (its main line of work)"
	}
	return " (this thread)"
}
