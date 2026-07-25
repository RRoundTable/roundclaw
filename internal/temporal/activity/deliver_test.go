package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// recordingSignaller stands in for the Temporal client so a notification can be
// observed without a server. TemporalSignaller exists as an interface precisely
// so this is possible.
type recordingSignaller struct {
	mu        sync.Mutex
	workflows []string
	signals   []string
}

func (r *recordingSignaller) SignalWithStartWorkflow(_ context.Context, workflowID, signalName string,
	_ any, _ client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workflows = append(r.workflows, workflowID)
	r.signals = append(r.signals, signalName)
	return nil, nil
}

func (r *recordingSignaller) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.workflows...)
}

// notifyHarness builds two agents — a delegator and a worker — and gives the
// delegator's conversation a Discord turn, which is the address a notification
// has to find.
func notifyHarness(t *testing.T) (*Activities, *recordingSignaller, *store.Store) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\n" +
		"container:\n  image: test-image\n" +
		"agents:\n  - id: pm\n  - id: dev\n"
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
	for _, id := range []string{"pm", "dev"} {
		if _, err := reg.Create(context.Background(), registry.Agent{ID: id, Enabled: true}); err != nil {
			t.Fatalf("create agent %s: %v", id, err)
		}
	}

	pm, err := stores.Get("pm")
	if err != nil {
		t.Fatalf("open pm store: %v", err)
	}
	// The turn a human started. Its origin is the address every later turn in
	// this conversation answers to, including one created by a notification.
	if _, _, err := pm.CreateTurn(context.Background(), store.NewTurn{
		Request: "dev한테 시켜줘", Origin: core.DiscordOrigin("thread-1", "msg-1"),
		Conversation: "thread-1",
	}); err != nil {
		t.Fatalf("seed pm turn: %v", err)
	}

	sig := &recordingSignaller{}
	return NewActivities(cfg, stores, reg, nil, sig), sig, pm
}

// runDeliver executes the activity in a real activity context, the same way
// run_turn_test does: activity.GetLogger only works inside one.
func runDeliver(t *testing.T, a *Activities, in DeliverInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a)
	_, err := env.ExecuteActivity(a.DeliverResponse, in)
	return err
}

func agentDelivery() DeliverInput {
	return DeliverInput{
		AgentID:      "dev",
		Conversation: "pm-thread-1",
		Origin:       core.AgentOrigin("pm", "thread-1"),
		Result: core.TurnResult{
			TurnID: 64, Status: core.TurnDone,
			Text: "버튼 배포 완료", CostUSD: 1.23,
		},
	}
}

