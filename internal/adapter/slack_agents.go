package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
)

// Agent management from Slack.
//
// Creating and editing open a Block Kit modal, for the reason the Discord edge
// gives: an agent has more fields than a command line reads comfortably, two of
// them are free-form lists, and a form keeps the whole definition visible while
// it is filled in — which matters most when editing, since it arrives
// pre-filled and shows what is about to change.
//
// Slack would allow far more than five inputs. The form deliberately carries
// the same five Discord's does. A definition that could be given a field from
// one chat tool and not the other is two different agents wearing one name, and
// the extra fields are reachable from the API from either.

const (
	viewAgentCreate = "agent-create"
	viewAgentEdit   = "agent-edit"
)

func (s *Slack) handleAgentCommand(ctx context.Context, cmd slack.SlashCommand) {
	sub, target := cutWord(cmd.Text)

	switch sub {
	case "create":
		s.openAgentForm(ctx, cmd, viewAgentCreate, "Create an agent", registry.Agent{
			PermissionMode: "default",
			AllowedTools:   []string{"Read", "Grep", "Glob"},
		})
	case "edit":
		agent, err := s.disp.Registry().Get(ctx, target)
		if err != nil {
			s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
			return
		}
		s.openAgentForm(ctx, cmd, viewAgentEdit, "Edit "+agent.ID, agent)
	case "show":
		s.showAgent(ctx, cmd, target)
	case "enable":
		s.setAgentEnabled(ctx, cmd, target, true)
	case "disable":
		s.setAgentEnabled(ctx, cmd, target, false)
	case "delete":
		s.deleteAgent(ctx, cmd, target)
	default:
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID,
			"Use `/agent create`, `edit <id>`, `show <id>`, `enable <id>`, `disable <id>` or `delete <id>`.")
	}
}

// openAgentForm must run while the trigger ID is still fresh — Slack expires
// one a few seconds after the command, so nothing slow may happen first.
func (s *Slack) openAgentForm(ctx context.Context, cmd slack.SlashCommand, callbackID, title string, prefill registry.Agent) {
	idHint := "letters, digits, - and _"
	if callbackID == viewAgentEdit {
		// The ID names the workspace and derives the Claude session, so renaming
		// would orphan the conversation. Shown for context, ignored on submit.
		idHint = "cannot be changed"
	}

	view := slack.ModalViewRequest{
		Type:       slack.VTModal,
		Title:      plainText(truncateRunes(title, 24)),
		Submit:     plainText("Save"),
		Close:      plainText("Cancel"),
		CallbackID: callbackID,
		// The channel the command was run in, and the agent being edited. A
		// view submission carries neither: it is a separate interaction with no
		// memory of the command that opened it.
		PrivateMetadata: cmd.ChannelID + "\n" + prefill.ID,
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			textInput(fieldID, "Agent ID", idHint, prefill.ID, false, 64, true),
			textInput(fieldDescription, "Description", "what this agent is for — shown in pickers",
				prefill.Description, false, 200, false),
			textInput(fieldPermission, "Permission mode", "default | acceptEdits | bypassPermissions",
				prefill.PermissionMode, false, 40, false),
			// Not "allowed tools": the field pre-approves rather than restricts,
			// and agents run headless where nothing asks, so a short list here
			// withholds nothing.
			textInput(fieldTools, "Auto-approve tools (does not restrict)",
				"Read, Grep, Glob, Bash, Edit, Write",
				strings.Join(prefill.AllowedTools, ", "), true, 500, false),
			textInput(fieldChannels, "Bound channels (comma separated)",
				"channel IDs, or `this` for this channel. Blank = none",
				strings.Join(prefill.DiscordChannels, ", "), true, 400, false),
		}},
	}

	if _, err := s.api.OpenViewContext(ctx, cmd.TriggerID, view); err != nil {
		s.log.Warn("failed to open the agent form", "error", err)
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not open the form: "+err.Error())
	}
}

