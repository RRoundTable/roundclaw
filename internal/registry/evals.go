package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// Evaluating an agent.
//
// An eval set is a list of cases — a prompt and how to tell whether the answer
// was any good — attached to one agent. Running it produces a run: one row per
// case, plus an aggregate. Runs are kept so two of them can be compared, which
// is the only way to answer "is the new version better" with something other
// than an opinion.
//
// A run pins the agent *version* it evaluated rather than the agent, so a
// comparison is between two known configurations and not between "before" and
// "whatever it happens to be now".
//
// Nothing here runs anything. The definitions live in the registry and Temporal
// executes them (workflow.EvalRun), the same split schedules and workflows use.

const evalSchema = `
CREATE TABLE IF NOT EXISTS eval_sets (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    cases       TEXT NOT NULL DEFAULT '[]',   -- JSON array of EvalCase
    -- full_grants injects the agent's secrets into eval containers. Off by
    -- default: a scheduled eval otherwise runs an agent's real credentials
    -- against a scratch workspace, and "test" runs that can push, deploy or
    -- post are not tests.
    full_grants INTEGER NOT NULL DEFAULT 0,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_eval_sets_agent ON eval_sets(agent_id);

CREATE TABLE IF NOT EXISTS eval_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    eval_set_id  TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    -- The agent version under test. 0 means the definition that was live when
    -- the run started, which is only useful for a one-off: a comparison needs
    -- both sides pinned.
    version      INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'running',
    score        REAL NOT NULL DEFAULT 0,
    passed       INTEGER NOT NULL DEFAULT 0,
    total        INTEGER NOT NULL DEFAULT 0,
    cost_usd     REAL NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    -- Where to report when the run ends. A run takes minutes, far longer than
    -- the turn that asked for it, so the requester is woken rather than made to
    -- wait — the same return address a delegated turn carries.
    notify_agent        TEXT NOT NULL DEFAULT '',
    notify_conversation TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_eval_runs_set ON eval_runs(eval_set_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_eval_runs_agent ON eval_runs(agent_id, id DESC);

CREATE TABLE IF NOT EXISTS eval_results (
    run_id      INTEGER NOT NULL,
    case_name   TEXT NOT NULL,
    output      TEXT NOT NULL DEFAULT '',
    score       REAL NOT NULL DEFAULT 0,
    passed      INTEGER NOT NULL DEFAULT 0,
    reason      TEXT NOT NULL DEFAULT '',
    cost_usd    REAL NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, case_name)
);
`

// Run states.
const (
	EvalRunning = "running"
	EvalDone    = "done"
	EvalFailed  = "error"
)

// EvalCase is one question put to an agent, with how to mark the answer.
//
// The two marking styles are deliberately both available. Assertions are exact
// and free; a rubric judges prose, which is what most agents produce. A case may
// use either, both, or neither — a case with neither is a smoke test that only
// asks whether the agent answered at all.
type EvalCase struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Rubric is handed to a judge model along with the answer. Write it as the
	// question a reviewer would ask: "does it name the failing test and say why".
	Rubric string `json:"rubric,omitempty"`
	// MustContain and MustNotContain are checked in Go before any judge runs, and
	// failing one fails the case outright. They cost nothing and cannot be talked
	// out of their verdict, so put the non-negotiables here.
	MustContain    []string `json:"must_contain,omitempty"`
	MustNotContain []string `json:"must_not_contain,omitempty"`
	// Weight scales this case in the run's aggregate score. Zero means 1.
	Weight float64 `json:"weight,omitempty"`
}

// EvalSet is a named list of cases for one agent.
type EvalSet struct {
	ID          string     `json:"id"`
	AgentID     string     `json:"agent_id"`
	Description string     `json:"description,omitempty"`
	Cases       []EvalCase `json:"cases"`
	FullGrants  bool       `json:"full_grants,omitempty"`
	Enabled     bool       `json:"enabled"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// MaxEvalCases bounds a set. Every case is a container start and at least one
// model call, so a set is a bill; a hundred of them scheduled weekly is a bill
// nobody signed off on.
const MaxEvalCases = 50

// Validate checks an eval set before it is written.
func (e EvalSet) Validate() error {
	if err := core.ValidateAgentID(e.ID); err != nil {
		return fmt.Errorf("eval set id: %w", err)
	}
	if err := core.ValidateAgentID(e.AgentID); err != nil {
		return fmt.Errorf("agent id: %w", err)
	}
	if len(e.Cases) == 0 {
		return fmt.Errorf("an eval set needs at least one case")
	}
	if len(e.Cases) > MaxEvalCases {
		return fmt.Errorf("an eval set may hold at most %d cases, got %d", MaxEvalCases, len(e.Cases))
	}
	seen := make(map[string]bool, len(e.Cases))
	for i, c := range e.Cases {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("case %d has no name", i+1)
		}
		// Results are keyed by (run, case name), and a comparison lines two runs
		// up by that name — so duplicates would silently overwrite each other and
		// then look like a case that vanished.
		if seen[c.Name] {
			return fmt.Errorf("case name %q is used twice; names identify a case across runs", c.Name)
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.Prompt) == "" {
			return fmt.Errorf("case %q has no prompt", c.Name)
		}
		if c.Weight < 0 {
			return fmt.Errorf("case %q has a negative weight", c.Name)
		}
	}
	return nil
}

// EvalRun is one execution of a set against one agent version.
type EvalRun struct {
	ID                 int64     `json:"id"`
	EvalSetID          string    `json:"eval_set_id"`
	AgentID            string    `json:"agent_id"`
	Version            int       `json:"version"`
	Status             string    `json:"status"`
	Score              float64   `json:"score"`
	Passed             int       `json:"passed"`
	Total              int       `json:"total"`
	CostUSD            float64   `json:"cost_usd"`
	Error              string    `json:"error,omitempty"`
	NotifyAgent        string    `json:"notify_agent,omitempty"`
	NotifyConversation string    `json:"notify_conversation,omitempty"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at,omitempty"`
}

