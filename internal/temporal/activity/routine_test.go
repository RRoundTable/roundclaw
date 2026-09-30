package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/store"
)

// routineRuntime stands in for the container runtime: an agent step (argv
// containing "claude") streams a result whose text is the last word of the
// prompt; a process step prints its argv and exits 3 when told to fail.
func routineRuntime(t *testing.T, dir string) string {
	t.Helper()
	body := "#!/bin/sh\n" +
		"echo \"$@\" >> " + filepath.Join(dir, "calls.log") + "\n" +
		"env | grep ^ROUNDCLAW_ >> " + filepath.Join(dir, "env.log") + "\n" +
		"case \"$1\" in\n" +
		"  run)\n" +
		"    case \"$*\" in\n" +
		"      *\" claude \"*)\n" +
		`        echo '{"type":"system","subtype":"init","session_id":"s","model":"claude-test"}'` + "\n" +
		`        echo '{"type":"result","subtype":"success","result":"all done here","total_cost_usd":0.02,"is_error":false,"modelUsage":{"claude-test":{"inputTokens":3,"outputTokens":4,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.02}}}'` + "\n" +
		"        ;;\n" +
		"      *fail*) echo \"process said no\"; exit 3 ;;\n" +
		"      *) echo \"process ran: $*\" ;;\n" +
		"    esac\n" +
		"    ;;\n" +
		"  stop|rm) exit 0 ;;\n" +
		"esac\n"
	path := filepath.Join(dir, "routine-runtime")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

func newRoutineActivities(t *testing.T) (*Activities, string) {
	t.Helper()
	dir := t.TempDir()
	rt := routineRuntime(t, dir)
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\n" +
		"container:\n  runtime: " + rt + "\n  image: test-image\n  stop_grace: 1s\n" +
		"routines:\n  process_images: [allowed:1]\n" +
		"agents:\n  - id: tester\n"
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))
	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })
	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { reg.Close() })
	_, err = reg.Create(context.Background(), registry.Agent{ID: "tester", Enabled: true})
	require.NoError(t, err)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-credential")
	return NewActivities(cfg, stores, reg, nil, nil, nil), dir
}

func runStepActivity(t *testing.T, a *Activities, in RoutineStepInput) (RoutineStepOutcome, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a)
	val, err := env.ExecuteActivity(a.RunRoutineStep, in)
	var out RoutineStepOutcome
	if err == nil {
		require.NoError(t, val.Get(&out))
	}
	return out, err
}

func exitCode(n int) *int { return &n }

func TestAgentStepPassesOnItsConditionsAndCarriesNoCredential(t *testing.T) {
	a, dir := newRoutineActivities(t)
	step := routine.Step{ID: "write", Kind: routine.KindAgent, Agent: "tester", Goal: "write it",
		Done: routine.Condition{MustContain: []string{"done"}}, MaxAttempts: 2, TimeoutSec: 60}
	out, err := runStepActivity(t, a, RoutineStepInput{RunID: 11, Step: step, Attempt: 1})
	require.NoError(t, err)
	assert.Equal(t, registry.RunPassed, out.Status)
	assert.Equal(t, "all done here", out.Output)

	steps, err := a.reg.ListStepRuns(context.Background(), 11)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, registry.RunPassed, steps[0].Status)
	assert.InDelta(t, 0.02, steps[0].CostUSD, 1e-9)
	assert.Equal(t, int64(7), steps[0].Usage.Tokens(), "usage is recorded per model on the trace")
	require.Len(t, steps[0].Verdicts, 1)
	assert.True(t, steps[0].Verdicts[0].Held)

	envLog, _ := os.ReadFile(filepath.Join(dir, "env.log"))
	assert.Empty(t, strings.TrimSpace(string(envLog)), "a step carries no roundclaw identity or credential")
	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	require.NoError(t, err)
	assert.Contains(t, string(calls), "--disallowedTools", "fleet-control tools are denied to a step")

	persona := filepath.Join(a.cfg.RoutineClaudeHome(11, "write"), "CLAUDE.md")
	_, err = os.Stat(persona)
	assert.NoError(t, err, "the persona lives in the step's own home, not the shared work area")
}

func TestAgentStepThatFallsShortSaysWhy(t *testing.T) {
	a, _ := newRoutineActivities(t)
	step := routine.Step{ID: "write", Kind: routine.KindAgent, Agent: "tester", Goal: "write it",
		Done: routine.Condition{MustContain: []string{"PASSED"}}, MaxAttempts: 2, TimeoutSec: 60}
	out, err := runStepActivity(t, a, RoutineStepInput{RunID: 12, Step: step, Attempt: 1})
	require.NoError(t, err)
	assert.Equal(t, registry.RunFailed, out.Status)
	assert.Contains(t, out.Reason, `missing "PASSED"`)
}

func TestProcessStepRunsTheLiteralArgvWithoutNetwork(t *testing.T) {
	a, dir := newRoutineActivities(t)
	step := routine.Step{ID: "vet", Kind: routine.KindProcess, Image: "allowed:1", Argv: []string{"go", "vet"},
		Done: routine.Condition{ExitCode: exitCode(0)}, MaxAttempts: 1, TimeoutSec: 60, Effects: routine.EffectNone}
	out, err := runStepActivity(t, a, RoutineStepInput{RunID: 13, Step: step, Attempt: 1})
	require.NoError(t, err)
	assert.Equal(t, registry.RunPassed, out.Status)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	assert.Contains(t, string(calls), "--network none")
	assert.Contains(t, string(calls), "allowed:1 go vet")
	assert.NotContains(t, string(calls), "docker.sock")
}

func TestProcessStepExitCodeIsARule(t *testing.T) {
	a, _ := newRoutineActivities(t)
	step := routine.Step{ID: "vet", Kind: routine.KindProcess, Image: "allowed:1", Argv: []string{"fail"},
		Done: routine.Condition{ExitCode: exitCode(0)}, MaxAttempts: 1, TimeoutSec: 60}
	out, err := runStepActivity(t, a, RoutineStepInput{RunID: 14, Step: step, Attempt: 1})
	require.NoError(t, err)
	assert.Equal(t, registry.RunFailed, out.Status)
	assert.Contains(t, out.Reason, "exit 3")
}

func TestProcessStepOffTheAllowlistIsAnError(t *testing.T) {
	a, _ := newRoutineActivities(t)
	step := routine.Step{ID: "vet", Kind: routine.KindProcess, Image: "evil:latest", Argv: []string{"x"},
		MaxAttempts: 1, TimeoutSec: 60}
	out, err := runStepActivity(t, a, RoutineStepInput{RunID: 15, Step: step, Attempt: 1})
	require.NoError(t, err)
	assert.Equal(t, registry.RunError, out.Status)
	assert.Contains(t, out.Reason, "allowlist")
}

func TestRoutineWakePromptIsBoundedAndNamesBothVerdicts(t *testing.T) {
	run := registry.RoutineRun{ID: 5, RoutineID: "r", Version: 2, Status: registry.RunPassed,
		Expect:         routine.Condition{MustContain: []string{"branch:"}},
		ExpectVerdicts: []routine.Verdict{{Rule: `must_contain "branch:"`, Detail: `missing "branch:"`}}}
	p := routineWakePrompt(run, "head", "inbox/results/x.md")
	assert.Contains(t, p, "your conditions NOT met")
	assert.Contains(t, p, `missing "branch:"`)
	assert.Contains(t, p, "inbox/results/x.md")
	assert.Contains(t, p, "--instead-of 5")
}
