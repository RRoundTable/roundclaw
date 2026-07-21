package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/registry"
)

// ExecuteAdmin applies a planned admin action against the registry and returns a
// human-readable result.
//
// The planner only proposes; this is where the change actually happens, through
// the same Create / PutSchedule calls the slash commands use. So every guard
// they have — ID validation, duplicate detection, unknown-agent rejection —
// applies here too, and a hallucinated proposal fails safely with a message
// rather than a bad write.
func (d *Dispatcher) ExecuteAdmin(ctx context.Context, action claude.AdminAction) (string, error) {
	switch action.Action {
	case claude.AdminClarify:
		if action.Reason == "" {
			return "🤔 I need more detail — could you rephrase that?", nil
		}
		return "🤔 " + action.Reason, nil

	case claude.AdminListAgents:
		return d.adminListAgents(ctx)

	case claude.AdminListSchedules:
		return d.adminListSchedules(ctx)

	case claude.AdminCreateAgent:
		return d.adminCreateAgent(ctx, action.Agent)

	case claude.AdminCreateSchedule:
		return d.adminCreateSchedule(ctx, action.Schedule)

	default:
		return "", fmt.Errorf("unknown admin action %q", action.Action)
	}
}

func (d *Dispatcher) adminCreateAgent(ctx context.Context, spec *claude.AdminAgentSpec) (string, error) {
	if spec == nil || spec.ID == "" {
		return "🤔 Which agent should I create? I need at least an id.", nil
	}
	// Sensible defaults for the flags the operator did not pin down: a bound
	// channel should not answer every message, and a fresh reply reads better in
	// a thread. Both match how the migrated agents are set up.
	requireMention := spec.RequireMention == nil || *spec.RequireMention
	replyInThread := spec.ReplyInThread == nil || *spec.ReplyInThread
	permission := spec.PermissionMode
	if permission == "" {
		permission = "acceptEdits"
	}

	agent := registry.Agent{
		ID:              spec.ID,
		Description:     spec.Description,
		PermissionMode:  permission,
		RequireMention:  requireMention,
		ReplyInThread:   replyInThread,
		DiscordChannels: spec.Channels,
		Enabled:         true,
	}
	created, err := d.reg.Create(ctx, agent)
	if err != nil {
		return "⚠️ Could not create that agent: " + err.Error(), nil
	}
	msg := fmt.Sprintf("✅ Created agent `%s`", created.ID)
	if len(created.DiscordChannels) > 0 {
		msg += fmt.Sprintf(", bound to %d channel(s)", len(created.DiscordChannels))
	}
	return msg + ".", nil
}

func (d *Dispatcher) adminCreateSchedule(ctx context.Context, spec *claude.AdminScheduleSpec) (string, error) {
	if spec == nil || spec.ID == "" || spec.AgentID == "" || spec.Cron == "" || spec.Prompt == "" {
		return "🤔 A schedule needs an id, an agent, a cron expression and a prompt — which are missing?", nil
	}
	// Reject an unknown agent here rather than saving a schedule that can never
	// fire against a real one.
	if _, err := d.reg.Get(ctx, spec.AgentID); err != nil {
		return fmt.Sprintf("⚠️ There is no agent `%s` to run that schedule. Create it first.", spec.AgentID), nil
	}
	tz := spec.Timezone
	if tz == "" {
		tz = "Asia/Seoul"
	}
	view, err := d.PutSchedule(ctx, registry.Schedule{
		ID:         spec.ID,
		AgentID:    spec.AgentID,
		Cron:       spec.Cron,
		Timezone:   tz,
		Prompt:     spec.Prompt,
		ChannelID:  spec.ChannelID,
		SuppressIf: spec.SuppressIf,
		Enabled:    true,
	})
	if err != nil {
		return "⚠️ Could not create that schedule: " + err.Error(), nil
	}
	return fmt.Sprintf("✅ Scheduled `%s` — agent `%s`, `%s` (%s).",
		view.ID, spec.AgentID, spec.Cron, tz), nil
}

func (d *Dispatcher) adminListAgents(ctx context.Context) (string, error) {
	agents, err := d.reg.List(ctx)
	if err != nil {
		return "", err
	}
	if len(agents) == 0 {
		return "No agents yet.", nil
	}
	var b strings.Builder
	b.WriteString("**Agents**\n")
	for _, a := range agents {
		state := ""
		if !a.Enabled {
			state = " _(disabled)_"
		}
		fmt.Fprintf(&b, "- `%s`%s — %s\n", a.ID, state, a.Description)
	}
	return b.String(), nil
}

func (d *Dispatcher) adminListSchedules(ctx context.Context) (string, error) {
	schedules, err := d.ListSchedules(ctx)
	if err != nil {
		return "", err
	}
	if len(schedules) == 0 {
		return "No schedules yet.", nil
	}
	var b strings.Builder
	b.WriteString("**Schedules**\n")
	for _, s := range schedules {
		fmt.Fprintf(&b, "- `%s` — agent `%s`, `%s` (%s)\n", s.ID, s.AgentID, s.Cron, s.Timezone)
	}
	return b.String(), nil
}
