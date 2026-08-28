package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Agent version history.
//
// A definition row answers "what is this agent now". This answers "what was it
// before, and what changed" — which is the question anyone asks the moment an
// agent starts behaving differently, and the one an eval comparison is built on:
// comparing two versions requires both to still exist.
//
// A version captures the definition *and* the persona together, because they are
// one artefact in practice. An agent whose CLAUDE.md was rewritten is a different
// agent even though every column is unchanged, so recording only the columns
// would make the most common kind of change invisible.
//
// Rows deliberately have no foreign key to agents. Deleting an agent keeps its
// workspace and session so that recreating the ID resumes it (see Delete); the
// history has to survive on the same terms, or an accidental delete would take
// the only record of what the agent used to be — exactly when it is needed.

const versionSchema = `
CREATE TABLE IF NOT EXISTS agent_versions (
    agent_id   TEXT NOT NULL,
    version    INTEGER NOT NULL,
    definition TEXT NOT NULL,             -- the Agent struct as JSON
    persona    TEXT NOT NULL DEFAULT '',  -- the workspace CLAUDE.md at that moment
    grants     TEXT NOT NULL DEFAULT '{}',-- tool and skill versions held at that moment
    note       TEXT NOT NULL DEFAULT '',  -- why it changed, if the caller said
    author     TEXT NOT NULL DEFAULT '',  -- who changed it, if the caller said
    created_at INTEGER NOT NULL,
    PRIMARY KEY (agent_id, version)
);
CREATE INDEX IF NOT EXISTS idx_agent_versions_recent ON agent_versions(agent_id, version DESC);
`