// handleAgentView applies a submitted create or edit form.
func (s *Slack) handleAgentView(ctx context.Context, cb slack.InteractionCallback, editing bool) {
	channelID, prefilledID := splitMetadata(cb.View.PrivateMetadata)
	fields := viewFields(cb.View)

	channels := splitList(fields[fieldChannels])
	for idx, ch := range channels {
		if strings.EqualFold(ch, "this") {
			ch = channelID
		}
		// Every id typed here is a Slack one, so it is stored as such. Without
		// the prefix it would be read back as a Discord channel and a reply
		// would be delivered to a tool this workspace has no channel in — see
		// adr/002-channel-refs.
		channels[idx] = core.FormatChannelRef(core.PlatformSlack, ch)
	}

	agent := registry.Agent{
		ID:              strings.TrimSpace(fields[fieldID]),
		Description:     strings.TrimSpace(fields[fieldDescription]),
		PermissionMode:  strings.TrimSpace(fields[fieldPermission]),
		AllowedTools:    splitList(fields[fieldTools]),
		DiscordChannels: channels,
		Enabled:         true,
	}

	by := "slack:" + slackUser(cb)

	if editing {
		// The ID is authoritative from the command that opened the form, not
		// from the form: it names the workspace and derives the Claude session,
		// so a typo must not silently create a second agent.
		existing, err := s.disp.Registry().Get(ctx, prefilledID)
		if err != nil {
			s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ "+err.Error())
			return
		}
		agent.ID = existing.ID
		// Fields the form does not show are preserved rather than blanked.
		agent.AgentName = existing.AgentName
		agent.AdditionalDirs = existing.AdditionalDirs
		agent.WorkDir = existing.WorkDir
		agent.DenyPaths = existing.DenyPaths
		agent.RequireMention = existing.RequireMention
		agent.ShareWorkspace = existing.ShareWorkspace
		agent.ReplyInThread = existing.ReplyInThread
		agent.Tools = existing.Tools
		agent.Skills = existing.Skills
		agent.Image = existing.Image
		agent.GroupAdd = existing.GroupAdd
		agent.Model = existing.Model
		agent.Enabled = existing.Enabled

		updated, err := s.disp.Registry().Update(ctx, agent,
			registry.Change{Note: "edited from slack", Author: by})
		if err != nil {
			s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ Could not save: "+err.Error())
			return
		}
		s.ephemeral(ctx, channelID, cb.User.ID, slackMarkup("✅ Updated `"+updated.ID+"`.\n"+describeAgent(updated)+
			"\n_Takes effect on the next turn; anything running now keeps its current settings._"))
		return
	}

	created, err := s.disp.Registry().Create(ctx, agent,
		registry.Change{Note: "created from slack", Author: by})
	if err != nil {
		s.ephemeral(ctx, channelID, cb.User.ID, "⚠️ Could not create: "+err.Error())
		return
	}
	s.log.Info("agent created from slack", "agent", created.ID, "by", by)
	s.ephemeral(ctx, channelID, cb.User.ID, slackMarkup("✅ Created `"+created.ID+"` — ready now, no restart.\n"+
		describeAgent(created)+"\nTry `/ask "+created.ID+" …`"))
}

func (s *Slack) showAgent(ctx context.Context, cmd slack.SlashCommand, agentID string) {
	agent, err := s.disp.Registry().Get(ctx, agentID)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "*%s*", agent.ID)
	if !agent.Enabled {
		b.WriteString("  _(disabled)_")
	}
	b.WriteString("\n" + describeAgent(agent) + "\n")
	if agent.AgentName != "" {
		fmt.Fprintf(&b, "runs as subagent `%s`\n", agent.AgentName)
	}
	if agent.RequireMention {
		b.WriteString("answers only when mentioned\n")
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
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, slackMarkup(b.String()))
}

