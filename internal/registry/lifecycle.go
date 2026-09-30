package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/roundtable/roundclaw/internal/routine"
)

// The routine lifecycle's records (adr/013): the independent cases a routine is
// held to, candidates drawn from recurring work, trials and their verdicts, and
// what the lifecycle itself spends. Decisions are made elsewhere, by rule; this
// only keeps what they decided on.

const lifecycleSchema = `
CREATE TABLE IF NOT EXISTS routine_cases (
    routine_id TEXT NOT NULL,
    name       TEXT NOT NULL,
    inputs     TEXT NOT NULL DEFAULT '{}',
    -- Written by the examiner from the work the case came from, never by the
    -- routine's author, and never changed by whoever improves the routine.
    checks     TEXT NOT NULL DEFAULT '{}',
    source     TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (routine_id, name)
);

CREATE TABLE IF NOT EXISTS routine_candidates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    shape      TEXT NOT NULL,
    evidence   TEXT NOT NULL DEFAULT '[]',
    state      TEXT NOT NULL DEFAULT 'waiting',  -- waiting, on_trial, admitted, rejected
    routine_id TEXT NOT NULL DEFAULT '',
    drafts     INTEGER NOT NULL DEFAULT 0,
    reason     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_routine_candidates_shape ON routine_candidates(shape, id);

CREATE TABLE IF NOT EXISTS routine_trials (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL,    -- admission, version, remeasure
    routine_id   TEXT NOT NULL,
    version      INTEGER NOT NULL,
    base_version INTEGER NOT NULL DEFAULT 0,
    candidate_id INTEGER NOT NULL DEFAULT 0,
    cases        TEXT NOT NULL DEFAULT '[]',
    state        TEXT NOT NULL DEFAULT 'running',  -- running, accepted, rejected, error
    rule         TEXT NOT NULL DEFAULT '',
    figures      TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_routine_trials ON routine_trials(routine_id, id);

CREATE TABLE IF NOT EXISTS lifecycle_spend (
    day        TEXT NOT NULL,
    routine_id TEXT NOT NULL,
    cost_usd   REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (day, routine_id)
);
`

// Trial kinds and states.
const (
	TrialAdmission = "admission"
	TrialVersion   = "version"
	TrialRemeasure = "remeasure"

	TrialRunning  = "running"
	TrialAccepted = "accepted"
	TrialRejected = "rejected"
	TrialError    = "error"
)

// Candidate states.
const (
	CandidateWaiting  = "waiting"
	CandidateOnTrial  = "on_trial"
	CandidateAdmitted = "admitted"
	CandidateRejected = "rejected"
)

// CaseSource is the work a case was drawn from.
type CaseSource struct {
	Agent   string  `json:"agent,omitempty"`
	TurnID  int64   `json:"turn_id,omitempty"`
	Request string  `json:"request,omitempty"`
	Outcome string  `json:"outcome,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// RoutineCase is one independent case a routine is held to.
type RoutineCase struct {
	RoutineID string            `json:"routine_id"`
	Name      string            `json:"name"`
	Inputs    map[string]string `json:"inputs"`
	Checks    routine.Condition `json:"checks"`
	Source    CaseSource        `json:"source"`
}

// PutRoutineCases records a routine's independent cases, once. A case already
// recorded is left as it is: nothing that changes a routine changes what it is
// measured by (invariant 8).
func (s *Store) PutRoutineCases(ctx context.Context, cases []RoutineCase) error {
	now := time.Now().UnixMilli()
	for _, c := range cases {
		inputs, err := json.Marshal(c.Inputs)
		if err != nil {
			return err
		}
		checks, err := json.Marshal(c.Checks)
		if err != nil {
			return err
		}
		source, err := json.Marshal(c.Source)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO routine_cases (routine_id, name, inputs, checks, source, created_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (routine_id, name) DO NOTHING`,
			c.RoutineID, c.Name, string(inputs), string(checks), string(source), now); err != nil {
			return fmt.Errorf("record case %s of %s: %w", c.Name, c.RoutineID, err)
		}
	}
	return nil
}

