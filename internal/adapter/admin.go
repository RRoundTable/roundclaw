package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	case claude.AdminShowAgent:
		return d.adminShowAgent(ctx, action.Target)

	case claude.AdminCreateAgent:
		return d.adminCreateAgent(ctx, action.Agent)

	case claude.AdminUpdateAgent:
		return d.adminUpdateAgent(ctx, action.Agent)

	case claude.AdminDeleteAgent:
		return d.adminDeleteAgent(ctx, action.Target)

	case claude.AdminEnableAgent:
		return d.adminSetEnabled(ctx, action.Target, true)

	case claude.AdminDisableAgent:
		return d.adminSetEnabled(ctx, action.Target, false)

	case claude.AdminShowPersona:
		return d.adminShowPersona(ctx, action.Target)

	case claude.AdminSetPersona:
		return d.adminSetPersona(ctx, action.Agent)

	case claude.AdminListSchedules:
		return d.adminListSchedules(ctx)

	case claude.AdminCreateSchedule:
		return d.adminCreateSchedule(ctx, action.Schedule)

	case claude.AdminListWorkflows:
		return d.adminListWorkflows(ctx)

	case claude.AdminCreateWorkflow:
		return d.adminCreateWorkflow(ctx, action.Workflow)

	case claude.AdminRunWorkflow:
		return d.adminRunWorkflow(ctx, action.Target)

	case claude.AdminDeleteWorkflow:
		return d.adminDeleteWorkflow(ctx, action.Target)

	default:
		return "", fmt.Errorf("unknown admin action %q", action.Action)
	}
}

func (d *Dispatcher) adminShowAgent(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "🤔 어느 에이전트를 볼까요?", nil
	}
	a, err := d.reg.Get(ctx, id)
	if err != nil {
		return fmt.Sprintf("⚠️ 에이전트 `%s`를 찾을 수 없어요.", id), nil
	}
	return formatAgent(a), nil
}

func formatAgent(a registry.Agent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**`%s`** — %s\n", a.ID, a.Description)
	fmt.Fprintf(&b, "- enabled: %v\n", a.Enabled)
	fmt.Fprintf(&b, "- permission_mode: `%s`\n", noneIfEmpty(a.PermissionMode))
	fmt.Fprintf(&b, "- require_mention: %v · reply_in_thread: %v\n", a.RequireMention, a.ReplyInThread)
	fmt.Fprintf(&b, "- channels: %s\n", noneIfEmpty(strings.Join(a.DiscordChannels, ", ")))
	if len(a.AllowedTools) > 0 {
		fmt.Fprintf(&b, "- allowed_tools: %s\n", strings.Join(a.AllowedTools, ", "))
	}
	return b.String()
}

func noneIfEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func (d *Dispatcher) adminUpdateAgent(ctx context.Context, spec *claude.AdminAgentSpec) (string, error) {
	if spec == nil || spec.ID == "" {
		return "🤔 어느 에이전트를 수정할까요?", nil
	}
	a, err := d.reg.Get(ctx, spec.ID)
	if err != nil {
		return fmt.Sprintf("⚠️ 에이전트 `%s`가 없어요.", spec.ID), nil
	}
	// Patch: apply only what the planner set, keep the rest.
	if spec.Description != "" {
		a.Description = spec.Description
	}
	if spec.AgentName != "" {
		a.AgentName = spec.AgentName
	}
	if spec.WorkDir != "" {
		a.WorkDir = spec.WorkDir
	}
	if spec.PermissionMode != "" {
		a.PermissionMode = spec.PermissionMode
	}
	if spec.RequireMention != nil {
		a.RequireMention = *spec.RequireMention
	}
	if spec.ReplyInThread != nil {
		a.ReplyInThread = *spec.ReplyInThread
	}
	if spec.Channels != nil {
		a.DiscordChannels = spec.Channels
	}
	if spec.AllowedTools != nil {
		a.AllowedTools = spec.AllowedTools
	}
	if _, err := d.reg.Update(ctx, a); err != nil {
		return "⚠️ 수정 실패: " + err.Error(), nil
	}
	return "✅ `" + a.ID + "` 수정됨.\n" + formatAgent(a), nil
}

