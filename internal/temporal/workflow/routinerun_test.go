package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

type routineHarness struct {
	env      *testsuite.TestWorkflowEnvironment
	mu       sync.Mutex
	finished []registry.RunOutcome
	delivers int
	steps    []activity.RoutineStepInput
	skipped  []registry.StepRun
}

func newRoutineEnv(t *testing.T, plan activity.RoutineRunPlan, step func(context.Context, activity.RoutineStepInput) (activity.RoutineStepOutcome, error)) *routineHarness {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	h := &routineHarness{env: s.NewTestWorkflowEnvironment()}
	h.env.RegisterWorkflow(RoutineRun)
	h.env.RegisterActivity(&activity.Activities{})

	h.env.OnActivity("LoadRoutineRun", mock.Anything, mock.Anything).Return(plan, nil)
	h.env.OnActivity("RunRoutineStep", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
			h.mu.Lock()
			h.steps = append(h.steps, in)
			h.mu.Unlock()
			return step(ctx, in)
		})
	h.env.OnActivity("SkipRoutineStep", mock.Anything, mock.Anything).Return(
		func(_ context.Context, row registry.StepRun) error {
			h.mu.Lock()
			h.skipped = append(h.skipped, row)
			h.mu.Unlock()
			return nil
		})
	h.env.OnActivity("FinishRoutineRun", mock.Anything, mock.Anything).Return(
		func(_ context.Context, in activity.FinishRoutineRunInput) (registry.RoutineRun, error) {
			h.mu.Lock()
			h.finished = append(h.finished, in.Outcome)
			h.mu.Unlock()
			return registry.RoutineRun{ID: in.RunID, Status: in.Outcome.Status}, nil
		})
	h.env.OnActivity("DeliverRoutineRun", mock.Anything, mock.Anything).Return(
		func(context.Context, int64) error {
			h.mu.Lock()
			h.delivers++
			h.mu.Unlock()
			return nil
		})
	return h
}

func twoStepPlan(goalAttempts int) activity.RoutineRunPlan {
	return activity.RoutineRunPlan{
		Run: registry.RoutineRun{ID: 1, RoutineID: "r", Version: 1, Inputs: map[string]string{"x": "in"}},
		Plan: routine.Plan{
			Source: routine.Source{ID: "r",
				Contract: routine.Contract{Intent: "i", Done: routine.Condition{MustContain: []string{"done"}}},
				Steps: []routine.Step{
					{ID: "a", Kind: routine.KindAgent, Agent: "dev", Task: "t", MaxAttempts: 1, TimeoutSec: 60},
					{ID: "b", Kind: routine.KindAgent, Agent: "dev", Goal: "g", After: []string{"a"},
						Done: routine.Condition{MustContain: []string{"done"}}, MaxAttempts: goalAttempts, TimeoutSec: 60},
				}},
			Order: []string{"a", "b"},
		},
	}
}

func pass(out string) activity.RoutineStepOutcome {
	return activity.RoutineStepOutcome{Status: registry.RunPassed, Output: out}
}

func TestRoutineRunPassesEveryStepAndDelivers(t *testing.T) {
	h := newRoutineEnv(t, twoStepPlan(3), func(_ context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		if in.Step.ID == "b" {
			assert.Equal(t, "from a", in.Outputs["a"], "a step sees what the steps it waits for produced")
			return pass("all done"), nil
		}
		return pass("from a"), nil
	})
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	require.Len(t, h.finished, 1)
	assert.Equal(t, registry.RunPassed, h.finished[0].Status)
	assert.Equal(t, "all done", h.finished[0].Output)
	assert.Equal(t, 1, h.delivers)
}

