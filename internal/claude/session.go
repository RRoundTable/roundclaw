// Package claude turns a roundclaw turn into a `claude` CLI invocation and
// turns that process's stream-json output back into events roundclaw can store.
// It never talks to the Anthropic API directly and never uses the Agent SDK:
// the container holds nothing but the `claude` binary.
package claude

import (
	"github.com/google/uuid"
)

// sessionNamespace is a fixed UUIDv5 namespace for roundclaw session IDs. It
// must never change: it is what makes a session ID reproducible from a workflow
// ID across restarts, redeploys, and Temporal activity retries.
var sessionNamespace = uuid.MustParse("6f0a1c2e-9d4b-5a7f-8c31-0b2d4e6f8a10")

// SessionID derives the Claude Code session UUID for a workflow.
//
// This is the load-bearing trick of the whole design. Because the ID is a pure
// function of the workflow ID, a retried activity — or one picked up by a
// different worker after a crash — reconnects to the same Claude session
// instead of starting a fresh one. Nothing has to capture a session ID from
// process output and persist it, so there is no window where that write can be
// lost.
func SessionID(workflowID string) string {
	return uuid.NewSHA1(sessionNamespace, []byte(workflowID)).String()
}