func (d *Dispatcher) adminDeleteAgent(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "🤔 어느 에이전트를 삭제할까요?", nil
	}
	if err := d.StopAll(ctx, id, "deleted via admin"); err != nil {
		// Best-effort stop; proceed to delete regardless.
		_ = err
	}
	if err := d.reg.Delete(ctx, id); err != nil {
		return fmt.Sprintf("⚠️ `%s` 삭제 실패: %s", id, err.Error()), nil
	}
	return fmt.Sprintf("🗑️ 에이전트 `%s` 삭제됨 (워크스페이스·세션은 유지).", id), nil
}

func (d *Dispatcher) adminSetEnabled(ctx context.Context, id string, enabled bool) (string, error) {
	if id == "" {
		return "🤔 어느 에이전트를요?", nil
	}
	a, err := d.reg.Get(ctx, id)
	if err != nil {
		return fmt.Sprintf("⚠️ 에이전트 `%s`가 없어요.", id), nil
	}
	a.Enabled = enabled
	if _, err := d.reg.Update(ctx, a); err != nil {
		return "⚠️ 실패: " + err.Error(), nil
	}
	if !enabled {
		// Disabling should stop work already running, like the slash command.
		_ = d.StopAll(ctx, id, "disabled via admin")
		return fmt.Sprintf("⏸️ `%s` 비활성화됨 (진행 중 턴 정지, 대화는 유지).", id), nil
	}
	return fmt.Sprintf("▶️ `%s` 활성화됨.", id), nil
}

// adminShowPersona returns the agent's CLAUDE.md — its instructions. The reply
// path chunks it if it is longer than one Discord message.
func (d *Dispatcher) adminShowPersona(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "🤔 어느 에이전트의 페르소나를요?", nil
	}
	if _, err := d.reg.Get(ctx, id); err != nil {
		return fmt.Sprintf("⚠️ 에이전트 `%s`가 없어요.", id), nil
	}
	data, err := os.ReadFile(d.personaPath(id))
	if os.IsNotExist(err) {
		return fmt.Sprintf("ℹ️ `%s`는 아직 페르소나(CLAUDE.md)가 없어요. `set_persona`로 만들 수 있어요.", id), nil
	}
	if err != nil {
		return "⚠️ 페르소나 읽기 실패: " + err.Error(), nil
	}
	return fmt.Sprintf("**`%s` 페르소나 (CLAUDE.md):**\n```md\n%s\n```", id, string(data)), nil
}

func (d *Dispatcher) adminSetPersona(ctx context.Context, spec *claude.AdminAgentSpec) (string, error) {
	if spec == nil || spec.ID == "" || spec.Persona == "" {
		return "🤔 어느 에이전트에 어떤 페르소나를 넣을지 알려주세요.", nil
	}
	if _, err := d.reg.Get(ctx, spec.ID); err != nil {
		return fmt.Sprintf("⚠️ 에이전트 `%s`가 없어요.", spec.ID), nil
	}
	if err := d.writePersona(spec.ID, spec.Persona); err != nil {
		return "⚠️ 페르소나 저장 실패: " + err.Error(), nil
	}
	return fmt.Sprintf("✅ `%s` 페르소나 저장됨 (다음 턴부터 적용). %d자.", spec.ID, len(spec.Persona)), nil
}

func (d *Dispatcher) personaPath(agentID string) string {
	return filepath.Join(d.cfg.WorkDir(agentID), "CLAUDE.md")
}

func (d *Dispatcher) writePersona(agentID, content string) error {
	dir := d.cfg.WorkDir(agentID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(content), 0o640)
}