// EvalResult is one case's outcome within a run.
type EvalResult struct {
	RunID      int64   `json:"run_id"`
	CaseName   string  `json:"case_name"`
	Output     string  `json:"output,omitempty"`
	Score      float64 `json:"score"`
	Passed     bool    `json:"passed"`
	Reason     string  `json:"reason,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMS int64   `json:"duration_ms"`
}

// ---- eval sets -------------------------------------------------------------

// PutEvalSet creates or replaces an eval set.
func (s *Store) PutEvalSet(ctx context.Context, e EvalSet) (EvalSet, error) {
	if err := e.Validate(); err != nil {
		return EvalSet{}, err
	}
	// Named rather than left to a foreign key: "FOREIGN KEY constraint failed"
	// tells whoever typed it nothing about which agent does not exist.
	if _, err := s.Get(ctx, e.AgentID); err != nil {
		return EvalSet{}, err
	}
	cases, err := json.Marshal(e.Cases)
	if err != nil {
		return EvalSet{}, fmt.Errorf("encode cases: %w", err)
	}
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO eval_sets (id, agent_id, description, cases, full_grants, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			agent_id = excluded.agent_id, description = excluded.description,
			cases = excluded.cases, full_grants = excluded.full_grants,
			enabled = excluded.enabled, updated_at = excluded.updated_at`,
		e.ID, e.AgentID, e.Description, string(cases), boolToInt(e.FullGrants), boolToInt(e.Enabled), now, now)
	if err != nil {
		return EvalSet{}, fmt.Errorf("store eval set %s: %w", e.ID, err)
	}
	return s.GetEvalSet(ctx, e.ID)
}

// GetEvalSet returns one eval set.
func (s *Store) GetEvalSet(ctx context.Context, id string) (EvalSet, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, description, cases, full_grants, enabled, created_at, updated_at
		FROM eval_sets WHERE id = ?`, id)
	e, err := scanEvalSet(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalSet{}, fmt.Errorf("%w: eval set %s", ErrNotFound, id)
	}
	return e, err
}

// ListEvalSets returns every eval set, or only those of one agent.
func (s *Store) ListEvalSets(ctx context.Context, agentID string) ([]EvalSet, error) {
	q := `SELECT id, agent_id, description, cases, full_grants, enabled, created_at, updated_at FROM eval_sets`
	var args []any
	if agentID != "" {
		q += " WHERE agent_id = ?"
		args = append(args, agentID)
	}
	q += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list eval sets: %w", err)
	}
	defer rows.Close()

	var out []EvalSet
	for rows.Next() {
		e, err := scanEvalSet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEvalSet removes a set. Its runs are kept: they are the record of how the
// agent scored, and deleting the questions does not unmake the answers.
func (s *Store) DeleteEvalSet(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM eval_sets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete eval set %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: eval set %s", ErrNotFound, id)
	}
	return nil
}

func scanEvalSet(row scanner) (EvalSet, error) {
	var (
		e                    EvalSet
		cases                string
		full, enabled        int
		createdAt, updatedAt int64
	)
	if err := row.Scan(&e.ID, &e.AgentID, &e.Description, &cases, &full, &enabled, &createdAt, &updatedAt); err != nil {
		return EvalSet{}, err
	}
	if err := json.Unmarshal([]byte(cases), &e.Cases); err != nil {
		return EvalSet{}, fmt.Errorf("decode cases of %s: %w", e.ID, err)
	}
	e.FullGrants = full != 0
	e.Enabled = enabled != 0
	e.CreatedAt = time.UnixMilli(createdAt)
	e.UpdatedAt = time.UnixMilli(updatedAt)
	return e, nil
}

// ---- runs ------------------------------------------------------------------

// StartEvalRun records a run in progress and returns it with its assigned ID.
//
// The row is written before the workflow starts, so the caller has something to
// poll and the workflow has somewhere to write — and a run that dies before its
// first case still leaves a trace saying it was attempted.
func (s *Store) StartEvalRun(ctx context.Context, r EvalRun) (EvalRun, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO eval_runs (eval_set_id, agent_id, version, status, total,
		                       notify_agent, notify_conversation, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.EvalSetID, r.AgentID, r.Version, EvalRunning, r.Total,
		r.NotifyAgent, r.NotifyConversation, now)
	if err != nil {
		return EvalRun{}, fmt.Errorf("start eval run of %s: %w", r.EvalSetID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return EvalRun{}, fmt.Errorf("eval run id: %w", err)
	}
	return s.GetEvalRun(ctx, id)
}

// FinishEvalRun closes a run out with its aggregate.
func (s *Store) FinishEvalRun(ctx context.Context, id int64, status string, score float64, passed int, cost float64, runErr string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE eval_runs SET status = ?, score = ?, passed = ?, cost_usd = ?, error = ?, finished_at = ?
		WHERE id = ?`,
		status, score, passed, cost, runErr, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("finish eval run %d: %w", id, err)
	}
	return nil
}

