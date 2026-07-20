package core

import (
	"fmt"
	"strings"
	"time"
)

// Request is what every inbound adapter produces and what the workflow queues.
// It travels as a Temporal signal payload, so every field must stay
// JSON-serialisable and additive-only: a signal written by an old binary has to
// stay readable by a new one.
type Request struct {
	// AgentID selects the SubAgentWorkflow. It is also the workspace directory
	// name, so it must be filesystem-safe.
	AgentID string `json:"agent_id"`

	// RequestID deduplicates. Discord supplies its message ID; HTTP supplies a
	// hash of the Idempotency-Key. It is not optional — dedup is what stops a
	// client retry from becoming a second agent run.
	RequestID string `json:"request_id"`

	// TurnID is the turns row the adapter already created. The adapter, not the
	// workflow, owns this insert: HTTP has to return a turn_id in its 202
	// before any workflow code runs, and a workflow cannot touch SQLite without
	// breaking determinism.
	TurnID int64 `json:"turn_id"`

	Text   string `json:"text"`
	Origin Origin `json:"origin"`

	ReceivedAt time.Time `json:"received_at"`

	// SuppressIf, when the result contains it, stops delivery. Set by
	// schedules: a daily job that usually has nothing to say would otherwise
	// post "nothing to report" every morning, and people stop reading a channel
	// that does that. The turn is still recorded, so the run is auditable.
	SuppressIf string `json:"suppress_if,omitempty"`
}

// Validate checks a Request at the adapter boundary.
func (r Request) Validate() error {
	if err := ValidateAgentID(r.AgentID); err != nil {
		return err
	}
	if r.RequestID == "" {
		return fmt.Errorf("request: request_id is required")
	}
	if r.TurnID <= 0 {
		return fmt.Errorf("request: turn_id must be set by the adapter before signalling")
	}
	if strings.TrimSpace(r.Text) == "" {
		return fmt.Errorf("request: text is empty")
	}
	return r.Origin.Validate()
}

// ValidateAgentID rejects an agent ID that would escape the workspace root.
// This runs before the ID is ever joined into a path or a workflow ID.
func ValidateAgentID(id string) error {
	if id == "" {
		return fmt.Errorf("agent id is required")
	}
	if len(id) > 64 {
		return fmt.Errorf("agent id %q is longer than 64 characters", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return fmt.Errorf("agent id %q contains disallowed character %q; use [A-Za-z0-9_-]", id, r)
		}
	}
	return nil
}

// TurnStatus is the lifecycle of a single agent turn, mirrored in turns.status.
type TurnStatus string

const (
	TurnRunning TurnStatus = "running"
	TurnDone    TurnStatus = "done"
	TurnStopped TurnStatus = "stopped"
	TurnError   TurnStatus = "error"
)

// AgentStatus is the coarse lifecycle shown by /status, mirrored in
// agent_runtime.status. The workflow owns the authoritative version; this copy
// exists so the gateway can answer without touching Temporal.
type AgentStatus string

const (
	AgentIdle     AgentStatus = "idle"
	AgentRunning  AgentStatus = "running"
	AgentStopping AgentStatus = "stopping"
	AgentFailed   AgentStatus = "failed"
)

// LogKind classifies a live_logs row. These map onto the stream-json event
// types the CLI emits, flattened to what a human actually wants to read.
type LogKind string

const (
	LogText       LogKind = "text"
	LogToolUse    LogKind = "tool_use"
	LogToolResult LogKind = "tool_result"
	LogAPIRetry   LogKind = "api_retry"
	LogSystem     LogKind = "system"
)

// TurnResult is what RunClaudeTurn returns and what DeliverResponse consumes.
type TurnResult struct {
	TurnID  int64      `json:"turn_id"`
	Status  TurnStatus `json:"status"`
	Text    string     `json:"text"`
	CostUSD float64    `json:"cost_usd"`
	// ErrorMessage is set when Status is TurnError. It is delivered to the
	// caller, so it must stay free of credentials and absolute host paths.
	ErrorMessage string `json:"error_message,omitempty"`

	// SessionEstablished reports that the CLI actually opened a session this
	// turn — it emitted an init event.
	//
	// The workflow decides --session-id versus --resume from this rather than
	// from a turn counter. Counting turns looked equivalent until a turn failed
	// before the CLI ever started: the count advanced anyway, every later turn
	// tried to resume a session that was never created, and the agent was
	// permanently wedged by one early failure.
	SessionEstablished bool `json:"session_established,omitempty"`
}
