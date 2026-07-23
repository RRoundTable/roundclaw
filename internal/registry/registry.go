// Package registry stores the definitions roundclaw can change without a
// restart: today agents, later schedules and pipelines.
//
// It is deliberately separate from the per-agent databases in internal/store.
// Those hold one agent's turns and transcript; this holds the answer to "which
// agents exist at all", which every process needs before it can pick one.
//
// Nothing here is read from workflow code. Definitions are consulted by the
// adapters at admission and by the activity when a turn starts, both of which
// may be non-deterministic. A workflow that re-read a definition mid-execution
// would break replay the moment somebody edited it.
package registry

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
)

var (
	// ErrNotFound is returned when no agent has the given ID.
	ErrNotFound = errors.New("agent not found")
	// ErrConflict is returned when an ID or a channel binding is already taken.
	ErrConflict = errors.New("conflict")
)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
    id              TEXT PRIMARY KEY,
    description     TEXT NOT NULL DEFAULT '',
    agent_name      TEXT NOT NULL DEFAULT '',
    permission_mode TEXT NOT NULL DEFAULT '',
    allowed_tools   TEXT NOT NULL DEFAULT '[]',
    additional_dirs TEXT NOT NULL DEFAULT '[]',
    work_dir        TEXT NOT NULL DEFAULT '',
    deny_paths      TEXT NOT NULL DEFAULT '[]',
    require_mention INTEGER NOT NULL DEFAULT 0,
    share_workspace INTEGER NOT NULL DEFAULT 0,
    reply_in_thread INTEGER NOT NULL DEFAULT 0,
    tools           TEXT NOT NULL DEFAULT '[]',
    skills          TEXT NOT NULL DEFAULT '[]',
    image           TEXT NOT NULL DEFAULT '',
    group_add       TEXT NOT NULL DEFAULT '[]',
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

