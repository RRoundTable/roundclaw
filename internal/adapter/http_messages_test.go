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
  - id: qa
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
		{ID: "pm"}, {ID: "dev"}, {ID: "qa"},
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

// delegate queues a turn for agent with a return address on it, and returns the
// turn it created — the row that carries the audience.
func delegate(t *testing.T, srv *httptest.Server, agent, notifyAgent, notifyConversation string) int64 {
	t.Helper()
	resp := post(t, srv, "/v1/agents/"+agent+"/requests", testToken, "", map[string]any{
		"text":   "QA 버튼 만들어줘",
		"notify": map[string]any{"agent": notifyAgent, "conversation": notifyConversation},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delegate to %s: status = %s, want 202", agent, resp.Status)
	}
	var sub struct {
		TurnID int64 `json:"turn_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatalf("decode submission: %v", err)
	}
	return sub.TurnID
}

// The reason the audience is stamped at admission at all.
//
// A delegated turn runs in a conversation that holds nothing but the delegation,
// so there is no history to infer an audience from and an agent working inside
// one had nowhere to report progress — this failed outright. It now reaches the
// thread the work was asked for in.
func TestSayFromADelegatedTurnReachesTheThreadItWasAskedIn(t *testing.T) {
	srv, sender, stores := twoAgentHarness(t)

	turnID := delegate(t, srv, "dev", "pm", "thread-1")

	dev, err := stores.Get("dev")
	if err != nil {
		t.Fatalf("open dev store: %v", err)
	}
	turn, err := dev.GetTurn(context.Background(), turnID)
	if err != nil {
		t.Fatalf("read dev turn: %v", err)
	}
	if turn.Origin.Type != core.OriginAgent || turn.Origin.Agent != "pm" {
		t.Errorf("origin = %s, want the result to still go back to pm", turn.Origin)
	}
	audience, ok := turn.Origin.Listening()
	if !ok || audience.ChannelID != "chan-1" {
		t.Fatalf("audience = %v (ok=%v), want pm's thread carried onto the row", audience, ok)
	}

	resp := post(t, srv, "/v1/agents/dev/messages", delegateToken, "", map[string]any{
		"text": "빌드 중, 5분쯤 더", "turn_id": turnID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if got := sender.messages(); len(got) != 1 || got[0] != "chan-1: 빌드 중, 5분쯤 더" {
		t.Errorf("sent %v, want [chan-1: 빌드 중, 5분쯤 더]", got)
	}
}

// Carrying the address rather than searching for it is what makes depth
// irrelevant: qa is two hops from the person, and the hop that admitted it only
// had to look at dev's row, which had been stamped the same way.
func TestTheAudienceSurvivesASecondHopOfDelegation(t *testing.T) {
	srv, sender, stores := twoAgentHarness(t)

	delegate(t, srv, "dev", "pm", "thread-1")
	// dev delegates onward from its own default conversation, which contains
	// only the turn pm gave it.
	deep := delegate(t, srv, "qa", "dev", "")

	qa, err := stores.Get("qa")
	if err != nil {
		t.Fatalf("open qa store: %v", err)
	}
	turn, err := qa.GetTurn(context.Background(), deep)
	if err != nil {
		t.Fatalf("read qa turn: %v", err)
	}
	if audience, ok := turn.Origin.Listening(); !ok || audience.ChannelID != "chan-1" {
		t.Fatalf("audience = %v (ok=%v), want the thread two hops up", audience, ok)
	}

	resp := post(t, srv, "/v1/agents/qa/messages", delegateToken, "", map[string]any{
		"text": "스모크 통과", "turn_id": deep,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if got := sender.messages(); len(got) != 1 || got[0] != "chan-1: 스모크 통과" {
		t.Errorf("sent %v, want [chan-1: 스모크 통과]", got)
	}
}

// Naming the turn is what makes the answer exact. A second delegation arriving
// while the first is still working is the newer row in the same conversation, so
// inferring from history would report progress into a stranger's thread.
func TestSayNamesItsOwnTurnRatherThanTheNewestOne(t *testing.T) {
	srv, sender, stores := twoAgentHarness(t)

	pm, err := stores.Get("pm")
	if err != nil {
		t.Fatalf("open pm store: %v", err)
	}
	if _, _, err := pm.CreateTurn(context.Background(), store.NewTurn{
		Request: "다른 스레드", Origin: core.DiscordOrigin("chan-2", "msg-2"), Conversation: "thread-2",
	}); err != nil {
		t.Fatalf("seed second pm thread: %v", err)
	}

	mine := delegate(t, srv, "dev", "pm", "thread-1")
	delegate(t, srv, "dev", "pm", "thread-2") // arrives behind me, in my queue

	resp := post(t, srv, "/v1/agents/dev/messages", delegateToken, "", map[string]any{
		"text": "빌드 중", "turn_id": mine,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if got := sender.messages(); len(got) != 1 || got[0] != "chan-1: 빌드 중" {
		t.Errorf("sent %v, want [chan-1: 빌드 중] — the thread of the turn I am running", got)
	}

	// And what naming the turn avoids: without one there is only the
	// conversation, whose newest turn is the stranger's.
	resp = post(t, srv, "/v1/agents/dev/messages", delegateToken, "", map[string]any{
		"text": "누구냐 넌",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if got := sender.messages(); len(got) != 2 || got[1] != "chan-2: 누구냐 넌" {
		t.Errorf("sent %v, want the second message in chan-2 — inference can only take the newest", got)
	}
}

// An API-driven delegator has nobody watching, and inheriting the last address
// anyone happened to use would put a progress note in an unrelated channel.
func TestADelegatedTurnWithNoWatcherInheritsNothing(t *testing.T) {
	srv, sender, stores := twoAgentHarness(t)

	turnID := delegate(t, srv, "dev", "qa", "") // qa has never been spoken to

	dev, err := stores.Get("dev")
	if err != nil {
		t.Fatalf("open dev store: %v", err)
	}
	turn, err := dev.GetTurn(context.Background(), turnID)
	if err != nil {
		t.Fatalf("read dev turn: %v", err)
	}
	if turn.Origin.Audience != nil {
		t.Errorf("audience = %v, want none", turn.Origin.Audience)
	}

	resp := post(t, srv, "/v1/agents/dev/messages", delegateToken, "", map[string]any{
		"text": "진행 중", "turn_id": turnID,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
	if len(sender.messages()) != 0 {
		t.Errorf("spoke anyway: %v", sender.messages())
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
