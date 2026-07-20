package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

// Workflow inspection.
//
// This is the other half of /status. SQLite says what the agent has been doing;
// Temporal says whether its workflow is actually alive, what it is waiting on,
// and whether an activity is retrying. When an agent looks stuck, the answer is
// usually here rather than in the transcript — a failing activity retries
// silently as far as the transcript is concerned.
type WorkflowInfo struct {
	AgentID    string `json:"agent_id"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at,omitempty"`
	// HistoryLength grows with every turn and is what Continue-As-New exists to
	// bound; a number climbing into the thousands means that is not happening.
	HistoryLength int64 `json:"history_length"`

	// Pending activity detail, present only while one is running. Attempt above
	// 1 is the signal that something is failing and being retried.
	ActivityType    string `json:"activity_type,omitempty"`
	ActivityAttempt int32  `json:"activity_attempt,omitempty"`
	ActivityState   string `json:"activity_state,omitempty"`
	LastFailure     string `json:"last_failure,omitempty"`

	// From the workflow's own query handler.
	QueueLength int `json:"queue_length"`
	TurnCount   int `json:"turn_count"`

	// Unavailable explains why the rest is empty rather than leaving a caller
	// to guess. An agent that has never run has no workflow, which is normal.
	Unavailable string `json:"unavailable,omitempty"`
}

// Workflow describes one agent's Temporal execution.
func (d *Dispatcher) Workflow(ctx context.Context, agentID string) (WorkflowInfo, error) {
	if _, err := d.reg.Get(ctx, agentID); err != nil {
		return WorkflowInfo{}, fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	}

	info := WorkflowInfo{AgentID: agentID, WorkflowID: rcworkflow.WorkflowID(agentID)}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	desc, err := d.tc.DescribeWorkflowExecution(ctx, info.WorkflowID, "")
	if err != nil {
		// Not an error to report upward: "this agent has never run" and
		// "Temporal is unreachable" both leave the caller with the SQLite half
		// of the picture, which is still worth returning.
		info.Unavailable = err.Error()
		return info, nil
	}

	if ex := desc.GetWorkflowExecutionInfo(); ex != nil {
		info.Status = strings.TrimPrefix(ex.GetStatus().String(), "WORKFLOW_EXECUTION_STATUS_")
		info.HistoryLength = ex.GetHistoryLength()
		if e := ex.GetExecution(); e != nil {
			info.RunID = e.GetRunId()
		}
		if t := ex.GetStartTime(); t != nil {
			info.StartedAt = t.AsTime().UTC().Format(time.RFC3339)
		}
	}

	for _, pa := range desc.GetPendingActivities() {
		info.ActivityType = pa.GetActivityType().GetName()
		info.ActivityAttempt = pa.GetAttempt()
		info.ActivityState = strings.TrimPrefix(pa.GetState().String(), "PENDING_ACTIVITY_STATE_")
		if f := pa.GetLastFailure(); f != nil {
			info.LastFailure = f.GetMessage()
		}
		break // one activity per agent by design; the first is the one
	}

	// The workflow's own view. Best-effort: a workflow mid-replay can refuse a
	// query, and that must not empty the rest of the report.
	if val, err := d.tc.QueryWorkflow(ctx, info.WorkflowID, "", rcworkflow.QueryStatus); err == nil {
		var st rcworkflow.Status
		if val.Get(&st) == nil {
			info.QueueLength = st.QueueLength
			info.TurnCount = st.TurnCount
		}
	}
	return info, nil
}
