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
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// newSecretHarness builds an HTTP server whose registry has the secret store
// enabled, and returns the registry so a test can inspect what the API wrote.
// withKey=false leaves the store disabled, to exercise the fail-closed path.
func newSecretHarness(t *testing.T, withKey bool) (*httptest.Server, *registry.Store) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	if err := os.WriteFile(configPath, []byte(`
workspace_root: ws
container:
  image: roundclaw/claude:test
http:
  wait_timeout: 300ms
  max_sse_per_agent: 2
agents:
  - id: pr-reviewer
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
	if _, err := reg.Seed(context.Background(), []registry.Agent{{ID: "pr-reviewer", Enabled: true}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if withKey {
		if err := reg.UseSecretKey("test-master-key"); err != nil {
			t.Fatalf("use key: %v", err)
		}
	}

	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := NewHTTP(disp, log, []string{testToken}, nil, cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, reg
}

func doReq(t *testing.T, srv *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSecretLifecycleOverHTTP(t *testing.T) {
	srv, reg := newSecretHarness(t, true)

	// Set a per-agent secret.
	resp := doReq(t, srv, http.MethodPut, "/v1/agents/pr-reviewer/secrets/GITHUB_TOKEN",
		map[string]string{"value": "ghp_supersecret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT secret: status %d", resp.StatusCode)
	}

	// List must show the name but never the value.
	resp = doReq(t, srv, http.MethodGet, "/v1/agents/pr-reviewer/secrets", nil)
	raw, _ := io.ReadAll(resp.Body)
	if bytes.Contains(raw, []byte("ghp_supersecret")) {
		t.Fatalf("secret value leaked into the list response: %s", raw)
	}
	if !bytes.Contains(raw, []byte("GITHUB_TOKEN")) {
		t.Fatalf("list did not include the secret name: %s", raw)
	}

	// The value must actually be stored and decryptable for injection.
	got, err := reg.SecretsForAgent(t.Context(), "pr-reviewer")
	if err != nil {
		t.Fatalf("secrets for agent: %v", err)
	}
	if got["GITHUB_TOKEN"] != "ghp_supersecret" {
		t.Errorf("stored value = %q, want the value that was PUT", got["GITHUB_TOKEN"])
	}

	// Delete removes it.
	resp = doReq(t, srv, http.MethodDelete, "/v1/agents/pr-reviewer/secrets/GITHUB_TOKEN", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE secret: status %d", resp.StatusCode)
	}
	got, _ = reg.SecretsForAgent(t.Context(), "pr-reviewer")
	if _, ok := got["GITHUB_TOKEN"]; ok {
		t.Error("secret still present after delete")
	}
}

func TestGlobalSecretReachesAgents(t *testing.T) {
	srv, reg := newSecretHarness(t, true)

	resp := doReq(t, srv, http.MethodPut, "/v1/secrets/SHARED", map[string]string{"value": "v"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT global: status %d", resp.StatusCode)
	}
	got, err := reg.SecretsForAgent(t.Context(), "pr-reviewer")
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	if got["SHARED"] != "v" {
		t.Errorf("global secret did not reach the agent: %v", got)
	}
}

func TestSecretEndpointFailsClosedWithoutKey(t *testing.T) {
	srv, _ := newSecretHarness(t, false)

	resp := doReq(t, srv, http.MethodPut, "/v1/agents/pr-reviewer/secrets/TOKEN",
		map[string]string{"value": "x"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("PUT without a master key: status %d, want 503", resp.StatusCode)
	}
}

func TestSecretForUnknownAgentIs404(t *testing.T) {
	srv, _ := newSecretHarness(t, true)

	resp := doReq(t, srv, http.MethodPut, "/v1/agents/ghost/secrets/TOKEN",
		map[string]string{"value": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT for unknown agent: status %d, want 404", resp.StatusCode)
	}
}
