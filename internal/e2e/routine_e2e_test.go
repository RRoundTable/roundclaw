//go:build e2e

// Package e2e drives routines end to end against a real Temporal server: the
// HTTP API admits a routine and a run, a real worker runs the interpreter and
// the activities, and the container runtime is a script, so no Docker or model
// is needed. Run with:
//
//	go test -tags e2e ./internal/e2e/ -v
//
// The first run downloads the Temporal CLI dev server.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/roundtable/roundclaw/internal/adapter"
	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

const token = "e2e-token"

// fakeRuntime answers `run` like the container runtime: an agent turn streams a
// long result (so bounded delivery has something to bound), a prompt asking to
// hang sleeps until stopped, and a process step echoes or fails.
func fakeRuntime(t *testing.T, dir string) string {
	t.Helper()
	long := strings.Repeat("detail ", 60)
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  run)\n" +
		"    case \"$*\" in\n" +
		"      *hang-please*) sleep 300 ;;\n" +
		"      *\" claude \"*)\n" +
		`        echo '{"type":"system","subtype":"init","session_id":"s","model":"claude-e2e"}'` + "\n" +
		`        echo '{"type":"result","subtype":"success","result":"all done here ` + long + `TAIL-MARKER","total_cost_usd":0.03,"is_error":false,"modelUsage":{"claude-e2e":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.03}}}'` + "\n" +
		"        ;;\n" +
		"      *fail*) echo nope; exit 3 ;;\n" +
		"      *) echo ok ;;\n" +
		"    esac\n" +
		"    ;;\n" +
		"  stop) pkill -f 'sleep 300' ; exit 0 ;;\n" +
		"  rm) exit 0 ;;\n" +
		"esac\n"
	path := filepath.Join(dir, "runtime")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

type harness struct {
	srv *httptest.Server
	reg *registry.Store
	st  *store.Registry
	cfg *config.Config
}

func setup(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	dev, err := testsuite.StartDevServer(ctx, testsuite.DevServerOptions{
		ClientOptions: &client.Options{Namespace: "default"},
		LogLevel:      "error",
	})
	require.NoError(t, err, "start the Temporal dev server")
	t.Cleanup(func() { _ = dev.Stop() })
	tc := dev.Client()

	dir := t.TempDir()
	queue := "e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cfgPath := filepath.Join(dir, "roundclaw.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"workspace_root: ws\n"+
			"temporal:\n  task_queue: "+queue+"\n"+
			"container:\n  runtime: "+fakeRuntime(t, dir)+"\n  image: test-image\n  stop_grace: 1s\n"+
			"routines:\n  process_images: [allowed:1]\n  result_limit: 80\n"+
			"agents:\n  - id: pm\n  - id: dev\n"), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-credential")

	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })
	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { reg.Close() })
	for _, id := range []string{"pm", "dev"} {
		_, err := reg.Create(ctx, registry.Agent{ID: id, Enabled: true})
		require.NoError(t, err)
	}

	acts := activity.NewActivities(cfg, stores, reg, nil, nil, tc)
	w := worker.New(tc, cfg.Temporal.TaskQueue, worker.Options{MaxHeartbeatThrottleInterval: time.Second})
	w.RegisterWorkflow(rcworkflow.SubAgent)
	w.RegisterWorkflow(rcworkflow.RoutineRun)
	w.RegisterActivity(acts)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)
	steps := worker.New(tc, cfg.RoutineStepQueue(), worker.Options{
		MaxHeartbeatThrottleInterval: time.Second, MaxConcurrentActivityExecutionSize: 2,
	})
	steps.RegisterActivity(acts)
	require.NoError(t, steps.Start())
	t.Cleanup(steps.Stop)

	disp := adapter.NewDispatcher(cfg, tc, stores, reg)
	api := adapter.NewHTTP(disp, slog.New(slog.NewTextHandler(io.Discard, nil)), []string{token}, nil,
		cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return harness{srv: srv, reg: reg, st: stores, cfg: cfg}
}

func (h harness) call(t *testing.T, method, path string, body any, into any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if into != nil {
		require.NoError(t, json.Unmarshal(raw, into), string(raw))
	}
	return resp.StatusCode
}

type runView struct {
	Run   registry.RoutineRun `json:"run"`
	Steps []registry.StepRun  `json:"steps"`
}