// An open step that falls short runs again, told why, until it passes.
func TestRoutineRunRetriesAnOpenStepWithFeedback(t *testing.T) {
	h := newRoutineEnv(t, twoStepPlan(3), func(_ context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		if in.Step.ID == "b" && in.Attempt < 3 {
			return activity.RoutineStepOutcome{Status: registry.RunFailed, Reason: `must_contain "done": missing "done"`}, nil
		}
		if in.Step.ID == "b" {
			return pass("done at last"), nil
		}
		return pass("a"), nil
	})
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.NoError(t, h.env.GetWorkflowError())
	var bAttempts []activity.RoutineStepInput
	for _, s := range h.steps {
		if s.Step.ID == "b" {
			bAttempts = append(bAttempts, s)
		}
	}
	require.Len(t, bAttempts, 3)
	assert.Empty(t, bAttempts[0].Feedback)
	assert.Contains(t, bAttempts[1].Feedback, `missing "done"`)
	assert.Equal(t, registry.RunPassed, h.finished[0].Status)
}

// A step that runs out of attempts fails the run, naming itself.
func TestRoutineRunFailsWhenAttemptsAreSpent(t *testing.T) {
	h := newRoutineEnv(t, twoStepPlan(2), func(_ context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		if in.Step.ID == "b" {
			return activity.RoutineStepOutcome{Status: registry.RunFailed, Reason: "never"}, nil
		}
		return pass("a"), nil
	})
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.NoError(t, h.env.GetWorkflowError())
	assert.Len(t, h.steps, 3, "a once, b twice")
	assert.Equal(t, registry.RunFailed, h.finished[0].Status)
	assert.Equal(t, "b", h.finished[0].FailedStep)
	assert.Equal(t, 1, h.delivers)
}

// A step whose predecessor failed is recorded as skipped, with the reason.
func TestRoutineRunSkipsWhatDependsOnAFailure(t *testing.T) {
	h := newRoutineEnv(t, twoStepPlan(3), func(_ context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		return activity.RoutineStepOutcome{Status: registry.RunError, Reason: "container failed"}, nil
	})
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.NoError(t, h.env.GetWorkflowError())
	require.Len(t, h.skipped, 1)
	assert.Equal(t, "b", h.skipped[0].StepID)
	assert.Contains(t, h.skipped[0].Error, `"a" did not pass`)
	assert.Equal(t, registry.RunError, h.finished[0].Status)
	assert.Equal(t, "a", h.finished[0].FailedStep)
}

// The routine is held to its own promise after its steps pass.
func TestRoutineRunChecksTheContract(t *testing.T) {
	h := newRoutineEnv(t, twoStepPlan(1), func(_ context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		// b's own condition is "done"; the contract also wants "done" — make b
		// pass on its own terms by stubbing, while its output lacks the word.
		return pass("finished"), nil
	})
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.NoError(t, h.env.GetWorkflowError())
	assert.Equal(t, registry.RunFailed, h.finished[0].Status)
	assert.Contains(t, h.finished[0].Reason, "own conditions")
}

// The caller's conditions are checked separately and recorded.
func TestRoutineRunChecksTheCallersConditions(t *testing.T) {
	plan := twoStepPlan(1)
	plan.Run.Expect = routine.Condition{MustContain: []string{"branch:"}}
	h := newRoutineEnv(t, plan, func(_ context.Context, in activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		return pass("done"), nil
	})
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.NoError(t, h.env.GetWorkflowError())
	assert.Equal(t, registry.RunPassed, h.finished[0].Status, "the routine kept its own promise")
	require.Len(t, h.finished[0].ExpectVerdicts, 1)
	assert.False(t, h.finished[0].ExpectVerdicts[0].Held, "but not the caller's")
}

// A run whose plan cannot load still closes out and delivers.
func TestRoutineRunThatCannotLoadStillReports(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(RoutineRun)
	env.RegisterActivity(&activity.Activities{})
	env.OnActivity("LoadRoutineRun", mock.Anything, mock.Anything).Return(activity.RoutineRunPlan{},
		temporal.NewNonRetryableApplicationError("gone", activity.NonRetryableType, errors.New("gone")))
	var got registry.RunOutcome
	env.OnActivity("FinishRoutineRun", mock.Anything, mock.Anything).Return(
		func(_ context.Context, in activity.FinishRoutineRunInput) (registry.RoutineRun, error) {
			got = in.Outcome
			return registry.RoutineRun{}, nil
		})
	delivered := false
	env.OnActivity("DeliverRoutineRun", mock.Anything, mock.Anything).Return(
		func(context.Context, int64) error { delivered = true; return nil })
	env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 9})
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, registry.RunError, got.Status)
	assert.True(t, delivered)
}