// AgentVersion is one snapshot of an agent.
type AgentVersion struct {
	AgentID string `json:"agent_id"`
	// Version counts from 1 and never repeats for an agent, including across a
	// delete and recreate: numbering continues from the history that was kept.
	Version    int    `json:"version"`
	Definition Agent  `json:"definition"`
	Persona    string `json:"persona,omitempty"`
	// Grants names which version of each tool and skill the agent held. Without
	// it a version describes a configuration that never existed, for the same
	// reason a definition without its persona does — and two evaluations "of the
	// same configuration" would silently measure different things when a tool
	// moved underneath them.
	Grants    Grants    `json:"grants,omitempty"`
	Note      string    `json:"note,omitempty"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Grants is the tool and skill versions an agent held at one moment.
//
// A granted id missing from these maps is one that resolved to nothing when the
// snapshot was taken — a tool deleted out from under the grant. Recording the
// absence is the point: it says the agent was configured to use something that
// was not there, which is exactly what a turn would have skipped over silently.
type Grants struct {
	Tools  map[string]int `json:"tools,omitempty"`
	Skills map[string]int `json:"skills,omitempty"`
}

// Change is the optional metadata recorded with a write: why, and by whom.
//
// It is passed variadically to Create and Update so that the many callers with
// nothing to say — tests, the seeder, a Discord modal — stay unchanged, while
// the ones that do know (an API caller naming itself, an agent explaining a
// proposal it applied) can say it. Empty fields simply record nothing.
type Change struct {
	Note   string
	Author string
}

func firstChange(c []Change) Change {
	if len(c) == 0 {
		return Change{}
	}
	return c[0]
}

// PersonaSource reads an agent's persona so a snapshot can capture it next to
// the definition. It is injected because the persona is a file in the agent's
// workspace, which the registry does not own and must not learn the layout of.
//
// Unset, snapshots record an empty persona rather than failing: a registry with
// no workspace in reach — a test, a tool that only edits definitions — should
// still keep version history.
type PersonaSource func(agentID string) string

// UsePersonaSource installs the reader used by every subsequent snapshot.
func (s *Store) UsePersonaSource(fn PersonaSource) { s.persona = fn }

// PersonaFromWorkspace builds a PersonaSource that reads CLAUDE.md out of the
// directory workDir names for an agent — i.e. config.WorkDir. A missing file is
// an empty persona, which is what an agent that has never been given one has.
func PersonaFromWorkspace(workDir func(agentID string) string) PersonaSource {
	return func(agentID string) string {
		data, err := os.ReadFile(filepath.Join(workDir(agentID), "CLAUDE.md"))
		if err != nil {
			return ""
		}
		return string(data)
	}
}

func (s *Store) personaOf(agentID string) string {
	if s.persona == nil {
		return ""
	}
	return s.persona(agentID)
}

// snapshotTx writes the next version of a, inside the caller's transaction,
// unless it would be identical to the current one.
//
// Being in the same transaction as the definition write is the point: a version
// that could be lost between the write and its snapshot would leave a history
// with a hole in it, and a hole is worse than no history at all — it reads as
// "nothing changed then".
//
// The identical-content check keeps the history readable. Toggling an agent off
// and on again, or a client PUTting back what it just read, would otherwise mint
// versions that record nothing, and a list where most rows are noise is one
// nobody scrolls through to find the row that matters.
func snapshotTx(ctx context.Context, tx *sql.Tx, a Agent, persona string, c Change, now int64) error {
	def, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode definition of %s: %w", a.ID, err)
	}
	grants, err := grantsTx(ctx, tx, a)
	if err != nil {
		return err
	}
	grantsJSON, err := json.Marshal(grants)
	if err != nil {
		return fmt.Errorf("encode grants of %s: %w", a.ID, err)
	}

	var (
		prevDef     string
		prevPersona string
		prevGrants  string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT definition, persona, grants FROM agent_versions WHERE agent_id = ? ORDER BY version DESC LIMIT 1`,
		a.ID).Scan(&prevDef, &prevPersona, &prevGrants)
	switch {
	case err == nil:
		same, cmpErr := sameContent(prevDef, prevPersona, a, persona)
		if cmpErr != nil {
			return cmpErr
		}
		// The grants comparison is what catches a tool that changed while the
		// agent's own row stood still: same definition, same persona, different
		// tool version. That is a different configuration and gets its own row.
		if same && prevGrants == string(grantsJSON) {
			return nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// First version; nothing to compare against.
	default:
		return fmt.Errorf("read current version of %s: %w", a.ID, err)
	}

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM agent_versions WHERE agent_id = ?`, a.ID).
		Scan(&next); err != nil {
		return fmt.Errorf("next version of %s: %w", a.ID, err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agent_versions (agent_id, version, definition, persona, grants, note, author, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, next, string(def), persona, string(grantsJSON), c.Note, c.Author, now)
	if err != nil {
		return fmt.Errorf("snapshot %s v%d: %w", a.ID, next, err)
	}
	return nil
}

// grantsTx resolves an agent's granted tool and skill ids to the versions those
// grants pointed at, inside the caller's transaction so the answer is the one
// that was true when the definition was written.
//
// A grant naming something with no version — deleted, or registered before
// history existed — is left out rather than recorded as zero. Absent says "there
// was nothing to pin"; a zero would read as a real version.
func grantsTx(ctx context.Context, tx *sql.Tx, a Agent) (Grants, error) {
	g := Grants{}
	for _, id := range a.Tools {
		var v int
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM tool_versions WHERE tool_id = ?`, id).Scan(&v)
		if err != nil {
			return Grants{}, fmt.Errorf("read tool version for grant %s of %s: %w", id, a.ID, err)
		}
		if v > 0 {
			if g.Tools == nil {
				g.Tools = map[string]int{}
			}
			g.Tools[id] = v
		}
	}
	for _, id := range a.Skills {
		var v int
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM skill_versions WHERE skill_id = ?`, id).Scan(&v)
		if err != nil {
			return Grants{}, fmt.Errorf("read skill version for grant %s of %s: %w", id, a.ID, err)
		}
		if v > 0 {
			if g.Skills == nil {
				g.Skills = map[string]int{}
			}
			g.Skills[id] = v
		}
	}
	return g, nil
}

// sameContent reports whether a stored snapshot and a proposed one describe the
// same agent.
//
// Timestamps are excluded because every write moves UpdatedAt, which would make
// every snapshot differ from the last and defeat the check entirely. What is
// being compared is the configuration, not when it was saved.
func sameContent(prevDef, prevPersona string, next Agent, nextPersona string) (bool, error) {
	if prevPersona != nextPersona {
		return false, nil
	}
	var prev Agent
	if err := json.Unmarshal([]byte(prevDef), &prev); err != nil {
		// An unreadable snapshot must not block a new one — that would freeze the
		// history at the corrupt row. Treat it as different and move on.
		return false, nil //nolint:nilerr // deliberate: prefer recording over failing
	}
	prev.CreatedAt, prev.UpdatedAt = time.Time{}, time.Time{}
	next.CreatedAt, next.UpdatedAt = time.Time{}, time.Time{}

	a, err := json.Marshal(prev)
	if err != nil {
		return false, fmt.Errorf("compare snapshot of %s: %w", next.ID, err)
	}
	b, err := json.Marshal(next)
	if err != nil {
		return false, fmt.Errorf("compare snapshot of %s: %w", next.ID, err)
	}
	return string(a) == string(b), nil
}

// Snapshot records the agent's current definition and persona as a new version,
// without changing either.
//
// The persona is a file rather than a column, so the write that changes it never
// reaches Create or Update — the persona endpoint calls this instead. It is also
// how a version is minted for something the registry cannot see changing at all,
// such as a granted tool's contents.
func (s *Store) Snapshot(ctx context.Context, agentID string, c Change) (AgentVersion, error) {
	a, err := s.Get(ctx, agentID)
	if err != nil {
		return AgentVersion{}, err
	}
	persona := s.personaOf(agentID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentVersion{}, fmt.Errorf("begin snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if err := snapshotTx(ctx, tx, a, persona, c, time.Now().UnixMilli()); err != nil {
		return AgentVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentVersion{}, fmt.Errorf("commit snapshot: %w", err)
	}
	return s.LatestVersion(ctx, agentID)
}

// ListVersions returns an agent's versions, newest first. limit <= 0 returns all
// of them.
func (s *Store) ListVersions(ctx context.Context, agentID string, limit int) ([]AgentVersion, error) {
	q := `SELECT agent_id, version, definition, persona, grants, note, author, created_at
	      FROM agent_versions WHERE agent_id = ? ORDER BY version DESC`
	args := []any{agentID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list versions of %s: %w", agentID, err)
	}
	defer rows.Close()

	var out []AgentVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion returns one snapshot.
func (s *Store) GetVersion(ctx context.Context, agentID string, version int) (AgentVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT agent_id, version, definition, persona, grants, note, author, created_at
		FROM agent_versions WHERE agent_id = ? AND version = ?`, agentID, version)
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentVersion{}, fmt.Errorf("%w: %s has no version %d", ErrNotFound, agentID, version)
	}
	return v, err
}

