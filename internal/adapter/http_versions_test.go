package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
)

// send is get/put/post with the headers this package's routes care about.
func send(t *testing.T, srv *httptest.Server, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHistoryReturnsTurnsNewestFirstAndTruncates(t *testing.T) {
	srv, _, st := newHarness(t)

	long := make([]byte, historyPreview*2)
	for i := range long {
		long[i] = 'x'
	}
	for _, req := range []string{"first", string(long)} {
		id, _, err := st.CreateTurn(t.Context(), store.NewTurn{Request: req, Origin: core.HTTPPollOrigin()})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		if err := st.FinishTurn(t.Context(), id, core.TurnResult{Status: core.TurnDone, Text: "ok"}); err != nil {
			t.Fatalf("finish turn: %v", err)
		}
	}

	resp := send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/turns", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decode[struct {
		Turns []historyTurn `json:"turns"`
		Count int           `json:"count"`
	}](t, resp)

	if body.Count != 2 {
		t.Fatalf("count = %d, want 2", body.Count)
	}
	if body.Turns[0].Request == "first" {
		t.Error("turns are oldest-first; the newest turn must come first")
	}
	if !body.Turns[0].Truncated {
		t.Error("a long request was returned whole; it must be truncated and flagged")
	}
	if len(body.Turns[0].Request) > historyPreview+8 {
		t.Errorf("truncated request is %d bytes, want about %d", len(body.Turns[0].Request), historyPreview)
	}

	// ?full=true is the escape hatch for the turn actually worth reading.
	resp = send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/turns?full=true", nil, nil)
	full := decode[struct {
		Turns []historyTurn `json:"turns"`
	}](t, resp)
	if full.Turns[0].Truncated || len(full.Turns[0].Request) != len(long) {
		t.Errorf("full=true returned %d bytes, want the whole %d", len(full.Turns[0].Request), len(long))
	}
}

func TestHistoryRejectsAnUnknownStatus(t *testing.T) {
	srv, _, _ := newHarness(t)
	resp := send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/turns?status=exploded", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A persona edit is the change most likely to alter how an agent behaves, and it
// never passes through the definition route — so the persona endpoint has to
// mint the version itself.
func TestPersonaWriteMintsAVersion(t *testing.T) {
	srv, _, _ := newHarness(t)

	resp := send(t, srv, http.MethodPut, "/v1/agents/pr-reviewer/persona",
		map[string]string{"persona": "Always answer in Korean."},
		map[string]string{headerNote: "korean please", headerAuthor: "agent:curator"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if v := decode[struct {
		Version int `json:"version"`
	}](t, resp); v.Version != 2 {
		t.Errorf("version = %d, want 2 (seed was 1)", v.Version)
	}

	list := decode[struct {
		Versions []versionSummary `json:"versions"`
	}](t, send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/versions", nil, nil))
	if len(list.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(list.Versions))
	}
	newest := list.Versions[0]
	if newest.Note != "korean please" || newest.Author != "agent:curator" {
		t.Errorf("change metadata = %q by %q", newest.Note, newest.Author)
	}
	if newest.PersonaBytes == 0 {
		t.Error("the newest version records no persona")
	}
}

// Rollback restores both halves and does so as a new version: rewinding would
// erase the change being undone, which is the thing worth looking at afterwards.
func TestRollbackRestoresPersonaAndDefinitionAsANewVersion(t *testing.T) {
	srv, _, _ := newHarness(t)

	// v2: the persona everyone liked. v3: a definition change that broke things.
	send(t, srv, http.MethodPut, "/v1/agents/pr-reviewer/persona",
		map[string]string{"persona": "good instructions"}, nil)
	send(t, srv, http.MethodPut, "/v1/agents/pr-reviewer/definition",
		map[string]any{"description": "broken", "model": "haiku", "enabled": true}, nil)
	send(t, srv, http.MethodPut, "/v1/agents/pr-reviewer/persona",
		map[string]string{"persona": "bad instructions"}, nil)

	resp := send(t, srv, http.MethodPost, "/v1/agents/pr-reviewer/versions/2/rollback", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rollback status = %d, want 200", resp.StatusCode)
	}
	got := decode[struct {
		RolledBack int `json:"rolled_back"`
		Version    int `json:"version"`
	}](t, resp)
	if got.RolledBack != 2 {
		t.Errorf("rolled_back = %d, want 2", got.RolledBack)
	}
	if got.Version != 5 {
		t.Errorf("rollback produced version %d, want 5 — history is append-only", got.Version)
	}

	persona := decode[struct {
		Persona string `json:"persona"`
	}](t, send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/persona", nil, nil))
	if persona.Persona != "good instructions" {
		t.Errorf("persona after rollback = %q", persona.Persona)
	}

	def := decode[struct {
		Description string `json:"description"`
		Model       string `json:"model"`
	}](t, send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/definition", nil, nil))
	if def.Description == "broken" || def.Model == "haiku" {
		t.Errorf("definition after rollback = %+v, want the v2 settings", def)
	}
}

func TestGetUnknownVersionIs404(t *testing.T) {
	srv, _, _ := newHarness(t)
	resp := send(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/versions/99", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
