package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// Skills an agent can be granted, by name.
//
// A skill is a Claude Code skill directory — a SKILL.md and whatever it needs —
// living on the host. Granting it to an agent mounts that directory read-only
// into the agent's ~/.claude/skills/<id>, where the CLI discovers it natively.
// Unlike a tool it carries no env and no injected prompt: a SKILL.md is
// self-describing, so roundclaw only has to put it where `claude` will find it.
//
// Registering a skill names a host path (sensitive), so it is an operator act,
// the same boundary tools draw; granting one to an agent only references a
// registered id.

const skillSchema = `
CREATE TABLE IF NOT EXISTS skills (
    id          TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    host_path   TEXT NOT NULL,          -- dir with SKILL.md; mounted at ~/.claude/skills/<id>
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
`

// Skill is a named Claude Code skill an agent can be granted.
type Skill struct {
	ID          string    `json:"id"`
	Description string    `json:"description,omitempty"`
	HostPath    string    `json:"host_path"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Validate checks a skill before it is written.
func (s Skill) Validate() error {
	if err := core.ValidateAgentID(s.ID); err != nil {
		return fmt.Errorf("skill id: %w", err)
	}
	if s.HostPath == "" || s.HostPath[0] != '/' {
		return fmt.Errorf("skill %s: host_path must be an absolute path", s.ID)
	}
	return nil
}

// PutSkill creates or replaces a skill.
func (s *Store) PutSkill(ctx context.Context, sk Skill) (Skill, error) {
	if err := sk.Validate(); err != nil {
		return Skill{}, err
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO skills (id, description, host_path, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			description = excluded.description, host_path = excluded.host_path,
			updated_at = excluded.updated_at`,
		sk.ID, sk.Description, sk.HostPath, now, now)
	if err != nil {
		return Skill{}, fmt.Errorf("store skill %s: %w", sk.ID, err)
	}
	return s.GetSkill(ctx, sk.ID)
}

// GetSkill returns one skill.
func (s *Store) GetSkill(ctx context.Context, id string) (Skill, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, description, host_path, created_at, updated_at FROM skills WHERE id = ?`, id)
	sk, err := scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Skill{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return sk, err
}

// ListSkills returns every skill, ordered by ID.
func (s *Store) ListSkills(ctx context.Context) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, description, host_path, created_at, updated_at FROM skills ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// DeleteSkill removes a skill.
func (s *Store) DeleteSkill(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete skill %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func scanSkill(row scanner) (Skill, error) {
	var (
		sk                   Skill
		createdAt, updatedAt int64
	)
	if err := row.Scan(&sk.ID, &sk.Description, &sk.HostPath, &createdAt, &updatedAt); err != nil {
		return Skill{}, err
	}
	sk.CreatedAt = time.UnixMilli(createdAt)
	sk.UpdatedAt = time.UnixMilli(updatedAt)
	return sk, nil
}