// LatestVersion returns the newest snapshot of an agent.
func (s *Store) LatestVersion(ctx context.Context, agentID string) (AgentVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT agent_id, version, definition, persona, grants, note, author, created_at
		FROM agent_versions WHERE agent_id = ? ORDER BY version DESC LIMIT 1`, agentID)
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentVersion{}, fmt.Errorf("%w: %s has no versions", ErrNotFound, agentID)
	}
	return v, err
}

// BackfillVersions gives every agent that has no history a version 1 recording
// what it is right now, and reports how many it minted.
//
// Without it, every agent that existed before this table did would look like it
// had never been configured — and the first ordinary edit would be recorded as
// version 1, quietly claiming the original settings were that edit's doing.
// Call it at startup, after UsePersonaSource, so the backfilled row carries the
// persona too.
func (s *Store) BackfillVersions(ctx context.Context) (int, error) {
	agents, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range agents {
		_, err := s.LatestVersion(ctx, a.ID)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return n, err
		}
		if _, err := s.Snapshot(ctx, a.ID, Change{
			Note:   "recorded as it stood when version history began",
			Author: "roundclaw",
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func scanVersion(row scanner) (AgentVersion, error) {
	var (
		v           AgentVersion
		def, grants string
		createdAt   int64
	)
	if err := row.Scan(&v.AgentID, &v.Version, &def, &v.Persona, &grants, &v.Note, &v.Author, &createdAt); err != nil {
		return AgentVersion{}, err
	}
	if err := json.Unmarshal([]byte(def), &v.Definition); err != nil {
		return AgentVersion{}, fmt.Errorf("decode definition of %s v%d: %w", v.AgentID, v.Version, err)
	}
	if err := json.Unmarshal([]byte(grants), &v.Grants); err != nil {
		return AgentVersion{}, fmt.Errorf("decode grants of %s v%d: %w", v.AgentID, v.Version, err)
	}
	v.CreatedAt = time.UnixMilli(createdAt)
	return v, nil
}
