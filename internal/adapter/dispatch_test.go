package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/roundtable/roundclaw/internal/registry"
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
