package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Agent management from Discord.
//
// Creating and editing go through a modal rather than slash-command options.
// An agent has more fields than a command line reads comfortably, two of them
// are free-form lists, and a modal keeps the whole definition visible while
// it is being filled in — which matters most when editing, since the form
// arrives pre-filled and shows what is about to change.
//
// Discord allows at most five inputs per modal, so the definition is folded
// into exactly five. Fields not exposed here (agent_name, additional_dirs) stay
// available through the HTTP API and are preserved on edit rather than wiped.

const (
	modalCreate     = "agent-create"
	modalEditPrefix = "agent-edit:"

	fieldID          = "id"
	fieldDescription = "description"
	fieldPermission  = "permission_mode"
	fieldTools       = "allowed_tools"
	fieldChannels    = "discord_channels"
)

func (d *Discord) handleAgentCommand(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	if len(data.Options) == 0 {
		d.respondNow(i, "Use `/agent create`, `/agent edit` or `/agent delete`.")
		return
	}
	sub := data.Options[0]
	target := optionString(sub.Options, "agent")

	switch sub.Name {
	case "create":
		d.openAgentForm(i, modalCreate, "Create an agent", registry.Agent{
			PermissionMode: "default",
			AllowedTools:   []string{"Read", "Grep", "Glob"},
		})
	case "edit":
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		agent, err := d.disp.Registry().Get(ctx, target)
		if err != nil {
			d.respondNow(i, "⚠️ "+err.Error())
			return
		}
		d.openAgentForm(i, modalEditPrefix+agent.ID, "Edit "+agent.ID, agent)
	case "show":
		d.showAgent(i, target)
	case "enable":
		d.setAgentEnabled(i, target, true)
	case "disable":
		d.setAgentEnabled(i, target, false)
	case "delete":
		d.deleteAgent(i, target)
	}
}

// openAgentForm must be the first response to the interaction: Discord does not
// allow a modal after a deferral.
func (d *Discord) openAgentForm(i *discordgo.InteractionCreate, customID, title string, prefill registry.Agent) {
	// Modal titles are capped at 45 characters.
	if len(title) > 45 {
		title = title[:45]
	}

	idInput := discordgo.TextInput{
		CustomID:    fieldID,
		Label:       "Agent ID",
		Style:       discordgo.TextInputShort,
		Placeholder: "letters, digits, - and _",
		Required:    true,
		Value:       prefill.ID,
		MaxLength:   64,
	}
	if customID != modalCreate {
		// The ID names the workspace and derives the Claude session, so renaming
		// would orphan the conversation. Shown for context, ignored on submit.
		idInput.Label = "Agent ID (cannot be changed)"
	}

	rows := []discordgo.MessageComponent{
		row(idInput),
		row(discordgo.TextInput{
			CustomID:    fieldDescription,
			Label:       "Description",
			Style:       discordgo.TextInputShort,
			Placeholder: "what this agent is for — shown in pickers",
			Value:       prefill.Description,
			MaxLength:   200,
		}),
		row(discordgo.TextInput{
			CustomID:    fieldPermission,
			Label:       "Permission mode",
			Style:       discordgo.TextInputShort,
			Placeholder: "default | acceptEdits | bypassPermissions",
			Value:       prefill.PermissionMode,
			MaxLength:   40,
		}),
		row(discordgo.TextInput{
			CustomID: fieldTools,
			// Not "allowed tools": the field pre-approves rather than restricts,
			// and agents run headless where nothing asks, so a short list here
			// withholds nothing. Saying "auto-approve" in the one place an
			// operator types it is cheaper than correcting the belief later.
			Label:       "Auto-approve tools (does not restrict)",
			Style:       discordgo.TextInputParagraph,
			Placeholder: "Read, Grep, Glob, Bash, Edit, Write",
			Value:       strings.Join(prefill.AllowedTools, ", "),
			MaxLength:   500,
		}),
		row(discordgo.TextInput{
			CustomID: fieldChannels,
			Label:    "Bound channels (comma separated)",
			Style:    discordgo.TextInputParagraph,
			// "this" saves anyone from having to enable developer mode and copy
			// a channel ID out of the client.
			Placeholder: "channel IDs, or `this` for this channel. Blank = none",
			Value:       strings.Join(prefill.DiscordChannels, ", "),
			MaxLength:   400,
		}),
	}

	err := d.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   customID,
			Title:      title,
			Components: rows,
		},
	})
	if err != nil {
		d.log.Warn("failed to open the agent form", "error", err)
	}
}

