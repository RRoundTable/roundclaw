package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The admin planner turns a natural-language management request into a
// structured action that roundclaw itself validates and executes.
//
// It mirrors the router: a stateless `claude -p --json-schema` call that only
// *proposes* — roundclaw does the actual registry write. So the LLM never holds
// a credential, never reaches the API, and a bad proposal is caught by the same
// validation the slash commands use. "Create an agent", not "run this SQL".

// AdminActionKind is what the admin planner decided to do.
type AdminActionKind string

const (
	AdminCreateAgent  AdminActionKind = "create_agent"
	AdminUpdateAgent  AdminActionKind = "update_agent"
	AdminDeleteAgent  AdminActionKind = "delete_agent"
	AdminShowAgent    AdminActionKind = "show_agent"
	AdminListAgents   AdminActionKind = "list_agents"
	AdminEnableAgent  AdminActionKind = "enable_agent"
	AdminDisableAgent AdminActionKind = "disable_agent"
	AdminShowPersona  AdminActionKind = "show_persona"
	AdminSetPersona   AdminActionKind = "set_persona"

	AdminCreateSchedule AdminActionKind = "create_schedule"
	AdminListSchedules  AdminActionKind = "list_schedules"

	AdminCreateWorkflow AdminActionKind = "create_workflow"
	AdminRunWorkflow    AdminActionKind = "run_workflow"
	AdminListWorkflows  AdminActionKind = "list_workflows"
	AdminDeleteWorkflow AdminActionKind = "delete_workflow"

	// Tools are registered by the operator (they name a host path); admin can only
	// grant an already-registered tool to an agent, take it away, or list them.
	AdminAttachTool AdminActionKind = "attach_tool"
	AdminDetachTool AdminActionKind = "detach_tool"
	AdminListTools  AdminActionKind = "list_tools"

	// AdminClarify asks the user for more detail. It is the safe default when the
	// request is ambiguous — a wrong create is easy to delete, but confirming
	// first is friendlier than guessing.
	AdminClarify AdminActionKind = "clarify"
)

// AdminAgentSpec is the subset of an agent an admin request can set. For an
// update, only the fields the operator changed need be set; the executor keeps
// the rest. The pointer bools distinguish "leave as is" (nil) from "set false".
type AdminAgentSpec struct {
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	AgentName      string   `json:"agent_name,omitempty"`
	WorkDir        string   `json:"work_dir,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	RequireMention *bool    `json:"require_mention,omitempty"`
	ReplyInThread  *bool    `json:"reply_in_thread,omitempty"`
	Channels       []string `json:"channels,omitempty"`
	AllowedTools   []string `json:"allowed_tools,omitempty"`
	// Persona is the CLAUDE.md content — the agent's instructions. Set on
	// create_agent or set_persona; it is a file, not a registry field.
	Persona string `json:"persona,omitempty"`
}

// AdminScheduleSpec is the subset of a schedule an admin request can set.
type AdminScheduleSpec struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	Cron       string `json:"cron"`
	Timezone   string `json:"timezone,omitempty"`
	Prompt     string `json:"prompt"`
	ChannelID  string `json:"channel_id,omitempty"`
	SuppressIf string `json:"suppress_if,omitempty"`
}

// AdminWorkflowStep is one step of a workflow an admin request defines.
type AdminWorkflowStep struct {
	Name           string `json:"name,omitempty"`
	Prompt         string `json:"prompt"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Model          string `json:"model,omitempty"`
}

// AdminWorkflowSpec is an agent-less multi-step workflow an admin request sets.
type AdminWorkflowSpec struct {
	ID          string              `json:"id"`
	Description string              `json:"description,omitempty"`
	ChannelID   string              `json:"channel_id,omitempty"`
	Steps       []AdminWorkflowStep `json:"steps,omitempty"`
}

// AdminAction is the planner's structured answer. Target names the id an action
// operates on when it takes no full spec — show/delete an agent, run/delete a
// workflow.
type AdminAction struct {
	Action   AdminActionKind    `json:"action"`
	Reason   string             `json:"reason,omitempty"`
	Target   string             `json:"target,omitempty"`
	Agent    *AdminAgentSpec    `json:"agent,omitempty"`
	Schedule *AdminScheduleSpec `json:"schedule,omitempty"`
	Workflow *AdminWorkflowSpec `json:"workflow,omitempty"`
	// Tool is the registered tool id for attach_tool / detach_tool; Target holds
	// the agent id those act on.
	Tool string `json:"tool,omitempty"`
}

