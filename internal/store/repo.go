package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// ErrNotFound is returned when a turn or runtime row does not exist.
var ErrNotFound = errors.New("not found")

// Runtime is the display-facing agent state that /status reads without going
// near Temporal.
type Runtime struct {
	AgentID     string
	Status      core.AgentStatus
	CurrentTurn int64
	SessionID   string
	UpdatedAt   time.Time
}

// Turn is one agent turn.
type Turn struct {
	ID      int64
	Request string
	Result  string
	Status  core.TurnStatus
	CostUSD float64
	Origin  core.Origin
	Error   string
	// Conversation is the thread this turn belongs to, or empty for the agent's
	// default conversation.
	Conversation string
	QueuedAt     time.Time
	FinishedAt   time.Time
}

// LogEntry is one live_logs row.
//
// The JSON tags are part of the public API: this type is serialised directly
// into GET /v1/agents/{id} responses, so without them the wire format would
// leak Go field names.
type LogEntry struct {
	ID        int64        `json:"id"`
	TurnID    int64        `json:"turn_id"`
	Kind      core.LogKind `json:"kind"`
	Content   string       `json:"content"`
	CreatedAt time.Time    `json:"created_at"`
}

// SetRuntime upserts the agent's coarse state.
func (s *Store) SetRuntime(ctx context.Context, status core.AgentStatus, currentTurn int64, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_runtime (agent_id, status, current_turn, session_id, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			status       = excluded.status,
			current_turn = excluded.current_turn,
			session_id   = excluded.session_id,
			updated_at   = excluded.updated_at`,
		s.agentID, string(status), currentTurn, sessionID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("set runtime for %s: %w", s.agentID, err)
	}
	return nil
}

// GetRuntime reads the agent's state. A missing row means the agent has never
// run, which is reported as idle rather than as an error.
func (s *Store) GetRuntime(ctx context.Context) (Runtime, error) {
	var (
		rt        Runtime
		sessionID sql.NullString
		updatedAt int64
		status    string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT agent_id, status, current_turn, session_id, updated_at
		FROM agent_runtime WHERE agent_id = ?`, s.agentID).
		Scan(&rt.AgentID, &status, &rt.CurrentTurn, &sessionID, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Runtime{AgentID: s.agentID, Status: core.AgentIdle}, nil
	}
	if err != nil {
		return rt, fmt.Errorf("get runtime for %s: %w", s.agentID, err)
	}
	rt.Status = core.AgentStatus(status)
	rt.SessionID = sessionID.String
	rt.UpdatedAt = time.UnixMilli(updatedAt)
	return rt, nil
}

// NewTurn describes a turn about to be queued.
type NewTurn struct {
	Request string
	Origin  core.Origin
	// Conversation is the thread this belongs to; empty is the agent's default
	// conversation.
	Conversation string
	// IdempotencyKey may be empty for sources that dedupe by other means.
	IdempotencyKey string
}

// CreateTurn records a queued turn and returns its ID.
//
// When an idempotency key is set this is atomic: a repeated key returns the
// original turn ID and existed=true, and no second turn is created. Without
// that atomicity a client retry racing the original would start two agent runs.
func (s *Store) CreateTurn(ctx context.Context, t NewTurn) (turnID int64, existed bool, err error) {
	request, origin, idempotencyKey := t.Request, t.Origin, t.IdempotencyKey
	encodedOrigin, err := core.EncodeOrigin(origin)
	if err != nil {
		return 0, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin create turn: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if idempotencyKey != "" {
		var existingID int64
		err := tx.QueryRowContext(ctx, `SELECT turn_id FROM idempotency WHERE key = ?`, idempotencyKey).Scan(&existingID)
		if err == nil {
			return existingID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("lookup idempotency key: %w", err)
		}
	}

	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO turns (request, status, origin, conversation, queued_at)
		VALUES (?, ?, ?, ?, ?)`,
		request, string(core.TurnRunning), encodedOrigin, t.Conversation, now)
	if err != nil {
		return 0, false, fmt.Errorf("insert turn: %w", err)
	}
	turnID, err = res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("turn id: %w", err)
	}

	if idempotencyKey != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency (key, turn_id, created_at) VALUES (?, ?, ?)`,
			idempotencyKey, turnID, now); err != nil {
			return 0, false, fmt.Errorf("record idempotency key: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit create turn: %w", err)
	}
	return turnID, false, nil
}

