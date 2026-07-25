package adapter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// fakeSender records what an agent said instead of sending it to Discord.
type fakeSender struct {
	mu   sync.Mutex
	sent []string // "channel: text"
	err  error
}

func (f *fakeSender) ChannelMessageSend(channelID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, channelID+": "+text)
	return nil
}

func (f *fakeSender) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

const delegateToken = "delegate-token-value"

// twoAgentHarness gives pm a conversation with a Discord audience and dev
// nothing, which is what the notify and say paths are decided against.
func twoAgentHarness(t *testing.T) (*httptest.Server, *fakeSender, *store.Registry) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := `
workspace_root: ws
container:
  image: roundclaw/claude:test
http:
  wait_timeout: 300ms
  max_sse_per_agent: 2
agents:
  - id: pm
  - id: dev
`
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
	if _, err := reg.Seed(context.Background(), []registry.Agent{
		{ID: "pm"}, {ID: "dev"},
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	pm, err := stores.Get("pm")
	if err != nil {
		t.Fatalf("open pm store: %v", err)
	}
	if _, _, err := pm.CreateTurn(context.Background(), store.NewTurn{
		Request: "시작", Origin: core.DiscordOrigin("chan-1", "msg-1"), Conversation: "thread-1",
	}); err != nil {
		t.Fatalf("seed pm turn: %v", err)
	}

	sender := &fakeSender{}
	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := NewHTTP(disp, log, []string{testToken}, []string{delegateToken},
		cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	api.SetMessageSender(sender)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, sender, stores
}

func TestSayReachesTheConversationsChannel(t *testing.T) {
	srv, sender, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/pm/messages", testToken, "", map[string]any{
		"text": "빌드 중, 5분쯤 더", "conversation": "thread-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	got := sender.messages()
	if len(got) != 1 || got[0] != "chan-1: 빌드 중, 5분쯤 더" {
		t.Errorf("sent %v, want [chan-1: 빌드 중, 5분쯤 더]", got)
	}
}

// The target is resolved from the conversation's own history, never from the
// request, so an agent with no audience cannot pick one.
func TestSayRefusesAConversationWithNoAudience(t *testing.T) {
	srv, sender, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/dev/messages", testToken, "", map[string]any{
		"text": "hello", "conversation": "nowhere",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
	if len(sender.messages()) != 0 {
		t.Errorf("spoke anyway: %v", sender.messages())
	}
}

func TestSayRejectsEmptyText(t *testing.T) {
	srv, _, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/pm/messages", testToken, "", map[string]any{
		"text": "   ", "conversation": "thread-1",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}

// Speaking is on the restricted surface: it starts no work and can only reach an
// audience the agent already has.
func TestSayIsAllowedForDelegateTokens(t *testing.T) {
	srv, sender, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/pm/messages", delegateToken, "", map[string]any{
		"text": "진행 중", "conversation": "thread-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if len(sender.messages()) != 1 {
		t.Errorf("delegate token could not speak: %v", sender.messages())
	}
}

// A turn sent with notify carries its return address on the row, which is what
// survives the caller going away.
func TestNotifyRecordsTheReturnAddress(t *testing.T) {
	srv, _, stores := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/dev/requests", delegateToken, "", map[string]any{
		"text":   "QA 버튼 만들어줘",
		"notify": map[string]any{"agent": "pm", "conversation": "thread-1"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %s, want 202", resp.Status)
	}
	out := decode[map[string]any](t, resp)
	turnID := int64(out["turn_id"].(float64))
	if turnID == 0 {
		t.Fatal("no turn id in the response")
	}

	// The response is a convenience; the row is what delivery reads hours later.
	dev, err := stores.Get("dev")
	if err != nil {
		t.Fatalf("open dev store: %v", err)
	}
	turn, err := dev.GetTurn(t.Context(), turnID)
	if err != nil {
		t.Fatalf("read dev turn: %v", err)
	}
	if turn.Origin.Type != core.OriginAgent {
		t.Fatalf("origin type = %q, want agent", turn.Origin.Type)
	}
	if turn.Origin.Agent != "pm" || turn.Origin.Conversation != "thread-1" {
		t.Errorf("return address = %s, want agent:pm/thread-1", turn.Origin)
	}
}

func TestNotifyRefusesAnEndlessSelfLoop(t *testing.T) {
	srv, _, _ := twoAgentHarness(t)

	// dev notifying dev in the same conversation would queue a successor of
	// itself forever, each iteration paying for a container.
	resp := post(t, srv, "/v1/agents/dev/requests", delegateToken, "", map[string]any{
		"text":            "go",
		"conversation_id": "loop",
		"notify":          map[string]any{"agent": "dev", "conversation": "loop"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}

// Delegating to yourself in a different conversation is a background job, not a
// loop: it terminates and is bounded by the ordinary queue.
func TestNotifyAllowsADifferentConversationOfTheSameAgent(t *testing.T) {
	srv, _, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/dev/requests", delegateToken, "", map[string]any{
		"text":            "go",
		"conversation_id": "worker",
		"notify":          map[string]any{"agent": "dev", "conversation": "main"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %s, want 202", resp.Status)
	}
}

func TestNotifyRejectsAnUnknownAgent(t *testing.T) {
	srv, _, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/dev/requests", delegateToken, "", map[string]any{
		"text":   "go",
		"notify": map[string]any{"agent": "ghost"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}

func TestNotifyAndCallbackAreMutuallyExclusive(t *testing.T) {
	srv, _, _ := twoAgentHarness(t)

	resp := post(t, srv, "/v1/agents/dev/requests", delegateToken, "", map[string]any{
		"text":         "go",
		"callback_url": "https://example.com/hook",
		"notify":       map[string]any{"agent": "pm"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}
