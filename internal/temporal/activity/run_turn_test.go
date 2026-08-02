package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// fakeRuntime writes a stand-in for the container runtime that records every
// invocation and can be told to hang, so cancellation has something to cancel.
//
// This deliberately does not use the workflow test environment. Cancelling a
// running activity is exactly what that harness cannot drive — it stops
// advancing its clock while an activity executes, so a signal sent mid-turn
// never reaches the workflow. The behaviour that matters is all in the
// activity, which is an ordinary function taking a context.
func fakeRuntime(t *testing.T, dir string, hang bool) string {
	t.Helper()

	body := "#!/bin/sh\n" +
		"echo \"$@\" >> " + filepath.Join(dir, "calls.log") + "\n" +
		"case \"$1\" in\n" +
		"  run)\n" +
		`    echo '{"type":"system","subtype":"init","session_id":"'"$4"'"}'` + "\n" +
		`    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}'` + "\n"
	if hang {
		// Sleep far longer than the test, so the only way this ends is a stop.
		body += "    sleep 300\n"
	} else {
		body += `    echo '{"type":"result","subtype":"success","result":"done","total_cost_usd":0.01,"is_error":false}'` + "\n"
	}
	body += "    ;;\n" +
		"  stop|rm)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"

	path := filepath.Join(dir, "fake-runtime")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	return path
}

func newActivities(t *testing.T, runtimePath string) (*Activities, *store.Store, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\n" +
		"container:\n" +
		"  runtime: " + runtimePath + "\n" +
		"  image: test-image\n" +
		"  stop_grace: 1s\n" +
		"agents:\n" +
		"  - id: tester\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })
	st, err := stores.Get("tester")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Create(context.Background(), registry.Agent{ID: "tester", Enabled: true}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-credential")
	return NewActivities(cfg, stores, reg, nil, nil), st, dir
}

func runTurn(t *testing.T, a *Activities, in RunTurnInput) (core.TurnResult, error) {
	t.Helper()
	// A real activity context, so activity.RecordHeartbeat and GetLogger work
	// exactly as they do in the worker.
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a)

	type outcome struct {
		result core.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		val, err := env.ExecuteActivity(a.RunClaudeTurn, in)
		var res core.TurnResult
		if err == nil {
			_ = val.Get(&res)
		}
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		return o.result, o.err
	case <-time.After(60 * time.Second):
		t.Fatal("activity did not finish")
		return core.TurnResult{}, nil
	}
}

func TestRunClaudeTurnCompletes(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	turnID, _, err := st.CreateTurn(t.Context(), store.NewTurn{Request: "hello", Origin: core.HTTPPollOrigin(), IdempotencyKey: ""})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	result, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: turnID, WorkflowID: "roundclaw-tester-default", Prompt: "hello",
	})
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	if result.Status != core.TurnDone || result.Text != "done" {
		t.Errorf("result = %+v, want done", result)
	}
	if result.CostUSD != 0.01 {
		t.Errorf("cost = %v, want 0.01", result.CostUSD)
	}

	turn, err := st.GetTurn(t.Context(), turnID)
	if err != nil || turn.Status != core.TurnDone {
		t.Errorf("stored turn = %+v, err = %v", turn, err)
	}
}

// stopContainer is the half of /stop that can be tested in isolation.
//
// The integrated path — signal reaches the workflow, workflow cancels the
// activity, activity lands here — is not unit-testable: the workflow test
// harness stops advancing its clock while an activity runs, so a signal sent
// mid-turn never arrives. That path is covered by an end-to-end measurement
// against a live Temporal server instead (container dead ~5s after /stop).
func TestStopContainerAsksGracefullyBeforeForcing(t *testing.T) {
	dir := t.TempDir()
	runtimePath := fakeRuntime(t, dir, false)
	a, _, _ := newActivities(t, runtimePath)

	a.stopContainer(claudeSpec(runtimePath, "roundclaw-tester-1"))

	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatalf("read runtime calls: %v", err)
	}
	if !strings.Contains(string(calls), "stop --time") {
		t.Errorf("container was not asked to stop gracefully; calls were:\n%s", calls)
	}
	// `claude` writes its session transcript on the way out, so a graceful stop
	// that succeeded must not be followed by a force-remove.
	if strings.Contains(string(calls), "rm -f") {
		t.Errorf("a successful graceful stop was followed by a force-remove:\n%s", calls)
	}
}

