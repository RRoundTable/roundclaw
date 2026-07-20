package workflow

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// After a resume finds nothing, the next turn starts a fresh session and must
// carry a recap — otherwise recovery costs the conversation silently, and the
// agent answers as though it had never spoken to anyone.
func TestTurnAfterALostSessionRebuildsContext(t *testing.T) {
	env := newEnv(t)

	type call struct {
		resume  bool
		rebuild bool
	}
	var (
		mu    sync.Mutex
		calls []call
	)

	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunTurnInput) (core.TurnResult, error) {
			mu.Lock()
			calls = append(calls, call{in.Resume, in.RebuildContext})
			n := len(calls)
			mu.Unlock()

			// The first turn resumes and finds nothing; the second opens a new
			// session; the third is an ordinary turn on that session.
			return core.TurnResult{
				TurnID: in.TurnID, Status: core.TurnDone,
				SessionEstablished: n > 1,
			}, nil
		})
	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).Return(nil)

	for i, id := range []int64{1, 2, 3} {
		req := request(id, "req")
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalEnqueue, req)
		}, time.Duration(i)*time.Millisecond)
	}
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)

	env.ExecuteWorkflow(SubAgent, Input{AgentID: "test-agent", SessionReady: true})

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("ran %d turns, want 3", len(calls))
	}
	if !calls[0].resume || calls[0].rebuild {
		t.Errorf("turn 1 = %+v; it should resume, with nothing to rebuild yet", calls[0])
	}
	if calls[1].resume || !calls[1].rebuild {
		t.Errorf("turn 2 = %+v; it should start a new session and carry a recap", calls[1])
	}
	// The recap belongs to the first turn of the new session. Repeating it
	// would pay for the same context on every turn afterwards.
	if calls[2].rebuild {
		t.Errorf("turn 3 = %+v; the recap should not repeat once the session is live", calls[2])
	}
	if !calls[2].resume {
		t.Errorf("turn 3 = %+v; it should resume the session turn 2 opened", calls[2])
	}
}

// A workflow continued mid-recovery must still rebuild: a truncation landing
// between the loss and the next turn would otherwise swallow the one chance.
func TestPendingRecapSurvivesContinueAsNew(t *testing.T) {
	env := newEnv(t)

	var (
		mu      sync.Mutex
		rebuild bool
	)
	env.OnActivity("RunClaudeTurn", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunTurnInput) (core.TurnResult, error) {
			mu.Lock()
			rebuild = in.RebuildContext
			mu.Unlock()
			return core.TurnResult{TurnID: in.TurnID, Status: core.TurnDone, SessionEstablished: true}, nil
		})
	env.OnActivity("DeliverResponse", mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalEnqueue, request(1, "req"))
	}, time.Millisecond)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)

	env.ExecuteWorkflow(SubAgent, Input{
		AgentID: "test-agent", TurnCount: 42, SessionLost: true,
	})

	mu.Lock()
	defer mu.Unlock()
	if !rebuild {
		t.Error("a workflow continued mid-recovery did not rebuild context")
	}
}
