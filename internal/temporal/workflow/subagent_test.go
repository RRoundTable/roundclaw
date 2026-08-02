package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

func newEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(SubAgent)
	env.RegisterActivity(&activity.Activities{})
	return env
}

func request(turnID int64, text string) core.Request {
	return core.Request{
		AgentID:   "test-agent",
		RequestID: text,
		TurnID:    turnID,
		Text:      text,
		Origin:    core.DiscordOrigin("chan-1", "msg-"+text),
	}
}

// This is the nanoclaw regression. nanoclaw collapses messages that arrive
// during a run into a single boolean flag, so a burst produces one merged
// reply. Every request must produce its own turn and its own delivery.
func TestBurstProducesOneTurnAndOneDeliveryPerRequest(t *testing.T) {
	env := newEnv(t)

	var (
		mu       sync.Mutex
		runTurns []int64
		delivers []int64
	)

	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunTurnInput) (core.TurnResult, error) {
			mu.Lock()
			runTurns = append(runTurns, in.TurnID)
			mu.Unlock()
			return core.TurnResult{TurnID: in.TurnID, Status: core.TurnDone, Text: "ok"}, nil
		})

	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.DeliverInput) error {
			mu.Lock()
			delivers = append(delivers, in.Result.TurnID)
			mu.Unlock()
			return nil
		})

	// Three requests land back to back, the way a user firing off three Discord
	// messages would deliver them.
	for i, id := range []int64{1, 2, 3} {
		req := request(id, "req")
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalEnqueue, req)
		}, time.Duration(i)*time.Millisecond)
	}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalStop, StopSignal{ClearQueue: true, Reason: "test done"})
		env.CancelWorkflow()
	}, time.Minute)

	env.ExecuteWorkflow(SubAgent, Input{AgentID: "test-agent"})

	mu.Lock()
	defer mu.Unlock()
	if len(runTurns) != 3 {
		t.Fatalf("ran %d turns (%v), want exactly 3 — requests were merged", len(runTurns), runTurns)
	}
	if len(delivers) != 3 {
		t.Fatalf("delivered %d responses (%v), want exactly 3", len(delivers), delivers)
	}
	for i, want := range []int64{1, 2, 3} {
		if runTurns[i] != want {
			t.Errorf("turn %d ran id %d, want %d (FIFO order broken)", i, runTurns[i], want)
		}
		if delivers[i] != want {
			t.Errorf("delivery %d was for turn %d, want %d", i, delivers[i], want)
		}
	}
}

// A failed turn must still tell the requester what happened rather than going
// silent, and must not stall the queue behind it.
func TestFailedTurnStillDeliversAndQueueContinues(t *testing.T) {
	env := newEnv(t)

	var (
		mu        sync.Mutex
		delivered []core.TurnStatus
	)

	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunTurnInput) (core.TurnResult, error) {
			if in.TurnID == 1 {
				return core.TurnResult{TurnID: 1, Status: core.TurnError, ErrorMessage: "boom"}, nil
			}
			return core.TurnResult{TurnID: in.TurnID, Status: core.TurnDone, Text: "ok"}, nil
		})
	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.DeliverInput) error {
			mu.Lock()
			delivered = append(delivered, in.Result.Status)
			mu.Unlock()
			return nil
		})

	for i, id := range []int64{1, 2} {
		req := request(id, "req")
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalEnqueue, req)
		}, time.Duration(i)*time.Millisecond)
	}
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)

	env.ExecuteWorkflow(SubAgent, Input{AgentID: "test-agent"})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("delivered %d responses, want 2 — a failed turn blocked the queue", len(delivered))
	}
	if delivered[0] != core.TurnError {
		t.Errorf("first delivery status = %q, want %q", delivered[0], core.TurnError)
	}
	if delivered[1] != core.TurnDone {
		t.Errorf("second delivery status = %q, want %q", delivered[1], core.TurnDone)
	}
}

// /stop with ClearQueue must drop everything still waiting, not just the turn
// in flight.
func TestStopClearsQueuedRequests(t *testing.T) {
	env := newEnv(t)

	var (
		mu   sync.Mutex
		runs int
	)
	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunTurnInput) (core.TurnResult, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			return core.TurnResult{TurnID: in.TurnID, Status: core.TurnDone}, nil
		})
	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).Return(nil)

	// Queue three, then stop before any of them can be dequeued.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalStop, StopSignal{ClearQueue: true, Reason: "user stop"})
		env.SignalWorkflow(SignalEnqueue, request(1, "a"))
		env.SignalWorkflow(SignalEnqueue, request(2, "b"))
		env.SignalWorkflow(SignalStop, StopSignal{ClearQueue: true, Reason: "user stop"})
	}, time.Millisecond)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)

	env.ExecuteWorkflow(SubAgent, Input{AgentID: "test-agent"})

	mu.Lock()
	defer mu.Unlock()
	if runs > 1 {
		t.Errorf("ran %d turns after a clearing stop; queued work was not dropped", runs)
	}
}

func TestStatusQueryReportsQueueDepth(t *testing.T) {
	env := newEnv(t)

	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(core.TurnResult{TurnID: 1, Status: core.TurnDone}, nil)
	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(QueryStatus)
		if err != nil {
			t.Errorf("query: %v", err)
			return
		}
		var st Status
		if err := val.Get(&st); err != nil {
			t.Errorf("decode status: %v", err)
			return
		}
		if st.AgentID != "test-agent" {
			t.Errorf("agent id = %q", st.AgentID)
		}
		if st.State != core.AgentIdle {
			t.Errorf("state = %q, want idle before any request", st.State)
		}
	}, time.Millisecond)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)

	env.ExecuteWorkflow(SubAgent, Input{AgentID: "test-agent"})
}

// The seam counter bounds one run's history, so the continued run — which starts
// with no history at all — must be handed a reset one.
//
// Carrying it forward made shouldContinue true again on the continued run's very
// first loop. An agent that reached the limit with anything queued therefore
// continued as new about once a second, forever, and never ran the turn that was
// waiting. It was found in production doing exactly that: a pm thread had a
// delegation result queued behind a counter stuck at the limit.
func TestContinueAsNewResetsTheSeamCounter(t *testing.T) {
	env := newEnv(t)

	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(core.TurnResult{TurnID: 1, Status: core.TurnDone}, nil)
	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).Return(nil)

	// At the limit with work waiting: the exact state that span.
	env.ExecuteWorkflow(SubAgent, Input{
		AgentID:   "test-agent",
		TurnCount: maxTurnsPerRun,
		Queue:     []core.Request{request(1, "waiting")},
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not finish")
	}

	var can *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &can) {
		t.Fatalf("workflow did not continue as new: %v", env.GetWorkflowError())
	}

	var next Input
	if err := converter.GetDefaultDataConverter().FromPayloads(can.Input, &next); err != nil {
		t.Fatalf("decode continue-as-new input: %v", err)
	}
	if next.TurnCount != 0 {
		t.Errorf("continued run starts at turn_count %d; it would continue as new "+
			"again immediately and spin without ever running the queued turn", next.TurnCount)
	}
	if len(next.Queue) != 1 || next.Queue[0].TurnID != 1 {
		t.Errorf("the queued request was lost across the seam: %+v", next.Queue)
	}
}
