package adapter

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/routine"
)

func readJSON(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(into))
}

func summariser(task string) routine.Source {
	return routine.Source{
		ID:       "summarise",
		Contract: routine.Contract{Intent: "summarise a text", Inputs: []string{"text"}},
		Steps:    []routine.Step{{ID: "write", Kind: routine.KindAgent, Agent: "dev", Task: task}},
	}
}

func TestSubmittingARoutineCompilesItAndRefusesWithEveryProblem(t *testing.T) {
	srv, _ := selfHarness(t)

	bad := summariser("{{inputs.missing}}")
	bad.Steps[0].Agent = "ghost"
	resp := do(t, srv, http.MethodPost, "/v1/routines", testToken, bad)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var refused struct {
		Diagnostics []routine.Diagnostic `json:"diagnostics"`
	}
	readJSON(t, resp, &refused)
	assert.GreaterOrEqual(t, len(refused.Diagnostics), 2, "every problem is named, not the first")

	resp = do(t, srv, http.MethodPost, "/v1/routines", testToken, summariser("summarise {{inputs.text}}"))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// A changed contract is a different routine.
	changed := summariser("summarise {{inputs.text}}")
	changed.Contract.Intent = "summarise loosely"
	resp = do(t, srv, http.MethodPut, "/v1/routines/summarise", testToken, changed)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

// A self credential creates routines it then owns, and cannot change another's.
func TestSelfCredentialsAreBoundedByOwner(t *testing.T) {
	srv, reg := selfHarness(t)
	devToken := core.DeriveAgentToken("dev", selfKey)
	opsToken := core.DeriveAgentToken("ops", selfKey)

	// An agent's submission is tried before it serves, so it must bring cases.
	resp := do(t, srv, http.MethodPost, "/v1/routines", devToken, summariser("summarise {{inputs.text}}"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
	src := summariser("summarise {{inputs.text}}")
	resp = do(t, srv, http.MethodPost, "/v1/routines", devToken, map[string]any{
		"routine": src,
		"cases": []map[string]any{
			{"inputs": map[string]string{"text": "a"}, "request": "summarise a", "outcome": "a summary"},
			{"inputs": map[string]string{"text": "b"}, "request": "summarise b", "outcome": "b summary"},
			{"inputs": map[string]string{"text": "c"}, "request": "summarise c", "outcome": "c summary"},
		},
	})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
	rt, err := reg.GetRoutine(t.Context(), "summarise")
	require.NoError(t, err)
	assert.Equal(t, "dev", rt.Owner)
	assert.Equal(t, "trial", rt.State, "not offered until its trial admits it")
	versions, err := reg.ListRoutineVersions(t.Context(), "summarise")
	require.NoError(t, err)
	assert.Equal(t, "agent:dev", versions[0].Author, "authorship comes from the credential")

	resp = do(t, srv, http.MethodPut, "/v1/routines/summarise", opsToken, summariser("changed {{inputs.text}}"))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	resp = do(t, srv, http.MethodPut, "/v1/routines/summarise", devToken, summariser("better {{inputs.text}}"))
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "a routine on its admission trial waits for the verdict")
	resp.Body.Close()
}

func TestRunningARoutineStartsItAndTheLibraryShowsIt(t *testing.T) {
	srv, tc, _ := newHarness(t)
	src := summariser("summarise {{inputs.text}}")
	src.Steps[0].Agent = "pr-reviewer"
	resp := do(t, srv, http.MethodPost, "/v1/routines", testToken, src)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Missing an input the contract declares is refused before anything runs.
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/runs", testToken, map[string]any{})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	before := tc.starts
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/runs", testToken,
		map[string]any{"inputs": map[string]string{"text": "hello"}})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var started struct {
		RunID int64 `json:"run_id"`
	}
	readJSON(t, resp, &started)
	assert.NotZero(t, started.RunID)
	assert.Equal(t, before+1, tc.starts)

	resp = do(t, srv, http.MethodGet, "/v1/routine-runs/"+runPath(started.RunID), testToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = do(t, srv, http.MethodPost, "/v1/routine-runs/"+runPath(started.RunID)+"/stop", testToken, nil)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
	assert.Equal(t, []string{"roundclaw-routine-" + runPath(started.RunID)}, tc.cancelled)

	resp = do(t, srv, http.MethodGet, "/v1/routines", testToken, nil)
	var lib struct {
		Routines []libraryEntry `json:"routines"`
	}
	readJSON(t, resp, &lib)
	require.Len(t, lib.Routines, 1)
	assert.True(t, lib.Routines[0].Runnable)
	assert.Equal(t, "summarise a text", lib.Routines[0].Intent)

	// Archived routines leave the library and take no new work.
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/archive", testToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/runs", testToken,
		map[string]any{"inputs": map[string]string{"text": "hello"}})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

// A routine that can act on the outside world is run only by its owner or a
// person; a delegate token cannot start it.
func TestOnlyTheOwnerRunsAnExternalRoutine(t *testing.T) {
	srv, _ := selfHarness(t)
	src := summariser("post {{inputs.text}}")
	src.Contract.Effects = routine.EffectExternal
	src.Steps[0].Effects = routine.EffectExternal
	resp := do(t, srv, http.MethodPost, "/v1/routines?owner=dev", testToken, src)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	body := map[string]any{"inputs": map[string]string{"text": "x"}}
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/runs", "delegate-token", body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/runs", core.DeriveAgentToken("ops", selfKey), body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	resp = do(t, srv, http.MethodPost, "/v1/routines/summarise/runs", core.DeriveAgentToken("dev", selfKey), body)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
}

func runPath(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
