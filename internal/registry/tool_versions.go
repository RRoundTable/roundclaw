package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Tool version history.
//
// The same question agent_versions answers, asked of a tool: what was this when
// the agent used it, and can it be put back. Until spec/003 a tool had no history
// at all, so a rollback had nothing to restore and an evaluation naming "the same
// configuration" could not know whether the tools underneath had moved.
//
// A version records the definition *and* a digest of what the definition points
// at. Both are needed for the same reason an agent version needs its persona: the
// row is one half of the artefact and the thing at the end of its host_path is
// the other, and either can change while the other stands still.
//
// Rows have no foreign key to tools, matching agent_versions: deleting a tool
// must not take the record of what it was, which is exactly what somebody needs
// when they want it back.

const toolVersionSchema = `
CREATE TABLE IF NOT EXISTS tool_versions (
    tool_id    TEXT NOT NULL,
    version    INTEGER NOT NULL,
    definition TEXT NOT NULL,             -- the Tool struct as JSON
    digest     TEXT NOT NULL DEFAULT '',  -- witness over the declared identity; '' = none declared
    digest_err TEXT NOT NULL DEFAULT '',  -- why the identity could not be read, if it could not
    note       TEXT NOT NULL DEFAULT '',
    author     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (tool_id, version)
);
CREATE INDEX IF NOT EXISTS idx_tool_versions_recent ON tool_versions(tool_id, version DESC);
`

