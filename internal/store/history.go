package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// Turn history as a queryable window, rather than the fixed "last N" that
// RecentTurns gives display and spend accounting.
//
// This exists for the question "what has actually been asked of this agent, and
// what went wrong" — the input to reviewing an agent rather than to watching
// one. It reads the agent's own state.db directly, like /status does, so it
// keeps working while the agent is wedged, which is when the question gets
// asked.

// TurnFilter narrows a history query. The zero value means "the most recent
// turns, any status, every conversation".
type TurnFilter struct {
	// Limit caps the rows returned. Zero or negative applies DefaultHistoryLimit;
	// anything above MaxHistoryLimit is clamped to it, because the caller is
	// usually a model with a context window rather than a paging UI.
	Limit int
	// Since bounds the window by queue time. Zero means no lower bound.
	Since time.Time
	// Status keeps only turns in that state — `error` is the one worth asking
	// for. Empty keeps every state.
	Status core.TurnStatus
	// Conversation selects one conversation; nil spans all of them. It is a
	// pointer because the empty string is itself a conversation — the agent's
	// default one — so there is no in-band value left to mean "any".
	Conversation *string
}

// History limits. A model reading this pays for every row, so the default is
// small and the ceiling is well short of what a busy agent has accumulated.
const (
	DefaultHistoryLimit = 20
	MaxHistoryLimit     = 200
)

// Turns returns matching turns, newest first.
func (s *Store) Turns(ctx context.Context, f TurnFilter) ([]Turn, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	if limit > MaxHistoryLimit {
		limit = MaxHistoryLimit
	}

	var (
		where []string
		args  []any
	)
	if !f.Since.IsZero() {
		where = append(where, "queued_at >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.Conversation != nil {
		where = append(where, "conversation = ?")
		args = append(args, *f.Conversation)
	}

	q := `SELECT id, request, result, status, cost_usd, origin, error, conversation, queued_at, finished_at
	      FROM turns`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query turn history of %s: %w", s.agentID, err)
	}
	defer rows.Close()
	return scanTurns(rows)
}