func row(input discordgo.TextInput) discordgo.MessageComponent {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{input}}
}

// handleAgentForm applies a submitted create or edit form.
func (d *Discord) handleAgentForm(i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	if strings.HasPrefix(data.CustomID, modalScheduleCreate) {
		d.handleScheduleForm(i)
		return
	}
	editing := strings.HasPrefix(data.CustomID, modalEditPrefix)
	if !editing && data.CustomID != modalCreate {
		return
	}

	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fields := modalFields(data)
	channels := splitList(fields[fieldChannels])
	for idx, ch := range channels {
		if strings.EqualFold(ch, "this") {
			channels[idx] = i.ChannelID
		}
	}

	agent := registry.Agent{
		ID:              strings.TrimSpace(fields[fieldID]),
		Description:     strings.TrimSpace(fields[fieldDescription]),
		PermissionMode:  strings.TrimSpace(fields[fieldPermission]),
		AllowedTools:    splitList(fields[fieldTools]),
		DiscordChannels: channels,
		Enabled:         true,
	}

	if editing {
		// The ID is authoritative from the command, not the form: it names the
		// workspace and derives the Claude session, so a typo in the form must
		// not silently create a second agent.
		existing, err := d.disp.Registry().Get(ctx, strings.TrimPrefix(data.CustomID, modalEditPrefix))
		if err != nil {
			d.followUp(i, "⚠️ "+err.Error())
			return
		}
		agent.ID = existing.ID
		// Fields the form does not show are preserved rather than blanked.
		agent.AgentName = existing.AgentName
		agent.AdditionalDirs = existing.AdditionalDirs
		agent.WorkDir = existing.WorkDir
		agent.DenyPaths = existing.DenyPaths
		agent.RequireMention = existing.RequireMention
		agent.Image = existing.Image
		agent.GroupAdd = existing.GroupAdd
		agent.Model = existing.Model
		agent.Enabled = existing.Enabled

		updated, err := d.disp.Registry().Update(ctx, agent, discordChange(i, "edited from discord"))
		if err != nil {
			d.followUp(i, "⚠️ Could not save: "+err.Error())
			return
		}
		d.followUp(i, "✅ Updated `"+updated.ID+"`.\n"+describeAgent(updated)+
			"\n_Takes effect on the next turn; anything running now keeps its current settings._")
		return
	}

	created, err := d.disp.Registry().Create(ctx, agent, discordChange(i, "created from discord"))
	if err != nil {
		d.followUp(i, "⚠️ Could not create: "+err.Error())
		return
	}
	d.log.Info("agent created from discord", "agent", created.ID, "by", interactionUser(i))
	d.followUp(i, "✅ Created `"+created.ID+"` — ready now, no restart.\n"+describeAgent(created)+
		"\nTry `/ask agent:"+created.ID+" prompt:…`")
}