func (d *Dispatcher) adminListWorkflows(ctx context.Context) (string, error) {
	wfs, err := d.reg.ListWorkflows(ctx)
	if err != nil {
		return "", err
	}
	if len(wfs) == 0 {
		return "등록된 워크플로가 없어요.", nil
	}
	var b strings.Builder
	b.WriteString("**Workflows**\n")
	for _, w := range wfs {
		fmt.Fprintf(&b, "- `%s` (%d단계) — %s\n", w.ID, len(w.Steps), w.Description)
	}
	return b.String(), nil
}

func (d *Dispatcher) adminCreateWorkflow(ctx context.Context, spec *claude.AdminWorkflowSpec) (string, error) {
	if spec == nil || spec.ID == "" || len(spec.Steps) == 0 {
		return "🤔 워크플로엔 id와 스텝이 최소 하나 필요해요.", nil
	}
	steps := make([]registry.WorkflowStep, 0, len(spec.Steps))
	for _, s := range spec.Steps {
		steps = append(steps, registry.WorkflowStep{
			Name:           s.Name,
			Prompt:         s.Prompt,
			PermissionMode: s.PermissionMode,
			Model:          s.Model,
		})
	}
	saved, err := d.reg.PutWorkflow(ctx, registry.Workflow{
		ID:          spec.ID,
		Description: spec.Description,
		ChannelID:   spec.ChannelID,
		Steps:       steps,
		Enabled:     true,
	})
	if err != nil {
		return "⚠️ 워크플로 생성 실패: " + err.Error(), nil
	}
	return fmt.Sprintf("✅ 워크플로 `%s` 생성됨 (%d단계). `run`으로 실행할 수 있어요.", saved.ID, len(saved.Steps)), nil
}

func (d *Dispatcher) adminRunWorkflow(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "🤔 어느 워크플로를 실행할까요?", nil
	}
	execID, err := d.RunWorkflow(ctx, id)
	if err != nil {
		return fmt.Sprintf("⚠️ `%s` 실행 실패: %s", id, err.Error()), nil
	}
	return fmt.Sprintf("▶️ 워크플로 `%s` 실행 시작 (execution `%s`). 완료되면 채널로 결과가 갑니다.", id, execID), nil
}

func (d *Dispatcher) adminDeleteWorkflow(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "🤔 어느 워크플로를 삭제할까요?", nil
	}
	if err := d.reg.DeleteWorkflow(ctx, id); err != nil {
		return fmt.Sprintf("⚠️ `%s` 삭제 실패: %s", id, err.Error()), nil
	}
	return fmt.Sprintf("🗑️ 워크플로 `%s` 삭제됨.", id), nil
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
		AgentName:       spec.AgentName,
		WorkDir:         spec.WorkDir,
		PermissionMode:  permission,
		RequireMention:  requireMention,
		ReplyInThread:   replyInThread,
		AllowedTools:    spec.AllowedTools,
		DiscordChannels: spec.Channels,
		Enabled:         true,
	}
	created, err := d.reg.Create(ctx, agent)
	if err != nil {
		return "⚠️ 에이전트를 만들지 못했어요: " + err.Error(), nil
	}
	msg := fmt.Sprintf("✅ 에이전트 `%s` 생성됨", created.ID)
	if len(created.DiscordChannels) > 0 {
		msg += fmt.Sprintf(" (채널 %d개 연결)", len(created.DiscordChannels))
	}
	// A persona given at creation is written as the agent's CLAUDE.md, so the
	// new agent has instructions from its first turn rather than none.
	if spec.Persona != "" {
		if err := d.writePersona(created.ID, spec.Persona); err != nil {
			msg += " — 단, 페르소나 저장 실패: " + err.Error()
		} else {
			msg += fmt.Sprintf(", 페르소나 설정됨(%d자)", len(spec.Persona))
		}
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
