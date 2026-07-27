package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/roundtable/roundclaw/internal/config"
)

// Reclaiming staged uploads.
//
// An upload is written to the staging directory at admission and hard-linked
// into the workspace when the turn runs, so the two names share one inode and
// the staged entry costs nothing while the workspace still has its link. It is
// left behind on purpose: a retried activity has to find its files.
//
// Once no retry can plausibly arrive, the entry is only useful for one thing,
// and that one thing is why this exists — when a conversation's workspace is
// torn down, the staging entry becomes the last link, and the bytes stay on disk
// under a name nothing will ever read again.
//
// What this deliberately does not touch is the workspace itself. inbox/ holds
// documents someone sent the agent and outbox/ holds work the agent produced;
// both are the agent's own files, in its own working directory, and deleting
// them on a timer is not roundclaw's call to make.

// PruneStaging removes staged uploads last modified before the cutoff, and
// reports how many went.
//
// Age is taken from the file's own mtime rather than from any turn, because the
// turn that owned it may have been pruned long before.
func PruneStaging(cfg *config.Config, agentID string, before time.Time) (int, error) {
	dir := cfg.InboxStagingDir(agentID)

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil // an agent that has never been sent a file
	}
	if err != nil {
		return 0, fmt.Errorf("read staging for %s: %w", agentID, err)
	}

	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished under us; nothing to reclaim
		}
		if !info.ModTime().Before(before) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove staged upload %s: %w", e.Name(), err)
		}
		removed++
	}
	return removed, nil
}