func (d *Discord) showAgent(i *discordgo.InteractionCreate, agentID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agent, err := d.disp.Registry().Get(ctx, agentID)
	if err != nil {
		d.respondNow(i, "⚠️ "+err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s**", agent.ID)
	if !agent.Enabled {
		b.WriteString("  _(disabled)_")
	}
	b.WriteString("\n" + describeAgent(agent) + "\n")
	if agent.AgentName != "" {
		fmt.Fprintf(&b, "runs as subagent `%s`\n", agent.AgentName)
	}
	if agent.RequireMention {
		b.WriteString("answers only when @-mentioned\n")
	}
	if agent.WorkDir != "" {
		fmt.Fprintf(&b, "works in `%s` (read-write)\n", agent.WorkDir)
	}
	if len(agent.DenyPaths) > 0 {
		fmt.Fprintf(&b, "hidden from it: `%s`\n", strings.Join(agent.DenyPaths, ", "))
	}
	if len(agent.AdditionalDirs) > 0 {
		fmt.Fprintf(&b, "extra dirs (read-only): `%s`\n", strings.Join(agent.AdditionalDirs, ", "))
	}
	for _, ch := range agent.DiscordChannels {
		fmt.Fprintf(&b, "bound to %s\n", channelLabel(ch))
	}
	fmt.Fprintf(&b, "_created %s · updated %s_",
		agent.CreatedAt.Format("2006-01-02 15:04"), agent.UpdatedAt.Format("2006-01-02 15:04"))
	d.respondNow(i, b.String())
}

// setAgentEnabled is the reversible way to take an agent out of service: the
// workspace, database and Claude conversation all survive, so re-enabling picks
// the conversation back up rather than starting over.
func (d *Discord) setAgentEnabled(i *discordgo.InteractionCreate, agentID string, enabled bool) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	agent, err := d.disp.Registry().Get(ctx, agentID)
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}
	agent.Enabled = enabled
	verb := "disabled"
	if enabled {
		verb = "enabled"
	}
	if _, err := d.disp.Registry().Update(ctx, agent, discordChange(i, verb+" from discord")); err != nil {
		d.followUp(i, "⚠️ Could not save: "+err.Error())
		return
	}

	if !enabled {
		// Disabling only blocks new requests; without this the turns already
		// running — the default conversation and every thread — would carry on,
		// which is not what "disable" looks like.
		if err := d.disp.StopAll(ctx, agentID, "agent disabled"); err != nil {
			d.log.Info("could not stop a disabled agent", "agent", agentID, "error", err)
		}
		d.followUp(i, "⏸️ `"+agentID+"` disabled. New requests are refused and every running turn is stopped; conversations are kept.")
		return
	}
	d.followUp(i, "▶️ `"+agentID+"` enabled — it picks up its existing conversation.")
}

func (d *Discord) deleteAgent(i *discordgo.InteractionCreate, agentID string) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Stop every conversation first, or work already queued — in the default
	// session or any thread — would run on and fail one turn at a time once the
	// definition is gone.
	if err := d.disp.StopAll(ctx, agentID, "agent deleted"); err != nil {
		d.log.Info("could not stop agent before deleting it", "agent", agentID, "error", err)
	}
	if err := d.disp.Registry().Delete(ctx, agentID); err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}

	d.log.Info("agent deleted from discord", "agent", agentID, "by", interactionUser(i))
	d.followUp(i, "🗑️ Deleted `"+agentID+"`.\n"+
		"Its workspace, database and Claude conversation are kept — recreating the same ID resumes where it left off.")
}

func describeAgent(a registry.Agent) string {
	var b strings.Builder
	if a.Description != "" {
		fmt.Fprintf(&b, "_%s_\n", a.Description)
	}
	fmt.Fprintf(&b, "permissions `%s`", orNone(a.PermissionMode, "default"))
	if len(a.AllowedTools) > 0 {
		fmt.Fprintf(&b, " · tools `%s`", strings.Join(a.AllowedTools, ", "))
	}
	if len(a.DiscordChannels) > 0 {
		fmt.Fprintf(&b, " · bound to %d channel(s)", len(a.DiscordChannels))
	}
	return b.String()
}

func orNone(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// modalFields flattens a modal submission into customID → value.
func modalFields(data discordgo.ModalSubmitInteractionData) map[string]string {
	out := map[string]string{}
	for _, row := range data.Components {
		actions, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range actions.Components {
			if input, ok := c.(*discordgo.TextInput); ok {
				out[input.CustomID] = input.Value
			}
		}
	}
	return out
}

// splitList parses a comma-separated field, dropping blanks so trailing commas
// and stray spaces do not become empty entries.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// interactionUser names whoever ran the command, for the audit line. An
// interaction from a guild carries Member; a DM carries User.
// discordChange labels the version an interaction mints, so the history says who
// made the change rather than only that something did.
func discordChange(i *discordgo.InteractionCreate, note string) registry.Change {
	return registry.Change{Note: note, Author: "discord:" + interactionUser(i)}
}

func interactionUser(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username
	}
	if i.User != nil {
		return i.User.Username
	}
	return "unknown"
}
