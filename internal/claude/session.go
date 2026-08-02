// Package claude turns a roundclaw turn into a `claude` CLI invocation and
// turns that process's stream-json output back into events roundclaw can store.
// It never talks to the Anthropic API directly and never uses the Agent SDK:
// the container holds nothing but the `claude` binary.
package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// TranscriptPath is where the CLI keeps a session's transcript, on the host
// side of the ClaudeHome mount.
//
// The CLI names a project directory after the working directory it was launched
// in, and every turn runs with the same cwd, so all of one agent's sessions land
// in one directory. The name is derived from ContainerWorkspace rather than
// written out, so the two cannot drift apart.
//
// This is the one thing roundclaw assumes about the CLI's private layout, and it
// is deliberately only ever used as a hint — SessionExists answering wrongly
// costs a container start, because the run itself corrects it. Nothing may
// conclude that a session is absent from this alone.
func TranscriptPath(claudeHome, sessionID string) string {
	project := strings.ReplaceAll(ContainerWorkspace, "/", "-")
	return filepath.Join(claudeHome, "projects", project, sessionID+".jsonl")
}

// SessionExists reports whether the CLI already holds this session.
//
// A missing file is an ordinary answer, not a failure. Any other error is
// returned alongside `false` so the caller can say so out loud: proceeding as if
// there were no session is the right move either way, but an unreadable
// claude-home is worth knowing about.
func SessionExists(claudeHome, sessionID string) (bool, error) {
	_, err := os.Stat(TranscriptPath(claudeHome, sessionID))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("stat session transcript: %w", err)
	}
}

// SessionTaken reports whether a failed run refused the session ID it was told
// to create, because the CLI already had one by that name.
//
// SessionNotFound is the same answer from the other direction: a run told to
// continue a session the CLI does not hold.
//
// Both match the CLI's own words, which couples roundclaw to strings it does not
// promise. Each is therefore one of two independent signals rather than the only
// one — SessionExists answers the same question from the filesystem, and either
// alone is enough. A change to these messages degrades the check to one signal
// instead of breaking it.
//
// Both are what the CLI in the agent image actually prints, not what it looks
// like it should print. They are worth re-checking against a new image.
func SessionTaken(output string) bool {
	return strings.Contains(output, "is already in use")
}

// SessionNotFound reports whether a failed run was told to continue a session
// the CLI does not have.
func SessionNotFound(output string) bool {
	return strings.Contains(output, "No conversation found with session ID")
}
