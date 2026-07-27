package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/roundtable/roundclaw/internal/config"
)

func stagingConfig(t *testing.T) *config.Config {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\ncontainer:\n  image: test\nagents:\n  - id: pm\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// stagedAt writes a staged upload and backdates it.
func stagedAt(t *testing.T, cfg *config.Config, agentID, name string, age time.Duration) string {
	t.Helper()

	dir := cfg.InboxStagingDir(agentID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("body"), 0o640); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("backdate %s: %v", name, err)
	}
	return path
}

// The case this exists for: a conversation's workspace is torn down, and the
// staged entry becomes the last link holding bytes nothing will ever read.
func TestPruneStagingRemovesOnlyWhatIsOldEnough(t *testing.T) {
	cfg := stagingConfig(t)

	old := stagedAt(t, cfg, "pm", "aa11-old.pdf", 30*24*time.Hour)
	fresh := stagedAt(t, cfg, "pm", "bb22-fresh.pdf", time.Hour)

	n, err := PruneStaging(cfg, "pm", time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("PruneStaging: %v", err)
	}
	if n != 1 {
		t.Errorf("removed %d uploads, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the old upload survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a recent upload was swept, so a retry would not find its files: %v", err)
	}
}

// A staged file is kept precisely so a retried activity can find it. Sweeping
// one whose turn may still run would turn a transient failure into a turn that
// can never succeed.
func TestPruneStagingKeepsEverythingWhenNothingIsOldEnough(t *testing.T) {
	cfg := stagingConfig(t)
	stagedAt(t, cfg, "pm", "aa11-recent.pdf", time.Minute)

	n, err := PruneStaging(cfg, "pm", time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("PruneStaging: %v", err)
	}
	if n != 0 {
		t.Errorf("removed %d uploads from a directory with nothing old in it", n)
	}
}

// Most agents are never sent a file, so there is no directory at all. That is
// not an error and must not make the sweep log a warning every interval.
func TestPruneStagingWithNoStagingDirectory(t *testing.T) {
	cfg := stagingConfig(t)

	n, err := PruneStaging(cfg, "pm", time.Now())
	if err != nil || n != 0 {
		t.Errorf("PruneStaging on a fresh agent = %d, %v; want 0, nil", n, err)
	}
}

// The sweep must not reach into the workspace. inbox/ holds documents someone
// sent the agent and outbox/ holds work it produced; both live in its working
// directory and are not roundclaw's to delete on a timer.
func TestPruneStagingLeavesTheWorkspaceAlone(t *testing.T) {
	cfg := stagingConfig(t)
	stagedAt(t, cfg, "pm", "aa11-old.pdf", 30*24*time.Hour)

	work := cfg.WorkDir("pm")
	for _, sub := range []string{"inbox", "outbox"} {
		if err := os.MkdirAll(filepath.Join(work, sub), 0o750); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
		path := filepath.Join(work, sub, "old.pdf")
		if err := os.WriteFile(path, []byte("body"), 0o640); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		when := time.Now().Add(-365 * 24 * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	if _, err := PruneStaging(cfg, "pm", time.Now()); err != nil {
		t.Fatalf("PruneStaging: %v", err)
	}

	for _, sub := range []string{"inbox", "outbox"} {
		if _, err := os.Stat(filepath.Join(work, sub, "old.pdf")); err != nil {
			t.Errorf("the sweep deleted the agent's own %s/ file: %v", sub, err)
		}
	}
}
