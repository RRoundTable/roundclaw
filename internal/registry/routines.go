package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/routine"
)

// Routines: known work handed off to a graph of steps (SPEC-004).
//
// A routine is a frozen contract plus a versioned graph. Versions are
// append-only and outlive the routine, like agent versions. A run is a row
// written before its execution starts; its steps are written as they start and
// finish, so the trace is readable while the run is wedged and never depends on
// Temporal's history (adr/006).

const routineSchema = `
CREATE TABLE IF NOT EXISTS routines (
    id            TEXT PRIMARY KEY,
    -- The agent whose authority bounds the routine, or '' for people (adr/009).
    owner         TEXT NOT NULL DEFAULT '',
    contract      TEXT NOT NULL,
    contract_hash TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'active',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS routine_versions (
    routine_id TEXT NOT NULL,
    version    INTEGER NOT NULL,
    plan       TEXT NOT NULL,   -- the compiled routine.Plan
    plan_hash  TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    author     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (routine_id, version)
);

CREATE TABLE IF NOT EXISTS routine_decisions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    routine_id TEXT NOT NULL,
    decision   TEXT NOT NULL,   -- admitted, promoted, rolled_back, archived, restored
    rule       TEXT NOT NULL,
    figures    TEXT NOT NULL DEFAULT '{}',
    author     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_routine_decisions ON routine_decisions(routine_id, id);

CREATE TABLE IF NOT EXISTS routine_runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    routine_id      TEXT NOT NULL,
    version         INTEGER NOT NULL,
    plan_hash       TEXT NOT NULL,
    -- The agent versions resolved when the run was admitted (adr/008).
    manifest        TEXT NOT NULL DEFAULT '{}',
    inputs          TEXT NOT NULL DEFAULT '{}',
    origin          TEXT NOT NULL,
    requested_by    TEXT NOT NULL DEFAULT '',
    parent_run_id   INTEGER NOT NULL DEFAULT 0,
    parent_step     TEXT NOT NULL DEFAULT '',
    parent_attempt  INTEGER NOT NULL DEFAULT 0,
    depth           INTEGER NOT NULL DEFAULT 0,
    expect          TEXT NOT NULL DEFAULT '{}',
    expect_verdicts TEXT NOT NULL DEFAULT '[]',
    instead_of      INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'running',
    reason          TEXT NOT NULL DEFAULT '',
    failed_step     TEXT NOT NULL DEFAULT '',
    output          TEXT NOT NULL DEFAULT '',
    cost_usd        REAL NOT NULL DEFAULT 0,
    usage           TEXT NOT NULL DEFAULT '{}',
    started_at      INTEGER NOT NULL,
    finished_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_routine_runs ON routine_runs(routine_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_routine_child_runs
    ON routine_runs(parent_run_id, parent_step, parent_attempt) WHERE parent_run_id <> 0;

CREATE TABLE IF NOT EXISTS routine_step_runs (
    run_id       INTEGER NOT NULL,
    step_id      TEXT NOT NULL,
    attempt      INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    status       TEXT NOT NULL,
    verdicts     TEXT NOT NULL DEFAULT '[]',
    feedback     TEXT NOT NULL DEFAULT '',
    output       TEXT NOT NULL DEFAULT '',
    exit_code    INTEGER,
    cost_usd     REAL NOT NULL DEFAULT 0,
    usage        TEXT NOT NULL DEFAULT '{}',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    turn_id      INTEGER NOT NULL DEFAULT 0,
    child_run_id INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, step_id, attempt)
);
`

// ErrContractFrozen is returned when a change would alter a routine's contract.
var ErrContractFrozen = errors.New("a routine's contract cannot change; register a new routine")

// Routine lifecycle states.
const (
	RoutineActive   = "active"
	RoutineArchived = "archived"
)

// Run and step statuses.
const (
	RunRunning  = "running"
	RunPassed   = "passed"
	RunFailed   = "failed"
	RunUnmarked = "unmarked"
	RunError    = "error"
	RunStopped  = "stopped"
	StepSkipped = "skipped"
)

