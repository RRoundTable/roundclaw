package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/routine"
)

type allowAll struct{}

func (allowAll) AgentUsable(string) (bool, error)            { return true, nil }
func (allowAll) ImageAllowed(string) bool                    { return true }
func (allowAll) RoutineSteps(string) ([]string, bool, error) { return nil, true, nil }

func compiled(t *testing.T, intent, task string) routine.Plan {
	t.Helper()
	plan, err := routine.Compile(routine.Source{
		ID:       "summarise",
		Contract: routine.Contract{Intent: intent, Inputs: []string{"text"}},
		Steps:    []routine.Step{{ID: "write", Kind: routine.KindAgent, Agent: "dev", Task: task}},
	}, allowAll{}, routine.DefaultCaps)
	require.NoError(t, err)
	return plan
}

func TestPutRoutineVersionsAndFreezesTheContract(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	res, err := s.PutRoutine(ctx, compiled(t, "summarise text", "summarise {{inputs.text}}"), "dev", Change{Author: "agent:dev"})
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.True(t, res.Minted)
	assert.Equal(t, 1, res.Routine.Version)
	assert.Equal(t, "dev", res.Routine.Owner)

	// The same plan again mints nothing.
	res, err = s.PutRoutine(ctx, compiled(t, "summarise text", "summarise {{inputs.text}}"), "dev", Change{})
	require.NoError(t, err)
	assert.False(t, res.Minted)

	// A changed implementation mints v2.
	res, err = s.PutRoutine(ctx, compiled(t, "summarise text", "summarise {{inputs.text}} in one line"), "dev", Change{})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Routine.Version)

	// A changed contract is refused.
	_, err = s.PutRoutine(ctx, compiled(t, "summarise text loosely", "x {{inputs.text}}"), "dev", Change{})
	assert.True(t, errors.Is(err, ErrContractFrozen))

	// Rolling back is itself a change.
	res, err = s.RollbackRoutine(ctx, "summarise", 1, Change{Author: "person"})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Routine.Version)
	assert.Equal(t, "summarise {{inputs.text}}", res.Routine.Plan.Source.Steps[0].Task)

	decisions, err := s.ListRoutineDecisions(ctx, "summarise")
	require.NoError(t, err)
	var kinds []string
	for _, d := range decisions {
		kinds = append(kinds, d.Decision)
	}
	assert.Contains(t, kinds, "admitted")
	assert.Contains(t, kinds, "rolled_back")
}

func TestArchivedRoutinesLeaveTheLibraryButKeepTheirRecord(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.PutRoutine(ctx, compiled(t, "summarise text", "summarise {{inputs.text}}"), "", Change{})
	require.NoError(t, err)

	require.NoError(t, s.SetRoutineState(ctx, "summarise", RoutineArchived, "archived by a person", nil, "person"))
	active, err := s.ListRoutines(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, active)
	all, err := s.ListRoutines(ctx, false)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	require.NoError(t, s.SetRoutineState(ctx, "summarise", RoutineActive, "restored by a person", nil, "person"))
	active, err = s.ListRoutines(ctx, true)
	require.NoError(t, err)
	assert.Len(t, active, 1)
}

func TestRunTraceAndMetricsAreComputedFromRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.PutRoutine(ctx, compiled(t, "summarise text", "summarise {{inputs.text}}"), "", Change{})
	require.NoError(t, err)

	mk := func(status string, expect routine.Condition, verdicts []routine.Verdict, cost float64, stepStatus string) int64 {
		run, err := s.StartRoutineRun(ctx, RoutineRun{
			RoutineID: "summarise", Version: 1, PlanHash: "h", Origin: core.HTTPPollOrigin(),
			Inputs: map[string]string{"text": "x"}, Expect: expect,
		})
		require.NoError(t, err)
		require.NoError(t, s.StartStepRun(ctx, run.ID, "write", 1, "agent", ""))
		require.NoError(t, s.FinishStepRun(ctx, StepRun{
			RunID: run.ID, StepID: "write", Attempt: 1, Kind: "agent", Status: stepStatus, CostUSD: cost,
			Usage: core.Usage{Models: map[string]core.ModelUsage{"m": {InputTokens: 10, OutputTokens: 5}}},
		}))
		_, err = s.FinishRoutineRun(ctx, run.ID, RunOutcome{Status: status, ExpectVerdicts: verdicts})
		require.NoError(t, err)
		return run.ID
	}
	mk(RunPassed, routine.Condition{MustContain: []string{"a"}}, []routine.Verdict{{Held: true}}, 0.2, RunPassed)
	failed := mk(RunFailed, routine.Condition{}, nil, 0.4, RunFailed)
	mk(RunUnmarked, routine.Condition{}, nil, 0.1, RunPassed)
	// A later hand-off replaced the failed run.
	_, err = s.StartRoutineRun(ctx, RoutineRun{RoutineID: "summarise", Version: 1, PlanHash: "h",
		Origin: core.HTTPPollOrigin(), InsteadOf: failed})
	require.NoError(t, err)

	cur, life, err := s.RoutineMetrics(ctx, "summarise", 1)
	require.NoError(t, err)
	assert.Equal(t, 3, cur.Runs, "a run still in progress is not counted")
	require.NotNil(t, cur.OwnPassRate)
	assert.InDelta(t, 0.5, *cur.OwnPassRate, 1e-9, "unmarked is not a failure")
	assert.Equal(t, 1, cur.CallerStated)
	assert.Equal(t, 1, cur.CallerMet)
	assert.Equal(t, 1, cur.Redone)
	require.NotNil(t, cur.CostPerPassed)
	assert.InDelta(t, 0.7, *cur.CostPerPassed, 1e-9, "one success costs everything spent")
	assert.Equal(t, int64(45), cur.Tokens)
	require.Len(t, cur.Hotspots, 1)
	assert.Equal(t, "write", cur.Hotspots[0].Step)
	assert.Equal(t, cur.Runs, life.Runs)
}

func TestChildRunsAreUniquePerParentStepAndAttempt(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	child := RoutineRun{RoutineID: "inner", Version: 1, PlanHash: "h", Origin: core.HTTPPollOrigin(),
		ParentRunID: 7, ParentStep: "call", ParentAttempt: 1, Depth: 1}
	a, err := s.StartRoutineRun(ctx, child)
	require.NoError(t, err)
	b, err := s.StartRoutineRun(ctx, child)
	require.NoError(t, err)
	assert.Equal(t, a.ID, b.ID, "a retried start returns the first child run")
}

func TestAStepFinishedKeepsItsFirstOutcome(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	require.NoError(t, s.StartStepRun(ctx, 1, "a", 1, "agent", ""))
	require.NoError(t, s.FinishStepRun(ctx, StepRun{RunID: 1, StepID: "a", Attempt: 1, Kind: "agent", Status: RunPassed, Output: "first"}))
	require.NoError(t, s.FinishStepRun(ctx, StepRun{RunID: 1, StepID: "a", Attempt: 1, Kind: "agent", Status: RunFailed, Output: "second"}))
	steps, err := s.ListStepRuns(ctx, 1)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, "first", steps[0].Output)
}