const adminSchema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": [
      "create_agent", "update_agent", "delete_agent", "show_agent", "list_agents",
      "enable_agent", "disable_agent", "show_persona", "set_persona",
      "create_schedule", "list_schedules",
      "create_workflow", "run_workflow", "list_workflows", "delete_workflow",
      "attach_tool", "detach_tool", "list_tools",
      "clarify"]},
    "reason": {"type": "string"},
    "target": {"type": "string"},
    "tool": {"type": "string"},
    "agent": {
      "type": "object",
      "properties": {
        "id": {"type": "string"},
        "description": {"type": "string"},
        "agent_name": {"type": "string"},
        "work_dir": {"type": "string"},
        "permission_mode": {"type": "string", "enum": ["default", "acceptEdits", "bypassPermissions", "plan"]},
        "require_mention": {"type": "boolean"},
        "reply_in_thread": {"type": "boolean"},
        "channels": {"type": "array", "items": {"type": "string"}},
        "allowed_tools": {"type": "array", "items": {"type": "string"}},
        "persona": {"type": "string"}
      }
    },
    "schedule": {
      "type": "object",
      "properties": {
        "id": {"type": "string"}, "agent_id": {"type": "string"}, "cron": {"type": "string"},
        "timezone": {"type": "string"}, "prompt": {"type": "string"},
        "channel_id": {"type": "string"}, "suppress_if": {"type": "string"}
      }
    },
    "workflow": {
      "type": "object",
      "properties": {
        "id": {"type": "string"}, "description": {"type": "string"}, "channel_id": {"type": "string"},
        "steps": {"type": "array", "items": {"type": "object", "properties": {
          "name": {"type": "string"}, "prompt": {"type": "string"},
          "permission_mode": {"type": "string"}, "model": {"type": "string"}
        }, "required": ["prompt"]}}
      }
    }
  },
  "required": ["action"]
}`

// Admin plans management actions from natural language. It carries the same
// credential and --bare behaviour as the Router.
type Admin struct {
	Runtime string
	Image   string
	Model   string
	Timeout time.Duration

	CredentialEnv   string
	CredentialValue string
	Bare            bool
}

// AdminContext is what the planner needs to know about the world to produce a
// valid action: which agents already exist, where the request came from, and —
// in an admin thread — what was said and done earlier so a follow-up like
// "actually make it 10am" resolves against the right thing.
type AdminContext struct {
	CurrentChannelID string
	// Agents, Schedules and Workflows are the current state, pre-formatted one
	// per line with enough detail for the planner to answer questions and resolve
	// references. Without them the model has no idea what exists and hallucinates.
	Agents    string
	Schedules string
	Workflows string
	// Tools is the registered tool catalogue, one per line, so the planner can
	// attach one by id and refuse to invent one that does not exist.
	Tools string
	// History is the prior admin exchange in this thread, oldest first, already
	// formatted for the prompt. Empty for a one-shot request.
	History string
}

// Plan asks the planner what to do with a management request.
func (a Admin) Plan(ctx context.Context, request string, world AdminContext) (AdminAction, error) {
	if a.Timeout <= 0 {
		a.Timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.Runtime, a.args(request, world)...)
	cmd.Env = append(cmd.Environ(), a.CredentialEnv+"="+a.CredentialValue)

	out, err := cmd.Output()
	if err != nil {
		if detail := failureDetail(out); detail != "" {
			return AdminAction{}, fmt.Errorf("run admin planner: %w: %s", err, detail)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return AdminAction{}, fmt.Errorf("run admin planner: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return AdminAction{}, fmt.Errorf("run admin planner: %w", err)
	}

	var envelope struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
		IsError          bool            `json:"is_error"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return AdminAction{}, fmt.Errorf("decode admin envelope: %w", err)
	}
	if envelope.IsError || len(envelope.StructuredOutput) == 0 {
		return AdminAction{}, fmt.Errorf("admin planner returned no structured output")
	}
	var action AdminAction
	if err := json.Unmarshal(envelope.StructuredOutput, &action); err != nil {
		return AdminAction{}, fmt.Errorf("decode admin action: %w", err)
	}
	return action, nil
}