-- channel_id is the primary key, so one channel can never be bound to two
-- agents. Enforcing it here rather than in a config check means it holds even
-- when two processes write concurrently.
CREATE TABLE IF NOT EXISTS agent_channels (
    channel_id TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_agent_channels_agent ON agent_channels(agent_id);
`

// migrations add columns to a database created by an older build.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// new column in the schema above is invisible to any deployment that has run
// before — every query then fails with "no such column" and the process
// crash-loops. Each statement here is applied on open and its "duplicate
// column" error ignored, which makes running them repeatedly a no-op.
//
// Only ever append. Rewriting or reordering these would diverge fresh
// databases from migrated ones.
var migrations = []string{
	`ALTER TABLE agents ADD COLUMN work_dir TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN deny_paths TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE agents ADD COLUMN require_mention INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE agents ADD COLUMN share_workspace INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE agents ADD COLUMN reply_in_thread INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE agents ADD COLUMN tools TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE agents ADD COLUMN skills TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE agents ADD COLUMN image TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE agents ADD COLUMN group_add TEXT NOT NULL DEFAULT '[]'`,
}

// Agent is a runtime agent definition.
type Agent struct {
	ID             string   `json:"id"`
	Description    string   `json:"description"`
	AgentName      string   `json:"agent_name"`
	PermissionMode string   `json:"permission_mode"`
	AllowedTools   []string `json:"allowed_tools"`
	AdditionalDirs []string `json:"additional_dirs"`
	// WorkDir points the agent's /workspace at an existing host directory,
	// read-write, instead of the empty one roundclaw manages. Empty keeps the
	// managed directory, which is the safe default.
	WorkDir string `json:"work_dir"`
	// DenyPaths are shadowed with /dev/null inside the workspace. Relative to
	// the workspace root.
	DenyPaths []string `json:"deny_paths"`
	// RequireMention makes the agent answer only messages that @-mention the
	// bot, in the channels bound to it.
	//
	// Without it a bound channel treats every message as a request, which is
	// only tolerable in a channel nobody else talks in. Any shared channel needs
	// this, or the bot answers other people's conversations — and bills for it.
	RequireMention bool `json:"require_mention"`
	// ShareWorkspace lets parallel conversations use the same directory when it
	// cannot be isolated — a work_dir that is not a git repository. Off by
	// default: silent conflicts surface as one conversation mysteriously
	// undoing another's work, which is a miserable thing to debug.
	ShareWorkspace bool `json:"share_workspace"`
	// ReplyInThread makes a message in a plain channel spawn a Discord thread,
	// with the agent's reply and the rest of that exchange living in the thread.
	// Each thread is its own conversation (session + workspace), so distinct
	// requests stay separated instead of interleaving in one channel.
	ReplyInThread bool `json:"reply_in_thread"`
	// Tools names the registered tools this agent is granted (see registry.Tool).
	// Each resolves at turn time to a read-only mount plus the env it needs, so an
	// agent gains a local capability — an Outline CLI, say — without any arbitrary
	// host path being named here: only IDs of tools an operator already registered.
	Tools []string `json:"tools"`
	// Skills names the registered Claude Code skills this agent is granted (see
	// registry.Skill). Each resolves at turn time to a read-only mount into the
	// agent's ~/.claude/skills, where the CLI discovers it. Like tools, only IDs
	// of skills an operator already registered.
	Skills []string `json:"skills"`
	// Image overrides the container image this agent runs in, instead of the
	// global container.image every agent shares by default. Empty keeps that
	// default. It lets one agent run a purpose-built image — a dev agent with the
	// docker CLI, say — without changing the image the rest of the fleet uses.
	//
	// The image must already exist on the host: the worker starts it by name and
	// builds nothing. This is the same class of host reach an operator already
	// grants through WorkDir, so it is a full-token (not delegate) operation.
	Image string `json:"image"`
	// GroupAdd lists supplementary groups (by GID or name) to add to the agent's
	// container process, emitted as docker `--group-add`. Empty adds none.
	//
	// The one case that needs it: a tool mounts a host socket owned by a group
	// the container's user is not in — /var/run/docker.sock, owned by the host's
	// docker group — so the mount alone is refused until the process joins that
	// group. Granting it is deliberately weighty: docker.sock plus its group is
	// effective host root inside that container, and the agent is a
	// prompt-injection target, so this belongs on a single trusted agent, never
	// the fleet.
	GroupAdd        []string  `json:"group_add"`
	DiscordChannels []string  `json:"discord_channels"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Validate checks an agent definition before it is written.
func (a Agent) Validate() error {
	if err := core.ValidateAgentID(a.ID); err != nil {
		return err
	}
	for _, d := range a.AdditionalDirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("additional dir %q must be absolute", d)
		}
	}
	if a.WorkDir != "" && !filepath.IsAbs(a.WorkDir) {
		return fmt.Errorf("work dir %q must be absolute", a.WorkDir)
	}
	for _, d := range a.DenyPaths {
		if err := claude.ValidateDenyPath(d); err != nil {
			return err
		}
	}
	// The image and each group become docker argv tokens directly, so a value
	// with whitespace could not be one argument — reject it here rather than
	// build a command that means something other than it reads.
	if strings.TrimSpace(a.Image) != a.Image || strings.ContainsAny(a.Image, " \t\n") {
		return fmt.Errorf("image %q must not contain whitespace", a.Image)
	}
	for _, g := range a.GroupAdd {
		if g == "" || strings.ContainsAny(g, " \t\n") {
			return fmt.Errorf("group_add entry %q must be a non-empty gid or name with no whitespace", g)
		}
	}
	return nil
}

// Label renders an agent for a picker: its ID plus a short description.
func (a Agent) Label() string {
	if a.Description == "" {
		return a.ID
	}
	label := a.ID + " — " + a.Description
	// Discord rejects choice names longer than 100 characters.
	if len(label) > 100 {
		label = label[:97] + "..."
	}
	return label
}

// Store is the registry database.
type Store struct {
	db *sql.DB
	// aead encrypts and decrypts secret values. Nil until UseSecretKey is
	// called, and secret operations fail closed while it is.
	aead cipher.AEAD
}

