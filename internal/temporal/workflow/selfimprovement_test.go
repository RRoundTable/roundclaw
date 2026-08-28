package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// One full turn of the self-improvement loop, through the real machinery.
//
// Everything here is the production code path: a real registry, real activities,
// the real EvalRun workflow, and the real SettleGate that FinishEvalRun calls.
// Two things are faked, and they are the same fake twice: what a model said —
// the container answering a case, and the judge marking the answer. Both cost
// money, neither is deterministic, and no logic from this capability lives in
// either. That is the honest place to stop.
//
// What it proves is the claim the whole capability rests on: an agent changes
// itself, the change is measured, and a regression puts it back without anybody
// asking — with the evidence left where the agent will read it.

func loopFixture(t *testing.T) (*registry.Store, *activity.Activities, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	if err := os.WriteFile(configPath, []byte(`
workspace_root: ws
container:
  image: roundclaw/claude:test
agents:
  - id: dev
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	reg.UsePersonaSource(registry.PersonaFromWorkspace(cfg.WorkDir))
	reg.UseIdentitySource(registry.IdentityByReading())

	return reg, activity.NewActivities(cfg, stores, reg, nil, nil, nil), cfg
}

func TestASelfMadeRegressionIsMeasuredAndPutBack(t *testing.T) {
	reg, acts, _ := loopFixture(t)
	ctx := t.Context()

	// v1: the agent as it stands.
	if _, err := reg.Create(ctx, registry.Agent{
		ID: "dev", Description: "answers questions", Enabled: true,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// What "better" means. The agent cannot reach this — /v1/evals is outside a
	// per-agent credential's surface — which is what stops it grading itself.
	if _, err := reg.PutEvalSet(ctx, registry.EvalSet{
		ID: "cases", AgentID: "dev", Enabled: true,
		Cases: []registry.EvalCase{
			{Name: "keeps-context", Prompt: "what did I just ask?", Rubric: "refers to the earlier question"},
			{Name: "answers-briefly", Prompt: "explain in one line", Rubric: "one line"},
		},
	}); err != nil {
		t.Fatalf("put eval set: %v", err)
	}

	// A completed baseline against v1: both cases pass.
	baseline := runCases(t, reg, acts, 0, 0, map[string]bool{
		"keeps-context": true, "answers-briefly": true,
	})

	// The agent changes itself. Authorship is what the server established for a
	// per-agent credential, which is what makes this a self-made change rather
	// than somebody's claim to be one.
	if _, err := reg.Update(ctx, registry.Agent{
		ID: "dev", Description: "answers questions tersely", Enabled: true,
	}, registry.Change{Author: "agent:dev", Note: "be terser"}); err != nil {
		t.Fatalf("self-made change: %v", err)
	}
	changed, err := reg.LatestVersion(ctx, "dev")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if changed.Version != 2 {
		t.Fatalf("the change minted v%d, want v2", changed.Version)
	}

	// Being terser lost the thread: keeps-context now fails. The gating run goes
	// through the real workflow.
	runCases(t, reg, acts, changed.Version, baseline, map[string]bool{
		"keeps-context": false, "answers-briefly": true,
	})

	// It was put back, without anybody asking.
	live, err := reg.Get(ctx, "dev")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if live.Description != "answers questions" {
		t.Errorf("description = %q, want the configuration from before the change", live.Description)
	}

	// Both the change and its reversal stand in the history. A rewind would read
	// as though the agent had never tried.
	versions, err := reg.ListVersions(ctx, "dev", 0)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3: v1, the change, and its reversal", len(versions))
	}
	if versions[1].Author != "agent:dev" {
		t.Errorf("the change is recorded as %q, want agent:dev", versions[1].Author)
	}

	// And the evidence is where the agent will read it. Told only that its change
	// is gone, it would make the same change again.
	reverts, err := reg.PendingRevertsFor(ctx, "dev", 0)
	if err != nil {
		t.Fatalf("pending reverts: %v", err)
	}
	if len(reverts) != 1 {
		t.Fatalf("reverts = %d, want 1", len(reverts))
	}
	if !strings.Contains(reverts[0].Note, "keeps-context") {
		t.Errorf("the reversal does not name the case that regressed: %q", reverts[0].Note)
	}
	if strings.Contains(reverts[0].Note, "answers-briefly") {
		t.Errorf("the reversal blames a case that did not regress: %q", reverts[0].Note)
	}
}

// runCases drives the real EvalRun workflow over a real registry, faking only
// what a model would have said. Returns the run's id.
func runCases(t *testing.T, reg *registry.Store, acts *activity.Activities,
	gates int, baseline int64, outcomes map[string]bool) int64 {
	t.Helper()
	ctx := t.Context()

	version := gates
	if version == 0 {
		v, err := reg.LatestVersion(ctx, "dev")
		if err != nil {
			t.Fatalf("latest version: %v", err)
		}
		version = v.Version
	}
	run, err := reg.StartEvalRun(ctx, registry.EvalRun{
		EvalSetID: "cases", AgentID: "dev", Version: version,
		GatesVersion: gates, BaselineRun: baseline, Total: len(outcomes),
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(EvalRun)
	env.RegisterActivity(acts)

	// The two fakes, and they are the same fake twice: what a model said. One is
	// the container answering the case, the other the judge marking it. Both cost
	// money and neither is deterministic, and no logic from this slice lives in
	// either. Recording, aggregating, comparing and settling all run for real.
	env.OnActivity("RunEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.RunCaseInput) (activity.CaseOutcome, error) {
			return activity.CaseOutcome{Output: in.Case.Name, CostUSD: 0.01}, nil
		})
	env.OnActivity("JudgeEvalCase", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in activity.JudgeInput) (activity.Judgement, error) {
			if outcomes[in.Case] {
				return activity.Judgement{Score: 1, Passed: true, Reason: "as specified"}, nil
			}
			return activity.Judgement{Score: 0, Passed: false, Reason: "not as specified"}, nil
		})

	env.ExecuteWorkflow(EvalRun, EvalRunInput{RunID: run.ID})
	if !env.IsWorkflowCompleted() {
		t.Fatal("the eval workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("eval workflow: %v", err)
	}
	return run.ID
}
