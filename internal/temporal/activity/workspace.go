package activity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
)

// Per-conversation workspaces.
//
// Conversations run in parallel, so they cannot share a working tree: two
// threads editing the same files at once produces a mess neither of them
// asked for. Each gets its own.
//
// How depends on what the agent's workspace is:
//
//   - the managed directory — a conversation gets its own directory under it.
//     It starts empty anyway, so there is nothing to branch from.
//   - a git repository — a conversation gets a `git worktree`, which is the
//     cheap way to have a second checkout of the same history.
//   - a directory that is not a repository — there is no cheap isolation to
//     give, so the agent must say what it wants. Sharing silently is the one
//     option not on offer, because the conflicts would surface as an agent
//     mysteriously undoing another's work.
//
// The default conversation always uses the agent's workspace directly. It is
// the one that /ask, schedules and webhooks share, and it is what existed
// before conversations did.

// conversationDir is where a conversation's workspace lives.
func conversationDir(cfg *config.Config, agentID, conversationID string) string {
	return filepath.Join(cfg.AgentDir(agentID), "conversations", conversationID)
}

// resolveWorkspace returns the host directory to mount at /workspace for one
// conversation, creating it if needed.
func (a *Activities) resolveWorkspace(ctx context.Context, agent registry.Agent, conversationID string) (string, error) {
	base := workDirFor(a.cfg, agent)
	if conversationID == "" {
		return base, nil
	}

	dir := conversationDir(a.cfg, agent.ID, conversationID)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil // already prepared by an earlier turn
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", fmt.Errorf("create conversation dir: %w", err)
	}

	// A managed workspace starts empty, so a plain directory is the whole job —
	// except for CLAUDE.md, which carries the agent's persona and instructions.
	// It is discovered from the cwd, and the cwd is the mount root, so a thread
	// that started empty would lose it. Seed it from the base workspace.
	if agent.WorkDir == "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("create conversation workspace: %w", err)
		}
		seedClaudeMD(base, dir)
		return dir, nil
	}

	if isGitRepo(ctx, base) {
		if err := addWorktree(ctx, base, dir); err != nil {
			return "", err
		}
		return dir, nil
	}

	if agent.ShareWorkspace {
		// Opted into explicitly: the operator has accepted that parallel
		// conversations may collide in this directory.
		return base, nil
	}
	return "", newNonRetryable(fmt.Errorf(
		"agent %s works in %s, which is not a git repository, so a thread cannot be "+
			"given an isolated workspace; set share_workspace on the agent to accept "+
			"that threads share it", agent.ID, base))
}

// seedClaudeMD copies the base workspace's CLAUDE.md into a fresh managed
// conversation directory, so a thread carries the same persona and instructions
// as the default workspace. Best-effort: an agent without a CLAUDE.md simply has
// none, and a copy failure must not fail the turn — the agent still runs, just
// without its instructions.
func seedClaudeMD(base, dir string) {
	data, err := os.ReadFile(filepath.Join(base, "CLAUDE.md"))
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), data, 0o640)
}

func isGitRepo(ctx context.Context, dir string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

// addWorktree creates a detached worktree of base at dir.
//
// Detached on purpose: a worktree checked out on a branch locks that branch, so
// two conversations — or a person working in the original checkout — would
// fight over it. Detached, each conversation starts from the current commit and
// can branch on its own if it wants to.
func addWorktree(ctx context.Context, base, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", base, "worktree", "add", "--detach", dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create worktree for %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveConversationInput names a conversation whose workspace can go.
type RemoveConversationInput struct {
	AgentID        string `json:"agent_id"`
	ConversationID string `json:"conversation_id"`
}

// RemoveConversationWorkspace tears down a conversation's workspace.
//
// A worktree is removed through git rather than deleted, so the repository does
// not keep a stale administrative entry pointing at a directory that no longer
// exists — `git worktree list` would go on reporting it forever.
//
// Uncommitted changes in the worktree are the reason this is not automatic: it
// runs when a conversation is explicitly finished, not when a thread goes quiet.
func (a *Activities) RemoveConversationWorkspace(ctx context.Context, in RemoveConversationInput) error {
	if in.ConversationID == "" {
		return newNonRetryable(fmt.Errorf("refusing to remove the default conversation's workspace"))
	}

	agent, err := a.reg.Get(ctx, in.AgentID)
	if err != nil {
		return newNonRetryable(err)
	}
	dir := conversationDir(a.cfg, in.AgentID, in.ConversationID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	base := workDirFor(a.cfg, agent)
	if agent.WorkDir != "" && isGitRepo(ctx, base) {
		rmCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		// --force because an agent will usually have left edits behind, and the
		// caller has already said it is done with them.
		cmd := exec.CommandContext(rmCtx, "git", "-C", base, "worktree", "remove", "--force", dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("remove worktree %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return os.RemoveAll(dir)
}
