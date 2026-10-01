package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/store"
)

func lifecycleActivities(t *testing.T, lifecycleYAML string) (*Activities, *recordingSignaller) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "roundclaw.yaml")
	require.NoError(t, os.WriteFile(path, []byte("workspace_root: ws\ncontainer:\n  image: test-image\n"+
		"routines:\n  steward: steward\n  lifecycle:\n"+lifecycleYAML+
		"agents:\n  - id: dev\n  - id: steward\n"), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })
	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { reg.Close() })
	for _, id := range []string{"dev", "steward"} {
		_, err := reg.Create(context.Background(), registry.Agent{ID: id, Enabled: true})
		require.NoError(t, err)
	}
	sig := &recordingSignaller{}
	return NewActivities(cfg, stores, reg, nil, nil, sig), sig
}

func sweep(t *testing.T, a *Activities) SweepResult {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a)
	val, err := env.ExecuteActivity(a.LifecycleSweep)
	require.NoError(t, err)
	var res SweepResult
	require.NoError(t, val.Get(&res))
	return res
}

func admitRoutine(t *testing.T, a *Activities, id string) registry.Routine {
	t.Helper()
	plan, err := routine.Compile(routine.Source{ID: id, Contract: routine.Contract{Intent: "do " + id},
		Steps: []routine.Step{{ID: "work", Kind: routine.KindAgent, Agent: "dev", Task: "do it"}}},
		regResolver{a.reg}, routine.DefaultCaps)
	require.NoError(t, err)
	res, err := a.reg.PutRoutine(context.Background(), plan, "dev", registry.Change{})
	require.NoError(t, err)
	return res.Routine
}

type regResolver struct{ reg *registry.Store }

func (r regResolver) AgentUsable(id string) (bool, error) {
	_, err := r.reg.Get(context.Background(), id)
	return err == nil, nil
}
func (regResolver) ImageAllowed(string) bool { return true }
func (r regResolver) RoutineSteps(id string) ([]string, bool, error) {
	return r.reg.RoutineUses(context.Background(), id)
}

func stewardPrompts(t *testing.T, a *Activities) []string {
	t.Helper()
	st, err := a.stores.Get("steward")
	require.NoError(t, err)
	conv := StewardConversation
	turns, err := st.Turns(context.Background(), store.TurnFilter{Limit: 50, Conversation: &conv})
	require.NoError(t, err)
	var out []string
	for _, tu := range turns {
		out = append(out, tu.Request)
	}
	return out
}

func TestSweepArchivesARoutineNobodyUses(t *testing.T) {
	a, _ := lifecycleActivities(t, "    unused_after: 1ms\n")
	admitRoutine(t, a, "idle")
	time.Sleep(5 * time.Millisecond)
	sweep(t, a)
	rt, err := a.reg.GetRoutine(context.Background(), "idle")
	require.NoError(t, err)
	assert.Equal(t, registry.RoutineArchived, rt.State)
	ds, err := a.reg.ListRoutineDecisions(context.Background(), "idle")
	require.NoError(t, err)
	assert.Equal(t, "archived", ds[0].Decision)
	assert.Contains(t, ds[0].Rule, "unused")
}

func TestSweepWakesTheStewardWhenFailuresGatherAtOneStep(t *testing.T) {
	a, _ := lifecycleActivities(t, "")
	rt := admitRoutine(t, a, "flaky")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		run, err := a.reg.StartRoutineRun(ctx, registry.RoutineRun{RoutineID: "flaky", Version: rt.Version,
			PlanHash: rt.Plan.Hash, Origin: core.HTTPPollOrigin()})
		require.NoError(t, err)
		_, err = a.reg.FinishRoutineRun(ctx, run.ID, registry.RunOutcome{Status: registry.RunFailed, FailedStep: "work", Reason: "missing \"done\""})
		require.NoError(t, err)
	}
	sweep(t, a)
	prompts := stewardPrompts(t, a)
	require.Len(t, prompts, 1)
	assert.Contains(t, prompts[0], `Failures are gathering at step "work"`)
	assert.Contains(t, prompts[0], "PUT /v1/routines/flaky")

	sweep(t, a)
	assert.Len(t, stewardPrompts(t, a), 1, "the same hotspot wakes the steward once")
}

func TestSweepRemeasuresWhenAnAgentItUsesChanges(t *testing.T) {
	a, _ := lifecycleActivities(t, "")
	admitRoutine(t, a, "measured")
	ctx := context.Background()
	require.NoError(t, a.reg.PutRoutineCases(ctx, []registry.RoutineCase{{RoutineID: "measured", Name: "c1",
		Checks: routine.Condition{MustContain: []string{"x"}}}}))
	manifest, err := a.reg.RoutineManifest(ctx, mustRoutine(t, a, "measured").Plan)
	require.NoError(t, err)
	require.NoError(t, a.reg.RecordMeasurement(ctx, "measured", 1, 1, manifest))

	assert.Empty(t, sweep(t, a).Trials, "measured just now, against the same agents")

	dev, err := a.reg.Get(ctx, "dev")
	require.NoError(t, err)
	dev.Description = "changed"
	_, err = a.reg.Update(ctx, dev)
	require.NoError(t, err)
	res := sweep(t, a)
	require.Len(t, res.Trials, 1, "the agent it uses moved, so it is measured again")
	tr, err := a.reg.GetTrial(ctx, res.Trials[0])
	require.NoError(t, err)
	assert.Equal(t, registry.TrialRemeasure, tr.Kind)
}

func TestSweepStopsDraftingAndMeasuringPastTheSpendCap(t *testing.T) {
	a, _ := lifecycleActivities(t, "    daily_spend_usd: 1\n")
	ctx := context.Background()
	require.NoError(t, a.reg.AddLifecycleSpend(ctx, "x", 2))
	res := sweep(t, a)
	require.NotEmpty(t, res.Notes)
	assert.True(t, strings.Contains(res.Notes[0], "cap"))
}

func mustRoutine(t *testing.T, a *Activities, id string) registry.Routine {
	t.Helper()
	rt, err := a.reg.GetRoutine(context.Background(), id)
	require.NoError(t, err)
	return rt
}

func TestParseChecksReadsTheExaminersJSON(t *testing.T) {
	c, ok := parseChecks("Here you go:\n```json\n{\"must_contain\": [\"branch:\"], \"must_not_contain\": [\"error\"], \"exit_code\": 0}\n```")
	require.True(t, ok)
	assert.Equal(t, []string{"branch:"}, c.MustContain)
	assert.Equal(t, []string{"error"}, c.MustNotContain)
	assert.Nil(t, c.ExitCode, "the examiner writes text checks only")
	_, ok = parseChecks("no json here")
	assert.False(t, ok)
}
