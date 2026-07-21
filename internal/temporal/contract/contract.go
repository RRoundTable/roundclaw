// Package contract holds the identifiers the workflow and the activities both
// need to agree on.
//
// It exists to break an import cycle. The workflow references activity methods
// to execute them; the scheduled-request activity needs to signal the agent
// workflow, which means knowing its ID, its input type and its signal name.
// Putting those three in a leaf package neither side owns is what lets both
// compile — and they genuinely are a shared contract rather than either side's
// private business, since renaming one breaks in-flight workflows.
package contract

import "github.com/roundtable/roundclaw/internal/core"

// Signal and query names.
const (
	SignalEnqueue = "enqueue"
	SignalStop    = "stop"
	SignalSteer   = "steer"
	QueryStatus   = "status"

	// AgentWorkflowType is the registered name of the agent workflow. Used as a
	// string rather than a function reference so that activities can start it
	// without importing the package that defines it.
	AgentWorkflowType = "SubAgent"

	// ScheduledWorkflowType is what a Temporal schedule starts on each firing.
	ScheduledWorkflowType = "ScheduledRequest"
)

// ScheduledInput names which schedule fired. Only the ID travels: the
// definition is read at fire time, so editing a schedule takes effect on its
// next run without recreating anything in Temporal.
type ScheduledInput struct {
	ScheduleID string `json:"schedule_id"`
}

// WorkflowID builds the deterministic workflow ID for one conversation.
//
// A conversation, not an agent, is the unit that owns a Claude session, a queue
// and a workspace. An empty conversationID means the agent's default
// conversation — the one that /ask, schedules and webhooks use, since none of
// them arrive with a thread.
//
// The Claude session UUID is derived from this string, so its format must stay
// stable: changing it orphans every existing session. The default form is
// deliberately byte-identical to what it was before conversations existed, so
// agents already running kept their sessions when this was introduced.
func WorkflowID(agentID, conversationID string) string {
	if conversationID == "" {
		return "roundclaw-agent-" + agentID
	}
	return "roundclaw-conv-" + agentID + "-" + conversationID
}

// AgentInput starts or continues an agent workflow.
type AgentInput struct {
	AgentID string `json:"agent_id"`

	// ConversationID is the Discord thread this workflow serves, or empty for
	// the agent's default conversation. It selects the Claude session and the
	// workspace, so two threads run in parallel without sharing either.
	ConversationID string `json:"conversation_id,omitempty"`

	// Queue carries requests across Continue-As-New so nothing is dropped at
	// the boundary.
	Queue []core.Request `json:"queue,omitempty"`

	// TurnCount is cumulative across Continue-As-New and is reported by the
	// status query. It does not decide --session-id versus --resume.
	TurnCount int `json:"turn_count,omitempty"`

	// SessionReady records that some turn actually opened the Claude session.
	// It must survive Continue-As-New or the agent would try to create an
	// already-existing session after every history truncation.
	SessionReady bool `json:"session_ready,omitempty"`

	// SessionLost carries a pending recap across Continue-As-New, so a
	// truncation landing between the loss and the next turn does not swallow
	// the one chance to rebuild context.
	SessionLost bool `json:"session_lost,omitempty"`
}