// When the runtime cannot stop the container, it has to be forced, or a crashed
// turn's container would linger and hold the deterministic name.
func TestStopContainerForcesWhenGracefulStopFails(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "failing-runtime")
	body := "#!/bin/sh\n" +
		"echo \"$@\" >> " + filepath.Join(dir, "calls.log") + "\n" +
		"case \"$1\" in stop) exit 1 ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(runtimePath, []byte(body), 0o755); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	a, _, _ := newActivities(t, runtimePath)

	a.stopContainer(claudeSpec(runtimePath, "roundclaw-tester-1"))

	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatalf("read runtime calls: %v", err)
	}
	if !strings.Contains(string(calls), "rm -f") {
		t.Errorf("a failed graceful stop was not escalated to a force-remove:\n%s", calls)
	}
}

// A deleted agent will not come back on retry, so this must fail rather than
// spin through the retry policy.
func TestRunClaudeTurnRejectsUnknownAgent(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := newActivities(t, fakeRuntime(t, dir, false))

	_, err := runTurn(t, a, RunTurnInput{
		AgentID: "no-such-agent", TurnID: 1, WorkflowID: "wf", Prompt: "hi",
	})
	if err == nil {
		t.Fatal("a turn for an unregistered agent succeeded")
	}
	if !strings.Contains(err.Error(), "no-such-agent") {
		t.Errorf("error does not name the missing agent: %v", err)
	}
}

// Which session flag reaches the CLI, and what happens when it is the wrong one,
// live in session_test.go — the decision is made from the transcript on disk
// rather than from anything this input carries.

// A model reaches the CLI only if the activity threads it from the definition
// into the argv, so this asserts on the runtime's recorded command rather than
// on the spec: the flag is what the container actually receives.
func TestRunClaudeTurnPassesAgentModel(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	agent, err := a.reg.Get(t.Context(), "tester")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	agent.Model = "claude-opus-5"
	if _, err := a.reg.Update(t.Context(), agent); err != nil {
		t.Fatalf("set model: %v", err)
	}

	turnID, _, err := st.CreateTurn(t.Context(), store.NewTurn{Request: "hi", Origin: core.HTTPPollOrigin()})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: turnID, WorkflowID: "roundclaw-tester-default", Prompt: "hi",
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatalf("read runtime calls: %v", err)
	}
	if !strings.Contains(string(calls), "--model claude-opus-5") {
		t.Errorf("the turn did not pass the agent's model: %s", calls)
	}
}

// The CLI inside the container reads its identity from the environment, so `say`
// and `send --notify-me` need no arguments to know where they are. Secrets pass
// by name, so the names show up in the argv the runtime records.
func TestRunClaudeTurnInjectsIdentity(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	// A turn that arrived by delegation: its origin is the return address, and the
	// agent is told who is waiting so it can report progress there.
	turnID, _, err := st.CreateTurn(t.Context(), store.NewTurn{
		Request: "hi", Origin: core.AgentOrigin("pm", "thread-1"), Conversation: "thread-9",
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: turnID, WorkflowID: "roundclaw-tester-thread-9",
		ConversationID: "thread-9", Prompt: "hi",
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatalf("read runtime calls: %v", err)
	}
	text := string(calls)
	for _, want := range []string{
		"-e ROUNDCLAW_AGENT_ID",
		"-e ROUNDCLAW_TURN_ID",
		"-e ROUNDCLAW_CONVERSATION_ID",
		"-e ROUNDCLAW_REPLY_TO",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("argv is missing %q:\n%s", want, text)
		}
	}
}

// stopContainer takes the runtime from the spec, not from config, so a spec
// built for a test has to carry it too.
func claudeSpec(runtime, name string) claude.RunSpec {
	return claude.RunSpec{Runtime: runtime, ContainerName: name}
}