func (a Admin) args(request string, world AdminContext) []string {
	args := []string{"run", "--rm", "-e", a.CredentialEnv, a.Image, "claude", "-p"}
	if a.Bare {
		args = append(args, "--bare")
	}
	args = append(args, "--output-format", "json", "--json-schema", adminSchema)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	return append(args, adminPrompt(request, world))
}

func adminPrompt(request string, world AdminContext) string {
	var b strings.Builder
	b.WriteString("You are roundclaw's admin. Turn the operator's request into one structured action.\n\n")

	section := func(title, body string) {
		fmt.Fprintf(&b, "%s:\n", title)
		if strings.TrimSpace(body) == "" {
			b.WriteString("- (none yet)\n")
		} else {
			b.WriteString(body)
		}
		b.WriteString("\n")
	}
	section("Existing agents", world.Agents)
	section("Existing schedules", world.Schedules)
	section("Existing workflows", world.Workflows)
	section("Registered tools (grantable to agents)", world.Tools)

	fmt.Fprintf(&b, "The request was sent from Discord channel %s. When the operator says \"here\" or\n"+
		"\"this channel\", use that ID.\n", world.CurrentChannelID)

	if strings.TrimSpace(world.History) != "" {
		b.WriteString("\nConversation so far (oldest first) — resolve follow-ups against it:\n")
		b.WriteString(world.History)
		b.WriteString("\n")
	}

	b.WriteString(`
An AGENT is a persistent, conversational bot bound to Discord channels. A
WORKFLOW is an agent-less, multi-step automation: a pipeline of prompts run in
order, each receiving the earlier steps' outputs — no agent, no channel binding.
A SCHEDULE runs a prompt on an existing agent on a cron.

Actions:
- create_agent: a new agent. Short lowercase id ([a-z0-9_-]), a one-line
  description, permission_mode (default acceptEdits). Default require_mention and
  reply_in_thread to true unless told otherwise. Only set channels to IDs given.
- update_agent: change an EXISTING agent (agent.id must be listed above). Set
  ONLY the fields to change; the rest are kept. Settable: description, agent_name
  (a native subagent persona), work_dir (an absolute host path, or empty for the
  managed workspace), permission_mode, require_mention, reply_in_thread, channels,
  allowed_tools.
- delete_agent: remove an agent. Put its id in "target".
- enable_agent / disable_agent: turn an agent on or off. Put its id in "target".
- show_agent / list_agents: report an agent's full settings, or all agents. For
  show_agent put the id in "target".
- show_persona: show an agent's CLAUDE.md — its instructions/persona (a file,
  separate from settings). Put the id in "target".
- set_persona: replace an agent's CLAUDE.md. Set agent.id and agent.persona (the
  full new instructions text). Use this for "change dev's persona to ...".
- create_agent may also set agent.persona to give the new agent its CLAUDE.md.
- create_schedule: recurring work on an EXISTING agent (agent_id must be listed).
  cron, timezone (default Asia/Seoul), prompt, channel_id. A schedule always runs
  on an agent — there is no agent-less schedule. If asked for one without an
  agent, clarify and explain, and suggest a workflow instead if it is multi-step
  or standalone.
- create_workflow: an agent-less pipeline. Set an id, an optional channel_id to
  report the final result into, and steps (each a prompt; optional name,
  permission_mode, model). Use this when the operator wants standalone or
  multi-step automation with no agent.
- run_workflow: run one now. Put its id in "target".
- delete_workflow / list_workflows: remove a workflow (id in "target"), or list.
- A TOOL is a local capability (a CLI and its config) an agent can be granted —
  e.g. an "outline" tool that lets an agent read and write the local Outline. The
  operator registers tools; you can only grant, revoke, or list them.
- attach_tool: grant a registered tool to an agent. Put the agent id in "target"
  and the tool id (must be listed under "Registered tools") in "tool". Takes
  effect on the agent's next turn.
- detach_tool: revoke a tool from an agent. agent id in "target", tool id in "tool".
- list_tools: report the registered tools. If the operator asks to register a NEW
  tool, or names a tool not listed, clarify — registering a tool names a host path
  and is done by the operator, not here.
- clarify: ambiguous, or references something not listed. Explain in reason.
  Do not loop on clarify — if you have enough to act, act.
- Never invent an id that is not listed above.
- Write "reason" in the same language as the request (Korean if it is Korean).

Request:
`)
	b.WriteString(request)
	return b.String()
}
