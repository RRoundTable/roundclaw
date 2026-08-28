package core

import (
	"strings"
	"testing"
)

func TestDerivedTokenNamesItsAgent(t *testing.T) {
	token := DeriveAgentToken("dev", "k")
	got, ok := VerifyAgentToken(token, "k")
	if !ok {
		t.Fatal("a freshly derived token did not verify")
	}
	if got != "dev" {
		t.Errorf("agent = %q, want dev", got)
	}
}

func TestDerivationIsStable(t *testing.T) {
	// The worker derives and the gateway derives; they never exchange the token
	// itself, so the two derivations have to agree exactly.
	if DeriveAgentToken("dev", "k") != DeriveAgentToken("dev", "k") {
		t.Error("the same agent and key produced two different tokens")
	}
}

func TestTwoAgentsGetDifferentTokens(t *testing.T) {
	if DeriveAgentToken("dev", "k") == DeriveAgentToken("ops", "k") {
		t.Error("two agents share a credential; then neither identifies anybody")
	}
}

// The attack the whole scheme exists to stop: take your own token, rewrite the
// id in it, and become somebody else. The id is what the MAC is over, so it
// cannot survive being changed.
func TestRewritingTheAgentIdBreaksTheToken(t *testing.T) {
	token := DeriveAgentToken("dev", "k")
	forged := strings.Replace(token, ".dev.", ".ops.", 1)
	if forged == token {
		t.Fatal("test did not actually rewrite the id")
	}
	if _, ok := VerifyAgentToken(forged, "k"); ok {
		t.Error("a token with a rewritten agent id verified")
	}
}

func TestAnotherKeyDoesNotVerify(t *testing.T) {
	token := DeriveAgentToken("dev", "k")
	if _, ok := VerifyAgentToken(token, "other"); ok {
		t.Error("a token verified under a key it was not derived from")
	}
}

// Rotating the key is how every agent credential is revoked at once, so an old
// token must stop verifying the moment the key moves.
func TestRotatingTheKeyRevokesEveryToken(t *testing.T) {
	old := DeriveAgentToken("dev", "before")
	if _, ok := VerifyAgentToken(old, "after"); ok {
		t.Error("a token survived key rotation")
	}
}

// No key means no per-agent credentials at all. Deriving one from "" would hand
// out a token anybody who guessed the scheme could compute.
func TestNoKeyMeansNoToken(t *testing.T) {
	if got := DeriveAgentToken("dev", ""); got != "" {
		t.Errorf("token = %q, want empty when no key is configured", got)
	}
	if _, ok := VerifyAgentToken("rcs.dev.deadbeef", ""); ok {
		t.Error("a token verified with no key configured")
	}
}

func TestMalformedTokensAreRefused(t *testing.T) {
	for _, tok := range []string{
		"",
		"rcs",
		"rcs.dev",
		"rcs.dev.",
		"rcs.dev.zz.extra",
		"xxx.dev.deadbeef",
		"rcs..deadbeef",
		"rcs.has space.deadbeef",
		DeriveAgentToken("dev", "k") + "a",
	} {
		if _, ok := VerifyAgentToken(tok, "k"); ok {
			t.Errorf("malformed token %q verified", tok)
		}
	}
}

func TestLooksLikeAgentTokenSeparatesShapes(t *testing.T) {
	if !LooksLikeAgentToken(DeriveAgentToken("dev", "k")) {
		t.Error("a real per-agent token was not recognised as one")
	}
	if LooksLikeAgentToken("some-operator-token") {
		t.Error("an operator token was mistaken for a per-agent one")
	}
}
