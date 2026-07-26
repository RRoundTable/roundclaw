package registry

import (
	"errors"
	"testing"
)

// personaSpy is a settable persona source, standing in for the file the gateway
// reads out of an agent's workspace.
type personaSpy struct{ text string }

func (p *personaSpy) source(string) string { return p.text }

func TestCreateRecordsVersionOne(t *testing.T) {
	s := newStore(t)
	persona := &personaSpy{text: "you are a reviewer"}
	s.UsePersonaSource(persona.source)

	if _, err := s.Create(t.Context(), Agent{ID: "dev", Description: "writes code", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}

	v, err := s.LatestVersion(t.Context(), "dev")
	if err != nil {
		t.Fatalf("latest version: %v", err)
	}
	if v.Version != 1 {
		t.Errorf("version = %d, want 1", v.Version)
	}
	if v.Definition.Description != "writes code" {
		t.Errorf("definition = %+v", v.Definition)
	}
	// The persona is half of what makes an agent behave as it does, so a snapshot
	// without it would not be a snapshot of anything useful.
	if v.Persona != "you are a reviewer" {
		t.Errorf("persona = %q, want the workspace CLAUDE.md", v.Persona)
	}
}

func TestUpdateRecordsANewVersion(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Description: "before", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Update(t.Context(), Agent{ID: "dev", Description: "after", Enabled: true},
		Change{Note: "clearer description", Author: "agent:curator"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	versions, err := s.ListVersions(t.Context(), "dev", 0)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	// Newest first, so the caller sees the change it just made without paging.
	if versions[0].Version != 2 || versions[0].Definition.Description != "after" {
		t.Errorf("newest = v%d %q", versions[0].Version, versions[0].Definition.Description)
	}
	if versions[0].Note != "clearer description" || versions[0].Author != "agent:curator" {
		t.Errorf("change metadata = %q by %q", versions[0].Note, versions[0].Author)
	}
	if versions[1].Definition.Description != "before" {
		t.Errorf("oldest = %q, want the original", versions[1].Definition.Description)
	}
}

// A history where most rows record nothing is one nobody reads. Writing back an
// unchanged definition — an enable/disable round trip, a client PUTting what it
// just read — must not mint a version.
func TestUnchangedWriteRecordsNoVersion(t *testing.T) {
	s := newStore(t)
	a := Agent{ID: "dev", Description: "same", AllowedTools: []string{"Read"}, Enabled: true}
	if _, err := s.Create(t.Context(), a); err != nil {
		t.Fatalf("create: %v", err)
	}
	for range 3 {
		if _, err := s.Update(t.Context(), a); err != nil {
			t.Fatalf("update: %v", err)
		}
	}

	versions, err := s.ListVersions(t.Context(), "dev", 0)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1 — identical writes are not changes", len(versions))
	}
}

// The persona never passes through Update, so a persona-only edit has to mint a
// version by itself or the most consequential change an agent can receive would
// leave no trace.
func TestPersonaChangeAloneRecordsAVersion(t *testing.T) {
	s := newStore(t)
	persona := &personaSpy{text: "v1 instructions"}
	s.UsePersonaSource(persona.source)

	if _, err := s.Create(t.Context(), Agent{ID: "dev", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	persona.text = "v2 instructions, now in Korean"
	if _, err := s.Snapshot(t.Context(), "dev", Change{Note: "persona edited"}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	v, err := s.LatestVersion(t.Context(), "dev")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if v.Version != 2 || v.Persona != "v2 instructions, now in Korean" {
		t.Errorf("latest = v%d persona %q", v.Version, v.Persona)
	}

	// And a snapshot taken when nothing moved stays at the same version.
	if _, err := s.Snapshot(t.Context(), "dev", Change{Note: "nothing happened"}); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if v, _ := s.LatestVersion(t.Context(), "dev"); v.Version != 2 {
		t.Errorf("version after a no-op snapshot = %d, want 2", v.Version)
	}
}

// Deleting an agent keeps its workspace and session so that recreating the ID
// resumes it. The history has to survive on the same terms — losing it would
// destroy the record of what the agent was exactly when someone needs to put it
// back.
func TestHistorySurvivesDeleteAndNumberingContinues(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Description: "original", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(t.Context(), "dev"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	versions, err := s.ListVersions(t.Context(), "dev", 0)
	if err != nil {
		t.Fatalf("list versions after delete: %v", err)
	}
	if len(versions) != 1 || versions[0].Definition.Description != "original" {
		t.Fatalf("versions after delete = %+v, want the pre-delete history", versions)
	}

	// Recreating continues the numbering rather than restarting it, so v1 keeps
	// meaning the first thing this ID ever was.
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Description: "recreated", Enabled: true}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if v, _ := s.LatestVersion(t.Context(), "dev"); v.Version != 2 {
		t.Errorf("version after recreate = %d, want 2", v.Version)
	}
}

func TestBackfillGivesExistingAgentsAFirstVersion(t *testing.T) {
	s := newStore(t)
	// Simulate a registry written before version history existed: insert the
	// agent rows directly, so no snapshot is taken.
	for _, id := range []string{"alpha", "beta"} {
		if _, err := s.db.ExecContext(t.Context(),
			`INSERT INTO agents (id, description, created_at, updated_at) VALUES (?, ?, 0, 0)`,
			id, "pre-existing"); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	n, err := s.BackfillVersions(t.Context())
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled = %d, want 2", n)
	}
	for _, id := range []string{"alpha", "beta"} {
		v, err := s.LatestVersion(t.Context(), id)
		if err != nil {
			t.Fatalf("latest of %s: %v", id, err)
		}
		if v.Version != 1 || v.Definition.Description != "pre-existing" {
			t.Errorf("%s = v%d %q", id, v.Version, v.Definition.Description)
		}
	}

	// Running it twice must not mint a second round: it is a startup step, and
	// startup happens often.
	if n, err := s.BackfillVersions(t.Context()); err != nil || n != 0 {
		t.Errorf("second backfill = %d, %v; want 0, nil", n, err)
	}
}

func TestGetVersionReportsNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.GetVersion(t.Context(), "dev", 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetVersion(9) error = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestVersion(t.Context(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestVersion(ghost) error = %v, want ErrNotFound", err)
	}
}
