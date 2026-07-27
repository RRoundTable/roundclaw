// Package store owns the per-agent SQLite file.
//
// Two separate processes write to it. The worker appends live logs and closes
// out turns; the gateway inserts the turn row before signalling, because HTTP
// has to return a turn_id in its 202 and a Temporal workflow cannot touch
// SQLite without breaking determinism. The gateway also reads the same file to
// answer /status and GET /v1/turns/{id}.
//
// That cross-process read/write mix is why WAL and a busy timeout are
// mandatory here rather than tuning: SQLite serialises the writers itself, and
// busy_timeout is what turns a collision into a short wait instead of an error.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, so the binary stays static
)

const schema = `
CREATE TABLE IF NOT EXISTS agent_runtime (
    agent_id     TEXT PRIMARY KEY,
    status       TEXT NOT NULL,
    current_turn INTEGER NOT NULL DEFAULT 0,
    session_id   TEXT,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS live_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id    INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_live_logs_turn ON live_logs(turn_id, id);

CREATE TABLE IF NOT EXISTS turns (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    request      TEXT NOT NULL,
    result       TEXT,
    status       TEXT NOT NULL,
    cost_usd     REAL,
    origin       TEXT NOT NULL,
    error        TEXT,
    conversation TEXT NOT NULL DEFAULT '',
    attachments  TEXT NOT NULL DEFAULT '[]',
    queued_at    INTEGER NOT NULL,
    finished_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_turns_status ON turns(status, id);
CREATE INDEX IF NOT EXISTS idx_turns_conversation ON turns(conversation, id);

CREATE TABLE IF NOT EXISTS idempotency (
    key        TEXT PRIMARY KEY,
    turn_id    INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
`

// migrations add columns to a database created by an older build. Only ever
// append; see the registry package for why.
var migrations = []string{
	`ALTER TABLE turns ADD COLUMN conversation TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE turns ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]'`,
}

// Store is one agent's database handle.
type Store struct {
	db      *sql.DB
	agentID string
}

// Mode selects the connection pool shape. A writer is capped at a single
// connection because SQLite serialises writes anyway and extra connections just
// convert that serialisation into SQLITE_BUSY errors.
type Mode int

const (
	// ReadWrite is used by both the worker and the gateway, and applies the
	// schema on open.
	ReadWrite Mode = iota
	// ReadOnly is for processes that only report state. It never writes and
	// never applies the schema, so it will not create a half-formed database
	// for an agent that has not run yet.
	ReadOnly
)

// Open opens (and creates if needed) an agent's database. The parent directory
// is created too, so a brand-new agent needs no separate provisioning step.
func Open(path, agentID string, mode Mode) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create agent dir for %s: %w", agentID, err)
	}

	// busy_timeout is what makes the gateway and worker coexist: a reader that
	// arrives mid-checkpoint waits instead of failing the request.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}

	if mode == ReadWrite {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db %s: %w", path, err)
	}

	s := &Store{db: db, agentID: agentID}
	if mode == ReadWrite {
		if err := applySchema(db, schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema to %s: %w", path, err)
		}
		if err := migrate(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate %s: %w", path, err)
		}
	}
	return s, nil
}

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

// AgentID reports which agent this store belongs to.
func (s *Store) AgentID() string { return s.agentID }

// Registry caches one Store per agent. Both binaries hold agent databases open
// for their whole lifetime; reopening per request would lose the WAL page cache
// and make /status measurably slower.
type Registry struct {
	mu     sync.Mutex
	mode   Mode
	pathFn func(agentID string) string
	stores map[string]*Store
}

// NewRegistry builds a registry. pathFn maps an agent ID to its DB path —
// normally config.DBPath.
func NewRegistry(mode Mode, pathFn func(agentID string) string) *Registry {
	return &Registry{mode: mode, pathFn: pathFn, stores: map[string]*Store{}}
}

// Get returns the agent's store, opening it on first use.
func (r *Registry) Get(agentID string) (*Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.stores[agentID]; ok {
		return s, nil
	}
	s, err := Open(r.pathFn(agentID), agentID, r.mode)
	if err != nil {
		return nil, err
	}
	r.stores[agentID] = s
	return s, nil
}

// Close releases every cached store.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	for id, s := range r.stores {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close store %s: %w", id, err)
		}
		delete(r.stores, id)
	}
	return firstErr
}

// applySchema creates the tables, retrying while the database is locked.
//
// The worker and the gateway both open these files at startup and both apply the
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
// SQLite has no ADD COLUMN IF NOT EXISTS, so the duplicate-column error is the
// signal that a migration has already run.
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