// Routine is a registered routine with its current version.
type Routine struct {
	ID           string           `json:"id"`
	Owner        string           `json:"owner"`
	Contract     routine.Contract `json:"contract"`
	ContractHash string           `json:"contract_hash"`
	State        string           `json:"state"`
	Version      int              `json:"version"`
	Plan         routine.Plan     `json:"plan"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// RoutineVersion is one recorded version.
type RoutineVersion struct {
	RoutineID string       `json:"routine_id"`
	Version   int          `json:"version"`
	Plan      routine.Plan `json:"plan"`
	PlanHash  string       `json:"plan_hash"`
	Note      string       `json:"note,omitempty"`
	Author    string       `json:"author,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// RoutineDecision is a lifecycle decision and the figures that made it.
type RoutineDecision struct {
	ID        int64          `json:"id"`
	RoutineID string         `json:"routine_id"`
	Decision  string         `json:"decision"`
	Rule      string         `json:"rule"`
	Figures   map[string]any `json:"figures,omitempty"`
	Author    string         `json:"author,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// PutResult says what a PutRoutine did.
type PutResult struct {
	Routine Routine `json:"routine"`
	Created bool    `json:"created"`
	// Minted is false when the plan matched the current version exactly.
	Minted bool `json:"minted"`
}

// PutRoutine records a compiled routine. A new routine is created with owner as
// its owner; an existing one must keep its contract, and gets a new version
// only if its plan changed.
func (s *Store) PutRoutine(ctx context.Context, plan routine.Plan, owner string, c Change) (PutResult, error) {
	contractJSON, err := json.Marshal(plan.Source.Contract)
	if err != nil {
		return PutResult{}, fmt.Errorf("encode contract: %w", err)
	}
	contractHash, err := routine.ContractHash(plan.Source.Contract)
	if err != nil {
		return PutResult{}, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return PutResult{}, fmt.Errorf("encode plan: %w", err)
	}
	id := plan.Source.ID
	now := time.Now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PutResult{}, err
	}
	defer tx.Rollback()

	var res PutResult
	var existingHash string
	err = tx.QueryRowContext(ctx, `SELECT contract_hash FROM routines WHERE id = ?`, id).Scan(&existingHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO routines (id, owner, contract, contract_hash, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, owner, string(contractJSON), contractHash, RoutineActive, now, now); err != nil {
			return PutResult{}, fmt.Errorf("create routine %s: %w", id, err)
		}
		res.Created = true
	case err != nil:
		return PutResult{}, fmt.Errorf("read routine %s: %w", id, err)
	case existingHash != contractHash:
		return PutResult{}, fmt.Errorf("%w: %s", ErrContractFrozen, id)
	}

	var latestHash string
	err = tx.QueryRowContext(ctx,
		`SELECT plan_hash FROM routine_versions WHERE routine_id = ? ORDER BY version DESC LIMIT 1`, id).
		Scan(&latestHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PutResult{}, fmt.Errorf("read latest version of %s: %w", id, err)
	}
	if latestHash != plan.Hash {
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM routine_versions WHERE routine_id = ?`, id).
			Scan(&next); err != nil {
			return PutResult{}, fmt.Errorf("next version of %s: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO routine_versions (routine_id, version, plan, plan_hash, note, author, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, next, string(planJSON), plan.Hash, c.Note, c.Author, now); err != nil {
			return PutResult{}, fmt.Errorf("record version %d of %s: %w", next, id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE routines SET updated_at = ? WHERE id = ?`, now, id); err != nil {
			return PutResult{}, fmt.Errorf("touch routine %s: %w", id, err)
		}
		decision := "promoted"
		if res.Created {
			decision = "admitted"
		}
		if err := recordDecisionTx(ctx, tx, id, decision, "compiled at admission",
			map[string]any{"version": next, "plan_hash": plan.Hash}, c.Author, now); err != nil {
			return PutResult{}, err
		}
		res.Minted = true
	}
	if err := tx.Commit(); err != nil {
		return PutResult{}, err
	}
	res.Routine, err = s.GetRoutine(ctx, id)
	return res, err
}

func recordDecisionTx(ctx context.Context, tx *sql.Tx, id, decision, rule string, figures map[string]any, author string, now int64) error {
	b, err := json.Marshal(figures)
	if err != nil {
		return fmt.Errorf("encode decision figures: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO routine_decisions (routine_id, decision, rule, figures, author, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, decision, rule, string(b), author, now); err != nil {
		return fmt.Errorf("record decision for %s: %w", id, err)
	}
	return nil
}

// GetRoutine reads a routine and its current version.
func (s *Store) GetRoutine(ctx context.Context, id string) (Routine, error) {
	var (
		r                  Routine
		contractJSON, plan string
		created, updated   int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.owner, r.contract, r.contract_hash, r.state, r.created_at, r.updated_at,
		       v.version, v.plan
		FROM routines r
		JOIN routine_versions v ON v.routine_id = r.id
		WHERE r.id = ?
		ORDER BY v.version DESC LIMIT 1`, id).
		Scan(&r.ID, &r.Owner, &contractJSON, &r.ContractHash, &r.State, &created, &updated, &r.Version, &plan)
	if errors.Is(err, sql.ErrNoRows) {
		return Routine{}, fmt.Errorf("%w: routine %s", ErrNotFound, id)
	}
	if err != nil {
		return Routine{}, fmt.Errorf("get routine %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(contractJSON), &r.Contract); err != nil {
		return Routine{}, fmt.Errorf("decode contract of %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(plan), &r.Plan); err != nil {
		return Routine{}, fmt.Errorf("decode plan of %s: %w", id, err)
	}
	r.CreatedAt, r.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return r, nil
}

// ListRoutines returns every routine, in id order, optionally only active ones.
func (s *Store) ListRoutines(ctx context.Context, activeOnly bool) ([]Routine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM routines ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list routines: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Routine, 0, len(ids))
	for _, id := range ids {
		r, err := s.GetRoutine(ctx, id)
		if err != nil {
			return nil, err
		}
		if activeOnly && r.State != RoutineActive {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// RoutineUses returns the routines a routine's current version uses as steps.
func (s *Store) RoutineUses(ctx context.Context, id string) ([]string, bool, error) {
	r, err := s.GetRoutine(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var uses []string
	for _, st := range r.Plan.Source.Steps {
		if st.Kind == routine.KindRoutine {
			uses = append(uses, st.Routine)
		}
	}
	return uses, true, nil
}

// GetRoutineVersion reads one version.
func (s *Store) GetRoutineVersion(ctx context.Context, id string, version int) (RoutineVersion, error) {
	var (
		v       RoutineVersion
		plan    string
		created int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT routine_id, version, plan, plan_hash, note, author, created_at
		FROM routine_versions WHERE routine_id = ? AND version = ?`, id, version).
		Scan(&v.RoutineID, &v.Version, &plan, &v.PlanHash, &v.Note, &v.Author, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return RoutineVersion{}, fmt.Errorf("%w: routine %s v%d", ErrNotFound, id, version)
	}
	if err != nil {
		return RoutineVersion{}, fmt.Errorf("get routine %s v%d: %w", id, version, err)
	}
	if err := json.Unmarshal([]byte(plan), &v.Plan); err != nil {
		return RoutineVersion{}, fmt.Errorf("decode plan of %s v%d: %w", id, version, err)
	}
	v.CreatedAt = time.UnixMilli(created)
	return v, nil
}

// ListRoutineVersions returns versions newest first.
func (s *Store) ListRoutineVersions(ctx context.Context, id string) ([]RoutineVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version FROM routine_versions WHERE routine_id = ? ORDER BY version DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("list versions of %s: %w", id, err)
	}
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]RoutineVersion, 0, len(versions))
	for _, v := range versions {
		rv, err := s.GetRoutineVersion(ctx, id, v)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, nil
}

// RollbackRoutine applies an old version's plan as a new version: rolling back
// is itself a change, recorded like any other.
func (s *Store) RollbackRoutine(ctx context.Context, id string, version int, c Change) (PutResult, error) {
	old, err := s.GetRoutineVersion(ctx, id, version)
	if err != nil {
		return PutResult{}, err
	}
	cur, err := s.GetRoutine(ctx, id)
	if err != nil {
		return PutResult{}, err
	}
	if c.Note == "" {
		c.Note = fmt.Sprintf("rolled back to v%d", version)
	}
	// Applying the old plan mints a new version whenever the current one differs
	// from it; rolling back to the version already current changes nothing.
	res, err := s.PutRoutine(ctx, old.Plan, cur.Owner, c)
	if err != nil {
		return res, err
	}
	if !res.Minted {
		return res, nil
	}
	return res, s.RecordRoutineDecision(ctx, id, "rolled_back",
		"returned to an earlier version", map[string]any{"to": version, "from": cur.Version}, c.Author)
}

// SetRoutineState archives or restores a routine, recording why.
func (s *Store) SetRoutineState(ctx context.Context, id, state, rule string, figures map[string]any, author string) error {
	if state != RoutineActive && state != RoutineArchived {
		return fmt.Errorf("unknown routine state %q", state)
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE routines SET state = ?, updated_at = ? WHERE id = ?`, state, now, id)
	if err != nil {
		return fmt.Errorf("set state of %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: routine %s", ErrNotFound, id)
	}
	decision := "archived"
	if state == RoutineActive {
		decision = "restored"
	}
	if err := recordDecisionTx(ctx, tx, id, decision, rule, figures, author, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordRoutineDecision records a lifecycle decision outside a state change.
func (s *Store) RecordRoutineDecision(ctx context.Context, id, decision, rule string, figures map[string]any, author string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordDecisionTx(ctx, tx, id, decision, rule, figures, author, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// ListRoutineDecisions returns a routine's lifecycle decisions, newest first.
func (s *Store) ListRoutineDecisions(ctx context.Context, id string) ([]RoutineDecision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, routine_id, decision, rule, figures, author, created_at
		FROM routine_decisions WHERE routine_id = ? ORDER BY id DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("list decisions of %s: %w", id, err)
	}
	defer rows.Close()
	var out []RoutineDecision
	for rows.Next() {
		var (
			d       RoutineDecision
			figures string
			created int64
		)
		if err := rows.Scan(&d.ID, &d.RoutineID, &d.Decision, &d.Rule, &figures, &d.Author, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(figures), &d.Figures); err != nil {
			return nil, fmt.Errorf("decode decision %d: %w", d.ID, err)
		}
		d.CreatedAt = time.UnixMilli(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RoutineRun is one run of a routine.
type RoutineRun struct {
	ID             int64             `json:"id"`
	RoutineID      string            `json:"routine_id"`
	Version        int               `json:"version"`
	PlanHash       string            `json:"plan_hash"`
	Manifest       map[string]int    `json:"manifest,omitempty"`
	Inputs         map[string]string `json:"inputs,omitempty"`
	Origin         core.Origin       `json:"origin"`
	RequestedBy    string            `json:"requested_by,omitempty"`
	ParentRunID    int64             `json:"parent_run_id,omitempty"`
	ParentStep     string            `json:"parent_step,omitempty"`
	ParentAttempt  int               `json:"parent_attempt,omitempty"`
	Depth          int               `json:"depth"`
	Expect         routine.Condition `json:"expect,omitempty"`
	ExpectVerdicts []routine.Verdict `json:"expect_verdicts,omitempty"`
	InsteadOf      int64             `json:"instead_of,omitempty"`
	Status         string            `json:"status"`
	Reason         string            `json:"reason,omitempty"`
	FailedStep     string            `json:"failed_step,omitempty"`
	Output         string            `json:"output,omitempty"`
	CostUSD        float64           `json:"cost_usd"`
	Usage          core.Usage        `json:"usage"`
	StartedAt      time.Time         `json:"started_at"`
	FinishedAt     time.Time         `json:"finished_at,omitempty"`
}

// StartRoutineRun writes a run row before its execution starts, so a caller can
// poll it at once and a retried start never makes a second run. A child run is
// unique per parent run, step and attempt; asking again returns the first.
func (s *Store) StartRoutineRun(ctx context.Context, run RoutineRun) (RoutineRun, error) {
	inputs, err := json.Marshal(run.Inputs)
	if err != nil {
		return RoutineRun{}, fmt.Errorf("encode inputs: %w", err)
	}
	origin, err := json.Marshal(run.Origin)
	if err != nil {
		return RoutineRun{}, fmt.Errorf("encode origin: %w", err)
	}
	expect, err := json.Marshal(run.Expect)
	if err != nil {
		return RoutineRun{}, fmt.Errorf("encode expect: %w", err)
	}
	manifest, err := json.Marshal(run.Manifest)
	if err != nil {
		return RoutineRun{}, fmt.Errorf("encode manifest: %w", err)
	}
	if run.ParentRunID != 0 {
		var existing int64
		err := s.db.QueryRowContext(ctx, `
			SELECT id FROM routine_runs WHERE parent_run_id = ? AND parent_step = ? AND parent_attempt = ?`,
			run.ParentRunID, run.ParentStep, run.ParentAttempt).Scan(&existing)
		if err == nil {
			return s.GetRoutineRun(ctx, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return RoutineRun{}, fmt.Errorf("find child run: %w", err)
		}
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO routine_runs (routine_id, version, plan_hash, manifest, inputs, origin, requested_by,
		    parent_run_id, parent_step, parent_attempt, depth, expect, instead_of, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.RoutineID, run.Version, run.PlanHash, string(manifest), string(inputs), string(origin), run.RequestedBy,
		run.ParentRunID, run.ParentStep, run.ParentAttempt, run.Depth, string(expect), run.InsteadOf,
		RunRunning, time.Now().UnixMilli())
	if err != nil {
		return RoutineRun{}, fmt.Errorf("start run of %s: %w", run.RoutineID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return RoutineRun{}, err
	}
	return s.GetRoutineRun(ctx, id)
}

const runColumns = `id, routine_id, version, plan_hash, manifest, inputs, origin, requested_by, parent_run_id,
	parent_step, parent_attempt, depth, expect, expect_verdicts, instead_of, status, reason,
	failed_step, output, cost_usd, usage, started_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (RoutineRun, error) {
	var (
		r                                                 RoutineRun
		inputs, origin, expect, verdicts, usage, manifest string
		started, finished                                 int64
	)
	if err := row.Scan(&r.ID, &r.RoutineID, &r.Version, &r.PlanHash, &manifest, &inputs, &origin, &r.RequestedBy,
		&r.ParentRunID, &r.ParentStep, &r.ParentAttempt, &r.Depth, &expect, &verdicts, &r.InsteadOf,
		&r.Status, &r.Reason, &r.FailedStep, &r.Output, &r.CostUSD, &usage, &started, &finished); err != nil {
		return RoutineRun{}, err
	}
	for _, f := range []struct {
		raw string
		dst any
	}{{manifest, &r.Manifest}, {inputs, &r.Inputs}, {expect, &r.Expect}, {verdicts, &r.ExpectVerdicts}, {usage, &r.Usage}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return RoutineRun{}, fmt.Errorf("decode run %d: %w", r.ID, err)
		}
	}
	o, err := core.DecodeOrigin(origin)
	if err != nil {
		return RoutineRun{}, fmt.Errorf("run %d: %w", r.ID, err)
	}
	r.Origin = o
	r.StartedAt = time.UnixMilli(started)
	if finished > 0 {
		r.FinishedAt = time.UnixMilli(finished)
	}
	return r, nil
}

// GetRoutineRun reads one run.
func (s *Store) GetRoutineRun(ctx context.Context, id int64) (RoutineRun, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM routine_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RoutineRun{}, fmt.Errorf("%w: routine run %d", ErrNotFound, id)
	}
	if err != nil {
		return RoutineRun{}, fmt.Errorf("get run %d: %w", id, err)
	}
	return r, nil
}

// RunFilter narrows ListRoutineRuns.
type RunFilter struct {
	RoutineID   string
	RequestedBy string
	Status      string
	Limit       int
}

// ListRoutineRuns returns runs newest first.
func (s *Store) ListRoutineRuns(ctx context.Context, f RunFilter) ([]RoutineRun, error) {
	q := `SELECT ` + runColumns + ` FROM routine_runs WHERE 1=1`
	var args []any
	if f.RoutineID != "" {
		q += ` AND routine_id = ?`
		args = append(args, f.RoutineID)
	}
	if f.RequestedBy != "" {
		q += ` AND requested_by = ?`
		args = append(args, f.RequestedBy)
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 20
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []RoutineRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunOutcome closes out a run.
type RunOutcome struct {
	Status         string
	Reason         string
	FailedStep     string
	Output         string
	ExpectVerdicts []routine.Verdict
}

// FinishRoutineRun closes out a run and totals its cost and usage from the step
// rows rather than from anything carried through the workflow. A run already
// finished keeps its first outcome.
func (s *Store) FinishRoutineRun(ctx context.Context, id int64, o RunOutcome) (RoutineRun, error) {
	steps, err := s.ListStepRuns(ctx, id)
	if err != nil {
		return RoutineRun{}, err
	}
	var (
		cost  float64
		usage core.Usage
	)
	for _, st := range steps {
		cost += st.CostUSD
		usage.Add(st.Usage)
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return RoutineRun{}, err
	}
	verdicts, err := json.Marshal(o.ExpectVerdicts)
	if err != nil {
		return RoutineRun{}, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE routine_runs SET status = ?, reason = ?, failed_step = ?, output = ?, expect_verdicts = ?,
		    cost_usd = ?, usage = ?, finished_at = ?
		WHERE id = ? AND status = ?`,
		o.Status, o.Reason, o.FailedStep, o.Output, string(verdicts), cost, string(usageJSON),
		time.Now().UnixMilli(), id, RunRunning); err != nil {
		return RoutineRun{}, fmt.Errorf("finish run %d: %w", id, err)
	}
	return s.GetRoutineRun(ctx, id)
}

// StepRun is one attempt of one step: the trace.
type StepRun struct {
	RunID      int64             `json:"run_id"`
	StepID     string            `json:"step_id"`
	Attempt    int               `json:"attempt"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status"`
	Verdicts   []routine.Verdict `json:"verdicts,omitempty"`
	Feedback   string            `json:"feedback,omitempty"`
	Output     string            `json:"output,omitempty"`
	ExitCode   *int              `json:"exit_code,omitempty"`
	CostUSD    float64           `json:"cost_usd"`
	Usage      core.Usage        `json:"usage"`
	DurationMS int64             `json:"duration_ms"`
	TurnID     int64             `json:"turn_id,omitempty"`
	ChildRunID int64             `json:"child_run_id,omitempty"`
	Error      string            `json:"error,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
}

// StartStepRun records an attempt the moment it starts, so a long step is
// visible while it runs. Starting an attempt that is already recorded — a
// retried activity — leaves the first start in place.
func (s *Store) StartStepRun(ctx context.Context, runID int64, stepID string, attempt int, kind, feedback string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO routine_step_runs (run_id, step_id, attempt, kind, status, feedback, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, step_id, attempt) DO NOTHING`,
		runID, stepID, attempt, kind, RunRunning, feedback, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("start step %s/%d of run %d: %w", stepID, attempt, runID, err)
	}
	return nil
}

// FinishStepRun records how an attempt ended. A step recorded as skipped was
// never started, so this upserts.
func (s *Store) FinishStepRun(ctx context.Context, st StepRun) error {
	verdicts, err := json.Marshal(st.Verdicts)
	if err != nil {
		return err
	}
	usage, err := json.Marshal(st.Usage)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO routine_step_runs (run_id, step_id, attempt, kind, status, verdicts, feedback, output,
		    exit_code, cost_usd, usage, duration_ms, turn_id, child_run_id, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, step_id, attempt) DO UPDATE SET
		    status = excluded.status, verdicts = excluded.verdicts, output = excluded.output,
		    exit_code = excluded.exit_code, cost_usd = excluded.cost_usd, usage = excluded.usage,
		    duration_ms = excluded.duration_ms, turn_id = excluded.turn_id,
		    child_run_id = excluded.child_run_id, error = excluded.error, finished_at = excluded.finished_at
		WHERE routine_step_runs.finished_at = 0`,
		st.RunID, st.StepID, st.Attempt, st.Kind, st.Status, string(verdicts), st.Feedback, st.Output,
		st.ExitCode, st.CostUSD, string(usage), st.DurationMS, st.TurnID, st.ChildRunID, st.Error, now, now)
	if err != nil {
		return fmt.Errorf("finish step %s/%d of run %d: %w", st.StepID, st.Attempt, st.RunID, err)
	}
	return nil
}

// ListStepRuns returns a run's step attempts in the order they started.
func (s *Store) ListStepRuns(ctx context.Context, runID int64) ([]StepRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, step_id, attempt, kind, status, verdicts, feedback, output, exit_code, cost_usd,
		       usage, duration_ms, turn_id, child_run_id, error, started_at, finished_at
		FROM routine_step_runs WHERE run_id = ? ORDER BY started_at, step_id, attempt`, runID)
	if err != nil {
		return nil, fmt.Errorf("list steps of run %d: %w", runID, err)
	}
	defer rows.Close()
	var out []StepRun
	for rows.Next() {
		var (
			st                StepRun
			verdicts, usage   string
			exit              sql.NullInt64
			started, finished int64
		)
		if err := rows.Scan(&st.RunID, &st.StepID, &st.Attempt, &st.Kind, &st.Status, &verdicts, &st.Feedback,
			&st.Output, &exit, &st.CostUSD, &usage, &st.DurationMS, &st.TurnID, &st.ChildRunID, &st.Error,
			&started, &finished); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(verdicts), &st.Verdicts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(usage), &st.Usage); err != nil {
			return nil, err
		}
		if exit.Valid {
			code := int(exit.Int64)
			st.ExitCode = &code
		}
		st.StartedAt = time.UnixMilli(started)
		if finished > 0 {
			st.FinishedAt = time.UnixMilli(finished)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Hotspot is how often one step fell short.
type Hotspot struct {
	Step             string  `json:"step"`
	Attempts         int     `json:"attempts"`
	Failures         int     `json:"failures"`
	FirstAttemptPass float64 `json:"first_attempt_pass_rate"`
}

// RoutineFigures is a routine's record for one scope — a version, or lifetime.
// Every figure is counted from recorded rows; none is described (invariant 12).
// Pass rates are kept apart by whose standard they measure.
type RoutineFigures struct {
	Version  int `json:"version,omitempty"` // 0 = lifetime
	Runs     int `json:"runs"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Unmarked int `json:"unmarked"`
	Errors   int `json:"errors"`
	Stopped  int `json:"stopped"`
	// OwnPassRate is passed ÷ (passed + failed) under the routine's own
	// conditions; unmarked runs and errors are not failures.
	OwnPassRate *float64 `json:"own_pass_rate"`
	// CallerStated is how many runs carried the caller's conditions, and
	// CallerMet how many of those met them.
	CallerStated  int      `json:"caller_stated"`
	CallerMet     int      `json:"caller_met"`
	CallerMetRate *float64 `json:"caller_met_rate"`
	// Redone counts runs a caller replaced with a new hand-off.
	Redone        int       `json:"redone"`
	P50DurationMS int64     `json:"p50_duration_ms"`
	P90DurationMS int64     `json:"p90_duration_ms"`
	MeanCostUSD   float64   `json:"mean_cost_usd"`
	CostPerPassed *float64  `json:"cost_per_passed_usd"`
	Tokens        int64     `json:"tokens"`
	Recent        []string  `json:"recent,omitempty"` // newest first
	Hotspots      []Hotspot `json:"hotspots,omitempty"`
}

// RoutineMetrics computes a routine's record for its current version and for
// its lifetime, from finished top-level runs.
func (s *Store) RoutineMetrics(ctx context.Context, id string, currentVersion int) (current, lifetime RoutineFigures, err error) {
	runs, err := s.ListRoutineRuns(ctx, RunFilter{RoutineID: id, Limit: 200})
	if err != nil {
		return current, lifetime, err
	}
	redone := map[int64]bool{}
	for _, r := range runs {
		if r.InsteadOf != 0 {
			redone[r.InsteadOf] = true
		}
	}
	var cur, all []RoutineRun
	for _, r := range runs {
		if r.Status == RunRunning {
			continue
		}
		all = append(all, r)
		if r.Version == currentVersion {
			cur = append(cur, r)
		}
	}
	current, err = s.figures(ctx, cur, redone)
	if err != nil {
		return current, lifetime, err
	}
	current.Version = currentVersion
	lifetime, err = s.figures(ctx, all, redone)
	return current, lifetime, err
}

func (s *Store) figures(ctx context.Context, runs []RoutineRun, redone map[int64]bool) (RoutineFigures, error) {
	var f RoutineFigures
	var durations []int64
	var cost float64
	hot := map[string]*Hotspot{}
	firstPass := map[string]int{}
	for i, r := range runs {
		f.Runs++
		switch r.Status {
		case RunPassed:
			f.Passed++
		case RunFailed:
			f.Failed++
		case RunUnmarked:
			f.Unmarked++
		case RunError:
			f.Errors++
		case RunStopped:
			f.Stopped++
		}
		if !r.Expect.Empty() {
			f.CallerStated++
			if routine.Held(r.ExpectVerdicts) {
				f.CallerMet++
			}
		}
		if redone[r.ID] {
			f.Redone++
		}
		if i < 10 {
			f.Recent = append(f.Recent, r.Status)
		}
		if !r.FinishedAt.IsZero() {
			durations = append(durations, r.FinishedAt.Sub(r.StartedAt).Milliseconds())
		}
		cost += r.CostUSD
		f.Tokens += r.Usage.Tokens()

		steps, err := s.ListStepRuns(ctx, r.ID)
		if err != nil {
			return f, err
		}
		for _, st := range steps {
			if st.Status == StepSkipped {
				continue
			}
			h := hot[st.StepID]
			if h == nil {
				h = &Hotspot{Step: st.StepID}
				hot[st.StepID] = h
			}
			h.Attempts++
			if st.Status == RunFailed || st.Status == RunError {
				h.Failures++
			}
			if st.Attempt == 1 && st.Status == RunPassed {
				firstPass[st.StepID]++
			}
		}
	}
	if decided := f.Passed + f.Failed; decided > 0 {
		rate := float64(f.Passed) / float64(decided)
		f.OwnPassRate = &rate
	}
	if f.CallerStated > 0 {
		rate := float64(f.CallerMet) / float64(f.CallerStated)
		f.CallerMetRate = &rate
	}
	if f.Runs > 0 {
		f.MeanCostUSD = cost / float64(f.Runs)
	}
	if f.Passed > 0 {
		per := cost / float64(f.Passed)
		f.CostPerPassed = &per
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	f.P50DurationMS = percentile(durations, 50)
	f.P90DurationMS = percentile(durations, 90)

	for id, h := range hot {
		if f.Runs > 0 {
			h.FirstAttemptPass = float64(firstPass[id]) / float64(f.Runs)
		}
		if h.Failures > 0 {
			f.Hotspots = append(f.Hotspots, *h)
		}
	}
	sort.Slice(f.Hotspots, func(i, j int) bool {
		if f.Hotspots[i].Failures != f.Hotspots[j].Failures {
			return f.Hotspots[i].Failures > f.Hotspots[j].Failures
		}
		return f.Hotspots[i].Step < f.Hotspots[j].Step
	})
	return f, nil
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*p + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}
