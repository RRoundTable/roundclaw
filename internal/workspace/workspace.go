// Package workspace answers where a conversation's files live.
//
// Two processes need that answer for different reasons, and only one of them may
// act on it. The worker resolves a workspace in order to create and mount it.
// The gateway only needs to read what is already there — an agent's outbox — and
// must never create anything, because a conversation directory that exists is
// how the worker knows an earlier turn already prepared it, worktree and all.
//
// So the naming lives here, as a pure function, and the creating stays in the
// worker. Keeping both in one function is what let the two drift far enough
// apart that uploads were being written to a directory no container mounted.
package workspace

import (
	"path/filepath"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
)

// Dir is the host directory mounted at /workspace for one conversation.
//
// Pure: it names a directory that may not exist yet. A caller that needs it to
// exist must be the worker, which creates it as part of running the turn.
func Dir(cfg *config.Config, agent registry.Agent, conversationID string) string {
	base := Base(cfg, agent)
	// The default conversation is the agent's own workspace — the one /ask,
	// schedules and webhooks share, and the only one that existed before
	// conversations did.
	if conversationID == "" {
		return base
	}
	// share_workspace says every conversation works in the one directory. It is
	// answered before anything else, including any per-thread directory an
	// earlier turn left behind.
	if agent.ShareWorkspace {
		return base
	}
	return ConversationDir(cfg, agent.ID, conversationID)
}

// Base is the agent's own workspace: its configured work_dir, or the managed
// directory roundclaw keeps for it.
func Base(cfg *config.Config, agent registry.Agent) string {
	if agent.WorkDir != "" {
		return agent.WorkDir
	}
	return cfg.WorkDir(agent.ID)
}

// ConversationDir is where a non-default conversation's workspace lives.
func ConversationDir(cfg *config.Config, agentID, conversationID string) string {
	return filepath.Join(cfg.AgentDir(agentID), "conversations", conversationID)
}
