package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Schedule management from Discord.
//
// Creating goes through a modal for the same reason agents do: a schedule has
// more fields than a slash command reads comfortably, and the prompt is
// free-form prose.
//
// Discord allows five modal inputs and a schedule has eight fields, so two are
// resolved from context instead. The agent comes from the slash command and
// rides in the modal's custom ID, and the target channel defaults to wherever
// the command was run — which is where a recurring report almost always wants
// to land anyway.
const modalScheduleCreate = "schedule-create:"

const (
	fieldScheduleID = "id"
	fieldCron       = "cron"
	fieldTimezone   = "timezone"
	fieldPrompt     = "prompt"
	fieldSuppress   = "suppress_if"
)

func (d *Discord) handleScheduleCommand(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	if len(data.Options) == 0 {
		d.respondNow(i, "Use `/schedule create`, `list`, `show`, `pause`, `resume` or `delete`.")
		return
	}
	sub := data.Options[0]

	switch sub.Name {
	case "create":
		d.openScheduleForm(i, optionString(sub.Options, "agent"))
	case "list":
		d.listSchedules(i)
	case "show":
		d.showSchedule(i, optionString(sub.Options, "schedule"))
	case "pause":
		d.setSchedulePaused(i, optionString(sub.Options, "schedule"), true)
	case "resume":
		d.setSchedulePaused(i, optionString(sub.Options, "schedule"), false)
	case "delete":
		d.deleteSchedule(i, optionString(sub.Options, "schedule"))
	}
}

func (d *Discord) openScheduleForm(i *discordgo.InteractionCreate, agentID string) {
	title := "Schedule for " + agentID
	if len(title) > 45 {
		title = title[:45]
	}

	rows := []discordgo.MessageComponent{
		row(discordgo.TextInput{
			CustomID: fieldScheduleID, Label: "Schedule ID",
			Style: discordgo.TextInputShort, Required: true,
			Placeholder: "letters, digits, - and _", MaxLength: 64,
		}),
		row(discordgo.TextInput{
			CustomID: fieldCron, Label: "Cron", Style: discordgo.TextInputShort,
			Required: true, Placeholder: "0 9 * * 1-5  (weekdays at 09:00)", MaxLength: 100,
		}),
		row(discordgo.TextInput{
			CustomID: fieldTimezone, Label: "Timezone", Style: discordgo.TextInputShort,
			// Defaulted rather than required: an empty box would silently mean
			// UTC, and a daily report firing nine hours early is a bad surprise.
			Value: "Asia/Seoul", Placeholder: "Asia/Seoul", MaxLength: 64,
		}),
		row(discordgo.TextInput{
			CustomID: fieldPrompt, Label: "What to run each time",
			Style: discordgo.TextInputParagraph, Required: true, MaxLength: 3000,
		}),
		row(discordgo.TextInput{
			CustomID: fieldSuppress, Label: "Skip posting when the result contains",
			Style:       discordgo.TextInputShort,
			Placeholder: "optional — e.g. nothing to report",
			MaxLength:   200,
		}),
	}

	err := d.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			// The agent rides here: a modal submission does not carry the
			// options of the command that opened it.
			CustomID:   modalScheduleCreate + agentID,
			Title:      title,
			Components: rows,
		},
	})
	if err != nil {
		d.log.Warn("failed to open the schedule form", "error", err)
	}
}

// handleScheduleForm applies a submitted schedule form.
func (d *Discord) handleScheduleForm(i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	agentID := strings.TrimPrefix(data.CustomID, modalScheduleCreate)

	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fields := modalFields(data)
	sched := registry.Schedule{
		ID:         strings.TrimSpace(fields[fieldScheduleID]),
		AgentID:    agentID,
		Cron:       strings.TrimSpace(fields[fieldCron]),
		Timezone:   strings.TrimSpace(fields[fieldTimezone]),
		Prompt:     strings.TrimSpace(fields[fieldPrompt]),
		SuppressIf: strings.TrimSpace(fields[fieldSuppress]),
		// Report where the command was run. A schedule created from Discord
		// that posted nowhere would look broken.
		ChannelID: i.ChannelID,
		Enabled:   true,
	}

	view, err := d.disp.PutSchedule(ctx, sched)
	if err != nil {
		d.followUp(i, "⚠️ Could not save: "+err.Error())
		return
	}
	d.log.Info("schedule created from discord",
		"schedule", view.ID, "agent", view.AgentID, "by", interactionUser(i))
	d.followUp(i, "✅ Scheduled `"+view.ID+"`.\n"+describeSchedule(view))
}

func (d *Discord) listSchedules(i *discordgo.InteractionCreate) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	views, err := d.disp.ListSchedules(ctx)
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}
	if len(views) == 0 {
		d.followUp(i, "No schedules yet. Create one with `/schedule create agent:<name>`.")
		return
	}

	var b strings.Builder
	b.WriteString("**Schedules**\n")
	for _, v := range views {
		fmt.Fprintf(&b, "• `%s` → `%s` · `%s` %s", v.ID, v.AgentID, v.Cron, v.Timezone)
		if v.Paused {
			b.WriteString("  ⏸️ paused")
		} else if v.NextRun != "" {
			fmt.Fprintf(&b, "  next %s", v.NextRun)
		}
		b.WriteString("\n")
	}
	d.followUp(i, b.String())
}

func (d *Discord) showSchedule(i *discordgo.InteractionCreate, id string) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	view, err := d.disp.GetSchedule(ctx, id)
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** → `%s`\n%s\n", view.ID, view.AgentID, describeSchedule(view))
	fmt.Fprintf(&b, "```\n%s\n```", truncate(view.Prompt, 1200))
	d.followUp(i, b.String())
}

func (d *Discord) setSchedulePaused(i *discordgo.InteractionCreate, id string, paused bool) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	view, err := d.disp.SetSchedulePaused(ctx, id, paused, "via discord")
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}
	if paused {
		d.followUp(i, "⏸️ `"+id+"` paused. Its definition is kept; resume it any time.")
		return
	}
	d.followUp(i, "▶️ `"+id+"` resumed.\n"+describeSchedule(view))
}

func (d *Discord) deleteSchedule(i *discordgo.InteractionCreate, id string) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := d.disp.DeleteSchedule(ctx, id); err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}
	d.log.Info("schedule deleted from discord", "schedule", id, "by", interactionUser(i))
	d.followUp(i, "🗑️ Deleted `"+id+"`. Turns it already ran are kept.")
}

func describeSchedule(v ScheduleView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` %s", v.Cron, v.Timezone)
	switch {
	case v.Unavailable != "":
		fmt.Fprintf(&b, " · _trigger state unavailable: %s_", truncate(v.Unavailable, 120))
	case v.Paused:
		b.WriteString(" · ⏸️ paused")
	case v.NextRun != "":
		fmt.Fprintf(&b, " · next %s", v.NextRun)
	}
	if v.ChannelID != "" {
		fmt.Fprintf(&b, " · reports to <#%s>", v.ChannelID)
	} else {
		b.WriteString(" · result recorded but not posted")
	}
	if v.SuppressIf != "" {
		fmt.Fprintf(&b, "\nskips posting when the result contains `%s`", v.SuppressIf)
	}
	return b.String()
}
