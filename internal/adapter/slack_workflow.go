package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// handleWorkflow is the other half of /status: SQLite says what the agent has
// done, this says whether its Temporal execution is actually alive and whether
// an activity is quietly retrying.
func (s *Slack) handleWorkflow(ctx context.Context, cmd slack.SlashCommand, agentID string) {
	info, err := s.disp.Workflow(ctx, agentID)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "*%s* — workflow `%s`\n", info.AgentID, info.WorkflowID)

	if info.Unavailable != "" {
		// Usually means the agent has never run, which is not a fault.
		b.WriteString("_No Temporal execution._ " + info.Unavailable)
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, b.String())
		return
	}

	fmt.Fprintf(&b, "status `%s` · %d queued · %d turns · history %d\n",
		info.Status, info.QueueLength, info.TurnCount, info.HistoryLength)
	if info.StartedAt != "" {
		fmt.Fprintf(&b, "started %s\n", info.StartedAt)
	}

	if info.ActivityType != "" {
		fmt.Fprintf(&b, "\nrunning `%s` (%s)", info.ActivityType, info.ActivityState)
		if info.ActivityAttempt > 1 {
			// The number that explains a "stuck" agent: the transcript looks
			// idle while the activity keeps failing and being retried.
			fmt.Fprintf(&b, " — *attempt %d*, it is failing and retrying", info.ActivityAttempt)
		}
		b.WriteString("\n")
		if info.LastFailure != "" {
			fmt.Fprintf(&b, "last error: `%s`\n", truncate(info.LastFailure, 300))
		}
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, b.String())
}

// onInteraction routes everything that is neither a message nor a slash
// command: a submitted modal, or a button on a proposal.
func (s *Slack) onInteraction(ctx context.Context, cb slack.InteractionCallback) {
	switch cb.Type {
	case slack.InteractionTypeViewSubmission:
		// The permission check runs again here rather than only on the command
		// that opened the form. A trigger ID is short-lived but a view is a
		// separate interaction, and the gate has to sit where the change is
		// actually applied.
		if !s.permittedView(cb) {
			channelID, _ := splitMetadata(cb.View.PrivateMetadata)
			s.ephemeral(ctx, channelID, cb.User.ID,
				"⛔ You are not on this bot's allow-list, so that was not saved.")
			return
		}
		switch cb.View.CallbackID {
		case viewAgentCreate:
			s.handleAgentView(ctx, cb, false)
		case viewAgentEdit:
			s.handleAgentView(ctx, cb, true)
		case viewScheduleCreate:
			s.handleScheduleView(ctx, cb)
		case viewSteer:
			s.handleSteerView(ctx, cb)
		}

	case slack.InteractionTypeBlockActions:
		s.handleProposalButton(ctx, cb)

	case slack.InteractionTypeMessageAction:
		// Stop and steer, scoped to the thread the message is in — the only
		// interaction Slack tells us a thread from. See slack_shortcuts.go.
		s.onMessageShortcut(ctx, cb)
	}
}

// permittedView gates a submitted form. None of these forms is a read: each
// creates or changes fleet state, or interrupts work in progress.
func (s *Slack) permittedView(cb slack.InteractionCallback) bool {
	switch cb.View.CallbackID {
	case viewScheduleCreate:
		return s.disp.Config().Slack.PermitsCommand("schedule", cb.User.ID)
	case viewSteer:
		return s.disp.Config().Slack.PermitsCommand("steer", cb.User.ID)
	default:
		return s.disp.Config().Slack.PermitsCommand("agent", cb.User.ID)
	}
}
