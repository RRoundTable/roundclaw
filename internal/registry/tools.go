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

// Tools an agent can be granted, by name.
//
// A tool bundles what it takes to use a local capability: a host directory to
// mount (a CLI, its config), the environment variables it needs, and a note on
// how to drive it. Registering a tool is an operator act — it names a host path,
// which is sensitive. Attaching one to an agent is then safe to do in natural
// language, because admin can only pick from tools already registered, never
// mount an arbitrary path.

const toolSchema = `
CREATE TABLE IF NOT EXISTS tools (
    id           TEXT PRIMARY KEY,
    description  TEXT NOT NULL DEFAULT '',
    host_path    TEXT NOT NULL,          -- mounted read-only at /mnt/<base>
    env          TEXT NOT NULL DEFAULT '{}',  -- JSON map of env vars to inject
    instructions TEXT NOT NULL DEFAULT '',    -- how the agent should use it
    identity     TEXT NOT NULL DEFAULT '[]',  -- JSON list of members it is made of
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
`

// Tool is a named local capability an agent can be granted.
type Tool struct {
	ID           string            `json:"id"`
	Description  string            `json:"description,omitempty"`
	HostPath     string            `json:"host_path"`
	Env          map[string]string `json:"env,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	// Identity is what this tool is made of, for the purpose of noticing that it
	// changed. Empty means it declares nothing and therefore has no version —
	// host_path is not read as a default, because a mount is the tool for a
	// bundle of scripts and a client's configuration for a server (see
	// identity.go).
	Identity  []IdentityMember `json:"identity,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// Validate checks a tool before it is written.
func (t Tool) Validate() error {
	if err := core.ValidateAgentID(t.ID); err != nil {
		return fmt.Errorf("tool id: %w", err)
	}
	if t.HostPath == "" || t.HostPath[0] != '/' {
		return fmt.Errorf("tool %s: host_path must be an absolute path", t.ID)
	}
	for _, m := range t.Identity {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("tool %s: %w", t.ID, err)
		}
	}
	return nil
}

// PutTool creates or replaces a tool, recording a version of it.
//
// The write and its snapshot share one transaction for the reason agent versions
// do (see snapshotTx): a version that could be lost in between would leave a hole
// in the history, and a hole reads as "nothing changed then".
func (s *Store) PutTool(ctx context.Context, t Tool, c ...Change) (Tool, error) {
	if err := t.Validate(); err != nil {
		return Tool{}, err
	}
	env, err := json.Marshal(t.Env)
	if err != nil {
		return Tool{}, fmt.Errorf("encode tool env: %w", err)
	}
	identity, err := json.Marshal(t.Identity)
	if err != nil {
		return Tool{}, fmt.Errorf("encode tool identity: %w", err)
	}
	digest, digestErr := s.digestOf(t.Identity)
	now := time.Now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tool{}, fmt.Errorf("begin tool write: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tools (id, description, host_path, env, instructions, identity, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			description = excluded.description, host_path = excluded.host_path,
			env = excluded.env, instructions = excluded.instructions,
			identity = excluded.identity, updated_at = excluded.updated_at`,
		t.ID, t.Description, t.HostPath, string(env), t.Instructions, string(identity), now, now)
	if err != nil {
		return Tool{}, fmt.Errorf("store tool %s: %w", t.ID, err)
	}
	if err := snapshotToolTx(ctx, tx, t, digest, digestErr, firstChange(c), now); err != nil {
		return Tool{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tool{}, fmt.Errorf("commit tool %s: %w", t.ID, err)
	}
	return s.GetTool(ctx, t.ID)
}

// GetTool returns one tool.
func (s *Store) GetTool(ctx context.Context, id string) (Tool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, description, host_path, env, instructions, identity, created_at, updated_at FROM tools WHERE id = ?`, id)
	t, err := scanTool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Tool{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return t, err
}

// ListTools returns every tool, ordered by ID.
func (s *Store) ListTools(ctx context.Context) ([]Tool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, description, host_path, env, instructions, identity, created_at, updated_at FROM tools ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	defer rows.Close()
	var out []Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTool removes a tool.
func (s *Store) DeleteTool(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tools WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete tool %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func scanTool(row scanner) (Tool, error) {
	var (
		t                    Tool
		env, identity        string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&t.ID, &t.Description, &t.HostPath, &env, &t.Instructions, &identity, &createdAt, &updatedAt); err != nil {
		return Tool{}, err
	}
	if err := json.Unmarshal([]byte(env), &t.Env); err != nil {
		return Tool{}, fmt.Errorf("decode env of tool %s: %w", t.ID, err)
	}
	if err := json.Unmarshal([]byte(identity), &t.Identity); err != nil {
		return Tool{}, fmt.Errorf("decode identity of tool %s: %w", t.ID, err)
	}
	t.CreatedAt = time.UnixMilli(createdAt)
	t.UpdatedAt = time.UnixMilli(updatedAt)
	return t, nil
}