// ToolVersion is one snapshot of a tool.
type ToolVersion struct {
	ToolID     string `json:"tool_id"`
	Version    int    `json:"version"`
	Definition Tool   `json:"definition"`
	// Digest witnesses what the tool was made of. Empty means the tool declared
	// no identity, which is not the same as an identity that hashed to nothing —
	// DigestErr distinguishes "declared nothing" from "could not be read".
	Digest    string    `json:"digest,omitempty"`
	DigestErr string    `json:"digest_error,omitempty"`
	Note      string    `json:"note,omitempty"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ToolRollback reports what putting a tool back actually achieved.
//
// DigestMatches is the honest half. Restoring the definition always works — it is
// a row. Restoring what the definition points at is not in roundclaw's gift, so a
// rollback whose recorded digest no longer matches says the configuration came
// back and the content did not, rather than reporting success (adr/005).
type ToolRollback struct {
	Tool           Tool   `json:"tool"`
	Version        int    `json:"version"`
	From           int    `json:"from"`
	DigestMatches  bool   `json:"digest_matches"`
	RecordedDigest string `json:"recorded_digest,omitempty"`
	CurrentDigest  string `json:"current_digest,omitempty"`
	// DigestError says why the content could not be checked, when it could not.
	// Without it "did not match" and "could not be read" look identical to a
	// caller, and only one of those is worth going and looking at.
	DigestError string `json:"digest_error,omitempty"`
}

// snapshotToolTx writes the next version of t inside the caller's transaction,
// unless it would be identical to the current one.
//
// "Identical" covers the digest as well as the definition, which is the whole
// point: a tool whose row is untouched and whose directory changed underneath has
// changed, and that is the case a version recording only the pointer would miss.
func snapshotToolTx(ctx context.Context, tx *sql.Tx, t Tool, digest, digestErr string, c Change, now int64) error {
	def, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode definition of tool %s: %w", t.ID, err)
	}

	var prevDef, prevDigest string
	err = tx.QueryRowContext(ctx,
		`SELECT definition, digest FROM tool_versions WHERE tool_id = ? ORDER BY version DESC LIMIT 1`,
		t.ID).Scan(&prevDef, &prevDigest)
	switch {
	case err == nil:
		if prevDigest == digest && sameToolDefinition(prevDef, t) {
			return nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// First version; nothing to compare against.
	default:
		return fmt.Errorf("read current version of tool %s: %w", t.ID, err)
	}

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM tool_versions WHERE tool_id = ?`, t.ID).
		Scan(&next); err != nil {
		return fmt.Errorf("next version of tool %s: %w", t.ID, err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tool_versions (tool_id, version, definition, digest, digest_err, note, author, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, next, string(def), digest, digestErr, c.Note, c.Author, now)
	if err != nil {
		return fmt.Errorf("snapshot tool %s v%d: %w", t.ID, next, err)
	}
	return nil
}

// sameToolDefinition reports whether a stored definition describes the tool next
// describes. Timestamps are excluded because every write moves UpdatedAt, which
// would defeat the check entirely.
func sameToolDefinition(prevDef string, next Tool) bool {
	var prev Tool
	if err := json.Unmarshal([]byte(prevDef), &prev); err != nil {
		// An unreadable snapshot must not block a new one; that would freeze the
		// history at the corrupt row. Treat it as different.
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

// SnapshotTool records the tool's current definition and identity as a new
// version, without changing either.
//
// This is how a version is minted for a change the registry cannot see happening:
// somebody edited a file the tool declared, and the row never moved.
func (s *Store) SnapshotTool(ctx context.Context, id string, c Change) (ToolVersion, error) {
	t, err := s.GetTool(ctx, id)
	if err != nil {
		return ToolVersion{}, err
	}
	digest, digestErr := s.digestOf(t.Identity)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ToolVersion{}, fmt.Errorf("begin tool snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if err := snapshotToolTx(ctx, tx, t, digest, digestErr, c, time.Now().UnixMilli()); err != nil {
		return ToolVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return ToolVersion{}, fmt.Errorf("commit tool snapshot: %w", err)
	}
	return s.LatestToolVersion(ctx, id)
}

// ListToolVersions returns a tool's versions, newest first. limit <= 0 returns
// all of them.
func (s *Store) ListToolVersions(ctx context.Context, id string, limit int) ([]ToolVersion, error) {
	q := `SELECT tool_id, version, definition, digest, digest_err, note, author, created_at
	      FROM tool_versions WHERE tool_id = ? ORDER BY version DESC`
	args := []any{id}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list versions of tool %s: %w", id, err)
	}
	defer rows.Close()

	var out []ToolVersion
	for rows.Next() {
		v, err := scanToolVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetToolVersion returns one snapshot.
func (s *Store) GetToolVersion(ctx context.Context, id string, version int) (ToolVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tool_id, version, definition, digest, digest_err, note, author, created_at
		FROM tool_versions WHERE tool_id = ? AND version = ?`, id, version)
	v, err := scanToolVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolVersion{}, fmt.Errorf("%w: tool %s has no version %d", ErrNotFound, id, version)
	}
	return v, err
}

// LatestToolVersion returns the newest snapshot of a tool.
func (s *Store) LatestToolVersion(ctx context.Context, id string) (ToolVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tool_id, version, definition, digest, digest_err, note, author, created_at
		FROM tool_versions WHERE tool_id = ? ORDER BY version DESC LIMIT 1`, id)
	v, err := scanToolVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolVersion{}, fmt.Errorf("%w: tool %s has no versions", ErrNotFound, id)
	}
	return v, err
}

// RollbackTool restores a tool to an earlier version and reports whether that
// actually put it back.
//
// The restore is applied as a *new* version rather than by rewinding, so the
// change being undone stays on the record — the append-only rule agent rollback
// already follows.
func (s *Store) RollbackTool(ctx context.Context, id string, version int, c Change) (ToolRollback, error) {
	old, err := s.GetToolVersion(ctx, id, version)
	if err != nil {
		return ToolRollback{}, err
	}
	restored := old.Definition
	restored.ID = id

	// Read the identity as it is now, before the write, so the comparison is
	// against what is on the host rather than against what is about to be stored.
	current, currentErr := s.digestOf(restored.Identity)

	if _, err := s.PutTool(ctx, restored, c); err != nil {
		return ToolRollback{}, err
	}
	latest, err := s.LatestToolVersion(ctx, id)
	if err != nil {
		return ToolRollback{}, err
	}
	return ToolRollback{
		Tool:    latest.Definition,
		Version: latest.Version,
		From:    version,
		// A version that recorded no identity cannot claim its content came back.
		DigestMatches:  old.Digest != "" && old.Digest == current,
		RecordedDigest: old.Digest,
		CurrentDigest:  current,
		DigestError:    currentErr,
	}, nil
}

// BackfillToolVersions gives every tool with no history a version 1 recording
// what it is right now, and reports how many it minted.
//
// Without it a tool that existed before this table would look like it had never
// been registered, and the first ordinary edit would be recorded as version 1 —
// quietly claiming the original settings were that edit's doing.
func (s *Store) BackfillToolVersions(ctx context.Context) (int, error) {
	tools, err := s.ListTools(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tools {
		_, err := s.LatestToolVersion(ctx, t.ID)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return n, err
		}
		if _, err := s.SnapshotTool(ctx, t.ID, Change{
			Note:   "recorded as it stood when version history began",
			Author: "roundclaw",
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func scanToolVersion(row scanner) (ToolVersion, error) {
	var (
		v         ToolVersion
		def       string
		createdAt int64
	)
	if err := row.Scan(&v.ToolID, &v.Version, &def, &v.Digest, &v.DigestErr, &v.Note, &v.Author, &createdAt); err != nil {
		return ToolVersion{}, err
	}
	if err := json.Unmarshal([]byte(def), &v.Definition); err != nil {
		return ToolVersion{}, fmt.Errorf("decode definition of tool %s v%d: %w", v.ToolID, v.Version, err)
	}
	v.CreatedAt = time.UnixMilli(createdAt)
	return v, nil
}
