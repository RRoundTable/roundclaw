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

// DefaultConversation is the sentinel that stands in for an empty conversation
// ID — the agent's default conversation, the one /ask, schedules and webhooks
// use, since none of them arrive with a thread. It gives every workflow ID the
// same three-part shape rather than a special form for the default.
//
// It can never collide with a real conversation ID: a thread's ID is its
// Discord channel snowflake, which is all digits, so no thread is ever named
// "default".
const DefaultConversation = "default"

// WorkflowID builds the deterministic workflow ID for one conversation.
//
// A conversation, not an agent, is the unit that owns a Claude session, a queue
// and a workspace. Every ID has the one shape `roundclaw-<agentID>-<conv>`; the
// default conversation uses the DefaultConversation sentinel rather than a
// second format.
//
// The Claude session UUID is derived from this string, so changing this format
// orphans every existing session — an accepted, one-time cost of unifying the
// scheme. It must not drift again afterwards.
func WorkflowID(agentID, conversationID string) string {
	if conversationID == "" {
		conversationID = DefaultConversation
	}
	return "roundclaw-" + agentID + "-" + conversationID
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
	// status query. It decides nothing.
	//
	// Nothing about the Claude session belongs here either. The workflow used to
	// carry whether the session had been opened, and a turn that failed before
	// the container started was read as the session being gone — after which
	// every later turn tried to create one that already existed, for good, since
	// Continue-As-New carried the mistake forward as faithfully as it would have
	// carried the truth. Whether to resume is now decided from the transcript on
	// disk, by the activity that can see it.
	TurnCount int `json:"turn_count,omitempty"`
}
