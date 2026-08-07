package adapter

import (
	"context"
	"fmt"
	"strconv"

	"github.com/slack-go/slack"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
)

// Approving a change from Slack.
//
//	/proposals            what is waiting on a decision
//	[Approve] [Reject]    the decision itself
//
// The buttons carry the proposal id rather than an index into a list, for the
// reason the Discord edge gives: the list is a snapshot, and by the time anyone
// clicks, another proposal may have been filed or decided.
//
// These messages are posted into the channel rather than shown to the person
// who ran the command alone. A decision that changes the fleet should be
// visible to the room it was made in, and an ephemeral message cannot be
// updated by anyone else who is looking at it.

const (
	actionApprove = "proposal-approve"
	actionReject  = "proposal-reject"
)

func (s *Slack) handleProposals(ctx context.Context, cmd slack.SlashCommand) {
	pending, err := s.disp.Registry().ListProposals(ctx, registry.ProposalPending, "", maxProposalsShown+1)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}
	if len(pending) == 0 {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "✅ Nothing waiting on a decision.")
		return
	}

	more := ""
	if len(pending) > maxProposalsShown {
		pending = pending[:maxProposalsShown]
		more = fmt.Sprintf("\n_Showing the %d oldest-pending; run this again after deciding._", maxProposalsShown)
	}

	header := fmt.Sprintf("*%d proposal(s) waiting.* Each one below changes the fleet only if you approve it.%s",
		len(pending), more)
	if _, _, err := s.api.PostMessageContext(ctx, cmd.ChannelID,
		slack.MsgOptionText(header, false)); err != nil {
		s.log.Error("could not post the proposal header", "error", err)
		return
	}

	for _, p := range pending {
		id := strconv.FormatInt(p.ID, 10)
		body := slackMarkup(ProposalSummary(p)) + "\n" + proposalDetail(p)

		blocks := []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, truncateRunes(body, 2900), false, false),
				nil, nil,
			),
			slack.NewActionBlock("",
				withStyle(slack.NewButtonBlockElement(actionApprove, id, plainText("Approve")), slack.StylePrimary),
				slack.NewButtonBlockElement(actionReject, id, plainText("Reject")),
			),
		}

		if _, _, err := s.api.PostMessageContext(ctx, cmd.ChannelID,
			slack.MsgOptionBlocks(blocks...),
			// The fallback text is what a notification shows and what a client
			// that cannot render blocks falls back to; without it the proposal
			// arrives as an empty message.
			slack.MsgOptionText(ProposalSummary(p), false),
		); err != nil {
			s.log.Error("could not post a proposal", "proposal", p.ID, "error", err)
		}
	}
}

// handleProposalButton applies or rejects a proposal from a button press.
//
// The allow-list is checked here and not only on the command that produced the
// buttons. A message with buttons is visible to everyone in the channel, so
// without this anyone who can see it could change the fleet.
func (s *Slack) handleProposalButton(ctx context.Context, cb slack.InteractionCallback) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	action := cb.ActionCallback.BlockActions[0]

	var approve bool
	switch action.ActionID {
	case actionApprove:
		approve = true
	case actionReject:
	default:
		return
	}

	channelID := cb.Channel.ID
	if !s.disp.Config().Slack.PermitsCommand("proposals", cb.User.ID) {
		s.ephemeral(ctx, channelID, cb.User.ID,
			"⛔ You are not on this bot's allow-list, so you cannot decide proposals.")
		s.log.Info("refused a proposal decision from an unlisted caller",
			"proposal", action.Value, "user", cb.User.ID)
		return
	}

	id, err := strconv.ParseInt(action.Value, 10, 64)
	if err != nil {
		s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ That button carries an unreadable proposal id.")
		return
	}

	by := "slack:" + slackUser(cb)

	if !approve {
		if _, err := s.disp.RejectProposal(ctx, id, by, ""); err != nil {
			s.sayPlain(ctx, channelID, decisionError(id, err))
			return
		}
		s.log.Info("proposal rejected from slack", "proposal", id, "by", by)
		s.sayPlain(ctx, channelID, fmt.Sprintf("🚫 Proposal #%d rejected by <@%s>. Nothing was changed.", id, cb.User.ID))
		return
	}

	decided, version, err := s.disp.ApproveProposal(ctx, id, by, "")
	if err != nil {
		s.sayPlain(ctx, channelID, decisionError(id, err))
		return
	}

	msg := fmt.Sprintf("✅ Proposal #%d applied by <@%s> — `%s` on `%s`.", id, cb.User.ID, decided.Kind, decided.Target)
	if version > 0 {
		msg += fmt.Sprintf("\nThis is now version %d, live on the next turn.", version)
	}
	if undo := UndoHint(decided, version); undo != "" {
		msg += "\nTo undo: `" + undo + "`"
	}
	s.log.Info("proposal applied from slack", "proposal", id, "by", by, "version", version)
	s.sayPlain(ctx, channelID, msg)
}

// sayPlain posts into a channel with no thread. The outcome of a decision
// belongs where the buttons were, in front of everyone who could have pressed
// them.
func (s *Slack) sayPlain(ctx context.Context, channelID, text string) {
	for _, part := range chunk(slackMarkup(text), core.SlackMaxMessage-10) {
		if _, _, err := s.api.PostMessageContext(ctx, channelID,
			slack.MsgOptionText(part, false)); err != nil {
			s.log.Warn("failed to send slack message", "channel", channelID, "error", err)
			return
		}
	}
}

// withStyle sets a button's colour. slack-go's constructor takes no style, and
// an approve button that looks like a reject button is worth one helper.
func withStyle(b *slack.ButtonBlockElement, style slack.Style) *slack.ButtonBlockElement {
	b.Style = style
	return b
}
