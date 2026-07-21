package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// Workflows: agent-less, multi-step automations.
//
// Unlike an agent — which is a persistent, conversational executor with a
// session, a queue and channel bindings — a workflow is a self-contained job.
// It carries its own steps and runs on a trigger (a schedule, or manually),
// executing each step in order and passing one step's output to the next. There
// is no agent to create first: the workflow is the unit of work.
//
// The definition lives here; Temporal runs it (RunWorkflow). Reading the
// definition at run time, not baking it into the trigger, means editing a step
// takes effect on the next run.

const workflowSchema = `
CREATE TABLE IF NOT EXISTS workflows (
    id          TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    -- Channel the final result is posted to. Empty records the run but delivers
    -- nowhere, which suits a workflow whose steps are their own side effect.
    channel_id  TEXT NOT NULL DEFAULT '',
    steps       TEXT NOT NULL DEFAULT '[]',   -- JSON array of WorkflowStep
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
`

// WorkflowStep is one stage of a workflow: a prompt run under its own execution
// settings. A later step receives the earlier steps' outputs, so a step can
// build on what the ones before it produced.
type WorkflowStep struct {
	Name           string   `json:"name"`
	Prompt         string   `json:"prompt"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	AllowedTools   []string `json:"allowed_tools,omitempty"`
	// Model overrides the CLI default for this step — e.g. a cheap model for a
	// mechanical step, a stronger one for analysis. Empty uses the default.
	Model string `json:"model,omitempty"`
}

// Workflow is an agent-less, multi-step automation.
type Workflow struct {
	ID          string         `json:"id"`
	Description string         `json:"description,omitempty"`
	ChannelID   string         `json:"channel_id,omitempty"`
	Steps       []WorkflowStep `json:"steps"`
	Enabled     bool           `json:"enabled"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Validate checks a workflow before it is written.
func (w Workflow) Validate() error {
	if err := core.ValidateAgentID(w.ID); err != nil {
		return fmt.Errorf("workflow id: %w", err)
	}
	if len(w.Steps) == 0 {
		return fmt.Errorf("a workflow needs at least one step")
	}
	for i, s := range w.Steps {
		if s.Prompt == "" {
			return fmt.Errorf("step %d (%q) has no prompt", i+1, s.Name)
		}
	}
	return nil
}

// PutWorkflow creates or replaces a workflow definition.
func (s *Store) PutWorkflow(ctx context.Context, w Workflow) (Workflow, error) {
	if err := w.Validate(); err != nil {
		return Workflow{}, err
	}
	steps, err := json.Marshal(w.Steps)
	if err != nil {
		return Workflow{}, fmt.Errorf("encode steps: %w", err)
	}
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workflows (id, description, channel_id, steps, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			description = excluded.description,
			channel_id  = excluded.channel_id,
			steps       = excluded.steps,
			enabled     = excluded.enabled,
			updated_at  = excluded.updated_at`,
		w.ID, w.Description, w.ChannelID, string(steps), boolToInt(w.Enabled), now, now)
	if err != nil {
		return Workflow{}, fmt.Errorf("store workflow %s: %w", w.ID, err)
	}
	return s.GetWorkflow(ctx, w.ID)
}

// GetWorkflow returns one workflow definition.
func (s *Store) GetWorkflow(ctx context.Context, id string) (Workflow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, description, channel_id, steps, enabled, created_at, updated_at FROM workflows WHERE id = ?`, id)
	w, err := scanWorkflow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return w, err
}

// ListWorkflows returns every workflow, ordered by ID.
func (s *Store) ListWorkflows(ctx context.Context) ([]Workflow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, description, channel_id, steps, enabled, created_at, updated_at FROM workflows ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var out []Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWorkflow removes a workflow definition.
func (s *Store) DeleteWorkflow(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete workflow %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func scanWorkflow(row scanner) (Workflow, error) {
	var (
		w                    Workflow
		steps                string
		enabled              int
		createdAt, updatedAt int64
	)
	if err := row.Scan(&w.ID, &w.Description, &w.ChannelID, &steps, &enabled, &createdAt, &updatedAt); err != nil {
		return Workflow{}, err
	}
	if err := json.Unmarshal([]byte(steps), &w.Steps); err != nil {
		return Workflow{}, fmt.Errorf("decode steps of %s: %w", w.ID, err)
	}
	w.Enabled = enabled != 0
	w.CreatedAt = time.UnixMilli(createdAt)
	w.UpdatedAt = time.UnixMilli(updatedAt)
	return w, nil
}