// GetEvalRun returns one run.
func (s *Store) GetEvalRun(ctx context.Context, id int64) (EvalRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, eval_set_id, agent_id, version, status, score, passed, total,
		       cost_usd, error, notify_agent, notify_conversation, started_at, finished_at
		FROM eval_runs WHERE id = ?`, id)
	r, err := scanEvalRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalRun{}, fmt.Errorf("%w: eval run %d", ErrNotFound, id)
	}
	return r, err
}

// ListEvalRuns returns runs newest first, optionally narrowed to one set or one
// agent. limit <= 0 applies a sane default rather than returning everything.
func (s *Store) ListEvalRuns(ctx context.Context, evalSetID, agentID string, limit int) ([]EvalRun, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, eval_set_id, agent_id, version, status, score, passed, total,
	             cost_usd, error, notify_agent, notify_conversation, started_at, finished_at
	      FROM eval_runs`
	var (
		where []string
		args  []any
	)
	if evalSetID != "" {
		where = append(where, "eval_set_id = ?")
		args = append(args, evalSetID)
	}
	if agentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, agentID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list eval runs: %w", err)
	}
	defer rows.Close()

	var out []EvalRun
	for rows.Next() {
		r, err := scanEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanEvalRun(row scanner) (EvalRun, error) {
	var (
		r                     EvalRun
		startedAt, finishedAt int64
	)
	err := row.Scan(&r.ID, &r.EvalSetID, &r.AgentID, &r.Version, &r.Status, &r.Score,
		&r.Passed, &r.Total, &r.CostUSD, &r.Error, &r.NotifyAgent, &r.NotifyConversation,
		&startedAt, &finishedAt)
	if err != nil {
		return EvalRun{}, err
	}
	r.StartedAt = time.UnixMilli(startedAt)
	if finishedAt > 0 {
		r.FinishedAt = time.UnixMilli(finishedAt)
	}
	return r, nil
}

// ---- results ---------------------------------------------------------------

// RecordEvalResult saves one case's outcome. Writing per case rather than per
// run means a run killed halfway still shows what it managed to learn, and a
// retried case overwrites its own row rather than doubling it.
func (s *Store) RecordEvalResult(ctx context.Context, r EvalResult) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO eval_results (run_id, case_name, output, score, passed, reason, cost_usd, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, case_name) DO UPDATE SET
			output = excluded.output, score = excluded.score, passed = excluded.passed,
			reason = excluded.reason, cost_usd = excluded.cost_usd, duration_ms = excluded.duration_ms`,
		r.RunID, r.CaseName, r.Output, r.Score, boolToInt(r.Passed), r.Reason, r.CostUSD, r.DurationMS)
	if err != nil {
		return fmt.Errorf("record result %q of run %d: %w", r.CaseName, r.RunID, err)
	}
	return nil
}

// EvalResults returns a run's per-case outcomes, in case order.
func (s *Store) EvalResults(ctx context.Context, runID int64) ([]EvalResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, case_name, output, score, passed, reason, cost_usd, duration_ms
		FROM eval_results WHERE run_id = ? ORDER BY case_name`, runID)
	if err != nil {
		return nil, fmt.Errorf("results of run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []EvalResult
	for rows.Next() {
		var (
			r      EvalResult
			passed int
		)
		if err := rows.Scan(&r.RunID, &r.CaseName, &r.Output, &r.Score, &passed,
			&r.Reason, &r.CostUSD, &r.DurationMS); err != nil {
			return nil, fmt.Errorf("scan eval result: %w", err)
		}
		r.Passed = passed != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