func (h harness) waitRun(t *testing.T, id int64) runView {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var v runView
		h.call(t, http.MethodGet, "/v1/routine-runs/"+strconv.FormatInt(id, 10)+"?full", nil, &v)
		if v.Run.Status != registry.RunRunning {
			return v
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("run %d did not finish", id)
	return runView{}
}

func source(processArgv []string) map[string]any {
	return map[string]any{
		"id": "fix-vet",
		"contract": map[string]any{
			"intent": "make vet pass", "inputs": []string{"repo"},
			"done": map[string]any{"must_contain": []string{"done"}}, "effects": "workspace",
		},
		"steps": []map[string]any{
			{"id": "vet", "kind": "process", "image": "allowed:1", "argv": processArgv,
				"done": map[string]any{"exit_code": 0}},
			{"id": "fix", "kind": "agent", "agent": "dev", "after": []string{"vet"}, "effects": "workspace",
				"goal": "fix {{inputs.repo}}: {{steps.vet.output}}", "done": map[string]any{"must_contain": []string{"done"}}},
			{"id": "report", "kind": "agent", "agent": "dev", "after": []string{"fix"},
				"task": "report on {{steps.fix.output}}"},
		},
	}
}

func TestRoutineEndToEnd(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	// Admission compiles the routine; a broken one is refused with reasons.
	var refused map[string]any
	bad := source([]string{"echo"})
	bad["steps"].([]map[string]any)[1]["agent"] = "ghost"
	require.Equal(t, http.StatusBadRequest, h.call(t, http.MethodPost, "/v1/routines", bad, &refused))
	require.Equal(t, http.StatusCreated, h.call(t, http.MethodPost, "/v1/routines", source([]string{"go", "vet"}), nil))

	// pm hands work to the routine, stating what it needs back, and carries on.
	var started struct {
		RunID int64 `json:"run_id"`
	}
	require.Equal(t, http.StatusAccepted, h.call(t, http.MethodPost, "/v1/routines/fix-vet/runs", map[string]any{
		"inputs": map[string]string{"repo": "r1"},
		"expect": map[string]any{"must_contain": []string{"branch:"}},
		"notify": map[string]string{"agent": "pm", "conversation": "thread-1"},
	}, &started))

	v := h.waitRun(t, started.RunID)
	assert.Equal(t, registry.RunPassed, v.Run.Status, v.Run.Reason)
	require.Len(t, v.Steps, 3)
	for _, s := range v.Steps {
		assert.Equal(t, registry.RunPassed, s.Status, s.StepID)
	}
	assert.InDelta(t, 0.06, v.Run.CostUSD, 1e-9, "cost is totalled from the step rows")
	assert.Equal(t, int64(30), v.Run.Usage.Tokens())
	assert.NotEmpty(t, v.Run.Manifest)
	require.Len(t, v.Run.ExpectVerdicts, 1)
	assert.False(t, v.Run.ExpectVerdicts[0].Held, "pm's own check did not hold")

	// pm is woken once, with a bounded result and the rest kept as a file.
	pm, err := h.st.Get("pm")
	require.NoError(t, err)
	conv := "thread-1"
	var wake store.Turn
	require.Eventually(t, func() bool {
		turns, err := pm.Turns(ctx, store.TurnFilter{Limit: 5, Conversation: &conv})
		if err != nil || len(turns) == 0 {
			return false
		}
		wake = turns[0]
		return true
	}, 30*time.Second, 200*time.Millisecond)
	assert.Contains(t, wake.Request, "your conditions NOT met")
	assert.Contains(t, wake.Request, "inbox/results/")
	assert.NotContains(t, wake.Request, "TAIL-MARKER", "the tail stays out of pm's context")

	// The library shows the record, counted from runs.
	var lib struct {
		Routines []struct {
			ID      string                  `json:"id"`
			Current registry.RoutineFigures `json:"current"`
		} `json:"routines"`
	}
	h.call(t, http.MethodGet, "/v1/routines", nil, &lib)
	require.Len(t, lib.Routines, 1)
	assert.Equal(t, 1, lib.Routines[0].Current.Runs)
	assert.Equal(t, 1, lib.Routines[0].Current.CallerStated)
	assert.Equal(t, 0, lib.Routines[0].Current.CallerMet)

	// A failing fixed program fails the run and skips what depends on it.
	require.Equal(t, http.StatusOK, h.call(t, http.MethodPut, "/v1/routines/fix-vet", source([]string{"fail"}), nil))
	require.Equal(t, http.StatusAccepted, h.call(t, http.MethodPost, "/v1/routines/fix-vet/runs",
		map[string]any{"inputs": map[string]string{"repo": "r1"}}, &started))
	v = h.waitRun(t, started.RunID)
	assert.Equal(t, registry.RunFailed, v.Run.Status)
	assert.Equal(t, "vet", v.Run.FailedStep)
	assert.Equal(t, 2, v.Run.Version, "the run pinned the new version")
	statuses := map[string]string{}
	for _, s := range v.Steps {
		statuses[s.StepID] = s.Status
	}
	assert.Equal(t, registry.StepSkipped, statuses["fix"])
	assert.Equal(t, registry.StepSkipped, statuses["report"])

	// Stopping reaches the running step and still closes the run out.
	hang := source([]string{"go", "vet"})
	hang["id"] = "hangs"
	hang["steps"].([]map[string]any)[1]["goal"] = "hang-please"
	require.Equal(t, http.StatusCreated, h.call(t, http.MethodPost, "/v1/routines", hang, nil))
	require.Equal(t, http.StatusAccepted, h.call(t, http.MethodPost, "/v1/routines/hangs/runs",
		map[string]any{"inputs": map[string]string{"repo": "r1"}}, &started))
	require.Eventually(t, func() bool {
		var rv runView
		h.call(t, http.MethodGet, "/v1/routine-runs/"+strconv.FormatInt(started.RunID, 10), nil, &rv)
		for _, s := range rv.Steps {
			if s.StepID == "fix" && s.Status == registry.RunRunning {
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "the running step is visible while it runs")
	require.Equal(t, http.StatusAccepted, h.call(t, http.MethodPost,
		"/v1/routine-runs/"+strconv.FormatInt(started.RunID, 10)+"/stop", nil, nil))
	v = h.waitRun(t, started.RunID)
	assert.Equal(t, registry.RunStopped, v.Run.Status)
}