// Open opens (and creates) the registry at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create registry dir: %w", err)
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open registry %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := applySchema(db, schema+scheduleSchema+secretSchema+workflowSchema+toolSchema+skillSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply registry schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate registry: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

// List returns every agent, ordered by ID so pickers are stable.
func (s *Store) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, description, agent_name, permission_mode,
		       allowed_tools, additional_dirs, work_dir, deny_paths, require_mention,
		       share_workspace, reply_in_thread, tools, skills, image, group_add, enabled, created_at, updated_at
		FROM agents ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].DiscordChannels, err = s.channelsOf(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Get returns one agent.
func (s *Store) Get(ctx context.Context, id string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, description, agent_name, permission_mode,
		       allowed_tools, additional_dirs, work_dir, deny_paths, require_mention,
		       share_workspace, reply_in_thread, tools, skills, image, group_add, enabled, created_at, updated_at
		FROM agents WHERE id = ?`, id)

	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Agent{}, err
	}
	if a.DiscordChannels, err = s.channelsOf(ctx, id); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// ByChannel returns the agent bound to a Discord channel.
func (s *Store) ByChannel(ctx context.Context, channelID string) (Agent, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id FROM agent_channels WHERE channel_id = ?`, channelID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w: channel %s", ErrNotFound, channelID)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("lookup channel %s: %w", channelID, err)
	}
	return s.Get(ctx, id)
}

