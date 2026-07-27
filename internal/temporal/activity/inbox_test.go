package activity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/roundtable/roundclaw/internal/registry"
)

// stage writes a file where admission would have put it, and returns its path.
func stage(t *testing.T, a *Activities, agentID, name, body string) string {
	t.Helper()
	dir := a.cfg.InboxStagingDir(agentID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create staging dir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
	return path
}

// The bug this pins: uploads were written into the agent's base work directory,
// but a conversation runs in its own workspace, and that is what gets mounted at
// /workspace. Every file attached in a thread — which, with reply_in_thread set,
// is nearly all of them — was announced at a path the agent could not open. The
// turn ran, was billed, and answered about a document it never saw.
func TestPlaceAttachmentsReachesAThreadWorkspace(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "pm", Enabled: true}
	base := workDirFor(a.cfg, agent)
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("create base: %v", err)
	}

	staged := stage(t, a, "pm", "ab12cd34-header.png", "pixels")

	ws, err := a.resolveWorkspace(context.Background(), agent, "thread-1")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if ws == base {
		t.Fatal("the thread shares the base workspace; this test would prove nothing")
	}
	if err := placeAttachments(ws, []string{staged}); err != nil {
		t.Fatalf("placeAttachments: %v", err)
	}

	// The prompt promised /workspace/inbox/<name>, and /workspace is ws.
	got, err := os.ReadFile(filepath.Join(ws, "inbox", "ab12cd34-header.png"))
	if err != nil {
		t.Fatalf("the upload is not where the agent was told to look: %v", err)
	}
	if string(got) != "pixels" {
		t.Errorf("upload contents = %q, want %q", got, "pixels")
	}
}

// The default conversation mounts the base work directory, which is where
// uploads used to land directly. Staging must not have moved them out from under
// it.
func TestPlaceAttachmentsServesTheDefaultConversation(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "pm", Enabled: true}
	staged := stage(t, a, "pm", "ab12cd34-notes.txt", "text")

	ws, err := a.resolveWorkspace(context.Background(), agent, "")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if err := placeAttachments(ws, []string{staged}); err != nil {
		t.Fatalf("placeAttachments: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "inbox", "ab12cd34-notes.txt")); err != nil {
		t.Fatalf("the upload is missing from the default workspace: %v", err)
	}
}

// An activity is retried, and a retry that failed on "the file is already there"
// would turn a transient container error into a turn that can never run.
func TestPlaceAttachmentsIsIdempotent(t *testing.T) {
	a, _, _ := notifyHarness(t)

	staged := stage(t, a, "pm", "ab12cd34-report.pdf", "body")
	ws := filepath.Join(t.TempDir(), "workspace")

	for attempt := range 3 {
		if err := placeAttachments(ws, []string{staged}); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "inbox", "ab12cd34-report.pdf")); err != nil {
		t.Fatalf("upload missing after repeated placement: %v", err)
	}
}

// Placement links rather than copies, so a 25MB upload is never written twice.
// Sharing the inode is also what makes leaving the staged entry behind free, and
// leaving it is what makes a retry find its files.
func TestPlaceAttachmentsLinksRatherThanCopies(t *testing.T) {
	a, _, _ := notifyHarness(t)

	staged := stage(t, a, "pm", "ab12cd34-big.bin", "payload")
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := placeAttachments(ws, []string{staged}); err != nil {
		t.Fatalf("placeAttachments: %v", err)
	}

	src, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("the staged file was consumed: %v", err)
	}
	dst, err := os.Stat(filepath.Join(ws, "inbox", "ab12cd34-big.bin"))
	if err != nil {
		t.Fatalf("stat placed file: %v", err)
	}
	if !os.SameFile(src, dst) {
		t.Error("the upload was copied, not linked — a large file now costs twice the disk")
	}
}

// A staged file that is gone will not come back, and no number of retries will
// change that. Failing fast says so; retrying would burn the policy and then
// fail anyway.
func TestPlaceAttachmentsRefusesToLoseAnUploadSilently(t *testing.T) {
	a, _, _ := notifyHarness(t)

	missing := filepath.Join(a.cfg.InboxStagingDir("pm"), "ab12cd34-vanished.pdf")
	ws := filepath.Join(t.TempDir(), "workspace")

	err := placeAttachments(ws, []string{missing})
	if err == nil {
		t.Fatal("a missing upload was accepted; the agent would answer about a file it never got")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.NonRetryable() {
		t.Errorf("a missing upload is retryable: %v (%T)", err, err)
	}
}

func TestPlaceAttachmentsWithNothingToPlace(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := placeAttachments(ws, nil); err != nil {
		t.Fatalf("placing no attachments failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "inbox")); !os.IsNotExist(err) {
		t.Error("an inbox was created for a turn with no uploads")
	}
}