// FinishTurn closes out a turn. It is safe to call more than once; a turn that
// already reached a terminal state keeps its first outcome, so a delivery retry
// cannot rewrite a stopped turn into a done one.
func (s *Store) FinishTurn(ctx context.Context, turnID int64, result core.TurnResult) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE turns
		SET result = ?, status = ?, cost_usd = ?, error = ?, finished_at = ?
		WHERE id = ? AND status = ?`,
		result.Text, string(result.Status), result.CostUSD, result.ErrorMessage,
		time.Now().UnixMilli(), turnID, string(core.TurnRunning))
	if err != nil {
		return fmt.Errorf("finish turn %d: %w", turnID, err)
	}
	return nil
}

// GetTurn reads one turn.
func (s *Store) GetTurn(ctx context.Context, turnID int64) (Turn, error) {
	var (
		t          Turn
		result     sql.NullString
		cost       sql.NullFloat64
		errMsg     sql.NullString
		originJSON string
		status     string
		queuedAt   int64
		finishedAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, request, result, status, cost_usd, origin, error, conversation, queued_at, finished_at
		FROM turns WHERE id = ?`, turnID).
		Scan(&t.ID, &t.Request, &result, &status, &cost, &originJSON, &errMsg,
			&t.Conversation, &queuedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("get turn %d: %w", turnID, err)
	}

	t.Result = result.String
	t.Status = core.TurnStatus(status)
	t.CostUSD = cost.Float64
	t.Error = errMsg.String
	t.QueuedAt = time.UnixMilli(queuedAt)
	if finishedAt.Valid {
		t.FinishedAt = time.UnixMilli(finishedAt.Int64)
	}
	if t.Origin, err = core.DecodeOrigin(originJSON); err != nil {
		return t, fmt.Errorf("turn %d: %w", turnID, err)
	}
	return t, nil
}

// RecentTurns returns the most recent turns, newest first, across every
// conversation. Used for display and for spend accounting.
func (s *Store) RecentTurns(ctx context.Context, limit int) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request, result, status, cost_usd, origin, error, conversation, queued_at, finished_at
		FROM turns ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent turns: %w", err)
	}
	defer rows.Close()
	return scanTurns(rows)
}