// Create inserts a new agent.
func (s *Store) Create(ctx context.Context, a Agent) (Agent, error) {
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, fmt.Errorf("begin create agent: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE id = ?`, a.ID).Scan(&exists); err != nil {
		return Agent{}, fmt.Errorf("check agent %s: %w", a.ID, err)
	}
	if exists > 0 {
		return Agent{}, fmt.Errorf("%w: agent %s already exists", ErrConflict, a.ID)
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agents (id, description, agent_name, permission_mode,
		                    allowed_tools, additional_dirs, work_dir, deny_paths,
		                    require_mention, share_workspace, reply_in_thread, tools, skills, image, group_add, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Description, a.AgentName, a.PermissionMode,
		encodeList(a.AllowedTools), encodeList(a.AdditionalDirs),
		a.WorkDir, encodeList(a.DenyPaths), boolToInt(a.RequireMention),
		boolToInt(a.ShareWorkspace), boolToInt(a.ReplyInThread), encodeList(a.Tools), encodeList(a.Skills), a.Image, encodeList(a.GroupAdd), boolToInt(a.Enabled), now, now,
	); err != nil {
		return Agent{}, fmt.Errorf("insert agent %s: %w", a.ID, err)
	}
	if err := replaceChannels(ctx, tx, a.ID, a.DiscordChannels); err != nil {
		return Agent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, fmt.Errorf("commit create agent: %w", err)
	}
	return s.Get(ctx, a.ID)
}

// Update replaces an existing agent's definition.
//
// The change takes effect on the agent's next turn, not the one in flight: the
// running turn already has its arguments. That is the behaviour to want —
// rewriting a turn's tools halfway through would be far stranger.
func (s *Store) Update(ctx context.Context, a Agent) (Agent, error) {
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, fmt.Errorf("begin update agent: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	res, err := tx.ExecContext(ctx, `
		UPDATE agents SET description = ?, agent_name = ?, permission_mode = ?,
		       allowed_tools = ?, additional_dirs = ?, work_dir = ?, deny_paths = ?,
		       require_mention = ?, share_workspace = ?, reply_in_thread = ?, tools = ?, skills = ?, image = ?, group_add = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		a.Description, a.AgentName, a.PermissionMode,
		encodeList(a.AllowedTools), encodeList(a.AdditionalDirs),
		a.WorkDir, encodeList(a.DenyPaths), boolToInt(a.RequireMention),
		boolToInt(a.ShareWorkspace), boolToInt(a.ReplyInThread), encodeList(a.Tools), encodeList(a.Skills), a.Image, encodeList(a.GroupAdd), boolToInt(a.Enabled), time.Now().UnixMilli(), a.ID)
	if err != nil {
		return Agent{}, fmt.Errorf("update agent %s: %w", a.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Agent{}, fmt.Errorf("%w: %s", ErrNotFound, a.ID)
	}
	if err := replaceChannels(ctx, tx, a.ID, a.DiscordChannels); err != nil {
		return Agent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, fmt.Errorf("commit update agent: %w", err)
	}
	return s.Get(ctx, a.ID)
}

// Delete removes an agent's definition and its channel bindings.
//
// It does not touch the agent's workspace, database or Claude session. Those
// are the expensive, irreplaceable parts, and an accidental delete should be
// recoverable by recreating the agent with the same ID. Reclaiming that disk is
// a separate, explicit step.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete agent %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// Seed populates an empty registry from the config file's agent list.
//
// It runs only when the registry has no agents at all, so it is a one-time
// bootstrap rather than a sync: once agents live in the registry, editing the
// YAML has no effect and the registry is the only source of truth. Reporting
// how many were seeded lets the caller say so loudly at startup, because a
// silent "your YAML is now ignored" would be a nasty surprise.
func (s *Store) Seed(ctx context.Context, agents []Agent) (int, error) {
	existing, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	if len(existing) > 0 {
		return 0, nil
	}
	for _, a := range agents {
		a.Enabled = true
		if _, err := s.Create(ctx, a); err != nil {
			return 0, fmt.Errorf("seed agent %s: %w", a.ID, err)
		}
	}
	return len(agents), nil
}

func (s *Store) channelsOf(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel_id FROM agent_channels WHERE agent_id = ? ORDER BY channel_id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list channels of %s: %w", agentID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

func replaceChannels(ctx context.Context, tx *sql.Tx, agentID string, channels []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_channels WHERE agent_id = ?`, agentID); err != nil {
		return fmt.Errorf("clear channels of %s: %w", agentID, err)
	}
	for _, ch := range channels {
		ch = strings.TrimSpace(ch)
		if ch == "" {
			continue
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO agent_channels (channel_id, agent_id) VALUES (?, ?)`, ch, agentID)
		if err != nil {
			// The primary key on channel_id is what surfaces this, so a
			// double binding cannot slip through a concurrent write.
			return fmt.Errorf("%w: channel %s is already bound to another agent", ErrConflict, ch)
		}
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanAgent(row scanner) (Agent, error) {
	var (
		a                     Agent
		allowed, dirs, deny   string
		tools, skills         string
		groupAdd              string
		mention, share, reply int
		enabled               int
		createdAt, updatedAt  int64
	)
	if err := row.Scan(&a.ID, &a.Description, &a.AgentName, &a.PermissionMode,
		&allowed, &dirs, &a.WorkDir, &deny, &mention, &share, &reply, &tools, &skills, &a.Image, &groupAdd, &enabled,
		&createdAt, &updatedAt); err != nil {
		return Agent{}, err
	}
	a.RequireMention = mention != 0
	a.ShareWorkspace = share != 0
	a.ReplyInThread = reply != 0
	a.AllowedTools = decodeList(allowed)
	a.AdditionalDirs = decodeList(dirs)
	a.DenyPaths = decodeList(deny)
	a.Tools = decodeList(tools)
	a.Skills = decodeList(skills)
	a.GroupAdd = decodeList(groupAdd)
	a.Enabled = enabled != 0
	a.CreatedAt = time.UnixMilli(createdAt)
	a.UpdatedAt = time.UnixMilli(updatedAt)
	return a, nil
}

func encodeList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeList(s string) []string {
	var v []string
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// applySchema creates the tables, retrying while the database is locked.
//
// The worker and the gateway both open this file at startup and both apply the
// schema. Switching a fresh database into WAL mode needs a brief exclusive
// lock, and busy_timeout does not cover that transition — so without a retry,
// whichever process loses the cold-start race simply fails to boot.
func applySchema(db *sql.DB, ddl string) error {
	const attempts = 20
	var err error
	for range attempts {
		if _, err = db.Exec(ddl); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "database is locked") {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// migrate applies each migration, treating "already applied" as success.
//
// SQLite has no ADD COLUMN IF NOT EXISTS, so the duplicate-column error is the
// signal that a migration has already run. Any other error is real and stops
// startup, because continuing with a half-migrated database would corrupt data
// rather than merely fail.
func migrate(db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}
