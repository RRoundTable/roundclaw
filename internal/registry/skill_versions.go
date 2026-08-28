package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Skill version history — tool_versions applied to the other grantable thing.
//
// Written beside it rather than abstracted over it, matching skills.go beside
// tools.go: two near-identical shapes are cheaper to read than one generic one
// parameterised over a kind that never branches (adr/005).

const skillVersionSchema = `
CREATE TABLE IF NOT EXISTS skill_versions (
    skill_id   TEXT NOT NULL,
    version    INTEGER NOT NULL,
    definition TEXT NOT NULL,             -- the Skill struct as JSON
    digest     TEXT NOT NULL DEFAULT '',  -- witness over the declared identity; '' = none declared
    digest_err TEXT NOT NULL DEFAULT '',  -- why the identity could not be read, if it could not
    note       TEXT NOT NULL DEFAULT '',
    author     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (skill_id, version)
);
CREATE INDEX IF NOT EXISTS idx_skill_versions_recent ON skill_versions(skill_id, version DESC);
`

// SkillVersion is one snapshot of a skill.
type SkillVersion struct {
	SkillID    string    `json:"skill_id"`
	Version    int       `json:"version"`
	Definition Skill     `json:"definition"`
	Digest     string    `json:"digest,omitempty"`
	DigestErr  string    `json:"digest_error,omitempty"`
	Note       string    `json:"note,omitempty"`
	Author     string    `json:"author,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// SkillRollback reports what putting a skill back actually achieved. See
// ToolRollback for why the digest half is reported separately.
type SkillRollback struct {
	Skill          Skill  `json:"skill"`
	Version        int    `json:"version"`
	From           int    `json:"from"`
	DigestMatches  bool   `json:"digest_matches"`
	RecordedDigest string `json:"recorded_digest,omitempty"`
	CurrentDigest  string `json:"current_digest,omitempty"`
	DigestError    string `json:"digest_error,omitempty"`
}

func snapshotSkillTx(ctx context.Context, tx *sql.Tx, sk Skill, digest, digestErr string, c Change, now int64) error {
	def, err := json.Marshal(sk)
	if err != nil {
		return fmt.Errorf("encode definition of skill %s: %w", sk.ID, err)
	}

	var prevDef, prevDigest string
	err = tx.QueryRowContext(ctx,
		`SELECT definition, digest FROM skill_versions WHERE skill_id = ? ORDER BY version DESC LIMIT 1`,
		sk.ID).Scan(&prevDef, &prevDigest)
	switch {
	case err == nil:
		if prevDigest == digest && sameSkillDefinition(prevDef, sk) {
			return nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// First version; nothing to compare against.
	default:
		return fmt.Errorf("read current version of skill %s: %w", sk.ID, err)
	}

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM skill_versions WHERE skill_id = ?`, sk.ID).
		Scan(&next); err != nil {
		return fmt.Errorf("next version of skill %s: %w", sk.ID, err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO skill_versions (skill_id, version, definition, digest, digest_err, note, author, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sk.ID, next, string(def), digest, digestErr, c.Note, c.Author, now)
	if err != nil {
		return fmt.Errorf("snapshot skill %s v%d: %w", sk.ID, next, err)
	}
	return nil
}

func sameSkillDefinition(prevDef string, next Skill) bool {
	var prev Skill
	if err := json.Unmarshal([]byte(prevDef), &prev); err != nil {
		return false
	}
	prev.CreatedAt, prev.UpdatedAt = time.Time{}, time.Time{}
	next.CreatedAt, next.UpdatedAt = time.Time{}, time.Time{}
	a, errA := json.Marshal(prev)
	b, errB := json.Marshal(next)
	if errA != nil || errB != nil {
		return false
	}
	return string(a) == string(b)
}

// SnapshotSkill records the skill's current definition and identity as a new
// version, without changing either.
func (s *Store) SnapshotSkill(ctx context.Context, id string, c Change) (SkillVersion, error) {
	sk, err := s.GetSkill(ctx, id)
	if err != nil {
		return SkillVersion{}, err
	}
	digest, digestErr := s.digestOf(sk.Identity)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("begin skill snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if err := snapshotSkillTx(ctx, tx, sk, digest, digestErr, c, time.Now().UnixMilli()); err != nil {
		return SkillVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return SkillVersion{}, fmt.Errorf("commit skill snapshot: %w", err)
	}
	return s.LatestSkillVersion(ctx, id)
}

// ListSkillVersions returns a skill's versions, newest first. limit <= 0 returns
// all of them.
func (s *Store) ListSkillVersions(ctx context.Context, id string, limit int) ([]SkillVersion, error) {
	q := `SELECT skill_id, version, definition, digest, digest_err, note, author, created_at
	      FROM skill_versions WHERE skill_id = ? ORDER BY version DESC`
	args := []any{id}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list versions of skill %s: %w", id, err)
	}
	defer rows.Close()

	var out []SkillVersion
	for rows.Next() {
		v, err := scanSkillVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetSkillVersion returns one snapshot.
func (s *Store) GetSkillVersion(ctx context.Context, id string, version int) (SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT skill_id, version, definition, digest, digest_err, note, author, created_at
		FROM skill_versions WHERE skill_id = ? AND version = ?`, id, version)
	v, err := scanSkillVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillVersion{}, fmt.Errorf("%w: skill %s has no version %d", ErrNotFound, id, version)
	}
	return v, err
}

// LatestSkillVersion returns the newest snapshot of a skill.
func (s *Store) LatestSkillVersion(ctx context.Context, id string) (SkillVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT skill_id, version, definition, digest, digest_err, note, author, created_at
		FROM skill_versions WHERE skill_id = ? ORDER BY version DESC LIMIT 1`, id)
	v, err := scanSkillVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillVersion{}, fmt.Errorf("%w: skill %s has no versions", ErrNotFound, id)
	}
	return v, err
}

