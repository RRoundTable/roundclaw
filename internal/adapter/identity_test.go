package adapter

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
	"testing"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// Two agents and a per-agent credential key, so a self-scoped token has both
// something it may change and something it may not.
const selfKey = "test-self-token-key"

func selfConfig(t *testing.T, dir string) *config.Config {
	t.Helper()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	err := os.WriteFile(configPath, []byte(`
workspace_root: ws
container:
  image: roundclaw/claude:test
http:
  wait_timeout: 300ms
  max_sse_per_agent: 2
agents:
  - id: dev
  - id: ops
`), 0o600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func selfHarness(t *testing.T) (*httptest.Server, *registry.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := selfConfig(t, dir)

	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	reg.UsePersonaSource(registry.PersonaFromWorkspace(cfg.WorkDir))
	reg.UseIdentitySource(registry.IdentityByReading())

	if _, err := reg.Seed(context.Background(), []registry.Agent{
		{ID: "dev", Description: "writes code"},
		{ID: "ops", Description: "runs things"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	api := NewHTTP(disp, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]string{testToken}, []string{"delegate-token"}, cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	api.UseSelfTokenKey(selfKey)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, reg
}

func do(t *testing.T, srv *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The point of the whole slice: an agent can change what it is.
func TestSelfTokenMayWriteItsOwnPersona(t *testing.T) {
	srv, _ := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	resp := do(t, srv, http.MethodPut, "/v1/agents/dev/persona", token,
		map[string]string{"persona": "Answer briefly."})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// And the bound: it is a credential for one agent, not for the fleet.
func TestSelfTokenMayNotWriteAnotherAgent(t *testing.T) {
	srv, _ := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	resp := do(t, srv, http.MethodPut, "/v1/agents/ops/persona", token,
		map[string]string{"persona": "Answer briefly."})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: dev's credential is not ops's", resp.StatusCode)
	}
}

func TestSelfTokenMayWriteItsOwnDefinition(t *testing.T) {
	srv, _ := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	resp := do(t, srv, http.MethodPut, "/v1/agents/dev/definition", token,
		map[string]any{"id": "dev", "description": "writes better code"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// Authorship the server established, not the caller's word for it.
func TestSelfWriteRecordsEstablishedAuthorship(t *testing.T) {
	srv, reg := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	// The header lies; the credential does not, and the credential wins.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/v1/agents/dev/persona",
		bytes.NewReader([]byte(`{"persona":"Answer briefly."}`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(headerAuthor, "somebody-else")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	v, err := reg.LatestVersion(t.Context(), "dev")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if v.Author != "agent:dev" {
		t.Errorf("author = %q, want agent:dev established by the credential", v.Author)
	}
}

// Everything the delegate scope could do, a self token can still do: it is a
// superset, so enabling per-agent credentials takes nothing away.
func TestSelfTokenKeepsTheDelegateSurface(t *testing.T) {
	srv, _ := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	if resp := do(t, srv, http.MethodGet, "/v1/agents", token, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("listing agents = %d, want 200", resp.StatusCode)
	}
	if resp := do(t, srv, http.MethodGet, "/v1/agents/ops", token, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("reading another agent's status = %d, want 200: status was never restricted", resp.StatusCode)
	}
}

// The fleet-wide surface stays shut. A prompt injection reaching one agent must
// not be able to read every secret the fleet holds.
func TestSelfTokenCannotReachTheFleet(t *testing.T) {
	srv, _ := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	for _, path := range []string{"/v1/secrets", "/v1/workflows"} {
		if resp := do(t, srv, http.MethodGet, path, token, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, resp.StatusCode)
		}
	}
	if resp := do(t, srv, http.MethodDelete, "/v1/agents/ops", token, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("deleting another agent = %d, want 403", resp.StatusCode)
	}
}

// A shared delegate token is exactly as restricted as it was: it names nobody,
// so it establishes nothing and gains no self surface.
func TestDelegateTokenGainsNothing(t *testing.T) {
	srv, _ := selfHarness(t)

	resp := do(t, srv, http.MethodPut, "/v1/agents/dev/persona", "delegate-token",
		map[string]string{"persona": "Answer briefly."})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a shared token cannot be an agent", resp.StatusCode)
	}
}

func TestForgedSelfTokenIsRefused(t *testing.T) {
	srv, _ := selfHarness(t)

	resp := do(t, srv, http.MethodGet, "/v1/agents", "rcs.dev.notarealmac", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// A tool nobody holds yet is an agent registering a capability for itself.
func TestSelfTokenMayCreateAToolItDoesNotYetHold(t *testing.T) {
	srv, _ := selfHarness(t)
	token := core.DeriveAgentToken("dev", selfKey)

	resp := do(t, srv, http.MethodPut, "/v1/tools/scanner", token,
		map[string]any{"id": "scanner", "host_path": t.TempDir()})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// A tool somebody else holds is the case with no defence: changing it on behalf
// of the agents that use it, without holding it.
func TestSelfTokenMayNotWriteAToolItDoesNotHold(t *testing.T) {
	srv, reg := selfHarness(t)
	dir := t.TempDir()
	if _, err := reg.PutTool(t.Context(), registry.Tool{ID: "deploy", HostPath: dir}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	if _, err := reg.Update(t.Context(), registry.Agent{ID: "ops", Tools: []string{"deploy"}}); err != nil {
		t.Fatalf("grant to ops: %v", err)
	}

	token := core.DeriveAgentToken("dev", selfKey)
	resp := do(t, srv, http.MethodPut, "/v1/tools/deploy", token,
		map[string]any{"id": "deploy", "host_path": "/etc"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: dev does not hold deploy", resp.StatusCode)
	}
}

// A tool it does hold, it may change — and every other holder gets the change,
// which the spec states plainly rather than pretending otherwise.
func TestSelfTokenMayWriteAToolItHolds(t *testing.T) {
	srv, reg := selfHarness(t)
	dir := t.TempDir()
	if _, err := reg.PutTool(t.Context(), registry.Tool{ID: "deploy", HostPath: dir}); err != nil {
		t.Fatalf("put tool: %v", err)
	}
	if _, err := reg.Update(t.Context(), registry.Agent{ID: "dev", Tools: []string{"deploy"}}); err != nil {
		t.Fatalf("grant to dev: %v", err)
	}

	token := core.DeriveAgentToken("dev", selfKey)
	resp := do(t, srv, http.MethodPut, "/v1/tools/deploy", token,
		map[string]any{"id": "deploy", "host_path": dir, "description": "changed by its holder"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	v, err := reg.LatestToolVersion(t.Context(), "deploy")
	if err != nil {
		t.Fatalf("latest tool version: %v", err)
	}
	if v.Author != "agent:dev" {
		t.Errorf("author = %q, want agent:dev", v.Author)
	}
}

// With no key configured nothing changes: a would-be per-agent token is simply
// not a credential, so a deployment that has not opted in is unaffected.
func TestWithoutAKeyPerAgentTokensDoNotExist(t *testing.T) {
	dir := t.TempDir()
	cfg := selfConfig(t, dir)
	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })
	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Seed(context.Background(), []registry.Agent{{ID: "dev"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	api := NewHTTP(disp, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]string{testToken}, nil, 0, 4)
	// No UseSelfTokenKey.
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	resp := do(t, srv, http.MethodGet, "/v1/agents", core.DeriveAgentToken("dev", selfKey), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 when no key is configured", resp.StatusCode)
	}
}
