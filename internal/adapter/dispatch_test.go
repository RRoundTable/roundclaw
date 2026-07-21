package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

func testRegistry(t *testing.T) *registry.Store {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })

	seeded, err := reg.Seed(context.Background(), []registry.Agent{
		{ID: "pr-reviewer", Description: "Reviews pull requests", DiscordChannels: []string{"chan-bound"}},
		{ID: "ops-helper", Description: "Answers infra questions"},
	})
	if err != nil || seeded != 2 {
		t.Fatalf("seed: %v (seeded %d)", err, seeded)
	}
	return reg
}

// An explicit agent argument must win, so any agent can be called from any
// channel — including one bound to a different agent.
func TestResolveAgentPrefersExplicitTarget(t *testing.T) {
	reg := testRegistry(t)

	agent, err := ResolveAgent(t.Context(), reg, "ops-helper", "chan-bound")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if agent.ID != "ops-helper" {
		t.Errorf("resolved %q, want ops-helper — the channel binding overrode the explicit target", agent.ID)
	}
}

// Calling an agent from a channel bound to nothing is the whole point of the
// direct-invocation command.
func TestResolveAgentWorksInUnboundChannel(t *testing.T) {
	reg := testRegistry(t)

	agent, err := ResolveAgent(t.Context(), reg, "pr-reviewer", "chan-unbound")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if agent.ID != "pr-reviewer" {
		t.Errorf("resolved %q, want pr-reviewer", agent.ID)
	}
}

func TestResolveAgentFallsBackToChannelBinding(t *testing.T) {
	reg := testRegistry(t)

	agent, err := ResolveAgent(t.Context(), reg, "", "chan-bound")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if agent.ID != "pr-reviewer" {
		t.Errorf("resolved %q, want the channel's bound agent", agent.ID)
	}
}

func TestResolveAgentRejectsUnknownAndUnbound(t *testing.T) {
	reg := testRegistry(t)

	if _, err := ResolveAgent(t.Context(), reg, "no-such-agent", "chan-bound"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("unknown agent: err = %v, want ErrUnknownAgent", err)
	}
	if _, err := ResolveAgent(t.Context(), reg, "", "chan-unbound"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("unbound channel with no explicit target: err = %v, want ErrUnknownAgent", err)
	}
}

func newStores(t *testing.T) *store.Registry {
	t.Helper()
	dir := t.TempDir()
	stores := store.NewRegistry(store.ReadWrite, func(agentID string) string {
		return filepath.Join(dir, agentID, "state.db")
	})
	t.Cleanup(func() { stores.Close() })
	return stores
}

// Disable and delete must stop every conversation the agent runs, not just the
// default one — otherwise a thread's turn keeps running after the agent is gone.
// The set is read from the agent's own state.db, so a '-' in either the agent ID
// or a thread ID can never make one agent's stop leak into another's.
func TestStopAllStopsEveryConversation(t *testing.T) {
	reg := testRegistry(t)
	stores := newStores(t)

	st, err := stores.Get("pr-reviewer")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, conv := range []string{"", "thread-a", "thread-b"} {
		if _, _, err := st.CreateTurn(t.Context(), store.NewTurn{
			Request: "hi", Origin: core.HTTPPollOrigin(), Conversation: conv,
		}); err != nil {
			t.Fatalf("create turn in %q: %v", conv, err)
		}
	}

	tc := &fakeTemporal{}
	disp := NewDispatcher(&config.Config{}, tc, stores, reg)

	if err := disp.StopAll(t.Context(), "pr-reviewer", "test"); err != nil {
		t.Fatalf("stop all: %v", err)
	}

	got := tc.signaledWorkflows()
	want := []string{
		"roundclaw-pr-reviewer-default",
		"roundclaw-pr-reviewer-thread-a",
		"roundclaw-pr-reviewer-thread-b",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StopAll signalled %v, want %v", got, want)
	}
	for _, s := range tc.sent() {
		if s != rcworkflow.SignalStop {
			t.Errorf("StopAll sent %q, want only stop signals", s)
		}
	}
}

// An agent that has never taken a turn has no state.db rows, but the default
// conversation must still be stopped — StopAll must never be weaker than the
// single-conversation stop it replaced.
func TestStopAllAlwaysStopsTheDefault(t *testing.T) {
	reg := testRegistry(t)
	stores := newStores(t)

	tc := &fakeTemporal{}
	disp := NewDispatcher(&config.Config{}, tc, stores, reg)

	if err := disp.StopAll(t.Context(), "ops-helper", "test"); err != nil {
		t.Fatalf("stop all: %v", err)
	}

	got := tc.signaledWorkflows()
	if len(got) != 1 || got[0] != "roundclaw-ops-helper-default" {
		t.Errorf("StopAll on a never-run agent signalled %v, want just the default", got)
	}
}

// Discord rejects choice names over 100 characters, which would make the whole
// autocomplete response fail rather than just truncate.
func TestAgentLabelFitsDiscordChoiceLimit(t *testing.T) {
	long := registry.Agent{
		ID:          "some-agent",
		Description: stringOfLength(200),
	}
	if got := len(long.Label()); got > 100 {
		t.Errorf("label is %d characters, Discord's limit is 100", got)
	}

	plain := registry.Agent{ID: "bare"}
	if plain.Label() != "bare" {
		t.Errorf("label without a description = %q, want %q", plain.Label(), "bare")
	}
}

func stringOfLength(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// The modal folds tools and channels into free text, so the parsing has to
// tolerate what people actually type.
func TestSplitListTolerates(t *testing.T) {
	cases := map[string][]string{
		"Read, Grep, Bash":   {"Read", "Grep", "Bash"},
		" Read ,, Grep , ":   {"Read", "Grep"},
		"Read":               {"Read"},
		"":                   nil,
		"   ":                nil,
		",,,":                nil,
		"Read,Grep,\nWrite ": {"Read", "Grep", "Write"},
	}
	for in, want := range cases {
		got := splitList(in)
		if len(got) != len(want) {
			t.Errorf("splitList(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitList(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

// Some Discord clients send an autocomplete choice's display label
// ("id — description") instead of its value. Resolution must recover the id.
func TestResolveAgentRecoversFromLeakedLabel(t *testing.T) {
	reg := testRegistry(t)
	label := "pr-reviewer — Reviews pull requests and more, at length…"
	agent, err := ResolveAgent(t.Context(), reg, label, "chan-unbound")
	if err != nil {
		t.Fatalf("resolve from label: %v", err)
	}
	if agent.ID != "pr-reviewer" {
		t.Errorf("resolved %q, want pr-reviewer from the leaked label", agent.ID)
	}
}
