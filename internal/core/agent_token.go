package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// The credential that says which agent is calling.
//
// Every other token roundclaw issues answers "what may be done" and says nothing
// about who is doing it, which is why authorship has always been a self-declared
// header and a changelog rather than an audit log. That is fine until something
// has to be gated on being an agent's own change — a gate keyed on an asserted
// author is escaped by asserting a different one (adr/003).
//
// Derived rather than stored: there is no table to keep, no issuance step, and
// rotating every agent's credential is rotating the one key. The same derivation
// runs in the worker, which injects the token into a container, and in the
// gateway, which recognises it, so neither has to look anything up or agree on
// anything but the key.
//
// The agent id travels in the clear inside the token. It is not a secret — it is
// already in paths, logs and channel bindings — and carrying it means the gateway
// verifies in one HMAC instead of one per registered agent.

const (
	agentTokenPrefix = "rcs"
	// agentTokenSep is not in the agent-id charset ([A-Za-z0-9_-]), so an id can
	// never contain it and the split is unambiguous.
	agentTokenSep = "."
	// agentTokenPurpose keeps this HMAC from colliding with any other use of the
	// same key, and the version in it leaves room to change the scheme without
	// silently accepting both.
	agentTokenPurpose = "roundclaw:agent-token:v1:"
)

// DeriveAgentToken returns the bearer credential identifying one agent.
//
// An empty key returns an empty token: a deployment that configured no key has
// no per-agent credentials, and must not get a guessable one derived from "".
func DeriveAgentToken(agentID, key string) string {
	if key == "" || agentID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(agentTokenPurpose))
	mac.Write([]byte(agentID))
	return agentTokenPrefix + agentTokenSep + agentID + agentTokenSep + hex.EncodeToString(mac.Sum(nil))
}

// VerifyAgentToken reports which agent a presented token identifies.
//
// The comparison is constant-time, and a token whose id was tampered with fails
// because the id is what the MAC is over: rewriting it to name another agent
// invalidates the very MAC that would have authorised it.
func VerifyAgentToken(presented, key string) (string, bool) {
	if key == "" {
		return "", false
	}
	parts := strings.Split(strings.TrimSpace(presented), agentTokenSep)
	if len(parts) != 3 || parts[0] != agentTokenPrefix {
		return "", false
	}
	agentID := parts[1]
	if ValidateAgentID(agentID) != nil {
		return "", false
	}
	want := DeriveAgentToken(agentID, key)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(presented))) != 1 {
		return "", false
	}
	return agentID, true
}

// LooksLikeAgentToken reports whether a credential is shaped like a per-agent
// one, so the gateway can tell "wrong key or forged" apart from "some other
// token entirely" without leaking which.
func LooksLikeAgentToken(presented string) bool {
	return strings.HasPrefix(strings.TrimSpace(presented), agentTokenPrefix+agentTokenSep)
}
