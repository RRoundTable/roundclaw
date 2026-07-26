package adapter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Approving a change from Discord.
//
//	/proposals            what is waiting on a decision
//	[Approve] [Reject]    the decision itself
//
// This is where the loop meets a person. A curator agent reviews the fleet on a
// schedule and writes down what it would change; nothing happens until somebody
// here presses a button.
//
// The buttons carry the proposal id rather than an index into a list, because
// the list is a snapshot: by the time anyone clicks, another proposal may have
// been filed or decided, and "approve the second one" would then mean something
// else than it did when it was rendered.

const (
	buttonApprove = "proposal:approve:"
	buttonReject  = "proposal:reject:"
	// Discord allows five action rows per message; one proposal per message with
	// its own pair of buttons is clearer than a wall of numbered buttons, and
	// this bounds how many go out at once.
	maxProposalsShown = 5
)

func (d *Discord) handleProposals(i *discordgo.InteractionCreate) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pending, err := d.disp.Registry().ListProposals(ctx, registry.ProposalPending, "", maxProposalsShown+1)
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}
	if len(pending) == 0 {
		d.followUp(i, "✅ Nothing waiting on a decision.")
		return
	}

	more := ""
	if len(pending) > maxProposalsShown {
		pending = pending[:maxProposalsShown]
		more = fmt.Sprintf("\n_Showing the %d oldest-pending; run this again after deciding._", maxProposalsShown)
	}
	d.followUp(i, fmt.Sprintf("**%d proposal(s) waiting.** Each one below changes the fleet only if you approve it.%s",
		len(pending), more))

	for _, p := range pending {
		id := strconv.FormatInt(p.ID, 10)
		_, err := d.session.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: ProposalSummary(p) + "\n" + proposalDetail(p),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.Button{
						CustomID: buttonApprove + id,
						Label:    "Approve",
						Style:    discordgo.SuccessButton,
					},
					discordgo.Button{
						CustomID: buttonReject + id,
						Label:    "Reject",
						Style:    discordgo.SecondaryButton,
					},
				}},
			},
		})
		if err != nil {
			d.log.Error("could not post a proposal", "proposal", p.ID, "error", err)
		}
	}
}

// proposalDetail renders what the change actually does, so the decision is made
// on the change rather than on the rationale alone — a rationale is written by
// the thing asking for approval.
func proposalDetail(p registry.Proposal) string {
	payload := strings.TrimSpace(string(p.Payload))
	if payload == "" || payload == "{}" {
		return "_no payload; the kind and target say everything it does_"
	}
	// Discord caps a message at 2000 characters and this is one of several
	// fields, so a long persona is cut here rather than losing the buttons.
	if len(payload) > 900 {
		payload = payload[:900] + "…\n(truncated — read it in full with `roundclaw proposal show " +
			strconv.FormatInt(p.ID, 10) + "`)"
	}
	return "```json\n" + payload + "\n```"
}

// handleProposalButton applies or rejects a proposal from a button press.
//
// The allow-list is checked here and not only on the command that produced the
// buttons. A message with buttons is visible to everyone in the channel, so
// without this anyone who can see it could approve a change to the fleet.
func (d *Discord) handleProposalButton(i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID

	var (
		raw     string
		approve bool
	)
	switch {
	case strings.HasPrefix(customID, buttonApprove):
		raw, approve = strings.TrimPrefix(customID, buttonApprove), true
	case strings.HasPrefix(customID, buttonReject):
		raw = strings.TrimPrefix(customID, buttonReject)
	default:
		return
	}

	if !d.permitted(i, "proposals") {
		d.respondNow(i, "⛔ You are not on this bot's allow-list, so you cannot decide proposals.")
		d.log.Info("refused a proposal decision from an unlisted caller",
			"proposal", raw, "user", interactionUser(i))
		return
	}

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		d.respondNow(i, "⚠️ That button carries an unreadable proposal id.")
		return
	}

	d.defer_(i)
	// Generous: applying a delete stops every conversation of the agent first,
	// which waits on Temporal.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	by := "discord:" + interactionUser(i)

	if !approve {
		if _, err := d.disp.RejectProposal(ctx, id, by, ""); err != nil {
			d.followUp(i, decisionError(id, err))
			return
		}
		d.log.Info("proposal rejected from discord", "proposal", id, "by", by)
		d.followUp(i, fmt.Sprintf("🚫 Proposal #%d rejected. Nothing was changed.", id))
		return
	}

	decided, version, err := d.disp.ApproveProposal(ctx, id, by, "")
	if err != nil {
		d.followUp(i, decisionError(id, err))
		return
	}

	msg := fmt.Sprintf("✅ Proposal #%d applied — `%s` on `%s`.", id, decided.Kind, decided.Target)
	if version > 0 {
		msg += fmt.Sprintf("\nThis is now version %d, live on the next turn.", version)
	}
	if undo := UndoHint(decided, version); undo != "" {
		msg += "\nTo undo: `" + undo + "`"
	}
	d.log.Info("proposal applied from discord", "proposal", id, "by", by, "version", version)
	d.followUp(i, msg)
}

// decisionError explains a refusal in the terms of the person who clicked,
// rather than handing them a wrapped Go error.
func decisionError(id int64, err error) string {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return fmt.Sprintf("⚠️ Proposal #%d no longer exists.", id)
	case errors.Is(err, registry.ErrConflict):
		return fmt.Sprintf("⚠️ %v — nothing was done twice.", err)
	default:
		return fmt.Sprintf("⚠️ Proposal #%d could not be applied: %v\nIt is recorded as failed, not pending, "+
			"so nobody approves the same broken change again.", id, err)
	}
}