// ListRoutineCases returns a routine's independent cases in name order.
func (s *Store) ListRoutineCases(ctx context.Context, routineID string) ([]RoutineCase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT routine_id, name, inputs, checks, source FROM routine_cases WHERE routine_id = ? ORDER BY name`, routineID)
	if err != nil {
		return nil, fmt.Errorf("list cases of %s: %w", routineID, err)
	}
	defer rows.Close()
	var out []RoutineCase
	for rows.Next() {
		var (
			c                      RoutineCase
			inputs, checks, source string
		)
		if err := rows.Scan(&c.RoutineID, &c.Name, &inputs, &checks, &source); err != nil {
			return nil, err
		}
		for _, f := range []struct {
			raw string
			dst any
		}{{inputs, &c.Inputs}, {checks, &c.Checks}, {source, &c.Source}} {
			if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
				return nil, fmt.Errorf("decode case %s: %w", c.Name, err)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Candidate is recurring work waiting to become a routine.
type Candidate struct {
	ID        int64             `json:"id"`
	Shape     string            `json:"shape"`
	Evidence  []json.RawMessage `json:"evidence"`
	State     string            `json:"state"`
	RoutineID string            `json:"routine_id,omitempty"`
	Drafts    int               `json:"drafts"`
	Reason    string            `json:"reason,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// CreateCandidate records recurring work to draft a routine from.
func (s *Store) CreateCandidate(ctx context.Context, shape string, evidence any) (Candidate, error) {
	b, err := json.Marshal(evidence)
	if err != nil {
		return Candidate{}, err
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO routine_candidates (shape, evidence, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		shape, string(b), CandidateWaiting, now, now)
	if err != nil {
		return Candidate{}, fmt.Errorf("record candidate: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Candidate{}, err
	}
	return s.GetCandidate(ctx, id)
}

// GetCandidate reads one candidate.
func (s *Store) GetCandidate(ctx context.Context, id int64) (Candidate, error) {
	var (
		c                Candidate
		evidence         string
		created, updated int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, shape, evidence, state, routine_id, drafts, reason, created_at, updated_at
		FROM routine_candidates WHERE id = ?`, id).
		Scan(&c.ID, &c.Shape, &evidence, &c.State, &c.RoutineID, &c.Drafts, &c.Reason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, fmt.Errorf("%w: candidate %d", ErrNotFound, id)
	}
	if err != nil {
		return Candidate{}, fmt.Errorf("get candidate %d: %w", id, err)
	}
	if err := json.Unmarshal([]byte(evidence), &c.Evidence); err != nil {
		return Candidate{}, fmt.Errorf("decode candidate %d: %w", id, err)
	}
	c.CreatedAt, c.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return c, nil
}

// ListCandidates returns candidates newest first.
func (s *Store) ListCandidates(ctx context.Context, limit int) ([]Candidate, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM routine_candidates ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		c, err := s.GetCandidate(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// UpdateCandidate moves a candidate on.
func (s *Store) UpdateCandidate(ctx context.Context, id int64, state, routineID, reason string, draft bool) error {
	add := 0
	if draft {
		add = 1
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE routine_candidates SET state = ?, routine_id = CASE WHEN ? = '' THEN routine_id ELSE ? END,
		    reason = ?, drafts = drafts + ?, updated_at = ? WHERE id = ?`,
		state, routineID, routineID, reason, add, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("update candidate %d: %w", id, err)
	}
	return nil
}

// ShapesToSkip are shapes recurrence mining must not raise again: those with a
// candidate still open, those admitted, and those rejected within the cooldown.
func (s *Store) ShapesToSkip(ctx context.Context, cooldown time.Duration) (map[string]bool, error) {
	cutoff := time.Now().Add(-cooldown).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT shape FROM routine_candidates
		WHERE state IN (?, ?, ?) OR (state = ? AND updated_at >= ?)`,
		CandidateWaiting, CandidateOnTrial, CandidateAdmitted, CandidateRejected, cutoff)
	if err != nil {
		return nil, fmt.Errorf("shapes to skip: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var shape string
		if err := rows.Scan(&shape); err != nil {
			return nil, err
		}
		out[shape] = true
	}
	return out, rows.Err()
}

// TrialCase is one case a trial runs.
type TrialCase struct {
	Name     string            `json:"name"`
	Inputs   map[string]string `json:"inputs"`
	Checks   routine.Condition `json:"checks"`
	Source   CaseSource        `json:"source"`
	Targeted bool              `json:"targeted,omitempty"` // a recent failure a version was drafted to fix
	// Unmarked is set when the examiner could not write checks for the case.
	Unmarked bool `json:"unmarked,omitempty"`
	// Runs are the routine runs this case produced: one per version tried.
	Runs map[int]int64 `json:"runs,omitempty"`
}

// Trial is one admission, version or re-measurement trial.
type Trial struct {
	ID          int64          `json:"id"`
	Kind        string         `json:"kind"`
	RoutineID   string         `json:"routine_id"`
	Version     int            `json:"version"`
	BaseVersion int            `json:"base_version,omitempty"`
	CandidateID int64          `json:"candidate_id,omitempty"`
	Cases       []TrialCase    `json:"cases"`
	State       string         `json:"state"`
	Rule        string         `json:"rule,omitempty"`
	Figures     map[string]any `json:"figures,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	FinishedAt  time.Time      `json:"finished_at,omitempty"`
}

// CreateTrial records a trial before it runs.
func (s *Store) CreateTrial(ctx context.Context, t Trial) (Trial, error) {
	cases, err := json.Marshal(t.Cases)
	if err != nil {
		return Trial{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO routine_trials (kind, routine_id, version, base_version, candidate_id, cases, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Kind, t.RoutineID, t.Version, t.BaseVersion, t.CandidateID, string(cases), TrialRunning, time.Now().UnixMilli())
	if err != nil {
		return Trial{}, fmt.Errorf("record trial of %s: %w", t.RoutineID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Trial{}, err
	}
	return s.GetTrial(ctx, id)
}

// GetTrial reads one trial.
func (s *Store) GetTrial(ctx context.Context, id int64) (Trial, error) {
	var (
		t                 Trial
		cases, figures    string
		created, finished int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, routine_id, version, base_version, candidate_id, cases, state, rule, figures, created_at, finished_at
		FROM routine_trials WHERE id = ?`, id).
		Scan(&t.ID, &t.Kind, &t.RoutineID, &t.Version, &t.BaseVersion, &t.CandidateID, &cases, &t.State,
			&t.Rule, &figures, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Trial{}, fmt.Errorf("%w: trial %d", ErrNotFound, id)
	}
	if err != nil {
		return Trial{}, fmt.Errorf("get trial %d: %w", id, err)
	}
	if err := json.Unmarshal([]byte(cases), &t.Cases); err != nil {
		return Trial{}, err
	}
	if err := json.Unmarshal([]byte(figures), &t.Figures); err != nil {
		return Trial{}, err
	}
	t.CreatedAt = time.UnixMilli(created)
	if finished > 0 {
		t.FinishedAt = time.UnixMilli(finished)
	}
	return t, nil
}

// SetTrialCases replaces a trial's cases — examiner checks and run ids are
// written onto them as the trial proceeds.
func (s *Store) SetTrialCases(ctx context.Context, id int64, cases []TrialCase) error {
	b, err := json.Marshal(cases)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE routine_trials SET cases = ? WHERE id = ?`, string(b), id)
	return err
}

// FinishTrial records a trial's verdict, once.
func (s *Store) FinishTrial(ctx context.Context, id int64, state, rule string, figures map[string]any) error {
	b, err := json.Marshal(figures)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE routine_trials SET state = ?, rule = ?, figures = ?, finished_at = ? WHERE id = ? AND state = ?`,
		state, rule, string(b), time.Now().UnixMilli(), id, TrialRunning)
	return err
}

// ListTrials returns a routine's trials newest first, or every routine's when
// routineID is empty.
func (s *Store) ListTrials(ctx context.Context, routineID string, limit int) ([]Trial, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id FROM routine_trials`
	var args []any
	if routineID != "" {
		q += ` WHERE routine_id = ?`
		args = append(args, routineID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]Trial, 0, len(ids))
	for _, id := range ids {
		t, err := s.GetTrial(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// RecentRejections counts a routine's consecutive rejected version trials,
// newest first, and when the last one finished.
func (s *Store) RecentRejections(ctx context.Context, routineID string) (int, time.Time, error) {
	trials, err := s.ListTrials(ctx, routineID, 20)
	if err != nil {
		return 0, time.Time{}, err
	}
	n := 0
	var last time.Time
	for _, t := range trials {
		if t.Kind != TrialVersion || t.State == TrialRunning {
			continue
		}
		if t.State != TrialRejected {
			break
		}
		if n == 0 {
			last = t.FinishedAt
		}
		n++
	}
	return n, last, nil
}

// PromoteRoutineVersion makes a version the one that serves, and admits the
// routine if it was on trial.
func (s *Store) PromoteRoutineVersion(ctx context.Context, id string, version int, rule string, figures map[string]any, author string) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE routine_versions SET promoted = 1 WHERE routine_id = ? AND version = ?`, id, version)
	if err != nil {
		return fmt.Errorf("promote %s v%d: %w", id, version, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: routine %s v%d", ErrNotFound, id, version)
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM routines WHERE id = ?`, id).Scan(&state); err != nil {
		return err
	}
	decision := "promoted"
	if state == RoutineTrial || state == RoutineRejected {
		decision = "admitted"
		if _, err := tx.ExecContext(ctx, `UPDATE routines SET state = ?, updated_at = ? WHERE id = ?`, RoutineActive, now, id); err != nil {
			return err
		}
	}
	if figures == nil {
		figures = map[string]any{}
	}
	figures["version"] = version
	if err := recordDecisionTx(ctx, tx, id, decision, rule, figures, author, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RejectRoutineVersion records that a version failed its trial. A routine that
// never had a serving version is rejected with it.
func (s *Store) RejectRoutineVersion(ctx context.Context, id string, version int, rule string, figures map[string]any) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE routine_versions SET promoted = -1 WHERE routine_id = ? AND version = ? AND promoted = 0`, id, version); err != nil {
		return err
	}
	var serving int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_versions WHERE routine_id = ? AND promoted = 1`, id).Scan(&serving); err != nil {
		return err
	}
	if serving == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE routines SET state = ?, updated_at = ? WHERE id = ?`, RoutineRejected, now, id); err != nil {
			return err
		}
	}
	if figures == nil {
		figures = map[string]any{}
	}
	figures["version"] = version
	if err := recordDecisionTx(ctx, tx, id, "rejected", rule, figures, "", now); err != nil {
		return err
	}
	return tx.Commit()
}

// SetRoutineUnrunnable records when a routine became unable to run, or clears
// it. The first moment is kept, so the grace period counts from it.
func (s *Store) SetRoutineUnrunnable(ctx context.Context, id string, unrunnable bool) error {
	var err error
	if unrunnable {
		_, err = s.db.ExecContext(ctx, `UPDATE routines SET unrunnable_since = ? WHERE id = ? AND unrunnable_since = 0`,
			time.Now().UnixMilli(), id)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE routines SET unrunnable_since = 0 WHERE id = ?`, id)
	}
	return err
}

// RecordMeasurement keeps the result of measuring a routine on its independent
// cases and the manifest it ran against, so a later dependency change is seen.
func (s *Store) RecordMeasurement(ctx context.Context, id string, passed, total int, manifest map[string]int) error {
	b, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE routines SET measured_at = ?, measured_manifest = ?, independent_passed = ?, independent_total = ?
		WHERE id = ?`, time.Now().UnixMilli(), string(b), passed, total, id)
	return err
}

// AddLifecycleSpend adds to what the lifecycle spent today on a routine.
func (s *Store) AddLifecycleSpend(ctx context.Context, routineID string, cost float64) error {
	if cost == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO lifecycle_spend (day, routine_id, cost_usd) VALUES (?, ?, ?)
		ON CONFLICT (day, routine_id) DO UPDATE SET cost_usd = cost_usd + excluded.cost_usd`,
		time.Now().UTC().Format("2006-01-02"), routineID, cost)
	return err
}

// LifecycleSpendToday is what the lifecycle has spent today, in total.
func (s *Store) LifecycleSpendToday(ctx context.Context) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM lifecycle_spend WHERE day = ?`,
		time.Now().UTC().Format("2006-01-02")).Scan(&total)
	return total, err
}

// LifecycleSpend is what the lifecycle has spent on one routine, all time.
func (s *Store) LifecycleSpend(ctx context.Context, routineID string) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM lifecycle_spend WHERE routine_id = ?`,
		routineID).Scan(&total)
	return total, err
}

// FindActiveByContract returns the other active routines that keep the same
// contract — the rivals archival compares a routine with.
func (s *Store) FindActiveByContract(ctx context.Context, contractHash, excluding string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM routines WHERE contract_hash = ? AND state = ? AND id <> ? ORDER BY id`,
		contractHash, RoutineActive, excluding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FindByContract returns any routine — archived, rejected or on trial — with a
// contract hash, so a new draft of the same promise restores it instead of
// making a duplicate.
func (s *Store) FindByContract(ctx context.Context, contractHash string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM routines WHERE contract_hash = ? ORDER BY created_at LIMIT 1`, contractHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// LastRunAt is when a routine last started a top-level run, zero if never.
func (s *Store) LastRunAt(ctx context.Context, routineID string) (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(started_at) FROM routine_runs WHERE routine_id = ? AND parent_run_id = 0 AND requested_by NOT LIKE 'trial:%'`,
		routineID).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(at.Int64), nil
}
