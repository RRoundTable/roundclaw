package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/roundtable/roundclaw/internal/routine"
)

func TestARoutineOnTrialServesOnlyOncePromoted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := compiled(t, "summarise text", "summarise {{inputs.text}}")
	res, err := s.PutRoutineWith(ctx, plan, "dev", Change{}, PutOptions{State: RoutineTrial, Promote: false})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Version)
	rt, err := s.GetRoutine(ctx, "summarise")
	require.NoError(t, err)
	assert.Equal(t, RoutineTrial, rt.State)
	active, err := s.ListRoutines(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, active, "a routine on trial is not offered")

	require.NoError(t, s.PromoteRoutineVersion(ctx, "summarise", 1, "did the work as well as before", map[string]any{"pass_rate": 1.0}, "lifecycle"))
	rt, err = s.GetRoutine(ctx, "summarise")
	require.NoError(t, err)
	assert.Equal(t, RoutineActive, rt.State)

	// A new version waits for its trial while v1 keeps serving.
	res, err = s.PutRoutineWith(ctx, compiled(t, "summarise text", "better {{inputs.text}}"), "dev", Change{}, PutOptions{Promote: false})
	require.NoError(t, err)
	rt, err = s.GetRoutine(ctx, "summarise")
	require.NoError(t, err)
	assert.Equal(t, 1, rt.Version)
	assert.Equal(t, 2, rt.PendingVersion)

	require.NoError(t, s.RejectRoutineVersion(ctx, "summarise", 2, "regressed on c1", nil))
	rt, err = s.GetRoutine(ctx, "summarise")
	require.NoError(t, err)
	assert.Equal(t, 1, rt.Version, "a rejected version never serves")
	assert.Zero(t, rt.PendingVersion)
	assert.Equal(t, RoutineActive, rt.State)

	decisions, err := s.ListRoutineDecisions(ctx, "summarise")
	require.NoError(t, err)
	var kinds []string
	for _, d := range decisions {
		kinds = append(kinds, d.Decision)
	}
	assert.Contains(t, kinds, "admitted")
	assert.Contains(t, kinds, "rejected")
}

func TestARoutineThatFailsAdmissionIsRejectedButKept(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.PutRoutineWith(ctx, compiled(t, "summarise text", "x {{inputs.text}}"), "", Change{}, PutOptions{State: RoutineTrial})
	require.NoError(t, err)
	require.NoError(t, s.RejectRoutineVersion(ctx, "summarise", 1, "pass rate 0.33 below 0.80", nil))
	rt, err := s.GetRoutine(ctx, "summarise")
	require.NoError(t, err)
	assert.Equal(t, RoutineRejected, rt.State)

	hash, err := routine.ContractHash(rt.Contract)
	require.NoError(t, err)
	id, found, err := s.FindByContract(ctx, hash)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "summarise", id, "a later draft of the same promise finds it")
}

func TestIndependentCasesNeverChangeOnceWritten(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := RoutineCase{RoutineID: "r", Name: "c1", Inputs: map[string]string{"x": "1"}, Checks: routine.Condition{MustContain: []string{"a"}}}
	require.NoError(t, s.PutRoutineCases(ctx, []RoutineCase{c}))
	c.Checks = routine.Condition{MustContain: []string{"weaker"}}
	require.NoError(t, s.PutRoutineCases(ctx, []RoutineCase{c}))
	got, err := s.ListRoutineCases(ctx, "r")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"a"}, got[0].Checks.MustContain)
}

func TestShapesToSkipCoverOpenAdmittedAndCoolingCandidates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	open, err := s.CreateCandidate(ctx, "A", []int{1})
	require.NoError(t, err)
	admitted, err := s.CreateCandidate(ctx, "B", nil)
	require.NoError(t, err)
	require.NoError(t, s.UpdateCandidate(ctx, admitted.ID, CandidateAdmitted, "b", "", false))
	rejected, err := s.CreateCandidate(ctx, "C", nil)
	require.NoError(t, err)
	require.NoError(t, s.UpdateCandidate(ctx, rejected.ID, CandidateRejected, "c", "", true))

	skip, err := s.ShapesToSkip(ctx, time.Hour)
	require.NoError(t, err)
	assert.True(t, skip["A"])
	assert.True(t, skip["B"])
	assert.True(t, skip["C"], "a rejection cools down")
	_ = open
	time.Sleep(5 * time.Millisecond)
	skip, err = s.ShapesToSkip(ctx, time.Millisecond)
	require.NoError(t, err)
	assert.False(t, skip["C"], "past the cooldown it may be raised again")
	c, err := s.GetCandidate(ctx, rejected.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, c.Drafts)
}

func TestLifecycleSpendAddsUpPerDay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	require.NoError(t, s.AddLifecycleSpend(ctx, "a", 0.25))
	require.NoError(t, s.AddLifecycleSpend(ctx, "a", 0.25))
	require.NoError(t, s.AddLifecycleSpend(ctx, "b", 1))
	today, err := s.LifecycleSpendToday(ctx)
	require.NoError(t, err)
	assert.InDelta(t, 1.5, today, 1e-9)
	a, err := s.LifecycleSpend(ctx, "a")
	require.NoError(t, err)
	assert.InDelta(t, 0.5, a, 1e-9)
}

func TestRecentRejectionsCountsConsecutiveVersionTrials(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, state := range []string{TrialAccepted, TrialRejected, TrialRejected} {
		tr, err := s.CreateTrial(ctx, Trial{Kind: TrialVersion, RoutineID: "r", Version: 2})
		require.NoError(t, err)
		require.NoError(t, s.FinishTrial(ctx, tr.ID, state, "rule", nil))
	}
	n, last, err := s.RecentRejections(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.False(t, last.IsZero())
}
