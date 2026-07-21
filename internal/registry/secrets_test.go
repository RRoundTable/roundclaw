package registry

import (
	"errors"
	"testing"
)

func newSecretStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	if err := s.UseSecretKey("test-master-key"); err != nil {
		t.Fatalf("use secret key: %v", err)
	}
	if _, err := s.Create(t.Context(), Agent{ID: "pr-reviewer", Enabled: true}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return s
}

func TestSecretRoundTripAndPerAgentOverridesGlobal(t *testing.T) {
	s := newSecretStore(t)
	ctx := t.Context()

	// A global default and a per-agent override of the same name, plus one that
	// only the agent has.
	if err := s.PutSecret(ctx, "", "TOKEN", "global-value"); err != nil {
		t.Fatalf("put global: %v", err)
	}
	if err := s.PutSecret(ctx, "pr-reviewer", "TOKEN", "agent-value"); err != nil {
		t.Fatalf("put agent: %v", err)
	}
	if err := s.PutSecret(ctx, "pr-reviewer", "GITHUB_TOKEN", "gh"); err != nil {
		t.Fatalf("put agent-only: %v", err)
	}

	got, err := s.SecretsForAgent(ctx, "pr-reviewer")
	if err != nil {
		t.Fatalf("secrets for agent: %v", err)
	}
	if got["TOKEN"] != "agent-value" {
		t.Errorf("TOKEN = %q, want the per-agent override to win", got["TOKEN"])
	}
	if got["GITHUB_TOKEN"] != "gh" {
		t.Errorf("GITHUB_TOKEN = %q, want gh", got["GITHUB_TOKEN"])
	}

	// An agent with no secrets of its own still sees the global one.
	if _, err := s.Create(ctx, Agent{ID: "other", Enabled: true}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	other, err := s.SecretsForAgent(ctx, "other")
	if err != nil {
		t.Fatalf("secrets for other: %v", err)
	}
	if other["TOKEN"] != "global-value" {
		t.Errorf("other's TOKEN = %q, want the global value", other["TOKEN"])
	}
}

func TestListSecretsNeverReturnsValues(t *testing.T) {
	s := newSecretStore(t)
	ctx := t.Context()
	if err := s.PutSecret(ctx, "pr-reviewer", "GITHUB_TOKEN", "super-secret"); err != nil {
		t.Fatalf("put: %v", err)
	}

	metas, err := s.ListSecrets(ctx, "pr-reviewer")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "GITHUB_TOKEN" {
		t.Fatalf("list = %+v, want one GITHUB_TOKEN entry", metas)
	}
	// SecretMeta has no value field at all — this is a structural guarantee, but
	// assert the name-only shape so a future field addition is a deliberate act.
	if metas[0].Scope != "pr-reviewer" {
		t.Errorf("scope = %q, want pr-reviewer", metas[0].Scope)
	}
}

func TestSecretsFailClosedWithoutKey(t *testing.T) {
	s := newStore(t) // no UseSecretKey
	ctx := t.Context()

	if s.SecretsEnabled() {
		t.Fatal("secrets should be disabled without a key")
	}
	if err := s.PutSecret(ctx, "", "TOKEN", "x"); !errors.Is(err, ErrSecretsDisabled) {
		t.Errorf("PutSecret without a key = %v, want ErrSecretsDisabled", err)
	}
	// Injection degrades to "no secrets", not an error, so an agent using none
	// runs unchanged on a deployment that never configured a key.
	got, err := s.SecretsForAgent(ctx, "any")
	if err != nil || got != nil {
		t.Errorf("SecretsForAgent without a key = %v, %v; want nil, nil", got, err)
	}
}

func TestSecretWrongKeyCannotDecrypt(t *testing.T) {
	s := newSecretStore(t)
	ctx := t.Context()
	if err := s.PutSecret(ctx, "pr-reviewer", "TOKEN", "value"); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Rotating the key must make the old ciphertext unreadable.
	if err := s.UseSecretKey("a-different-master-key"); err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if _, err := s.SecretsForAgent(ctx, "pr-reviewer"); err == nil {
		t.Error("decryption succeeded after a key rotation; the old value should be unreadable")
	}
}

func TestSecretNameValidation(t *testing.T) {
	s := newSecretStore(t)
	ctx := t.Context()
	for _, bad := range []string{"", "1STARTS_WITH_DIGIT", "has-dash", "has space", "has$dollar"} {
		if err := s.PutSecret(ctx, "pr-reviewer", bad, "x"); err == nil {
			t.Errorf("PutSecret accepted invalid name %q", bad)
		}
	}
	for _, ok := range []string{"TOKEN", "GITHUB_TOKEN", "_underscore", "A1"} {
		if err := s.PutSecret(ctx, "pr-reviewer", ok, "x"); err != nil {
			t.Errorf("PutSecret rejected valid name %q: %v", ok, err)
		}
	}
}

func TestPutSecretRejectsUnknownAgent(t *testing.T) {
	s := newSecretStore(t)
	if err := s.PutSecret(t.Context(), "no-such-agent", "TOKEN", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("PutSecret for unknown agent = %v, want ErrNotFound", err)
	}
}
