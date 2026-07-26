package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

func newEvalEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(EvalRun)
	env.RegisterActivity(&activity.Activities{})
	return env
}

func plan(cases ...registry.EvalCase) activity.EvalPlan {
	return activity.EvalPlan{
		Run:     registry.EvalRun{ID: 7, EvalSetID: "dev-basic", AgentID: "dev", Version: 3},
		Set:     registry.EvalSet{ID: "dev-basic", AgentID: "dev", Cases: cases, Enabled: true},
		Agent:   registry.Agent{ID: "dev", Enabled: true},
		Persona: "you are dev",
		Version: 3,
	}
}

// A case that never ran must still be recorded. A missing row reads as a case
// that was never asked, and the next comparison would report it as removed
// rather than as broken.
func TestAFailedCaseIsRecordedAsAZeroAndTheRunContinues(t *testing.T) {
	env := newEvalEnv(t)

	var (
		mu       sync.Mutex
		recorded []registry.EvalResult
	)
	env.OnActivity("LoadEvalPlan", mock.Anything, mock.Anything).
		Return(plan(
			registry.EvalCase{Name: "one", Prompt: "p1"},
			registry.EvalCase{Name: "two", Prompt: "p2"},
		), nil)

	env.OnActivity("RunEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunCaseInput) (activity.CaseOutcome, error) {
			if in.Case.Name == "one" {
				return activity.CaseOutcome{}, errors.New("container would not start")
			}
			return activity.CaseOutcome{Output: "an answer", CostUSD: 0.01}, nil
		})
	env.OnActivity("RecordEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, r registry.EvalResult) error {
			mu.Lock()
			defer mu.Unlock()
			recorded = append(recorded, r)
			return nil
		})
	env.OnActivity("CleanEvalWorkspaces", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("FinishEvalRun", mock.Anything, mock.Anything).
		Return(registry.EvalRun{ID: 7, Status: registry.EvalDone, Passed: 1, Total: 2}, nil)
	env.OnActivity("NotifyEvalRun", mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(EvalRun, EvalRunInput{RunID: 7})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a failing case must not fail the run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 2 {
		t.Fatalf("recorded %d results, want 2 — both cases must leave a row", len(recorded))
	}
	byName := map[string]registry.EvalResult{}
	for _, r := range recorded {
		byName[r.CaseName] = r
	}
	if got := byName["one"]; got.Passed || got.Score != 0 || got.Reason == "" {
		t.Errorf("the failed case = %+v; want a zero carrying the reason", got)
	}
	if got := byName["two"]; !got.Passed {
		t.Errorf("the case after the failure = %+v; want it to have run and passed", got)
	}
}

// An exact rule is not a matter of opinion. A case that breaks must_contain
// fails without a judge ever being asked — which also means it costs nothing.
func TestABrokenAssertionSkipsTheJudge(t *testing.T) {
	env := newEvalEnv(t)

	judged := false
	env.OnActivity("LoadEvalPlan", mock.Anything, mock.Anything).
		Return(plan(registry.EvalCase{
			Name: "must-cite", Prompt: "p", Rubric: "does it cite the file",
			MustContain: []string{"main.go"},
		}), nil)
	env.OnActivity("RunEvalCase", mock.Anything, mock.Anything).
		Return(activity.CaseOutcome{
			Output:           "I could not find it",
			AssertionFailure: `the answer never mentions "main.go"`,
		}, nil)
	env.OnActivity("JudgeEvalCase", mock.Anything, mock.Anything).
		Return(func(context.Context, activity.JudgeInput) (activity.Judgement, error) {
			judged = true
			return activity.Judgement{Score: 1, Passed: true}, nil
		})

	var recorded registry.EvalResult
	env.OnActivity("RecordEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, r registry.EvalResult) error {
			recorded = r
			return nil
		})
	env.OnActivity("CleanEvalWorkspaces", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("FinishEvalRun", mock.Anything, mock.Anything).Return(registry.EvalRun{ID: 7}, nil)
	env.OnActivity("NotifyEvalRun", mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(EvalRun, EvalRunInput{RunID: 7})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if judged {
		t.Error("a judge was asked about a case that already broke an exact rule")
	}
	if recorded.Passed || recorded.Reason == "" {
		t.Errorf("result = %+v; want a fail carrying the broken assertion", recorded)
	}
}

// A judge that cannot run leaves the answer unmarked rather than scored zero:
// the agent answered, and calling that a failure would invent a regression.
func TestAnUnreachableJudgeLeavesTheCaseUnmarked(t *testing.T) {
	env := newEvalEnv(t)

	env.OnActivity("LoadEvalPlan", mock.Anything, mock.Anything).
		Return(plan(registry.EvalCase{Name: "graded", Prompt: "p", Rubric: "is it right"}), nil)
	env.OnActivity("RunEvalCase", mock.Anything, mock.Anything).
		Return(activity.CaseOutcome{Output: "a fine answer"}, nil)
	env.OnActivity("JudgeEvalCase", mock.Anything, mock.Anything).
		Return(activity.Judgement{}, errors.New("judge image missing"))

	var recorded registry.EvalResult
	env.OnActivity("RecordEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, r registry.EvalResult) error {
			recorded = r
			return nil
		})
	env.OnActivity("CleanEvalWorkspaces", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("FinishEvalRun", mock.Anything, mock.Anything).Return(registry.EvalRun{ID: 7}, nil)
	env.OnActivity("NotifyEvalRun", mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(EvalRun, EvalRunInput{RunID: 7})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("an unmarked case must not fail the run: %v", err)
	}
	if recorded.Output != "a fine answer" {
		t.Errorf("the answer was discarded: %+v", recorded)
	}
	if recorded.Reason == "" {
		t.Error("an unmarked case must say why it was not marked")
	}
}

// A case with no rubric and no assertions is a smoke test: it asks only whether
// the agent can answer at all, and it must not need a judge to say so.
func TestACaseWithNoRubricPassesOnAnAnswer(t *testing.T) {
	env := newEvalEnv(t)

	env.OnActivity("LoadEvalPlan", mock.Anything, mock.Anything).
		Return(plan(registry.EvalCase{Name: "smoke", Prompt: "are you there"}), nil)
	env.OnActivity("RunEvalCase", mock.Anything, mock.Anything).
		Return(activity.CaseOutcome{Output: "yes", CostUSD: 0.002}, nil)

	var recorded registry.EvalResult
	env.OnActivity("RecordEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, r registry.EvalResult) error {
			recorded = r
			return nil
		})
	env.OnActivity("CleanEvalWorkspaces", mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("FinishEvalRun", mock.Anything, mock.Anything).Return(registry.EvalRun{ID: 7}, nil)
	env.OnActivity("NotifyEvalRun", mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(EvalRun, EvalRunInput{RunID: 7})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if !recorded.Passed || recorded.Score != 1 {
		t.Errorf("result = %+v; want a pass", recorded)
	}
}

// A disabled set must not spend anything, and must still close its run out —
// a row left at "running" describes work that will never happen.
func TestADisabledSetFinishesWithoutRunningAnything(t *testing.T) {
	env := newEvalEnv(t)

	p := plan(registry.EvalCase{Name: "one", Prompt: "p"})
	p.Set.Enabled = false
	env.OnActivity("LoadEvalPlan", mock.Anything, mock.Anything).Return(p, nil)
	env.OnActivity("RunEvalCase", mock.Anything, mock.Anything).
		Return(func(context.Context, activity.RunCaseInput) (activity.CaseOutcome, error) {
			t.Error("a disabled eval set ran a case")
			return activity.CaseOutcome{}, nil
		})

	finished := false
	env.OnActivity("FinishEvalRun", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.FinishEvalInput) (registry.EvalRun, error) {
			finished = true
			if in.Error == "" {
				t.Error("a skipped run must record why it produced nothing")
			}
			return registry.EvalRun{ID: 7, Status: registry.EvalFailed}, nil
		})
	env.OnActivity("NotifyEvalRun", mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(EvalRun, EvalRunInput{RunID: 7})

	if !finished {
		t.Error("a disabled set left its run row open")
	}
}
