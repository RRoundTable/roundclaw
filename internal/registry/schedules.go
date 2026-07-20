package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// Recurring work.
//
// The definition lives here; Temporal owns the trigger. Splitting them that way
// keeps the prompt editable without touching the schedule, and means a fired
// schedule reads the current definition rather than a snapshot taken when it
// was created.
//
// A fired schedule joins the agent's normal queue. Scheduled work and human
// requests therefore share one session, one ordering rule and one set of spend
// limits — there is no second execution path to keep in step.

const scheduleSchema = `
CREATE TABLE IF NOT EXISTS schedules (
    id           TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    description  TEXT NOT NULL DEFAULT '',
    cron         TEXT NOT NULL,
    timezone     TEXT NOT NULL DEFAULT 'UTC',
    prompt       TEXT NOT NULL,
    -- Channel to report into. Empty means the result is recorded but not
    -- delivered anywhere, which is useful for jobs whose only output is a side
    -- effect.
    channel_id   TEXT NOT NULL DEFAULT '',
    -- A result containing this substring is not delivered. Daily jobs that
    -- often have nothing to say would otherwise post "nothing to report" every
    -- morning, and people stop reading a channel that does that.
    suppress_if  TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_schedules_agent ON schedules(agent_id);
`

// Schedule is one recurring request.
type Schedule struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	Description string    `json:"description"`
	Cron        string    `json:"cron"`
	Timezone    string    `json:"timezone"`
	Prompt      string    `json:"prompt"`
	ChannelID   string    `json:"channel_id,omitempty"`
	SuppressIf  string    `json:"suppress_if,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Validate checks a schedule before it is written.
func (s Schedule) Validate() error {
	if err := core.ValidateAgentID(s.ID); err != nil {
		return fmt.Errorf("schedule id: %w", err)
	}
	if err := core.ValidateAgentID(s.AgentID); err != nil {
		return fmt.Errorf("agent id: %w", err)
	}
	if strings.TrimSpace(s.Cron) == "" {
		return fmt.Errorf("cron expression is required")
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	// Validated here rather than at fire time: a bad zone would otherwise mean
	// the schedule silently runs on UTC, hours from when anyone expected.
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("unknown timezone %q", s.Timezone)
		}
	}
	return nil
}

// ListSchedules returns every schedule, ordered by ID.
func (st *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := st.db.QueryContext(ctx, `
		SELECT id, agent_id, description, cron, timezone, prompt,
		       channel_id, suppress_if, enabled, created_at, updated_at
		FROM schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSchedule returns one schedule.
func (st *Store) GetSchedule(ctx context.Context, id string) (Schedule, error) {
	row := st.db.QueryRowContext(ctx, `
		SELECT id, agent_id, description, cron, timezone, prompt,
		       channel_id, suppress_if, enabled, created_at, updated_at
		FROM schedules WHERE id = ?`, id)

	s, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	return s, err
}

// PutSchedule creates or replaces a schedule definition.
func (st *Store) PutSchedule(ctx context.Context, s Schedule) (Schedule, error) {
	if err := s.Validate(); err != nil {
		return Schedule{}, err
	}
	// The foreign key would catch this too, but its error names a constraint
	// rather than the agent, which is not much use to whoever typed it.
	if _, err := st.Get(ctx, s.AgentID); err != nil {
		return Schedule{}, err
	}
	if s.Timezone == "" {
		s.Timezone = "UTC"
	}

	now := time.Now().UnixMilli()
	_, err := st.db.ExecContext(ctx, `
		INSERT INTO schedules (id, agent_id, description, cron, timezone, prompt,
		                       channel_id, suppress_if, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			agent_id = excluded.agent_id, description = excluded.description,
			cron = excluded.cron, timezone = excluded.timezone,
			prompt = excluded.prompt, channel_id = excluded.channel_id,
			suppress_if = excluded.suppress_if, enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		s.ID, s.AgentID, s.Description, s.Cron, s.Timezone, s.Prompt,
		s.ChannelID, s.SuppressIf, boolToInt(s.Enabled), now, now)
	if err != nil {
		return Schedule{}, fmt.Errorf("save schedule %s: %w", s.ID, err)
	}
	return st.GetSchedule(ctx, s.ID)
}

// DeleteSchedule removes a schedule definition.
func (st *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := st.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete schedule %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	return nil
}

func scanSchedule(row scanner) (Schedule, error) {
	var (
		s                    Schedule
		enabled              int
		createdAt, updatedAt int64
	)
	err := row.Scan(&s.ID, &s.AgentID, &s.Description, &s.Cron, &s.Timezone,
		&s.Prompt, &s.ChannelID, &s.SuppressIf, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return Schedule{}, err
	}
	s.Enabled = enabled != 0
	s.CreatedAt = time.UnixMilli(createdAt)
	s.UpdatedAt = time.UnixMilli(updatedAt)
	return s, nil
}
