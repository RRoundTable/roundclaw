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

// RouteAction is what the router decided to do with a message.
type RouteAction string

const (
	// RouteDispatch sends the message to the named agent.
	RouteDispatch RouteAction = "dispatch"
	// RouteIgnore drops it. Most chatter in a busy channel is not a request,
	// and answering all of it would be both expensive and annoying.
	RouteIgnore RouteAction = "ignore"
	// RouteClarify asks the user which agent they meant.
	RouteClarify RouteAction = "clarify"
)

// RouteDecision is the router's structured answer.
type RouteDecision struct {
	Action  RouteAction `json:"action"`
	AgentID string      `json:"agent_id,omitempty"`
	Reason  string      `json:"reason,omitempty"`
}

// AgentSummary describes one routable agent to the router.
type AgentSummary struct {
	ID          string
	Description string
}

// routerSchema constrains the router's output. Passing a schema is what makes
// this a parser-free call: the CLI validates the shape, so there is no prose to
// scrape and no retry loop to write here.
const routerSchema = `{
  "type": "object",
  "properties": {
    "action":   {"type": "string", "enum": ["dispatch", "ignore", "clarify"]},
    "agent_id": {"type": "string"},
    "reason":   {"type": "string"}
  },
  "required": ["action"]
}`

// Router picks an agent for a message that no channel binding covers.
//
// It is deliberately stateless: one short-lived `claude -p` per message, no
// session, no conversation. A router with a session would serialise every
// unrouted message behind the previous one — reintroducing, one layer up, the
// exact head-of-line blocking the agent queues were built to avoid.
//
// It never decides to stop or steer anything. The only destructive controls are
// explicit commands, so a routing mistake costs a wasted dispatch at worst,
// never lost work.
type Router struct {
	Runtime string
	Image   string
	Model   string
	// Timeout bounds the call. Routing sits in front of a human waiting in a
	// chat window, so it must fail fast rather than hang.
	Timeout time.Duration

	// CredentialEnv / CredentialValue mirror RunSpec: `claude` accepts either
	// an API key or a setup-token, selected by variable name.
	CredentialEnv   string
	CredentialValue string
}

// Route asks the router which agent should handle message.
func (r Router) Route(ctx context.Context, message string, agents []AgentSummary) (RouteDecision, error) {
	if len(agents) == 0 {
		return RouteDecision{Action: RouteIgnore, Reason: "no agents are configured"}, nil
	}
	if r.Timeout <= 0 {
		r.Timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	args := []string{
		"run", "--rm",
		"-e", r.CredentialEnv,
		r.Image,
		"claude", "-p",
		// --bare skips hooks, skills, plugins, MCP servers and CLAUDE.md
		// discovery. Routing needs none of that, and skipping it is most of
		// why this call is cheap enough to run per message.
		"--bare",
		"--output-format", "json",
		"--json-schema", routerSchema,
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, routerPrompt(message, agents))

	cmd := exec.CommandContext(ctx, r.Runtime, args...)
	cmd.Env = append(cmd.Environ(), r.CredentialEnv+"="+r.CredentialValue)

	out, err := cmd.Output()
	if err != nil {
		// A bare "exit status 1" is undiagnosable — an invalid key, an unknown
		// model and a missing image all look alike. `claude --output-format json`
		// reports failures as a JSON result on *stdout* (not stderr) and still
		// exits non-zero, so check there first, then fall back to stderr.
		if detail := failureDetail(out); detail != "" {
			return RouteDecision{}, fmt.Errorf("run router: %w: %s", err, detail)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return RouteDecision{}, fmt.Errorf("run router: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return RouteDecision{}, fmt.Errorf("run router: %w", err)
	}

	var envelope struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
		IsError          bool            `json:"is_error"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return RouteDecision{}, fmt.Errorf("decode router envelope: %w", err)
	}
	if envelope.IsError || len(envelope.StructuredOutput) == 0 {
		return RouteDecision{}, fmt.Errorf("router returned no structured output")
	}

	var decision RouteDecision
	if err := json.Unmarshal(envelope.StructuredOutput, &decision); err != nil {
		return RouteDecision{}, fmt.Errorf("decode router decision: %w", err)
	}
	return ValidateDecision(decision, agents), nil
}

// ValidateDecision clamps a decision to something safe to act on.
//
// A model naming an agent that does not exist is the failure mode that matters:
// acted on blindly it would create a workflow for a hallucinated ID. It is
// downgraded to clarify so a human resolves it.
func ValidateDecision(d RouteDecision, agents []AgentSummary) RouteDecision {
	switch d.Action {
	case RouteIgnore, RouteClarify:
		return d
	case RouteDispatch:
		for _, a := range agents {
			if a.ID == d.AgentID {
				return d
			}
		}
		return RouteDecision{
			Action: RouteClarify,
			Reason: fmt.Sprintf("the router chose %q, which is not a configured agent", d.AgentID),
		}
	default:
		return RouteDecision{
			Action: RouteClarify,
			Reason: fmt.Sprintf("the router returned an unknown action %q", d.Action),
		}
	}
}

// failureDetail pulls a human-readable reason out of a failed CLI run. On an
// API error `claude --output-format json` still prints its result envelope to
// stdout — `{"result":"Invalid API key ...","api_error_status":401}` — so that
// carries the real cause when the process exits non-zero.
func failureDetail(stdout []byte) string {
	var r struct {
		Result         string `json:"result"`
		APIErrorStatus int    `json:"api_error_status"`
	}
	if json.Unmarshal(stdout, &r) == nil && r.Result != "" {
		if r.APIErrorStatus != 0 {
			return fmt.Sprintf("%s (HTTP %d)", r.Result, r.APIErrorStatus)
		}
		return r.Result
	}
	return strings.TrimSpace(string(stdout))
}

func routerPrompt(message string, agents []AgentSummary) string {
	var b strings.Builder
	b.WriteString("You are a router. Decide which agent, if any, should handle a chat message.\n\n")
	b.WriteString("Available agents:\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "- %s", a.ID)
		if a.Description != "" {
			fmt.Fprintf(&b, ": %s", a.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- dispatch: the message is a request one of these agents clearly handles. Set agent_id.\n")
	b.WriteString("- ignore: ordinary conversation, or nothing an agent should act on. Prefer this when unsure.\n")
	b.WriteString("- clarify: plausibly a request, but you cannot tell which agent it belongs to.\n")
	b.WriteString("- Never invent an agent_id that is not listed above.\n")
	b.WriteString("\nMessage:\n")
	b.WriteString(message)
	return b.String()
}