// RollbackSkill restores a skill to an earlier version and reports whether that
// actually put it back. Applied as a new version, never as a rewind.
func (s *Store) RollbackSkill(ctx context.Context, id string, version int, c Change) (SkillRollback, error) {
	old, err := s.GetSkillVersion(ctx, id, version)
	if err != nil {
		return SkillRollback{}, err
	}
	restored := old.Definition
	restored.ID = id

	current, currentErr := s.digestOf(restored.Identity)

	if _, err := s.PutSkill(ctx, restored, c); err != nil {
		return SkillRollback{}, err
	}
	latest, err := s.LatestSkillVersion(ctx, id)
	if err != nil {
		return SkillRollback{}, err
	}
	return SkillRollback{
		Skill:          latest.Definition,
		Version:        latest.Version,
		From:           version,
		DigestMatches:  old.Digest != "" && old.Digest == current,
		RecordedDigest: old.Digest,
		CurrentDigest:  current,
		DigestError:    currentErr,
	}, nil
}

// BackfillSkillVersions gives every skill with no history a version 1 recording
// what it is right now, and reports how many it minted.
func (s *Store) BackfillSkillVersions(ctx context.Context) (int, error) {
	skills, err := s.ListSkills(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sk := range skills {
		_, err := s.LatestSkillVersion(ctx, sk.ID)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return n, err
		}
		if _, err := s.SnapshotSkill(ctx, sk.ID, Change{
			Note:   "recorded as it stood when version history began",
			Author: "roundclaw",
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func scanSkillVersion(row scanner) (SkillVersion, error) {
	var (
		v         SkillVersion
		def       string
		createdAt int64
	)
	if err := row.Scan(&v.SkillID, &v.Version, &def, &v.Digest, &v.DigestErr, &v.Note, &v.Author, &createdAt); err != nil {
		return SkillVersion{}, err
	}
	if err := json.Unmarshal([]byte(def), &v.Definition); err != nil {
		return SkillVersion{}, fmt.Errorf("decode definition of skill %s v%d: %w", v.SkillID, v.Version, err)
	}
	v.CreatedAt = time.UnixMilli(createdAt)
	return v, nil
}
