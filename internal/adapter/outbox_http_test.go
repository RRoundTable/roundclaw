package adapter

import (
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
	"github.com/roundtable/roundclaw/internal/workspace"
)

// outboxHarness gives pm one conversation with a Discord audience, and returns
// the workspace that conversation runs in — the directory an agent's outbox has
// to be found in for any of this to work.
func outboxHarness(t *testing.T) (*httptest.Server, *fakeSender, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\ncontainer:\n  image: test\n" +
		"http:\n  wait_timeout: 300ms\n  max_sse_per_agent: 2\nagents:\n  - id: pm\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
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

	agents, err := reg.Seed(context.Background(), []registry.Agent{{ID: "pm"}})
	if err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	_ = agents

	// The audience: one turn that answered into a Discord thread. Without it the
	// agent has nowhere it is allowed to speak.
	pm, err := stores.Get("pm")
	if err != nil {
		t.Fatalf("open pm store: %v", err)
	}
	if _, _, err := pm.CreateTurn(context.Background(), store.NewTurn{
		Request: "시작", Origin: core.DiscordOrigin("chan-1", "msg-1"), Conversation: "thread-1",
	}); err != nil {
		t.Fatalf("seed pm turn: %v", err)
	}

	agent, err := reg.Get(context.Background(), "pm")
	if err != nil {
		t.Fatalf("get pm: %v", err)
	}
	ws := workspace.Dir(cfg, agent, "thread-1")
	if err := os.MkdirAll(filepath.Join(ws, outboxDir), 0o750); err != nil {
		t.Fatalf("create outbox: %v", err)
	}

	sender := &fakeSender{}
	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	api := NewHTTP(disp, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]string{testToken}, nil, cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	api.SetMessageSender(core.OriginDiscord, sender)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, sender, ws
}

func sayWithFiles(t *testing.T, srv *httptest.Server, body map[string]any) *http.Response {
	t.Helper()
	return post(t, srv, "/v1/agents/pm/messages", testToken, "", body)
}

// The whole point of the outbound path: a result that would be twenty Discord
// messages as text, and would then sit in the session context being re-sent on
// every later turn, leaves as one file instead.
func TestSayAttachesAFileFromTheOutbox(t *testing.T) {
	srv, sender, ws := outboxHarness(t)

	if err := os.WriteFile(filepath.Join(ws, outboxDir, "report.pdf"), []byte("body"), 0o640); err != nil {
		t.Fatalf("write outbox file: %v", err)
	}

	resp := sayWithFiles(t, srv, map[string]any{
		"text":         "리포트 나왔습니다",
		"conversation": "thread-1",
		"files":        []string{"report.pdf"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}

	var got messageResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Files != 1 {
		t.Errorf("response reports %d files, want 1", got.Files)
	}
	if names := sender.attachments(); len(names) != 1 || names[0] != "report.pdf" {
		t.Errorf("attachments = %v, want [report.pdf]", names)
	}
	// The note and its file arrive as one message, not two.
	if msgs := sender.messages(); len(msgs) != 1 {
		t.Errorf("sent %d messages, want 1: %v", len(msgs), msgs)
	}
}

// A file is a complete thought on its own; requiring words with it would just
// produce filler.
func TestSayAcceptsFilesWithNoText(t *testing.T) {
	srv, sender, ws := outboxHarness(t)

	if err := os.WriteFile(filepath.Join(ws, outboxDir, "chart.png"), []byte("png"), 0o640); err != nil {
		t.Fatalf("write outbox file: %v", err)
	}

	resp := sayWithFiles(t, srv, map[string]any{
		"conversation": "thread-1",
		"files":        []string{"chart.png"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if names := sender.attachments(); len(names) != 1 {
		t.Errorf("attachments = %v, want one", names)
	}
}

func TestSayStillRejectsAnEmptyMessage(t *testing.T) {
	srv, _, _ := outboxHarness(t)

	resp := sayWithFiles(t, srv, map[string]any{"conversation": "thread-1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400 for a message with neither text nor files", resp.Status)
	}
}

// The path check is not decoration: it is what stands between a prompt-injected
// agent and the workspace's secrets.
func TestSayRefusesAFileOutsideTheOutbox(t *testing.T) {
	srv, sender, ws := outboxHarness(t)

	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=1"), 0o640); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	resp := sayWithFiles(t, srv, map[string]any{
		"text":         "여기요",
		"conversation": "thread-1",
		"files":        []string{"../.env"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400; the workspace .env would have gone to Discord", resp.Status)
	}
	if msgs := sender.messages(); len(msgs) != 0 {
		t.Errorf("a rejected request still sent %v", msgs)
	}
}

// A file written in one thread must not be reachable from another. The outbox is
// inside the conversation's workspace, so this follows from resolving against
// the right one — which is exactly what the inbox got wrong.
func TestSayResolvesTheOutboxOfTheNamedConversation(t *testing.T) {
	srv, _, ws := outboxHarness(t)

	if err := os.WriteFile(filepath.Join(ws, outboxDir, "thread-only.pdf"), []byte("x"), 0o640); err != nil {
		t.Fatalf("write outbox file: %v", err)
	}

	// The default conversation has its own workspace and never saw this file.
	resp := sayWithFiles(t, srv, map[string]any{
		"text":  "여기요",
		"files": []string{"thread-only.pdf"},
	})
	if resp.StatusCode == http.StatusOK {
		t.Error("a file from a thread's outbox was sent from the default conversation")
	}
}
