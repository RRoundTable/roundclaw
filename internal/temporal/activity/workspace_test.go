package activity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/roundtable/roundclaw/internal/registry"
)

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	cmd := exec.Command("git", "init", "--quiet", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
}

// The bug this pins: asking git whether a directory is in a repository walks up
// through the parents, and every agent workspace sits under the roundclaw
// checkout. A plain directory answered yes, and the caller responded by grafting
// a worktree of the *enclosing* repository onto the agent — an agent asked to
// work on its own project got roundclaw's source instead.
func TestIsGitRepoRejectsPlainDirInsideRepo(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)

	nested := filepath.Join(root, "workspace", "dev", "work")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}

	if isGitRepo(context.Background(), nested) {
		t.Errorf("%s is not a repository, but was reported as one — a conversation "+
			"would be given a worktree of the enclosing checkout", nested)
	}
}

func TestIsGitRepoAcceptsRepoRoot(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)

	if !isGitRepo(context.Background(), root) {
		t.Errorf("%s is a repository root but was rejected — its conversations "+
			"would lose worktree isolation", root)
	}
}

func TestIsGitRepoRejectsDirOutsideAnyRepo(t *testing.T) {
	if isGitRepo(context.Background(), t.TempDir()) {
		t.Error("a directory outside any repository was reported as one")
	}
}

// A managed workspace is meant to start empty, so each conversation gets its own
// directory. The agent's persona has to come along or the thread runs without
// its instructions.
func TestResolveWorkspaceManagedIsolatesAndSeedsClaudeMD(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "dev", Enabled: true}
	base := workDirFor(a.cfg, agent)
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("create base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "CLAUDE.md"), []byte("persona"), 0o600); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	dir, err := a.resolveWorkspace(context.Background(), agent, "thread-1")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if dir == base {
		t.Fatalf("conversation shares the base workspace %s without share_workspace", base)
	}
	got, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md was not seeded into %s: %v", dir, err)
	}
	if string(got) != "persona" {
		t.Errorf("seeded CLAUDE.md = %q, want %q", got, "persona")
	}
}

// share_workspace used to be dead on this path: the managed branch returned
// before anything read the flag, so an agent whose managed directory held real
// checkouts got a fresh empty one per thread and no error saying why.
func TestResolveWorkspaceManagedHonoursShareWorkspace(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "dev", Enabled: true, ShareWorkspace: true}
	base := workDirFor(a.cfg, agent)
	if err := os.MkdirAll(filepath.Join(base, "egg-money"), 0o750); err != nil {
		t.Fatalf("create base: %v", err)
	}

	dir, err := a.resolveWorkspace(context.Background(), agent, "thread-1")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if dir != base {
		t.Errorf("conversation got %s, want the shared base %s — share_workspace was ignored", dir, base)
	}
}

// An agent that ran threads before share_workspace was set has an isolated
// directory per thread already sitting on disk. Those must not win: the flag is
// set precisely to stop those threads working in an empty directory, and the
// "already prepared by an earlier turn" shortcut used to hand one straight back.
func TestResolveWorkspaceShareWorkspaceBeatsStaleConversationDir(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "dev", Enabled: true, ShareWorkspace: true}
	base := workDirFor(a.cfg, agent)
	if err := os.MkdirAll(filepath.Join(base, "egg-money"), 0o750); err != nil {
		t.Fatalf("create base: %v", err)
	}
	stale := conversationDir(a.cfg, agent.ID, "thread-1")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatalf("create stale dir: %v", err)
	}

	dir, err := a.resolveWorkspace(context.Background(), agent, "thread-1")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if dir != base {
		t.Errorf("conversation got the stale empty %s, want the shared base %s", dir, base)
	}
}

// The default conversation predates conversations and always uses the agent's
// workspace directly, whatever the flags say.
func TestResolveWorkspaceDefaultConversationUsesBase(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "dev", Enabled: true}
	base := workDirFor(a.cfg, agent)

	dir, err := a.resolveWorkspace(context.Background(), agent, "")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if dir != base {
		t.Errorf("default conversation got %s, want %s", dir, base)
	}
}

// A work_dir that is not a repository cannot be isolated cheaply, and sharing it
// silently is the one thing not on offer.
func TestResolveWorkspaceNonRepoWorkDirRefusesWithoutShare(t *testing.T) {
	a, _, _ := notifyHarness(t)

	work := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatalf("create work dir: %v", err)
	}
	agent := registry.Agent{ID: "dev", Enabled: true, WorkDir: work}

	if _, err := a.resolveWorkspace(context.Background(), agent, "thread-1"); err == nil {
		t.Error("a non-repository work_dir was silently shared")
	}
}

// A work_dir repository is the case worktrees exist for: the conversation starts
// with the files already there.
func TestResolveWorkspaceRepoWorkDirGetsWorktree(t *testing.T) {
	a, _, _ := notifyHarness(t)

	work := filepath.Join(t.TempDir(), "project")
	initRepo(t, work)
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, args := range [][]string{
		{"-C", work, "-c", "user.email=t@t", "-c", "user.name=t", "add", "main.go"},
		{"-C", work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--quiet", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	agent := registry.Agent{ID: "dev", Enabled: true, WorkDir: work}

	dir, err := a.resolveWorkspace(context.Background(), agent, "thread-1")
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Errorf("worktree at %s does not have the repository's files: %v", dir, err)
	}
}