// scanTurns reads a turn result set. A row whose origin will not decode is
// still returned with an empty origin rather than failing the query: it is
// worth showing, and the origin only matters for delivery, which is long past.
func scanTurns(rows *sql.Rows) ([]Turn, error) {
	var out []Turn
	for rows.Next() {
		var (
			t          Turn
			result     sql.NullString
			cost       sql.NullFloat64
			errMsg     sql.NullString
			originJSON string
			status     string
			queuedAt   int64
			finishedAt sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.Request, &result, &status, &cost, &originJSON, &errMsg,
			&t.Conversation, &queuedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		t.Result = result.String
		t.Status = core.TurnStatus(status)
		t.CostUSD = cost.Float64
		t.Error = errMsg.String
		t.QueuedAt = time.UnixMilli(queuedAt)
		if finishedAt.Valid {
			t.FinishedAt = time.UnixMilli(finishedAt.Int64)
		}
		t.Origin, _ = core.DecodeOrigin(originJSON)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RecentTurnsIn returns the most recent turns of one conversation, newest
// first.
//
// This is the window used to rebuild context after a lost session, and it has
// to be conversation-scoped: recapping a thread with another thread's history
// would hand the agent a conversation it never had.
func (s *Store) RecentTurnsIn(ctx context.Context, conversation string, limit int) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request, result, status, cost_usd, origin, error, conversation, queued_at, finished_at
		FROM turns WHERE conversation = ? ORDER BY id DESC LIMIT ?`, conversation, limit)
	if err != nil {
		return nil, fmt.Errorf("recent turns in %q: %w", conversation, err)
	}
	defer rows.Close()
	return scanTurns(rows)
}

// Conversations returns every conversation this agent has ever run a turn for,
// including the default one (reported as the empty string). It is the source of
// truth for "which conversations does this agent have", read straight from the
// agent's own state.db just like /status — no Temporal round-trip and no
// parsing of workflow IDs, both of which would be fragile.
//
// A brand-new agent that has never taken a turn returns nothing, which is
// correct: there is nothing to stop.
func (s *Store) Conversations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT conversation FROM turns ORDER BY conversation`)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppendLog writes one live-log line. This is on the hot path — the activity
// calls it for every stream-json event — so it stays a single INSERT.
func (s *Store) AppendLog(ctx context.Context, turnID int64, kind core.LogKind, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO live_logs (turn_id, kind, content, created_at) VALUES (?, ?, ?, ?)`,
		turnID, string(kind), content, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("append log to turn %d: %w", turnID, err)
	}
	return nil
}

// LogsAfter returns log entries for a turn with ID greater than afterID, oldest
// first. Passing 0 starts from the beginning. This is the SSE cursor as well as
// the /status tail, so the caller controls how much it pulls.
func (s *Store) LogsAfter(ctx context.Context, turnID, afterID int64, limit int) ([]LogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, turn_id, kind, content, created_at
		FROM live_logs WHERE turn_id = ? AND id > ?
		ORDER BY id ASC LIMIT ?`, turnID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("logs after %d for turn %d: %w", afterID, turnID, err)
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var (
			e         LogEntry
			kind      string
			createdAt int64
		)
		if err := rows.Scan(&e.ID, &e.TurnID, &kind, &e.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan log: %w", err)
		}
		e.Kind = core.LogKind(kind)
		e.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TailLogs returns the last n log entries for a turn, oldest first. /status
// uses this to show what the agent is doing right now.
func (s *Store) TailLogs(ctx context.Context, turnID int64, n int) ([]LogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, turn_id, kind, content, created_at FROM (
			SELECT id, turn_id, kind, content, created_at
			FROM live_logs WHERE turn_id = ?
			ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`, turnID, n)
	if err != nil {
		return nil, fmt.Errorf("tail logs for turn %d: %w", turnID, err)
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var (
			e         LogEntry
			kind      string
			createdAt int64
		)
		if err := rows.Scan(&e.ID, &e.TurnID, &kind, &e.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan log: %w", err)
		}
		e.Kind = core.LogKind(kind)
		e.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Usage is what an agent has spent and run inside a window.
type Usage struct {
	Turns   int
	CostUSD float64
}

// UsageSince totals an agent's activity since a point in time.
//
// Turns are counted from when they were queued, not when they finished, so a
// burst cannot slip past a rate limit by having its turns still running. Cost
// only accrues on completion, because that is when the CLI reports it.
func (s *Store) UsageSince(ctx context.Context, since time.Time) (Usage, error) {
	var (
		u    Usage
		cost sql.NullFloat64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(cost_usd) FROM turns WHERE queued_at >= ?`,
		since.UnixMilli()).Scan(&u.Turns, &cost)
	if err != nil {
		return u, fmt.Errorf("usage for %s: %w", s.agentID, err)
	}
	u.CostUSD = cost.Float64
	return u, nil
}

// Pruned reports what a retention pass removed.
type Pruned struct {
	Logs  int64
	Turns int64
	Keys  int64
}

// Prune removes history older than the cutoffs.
//
// live_logs is the reason this exists: every stream-json event of every turn
// appends a row, and nothing ever deleted them. A busy agent grows its database
// without bound, and the transcript of a turn from six weeks ago is not what
// anyone is reading in /status.
//
// Logs are pruned harder than turns. A turn row is small and is the audit
// trail — what was asked, what came back, what it cost — while its transcript
// is bulky and only interesting while the work is recent.
//
// Rows belonging to a turn that is still running are never touched, however old
// the cutoff: a long turn is exactly the one someone is watching.
func (s *Store) Prune(ctx context.Context, logsBefore, turnsBefore time.Time) (Pruned, error) {
	var p Pruned

	res, err := s.db.ExecContext(ctx, `
		DELETE FROM live_logs WHERE turn_id IN (
			SELECT id FROM turns WHERE finished_at IS NOT NULL AND finished_at < ?
		)`, logsBefore.UnixMilli())
	if err != nil {
		return p, fmt.Errorf("prune logs for %s: %w", s.agentID, err)
	}
	p.Logs, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx, `
		DELETE FROM turns WHERE finished_at IS NOT NULL AND finished_at < ?`,
		turnsBefore.UnixMilli())
	if err != nil {
		return p, fmt.Errorf("prune turns for %s: %w", s.agentID, err)
	}
	p.Turns, _ = res.RowsAffected()

	// Idempotency keys outlive their usefulness the moment a client would have
	// given up retrying, and they are pure overhead after that.
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM idempotency WHERE created_at < ?`, turnsBefore.UnixMilli())
	if err != nil {
		return p, fmt.Errorf("prune idempotency keys for %s: %w", s.agentID, err)
	}
	p.Keys, _ = res.RowsAffected()

	return p, nil
}
