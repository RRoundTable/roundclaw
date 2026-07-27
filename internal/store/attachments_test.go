package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/roundtable/roundclaw/internal/core"
)

// Uploads are staged at admission and placed by the worker, which is a different
// process. The row is the only thing the two share, so the paths have to survive
// it intact — a lost path is a file the agent is told about and never gets.
func TestAttachmentsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := []string{
		"/srv/ws/pm/inbox-staging/ab12-report.pdf",
		"/srv/ws/pm/inbox-staging/ab12-보고서.csv",
	}
	id, _, err := s.CreateTurn(ctx, NewTurn{
		Request: "review these", Origin: core.HTTPPollOrigin(), Attachments: want,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	got, err := s.TurnAttachments(ctx, id)
	if err != nil {
		t.Fatalf("TurnAttachments: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d attachments, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attachment %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Most turns carry no uploads, and that path must not need special handling
// downstream: placement asks every turn and expects an empty answer, not an
// error and not a null.
func TestAttachmentsDefaultToNone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, _, err := s.CreateTurn(ctx, NewTurn{Request: "hi", Origin: core.HTTPPollOrigin()})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	got, err := s.TurnAttachments(ctx, id)
	if err != nil {
		t.Fatalf("TurnAttachments: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a turn with no uploads reported %v", got)
	}
}

// Every agent already has a database, so the column arrives by migration rather
// than by CREATE TABLE. Their existing turns must keep reading — the worker asks
// this of every turn it runs, including ones queued by the previous build.
func TestAttachmentsMigrateOntoAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	// A database as the previous build left it: turns without the column.
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	_, err = old.Exec(`
		CREATE TABLE turns (
		    id           INTEGER PRIMARY KEY AUTOINCREMENT,
		    request      TEXT NOT NULL,
		    result       TEXT,
		    status       TEXT NOT NULL,
		    cost_usd     REAL,
		    origin       TEXT NOT NULL,
		    error        TEXT,
		    conversation TEXT NOT NULL DEFAULT '',
		    queued_at    INTEGER NOT NULL,
		    finished_at  INTEGER
		);
		INSERT INTO turns (request, status, origin, queued_at)
		VALUES ('older request', 'running', '{"type":"http_poll"}', 0);`)
	if err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(path, "pm", ReadWrite)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer s.Close()

	got, err := s.TurnAttachments(ctx, 1)
	if err != nil {
		t.Fatalf("reading a pre-migration turn failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a pre-migration turn reported uploads: %v", got)
	}

	// And the column is live for new turns.
	id, _, err := s.CreateTurn(ctx, NewTurn{
		Request: "new", Origin: core.HTTPPollOrigin(),
		Attachments: []string{"/staging/ab12-x.pdf"},
	})
	if err != nil {
		t.Fatalf("create turn after migration: %v", err)
	}
	if got, err := s.TurnAttachments(ctx, id); err != nil || len(got) != 1 {
		t.Errorf("TurnAttachments after migration = %v, %v; want one path", got, err)
	}
}