// Stopping a run closes it out as stopped and still delivers.
func TestRoutineRunStoppedStillClosesOut(t *testing.T) {
	h := newRoutineEnv(t, twoStepPlan(1), func(ctx context.Context, _ activity.RoutineStepInput) (activity.RoutineStepOutcome, error) {
		<-ctx.Done()
		return activity.RoutineStepOutcome{Status: registry.RunStopped}, ctx.Err()
	})
	h.env.RegisterDelayedCallback(func() { h.env.CancelWorkflow() }, time.Second)
	h.env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.True(t, h.env.IsWorkflowCompleted())
	require.Len(t, h.finished, 1)
	assert.Equal(t, registry.RunStopped, h.finished[0].Status)
	assert.Equal(t, 1, h.delivers)
	require.Len(t, h.skipped, 1, "the step that never started is recorded")
}

// An inner routine runs as its own run and its outcome becomes the step's.
func TestRoutineRunRunsAnInnerRoutine(t *testing.T) {
	outer := activity.RoutineRunPlan{
		Run: registry.RoutineRun{ID: 1, RoutineID: "outer", Version: 1},
		Plan: routine.Plan{Source: routine.Source{ID: "outer", Contract: routine.Contract{Intent: "i"},
			Steps: []routine.Step{{ID: "call", Kind: routine.KindRoutine, Routine: "inner", MaxAttempts: 1, TimeoutSec: 60}}},
			Order: []string{"call"}},
	}
	inner := activity.RoutineRunPlan{
		Run: registry.RoutineRun{ID: 2, RoutineID: "inner", Version: 1, ParentRunID: 1, Depth: 1},
		Plan: routine.Plan{Source: routine.Source{ID: "inner", Contract: routine.Contract{Intent: "j"},
			Steps: []routine.Step{{ID: "work", Kind: routine.KindAgent, Agent: "dev", Task: "t", MaxAttempts: 1, TimeoutSec: 60}}},
			Order: []string{"work"}},
	}
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(RoutineRun)
	env.RegisterActivity(&activity.Activities{})
	env.OnActivity("LoadRoutineRun", mock.Anything, int64(1)).Return(outer, nil)
	env.OnActivity("LoadRoutineRun", mock.Anything, int64(2)).Return(inner, nil)
	env.OnActivity("AdmitChildRun", mock.Anything, mock.Anything).Return(int64(2), nil)
	env.OnActivity("RunRoutineStep", mock.Anything, mock.Anything).Return(pass("inner result"), nil)
	var outcomes sync.Map
	env.OnActivity("FinishRoutineRun", mock.Anything, mock.Anything).Return(
		func(_ context.Context, in activity.FinishRoutineRunInput) (registry.RoutineRun, error) {
			outcomes.Store(in.RunID, in.Outcome)
			return registry.RoutineRun{}, nil
		})
	env.OnActivity("RoutineRunStatus", mock.Anything, int64(2)).Return(
		registry.RoutineRun{ID: 2, Status: registry.RunPassed, Output: "inner result"}, nil)
	env.OnActivity("RecordRoutineStep", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("DeliverRoutineRun", mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(RoutineRun, RoutineRunInput{RunID: 1})
	require.NoError(t, env.GetWorkflowError())
	o, ok := outcomes.Load(int64(1))
	require.True(t, ok)
	assert.Equal(t, registry.RunPassed, o.(registry.RunOutcome).Status)
	assert.Equal(t, "inner result", o.(registry.RunOutcome).Output)
	_, innerFinished := outcomes.Load(int64(2))
	assert.True(t, innerFinished, "the inner run closes itself out")
}