// setAgentEnabled is the reversible way to take an agent out of service: the
// workspace, database and Claude conversation all survive, so re-enabling picks
// the conversation back up rather than starting over.
func (s *Slack) setAgentEnabled(ctx context.Context, cmd slack.SlashCommand, agentID string, enabled bool) {
	agent, err := s.disp.Registry().Get(ctx, agentID)
	if err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}
	agent.Enabled = enabled
	verb := "disabled"
	if enabled {
		verb = "enabled"
	}
	if _, err := s.disp.Registry().Update(ctx, agent, registry.Change{
		Note: verb + " from slack", Author: "slack:" + cmd.UserName,
	}); err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ Could not save: "+err.Error())
		return
	}

	if !enabled {
		// Disabling only blocks new requests; without this the turns already
		// running — the default conversation and every thread — would carry on,
		// which is not what "disable" looks like.
		if err := s.disp.StopAll(ctx, agentID, "agent disabled"); err != nil {
			s.log.Info("could not stop a disabled agent", "agent", agentID, "error", err)
		}
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⏸️ `"+agentID+
			"` disabled. New requests are refused and every running turn is stopped; conversations are kept.")
		return
	}
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "▶️ `"+agentID+"` enabled — it picks up its existing conversation.")
}

func (s *Slack) deleteAgent(ctx context.Context, cmd slack.SlashCommand, agentID string) {
	// Stop every conversation first, or work already queued — in the default
	// session or any thread — would run on and fail one turn at a time once the
	// definition is gone.
	if err := s.disp.StopAll(ctx, agentID, "agent deleted"); err != nil {
		s.log.Info("could not stop agent before deleting it", "agent", agentID, "error", err)
	}
	if err := s.disp.Registry().Delete(ctx, agentID); err != nil {
		s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "⚠️ "+err.Error())
		return
	}

	s.log.Info("agent deleted from slack", "agent", agentID, "by", cmd.UserID)
	s.ephemeral(ctx, cmd.ChannelID, cmd.UserID, "🗑️ Deleted `"+agentID+"`.\n"+
		"Its workspace, database and Claude conversation are kept — recreating the same ID resumes where it left off.")
}

// channelLabel renders a stored channel reference as something clickable.
//
// Both tools happen to spell a channel link the same way, <#id>, so only the
// tool prefix has to come off. A reference that cannot be read is shown raw
// rather than hidden: somebody has to be able to see what is wrong with it.
func channelLabel(ref string) string {
	_, id, err := core.ParseChannelRef(ref)
	if err != nil {
		return "`" + ref + "` _(unreadable)_"
	}
	return "<#" + id + ">"
}

// plainText is Slack's required wrapper for any label, title or placeholder.
func plainText(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, s, false, false)
}

// textInput builds one labelled field of a modal.
func textInput(id, label, hint, initial string, multiline bool, maxLength int, required bool) *slack.InputBlock {
	el := slack.NewPlainTextInputBlockElement(plainText(truncateRunes(hint, 150)), id)
	el.InitialValue = initial
	el.Multiline = multiline
	el.MaxLength = maxLength

	block := slack.NewInputBlock(id, plainText(truncateRunes(label, 2000)), nil, el)
	// Slack refuses a submission that leaves a required field empty, which is
	// what keeps a half-filled definition from reaching the registry.
	block.Optional = !required
	return block
}

// viewFields flattens a view submission into blockID → value.
func viewFields(v slack.View) map[string]string {
	out := map[string]string{}
	for blockID, actions := range v.State.Values {
		for _, val := range actions {
			out[blockID] = val.Value
		}
	}
	return out
}

// splitMetadata reads back what openAgentForm stashed on the view. A view
// submission is a separate interaction and carries nothing of the command that
// opened it, so anything the handler needs has to travel here.
func splitMetadata(meta string) (channelID, target string) {
	channelID, target, _ = strings.Cut(meta, "\n")
	return channelID, target
}

func slackUser(cb slack.InteractionCallback) string {
	if cb.User.Name != "" {
		return cb.User.Name
	}
	return cb.User.ID
}

// truncateRunes caps a string by characters rather than bytes: Slack's limits
// count characters, and cutting Korean or emoji at a byte offset would send a
// broken character.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
