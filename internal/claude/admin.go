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
	AdminCreateAgent    AdminActionKind = "create_agent"
	AdminCreateSchedule AdminActionKind = "create_schedule"
	AdminListAgents     AdminActionKind = "list_agents"
	AdminListSchedules  AdminActionKind = "list_schedules"
	// AdminClarify asks the user for more detail. It is the safe default when the
	// request is ambiguous — a wrong create is easy to delete, but confirming
	// first is friendlier than guessing.
	AdminClarify AdminActionKind = "clarify"
)

// AdminAgentSpec is the subset of an agent an admin request can set.
type AdminAgentSpec struct {
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	RequireMention *bool    `json:"require_mention,omitempty"`
	ReplyInThread  *bool    `json:"reply_in_thread,omitempty"`
	Channels       []string `json:"channels,omitempty"`
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

// AdminAction is the planner's structured answer.
type AdminAction struct {
	Action   AdminActionKind    `json:"action"`
	Reason   string             `json:"reason,omitempty"`
	Agent    *AdminAgentSpec    `json:"agent,omitempty"`
	Schedule *AdminScheduleSpec `json:"schedule,omitempty"`
}

const adminSchema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["create_agent", "create_schedule", "list_agents", "list_schedules", "clarify"]},
    "reason": {"type": "string"},
    "agent": {
      "type": "object",
      "properties": {
        "id": {"type": "string"},
        "description": {"type": "string"},
        "permission_mode": {"type": "string", "enum": ["default", "acceptEdits", "bypassPermissions", "plan"]},
        "require_mention": {"type": "boolean"},
        "reply_in_thread": {"type": "boolean"},
        "channels": {"type": "array", "items": {"type": "string"}}
      }
    },
    "schedule": {
      "type": "object",
      "properties": {
        "id": {"type": "string"},
        "agent_id": {"type": "string"},
        "cron": {"type": "string"},
        "timezone": {"type": "string"},
        "prompt": {"type": "string"},
        "channel_id": {"type": "string"},
        "suppress_if": {"type": "string"}
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
	Agents           []AgentSummary
	CurrentChannelID string
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

	b.WriteString("Existing agents:\n")
	if len(world.Agents) == 0 {
		b.WriteString("- (none yet)\n")
	}
	for _, a := range world.Agents {
		fmt.Fprintf(&b, "- %s", a.ID)
		if a.Description != "" {
			fmt.Fprintf(&b, ": %s", a.Description)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nThe request was sent from Discord channel %s. When the operator says \"here\" or\n"+
		"\"this channel\", use that ID.\n", world.CurrentChannelID)

	if strings.TrimSpace(world.History) != "" {
		b.WriteString("\nConversation so far (oldest first) — resolve follow-ups against it:\n")
		b.WriteString(world.History)
		b.WriteString("\n")
	}

	b.WriteString(`
Rules:
- create_agent: the operator wants a new agent. Set a short lowercase id
  ([a-z0-9_-]), a one-line description, and permission_mode (default acceptEdits).
  Default require_mention and reply_in_thread to true unless they say otherwise.
  Only set channels to Discord channel IDs the operator actually gave.
- create_schedule: recurring work for an EXISTING agent (agent_id must be one
  listed above). Set a cron expression, a timezone (default Asia/Seoul), the
  prompt to run, and channel_id to report into.
- list_agents / list_schedules: they are asking what exists.
- clarify: the request is ambiguous or references an agent that does not exist.
  Put the question in reason. Prefer this over guessing.
- Never invent an agent_id that is not in the list above.
- Write "reason" in the same language as the request (Korean if it is Korean).

Request:
`)
	b.WriteString(request)
	return b.String()
}
