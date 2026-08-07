package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
)

// Schedule management from Slack.
//
// Creating opens a modal for the reason agents do: a schedule has more fields
// than a slash command reads comfortably, and the prompt is free-form prose.
// The agent it belongs to comes from the command and rides in the view's
// private metadata, and the channel it reports to defaults to wherever the
// command was run — which is where a recurring report almost always wants to
// land anyway.

const viewScheduleCreate = "schedule-create"

func (s *Slack) handleScheduleCommand(ctx context.Context, cmd slack.SlashCommand) {
	sub, target := cutWord(cmd.Text)

	switch sub {
	case "create":
		if target == "" {
			s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "Usage: `/schedule create <agent>`.")
			return
		}
		if _, err := s.disp.Registry().Get(ctx, target); err != nil {
			// Checked before the form opens: filling one in and being told the
			// agent does not exist is the worst moment to find out.
			s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
			return
		}
		s.openScheduleForm(ctx, cmd, target)
	case "list":
		s.listSchedules(ctx, cmd)
	case "show":
		s.showSchedule(ctx, cmd, target)
	case "pause":
		s.setSchedulePaused(ctx, cmd, target, true)
	case "resume":
		s.setSchedulePaused(ctx, cmd, target, false)
	case "delete":
		s.deleteSchedule(ctx, cmd, target)
	default:
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID,
			"Use `/schedule create <agent>`, `list`, `show <id>`, `pause <id>`, `resume <id>` or `delete <id>`.")
	}
}

func (s *Slack) openScheduleForm(ctx context.Context, cmd slack.SlashCommand, agentID string) {
	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           plainText(truncateRunes("Schedule for "+agentID, 24)),
		Submit:          plainText("Save"),
		Close:           plainText("Cancel"),
		CallbackID:      viewScheduleCreate,
		PrivateMetadata: cmd.ChannelID + "\n" + agentID,
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			textInput(fieldScheduleID, "Schedule ID", "letters, digits, - and _", "", false, 64, true),
			textInput(fieldCron, "Cron", "0 9 * * 1-5  (weekdays at 09:00)", "", false, 100, true),
			// Defaulted rather than required: an empty box would silently mean
			// UTC, and a daily report firing nine hours early is a bad surprise.
			textInput(fieldTimezone, "Timezone", "Asia/Seoul", "Asia/Seoul", false, 64, false),
			textInput(fieldPrompt, "What to run each time", "", "", true, 3000, true),
			textInput(fieldSuppress, "Skip posting when the result contains",
				"optional — e.g. nothing to report", "", false, 200, false),
		}},
	}

	if _, err := s.api.OpenViewContext(ctx, cmd.TriggerID, view); err != nil {
		s.log.Warn("failed to open the schedule form", "error", err)
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not open the form: "+err.Error())
	}
}

// handleScheduleView applies a submitted schedule form.
func (s *Slack) handleScheduleView(ctx context.Context, cb slack.InteractionCallback) {
	channelID, agentID := splitMetadata(cb.View.PrivateMetadata)
	fields := viewFields(cb.View)

	sched := registry.Schedule{
		ID:         strings.TrimSpace(fields[fieldScheduleID]),
		AgentID:    agentID,
		Cron:       strings.TrimSpace(fields[fieldCron]),
		Timezone:   strings.TrimSpace(fields[fieldTimezone]),
		Prompt:     strings.TrimSpace(fields[fieldPrompt]),
		SuppressIf: strings.TrimSpace(fields[fieldSuppress]),
		// Reports where the command was run, stored as a Slack reference so the
		// firing knows which chat tool to deliver into.
		ChannelID: core.FormatChannelRef(core.PlatformSlack, channelID),
		Enabled:   true,
	}

	view, err := s.disp.PutSchedule(ctx, sched)
	if err != nil {
		s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ Could not save: "+err.Error())
		return
	}
	s.log.Info("schedule created from slack",
		"schedule", view.ID, "agent", view.AgentID, "by", slackUser(cb))
	s.ephemeral(ctx, channelID, cb.User.ID,
		slackMarkup("✅ Scheduled `"+view.ID+"`.\n"+describeSchedule(view)))
}

func (s *Slack) listSchedules(ctx context.Context, cmd slack.SlashCommand) {
	views, err := s.disp.ListSchedules(ctx)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}
	if len(views) == 0 {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "No schedules yet. Create one with `/schedule create <agent>`.")
		return
	}

	var b strings.Builder
	b.WriteString("*Schedules*\n")
	for _, v := range views {
		fmt.Fprintf(&b, "• `%s` → `%s` · `%s` %s", v.ID, v.AgentID, v.Cron, v.Timezone)
		if v.Paused {
			b.WriteString("  ⏸️ paused")
		} else if v.NextRun != "" {
			fmt.Fprintf(&b, "  next %s", v.NextRun)
		}
		b.WriteString("\n")
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, slackMarkup(b.String()))
}

func (s *Slack) showSchedule(ctx context.Context, cmd slack.SlashCommand, id string) {
	view, err := s.disp.GetSchedule(ctx, id)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "*%s* → `%s`\n%s\n", view.ID, view.AgentID, describeSchedule(view))
	fmt.Fprintf(&b, "```\n%s\n```", truncate(view.Prompt, 1200))
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, slackMarkup(b.String()))
}

func (s *Slack) setSchedulePaused(ctx context.Context, cmd slack.SlashCommand, id string, paused bool) {
	view, err := s.disp.SetSchedulePaused(ctx, id, paused, "via slack")
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}
	if paused {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⏸️ `"+id+"` paused. Its definition is kept; resume it any time.")
		return
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, slackMarkup("▶️ `"+id+"` resumed.\n"+describeSchedule(view)))
}

func (s *Slack) deleteSchedule(ctx context.Context, cmd slack.SlashCommand, id string) {
	if err := s.disp.DeleteSchedule(ctx, id); err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}
	s.log.Info("schedule deleted from slack", "schedule", id, "by", cmd.UserID)
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "🗑️ Deleted `"+id+"`. Turns it already ran are kept.")
}