func TestDeliverToAgentWakesTheDelegator(t *testing.T) {
	a, sig, pm := notifyHarness(t)

	if err := runDeliver(t, a, agentDelivery()); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	turns, err := pm.RecentTurnsIn(t.Context(), "thread-1", 10)
	if err != nil {
		t.Fatalf("read pm turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("pm has %d turns in the conversation, want 2 (the human's and the notification)", len(turns))
	}

	// The notification is the newest turn, and it must answer where the
	// conversation answers — not back into the agent that sent it.
	got := turns[0]
	if got.Origin.Type != core.OriginDiscord || got.Origin.ChannelID != "thread-1" {
		t.Errorf("notification origin = %s, want discord:thread-1", got.Origin)
	}
	if got.Conversation != "thread-1" {
		t.Errorf("notification conversation = %q, want thread-1", got.Conversation)
	}
	// The handle must name the conversation that did the work (dev's), not the one
	// being notified (pm's) — a follow-up has to resume the session that knows it.
	for _, want := range []string{"버튼 배포 완료", "turn 64", "roundclaw send dev --conversation pm-thread-1"} {
		if !strings.Contains(got.Request, want) {
			t.Errorf("notification prompt is missing %q:\n%s", want, got.Request)
		}
	}

	if sent := sig.sent(); len(sent) != 1 || sent[0] != "roundclaw-pm-thread-1" {
		t.Errorf("signalled %v, want [roundclaw-pm-thread-1]", sent)
	}
}

// DeliverResponse is retried by the workflow, so the same delivery can arrive
// more than once. The second one must be a no-op, or the delegator answers the
// human twice for one delegated task.
func TestDeliverToAgentIsIdempotent(t *testing.T) {
	a, sig, pm := notifyHarness(t)

	for i := 0; i < 3; i++ {
		if err := runDeliver(t, a, agentDelivery()); err != nil {
			t.Fatalf("deliver %d: %v", i, err)
		}
	}

	turns, err := pm.RecentTurnsIn(t.Context(), "thread-1", 10)
	if err != nil {
		t.Fatalf("read pm turns: %v", err)
	}
	if len(turns) != 2 {
		t.Errorf("pm has %d turns after three deliveries, want 2", len(turns))
	}
	if sent := sig.sent(); len(sent) != 1 {
		t.Errorf("signalled %d times, want 1: %v", len(sent), sent)
	}
}

// A failed delegation has to travel the same path as a successful one. Reporting
// only successes is how a dead delegated turn becomes silence.
func TestDeliverToAgentCarriesFailures(t *testing.T) {
	a, _, pm := notifyHarness(t)

	in := agentDelivery()
	in.Result = core.TurnResult{
		TurnID: 65, Status: core.TurnError,
		ErrorMessage: "container exited with code 1",
	}
	if err := runDeliver(t, a, in); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	turns, err := pm.RecentTurnsIn(t.Context(), "thread-1", 10)
	if err != nil {
		t.Fatalf("read pm turns: %v", err)
	}
	if !strings.Contains(turns[0].Request, "container exited with code 1") {
		t.Errorf("failure was not reported to the delegator:\n%s", turns[0].Request)
	}
	if !strings.Contains(turns[0].Request, "실패") {
		t.Errorf("failure was not labelled as one:\n%s", turns[0].Request)
	}
}

// A deleted delegator cannot be woken, and retrying until it reappears would
// spin forever.
func TestDeliverToAgentGivesUpOnUnknownAgent(t *testing.T) {
	a, sig, _ := notifyHarness(t)

	in := agentDelivery()
	in.Origin = core.AgentOrigin("ghost", "")
	err := runDeliver(t, a, in)
	if err == nil {
		t.Fatal("delivery to an unknown agent was accepted")
	}
	if len(sig.sent()) != 0 {
		t.Errorf("signalled %v for an unknown agent", sig.sent())
	}
}

// A conversation whose turns are all notifications has no human audience. The
// result is still recorded; there is simply nowhere to announce it.
func TestReplyOriginFallsBackToPollWhenNoAudience(t *testing.T) {
	a, _, pm := notifyHarness(t)

	origin, err := a.replyOriginFor(t.Context(), pm, "unseen-thread")
	if err != nil {
		t.Fatalf("reply origin: %v", err)
	}
	if origin.Type != core.OriginHTTPPoll {
		t.Errorf("origin = %s, want http_poll", origin)
	}
}

// The human-facing turn can be several notifications back once a delegator has
// fanned out a few tasks; the address must still be found.
func TestReplyOriginLooksPastNotifications(t *testing.T) {
	a, _, pm := notifyHarness(t)

	for i := 0; i < 5; i++ {
		if _, _, err := pm.CreateTurn(t.Context(), store.NewTurn{
			Request: "notification", Origin: core.HTTPPollOrigin(), Conversation: "thread-1",
		}); err != nil {
			t.Fatalf("seed notification %d: %v", i, err)
		}
	}

	origin, err := a.replyOriginFor(t.Context(), pm, "thread-1")
	if err != nil {
		t.Fatalf("reply origin: %v", err)
	}
	if origin.Type != core.OriginDiscord || origin.ChannelID != "thread-1" {
		t.Errorf("origin = %s, want discord:thread-1", origin)
	}
}
