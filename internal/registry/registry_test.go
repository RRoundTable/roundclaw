package registry

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateGetRoundTrip(t *testing.T) {
	s := newStore(t)

	want := Agent{
		ID:              "pr-reviewer",
		Description:     "Reviews pull requests",
		AgentName:       "reviewer",
		PermissionMode:  "acceptEdits",
		AllowedTools:    []string{"Read", "Grep"},
		AdditionalDirs:  []string{"/srv/docs"},
		DiscordChannels: []string{"chan-1", "chan-2"},
		Enabled:         true,
	}
	if _, err := s.Create(t.Context(), want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.Get(t.Context(), "pr-reviewer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Description != want.Description || got.AgentName != want.AgentName ||
		got.PermissionMode != want.PermissionMode || !got.Enabled {
		t.Errorf("agent = %+v", got)
	}
	if len(got.AllowedTools) != 2 || got.AllowedTools[0] != "Read" {
		t.Errorf("allowed tools = %v", got.AllowedTools)
	}
	if len(got.DiscordChannels) != 2 {
		t.Errorf("channels = %v", got.DiscordChannels)
	}
}

// One channel must never map to two agents, or a reply would land in the wrong
// agent's conversation. This is a primary key rather than an in-memory check
// precisely so it survives concurrent writers.
func TestChannelCannotBeBoundTwice(t *testing.T) {
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{ID: "first", DiscordChannels: []string{"shared"}}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	_, err := s.Create(t.Context(), Agent{ID: "second", DiscordChannels: []string{"shared"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second create err = %v, want ErrConflict", err)
	}

	// The failed create must not have left a half-written agent behind.
	if _, err := s.Get(t.Context(), "second"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rolled-back agent is still present: %v", err)
	}
}

func TestConcurrentCreatesCannotShareAChannel(t *testing.T) {
	s := newStore(t)

	const racers = 6
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Create(t.Context(), Agent{
				ID:              "agent-" + string(rune('a'+i)),
				DiscordChannels: []string{"contested"},
			})
		}()
	}
	wg.Wait()

	var winners int
	for _, err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d creates succeeded on the same channel, want exactly 1", winners)
	}
}

func TestByChannel(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "owner", DiscordChannels: []string{"chan-1"}}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.ByChannel(t.Context(), "chan-1")
	if err != nil {
		t.Fatalf("by channel: %v", err)
	}
	if got.ID != "owner" {
		t.Errorf("agent = %q, want owner", got.ID)
	}
	if _, err := s.ByChannel(t.Context(), "unbound"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unbound channel err = %v, want ErrNotFound", err)
	}
}

func TestUpdateReplacesChannels(t *testing.T) {
	s := newStore(t)
	created, err := s.Create(t.Context(), Agent{ID: "mover", DiscordChannels: []string{"old"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	created.DiscordChannels = []string{"new"}
	created.Description = "moved"
	if _, err := s.Update(t.Context(), created); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, err := s.ByChannel(t.Context(), "old"); !errors.Is(err, ErrNotFound) {
		t.Error("the old channel binding survived the update")
	}
	got, err := s.ByChannel(t.Context(), "new")
	if err != nil || got.Description != "moved" {
		t.Errorf("new binding = %+v, err = %v", got, err)
	}
}

func TestDeleteRemovesAgentAndChannels(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "doomed", DiscordChannels: []string{"chan-1"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(t.Context(), "doomed"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := s.Get(t.Context(), "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("agent survived delete: %v", err)
	}
	// The cascade matters: a stale binding would silently route messages to a
	// deleted agent.
	if _, err := s.ByChannel(t.Context(), "chan-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("channel binding survived delete: %v", err)
	}
	// The channel must now be free for another agent.
	if _, err := s.Create(t.Context(), Agent{ID: "successor", DiscordChannels: []string{"chan-1"}}); err != nil {
		t.Errorf("channel was not released: %v", err)
	}
}

func TestDeleteUnknownIsNotFound(t *testing.T) {
	if err := newStore(t).Delete(t.Context(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Seed is a one-time bootstrap, not a sync. Running it against a non-empty
// registry must not resurrect an agent someone deliberately deleted.
func TestSeedOnlyRunsOnAnEmptyRegistry(t *testing.T) {
	s := newStore(t)
	seed := []Agent{{ID: "from-config"}, {ID: "also-from-config"}}

	n, err := s.Seed(t.Context(), seed)
	if err != nil || n != 2 {
		t.Fatalf("first seed: n = %d, err = %v", n, err)
	}
	if err := s.Delete(t.Context(), "from-config"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	n, err = s.Seed(t.Context(), seed)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if n != 0 {
		t.Errorf("second seed created %d agents; it must be a no-op", n)
	}
	if _, err := s.Get(t.Context(), "from-config"); !errors.Is(err, ErrNotFound) {
		t.Error("a deleted agent was resurrected by re-seeding")
	}
}

func TestValidateRejectsUnsafeIDsAndRelativeDirs(t *testing.T) {
	s := newStore(t)

	for _, id := range []string{"", "../escape", "has space", "has/slash"} {
		if _, err := s.Create(t.Context(), Agent{ID: id}); err == nil {
			t.Errorf("agent id %q was accepted", id)
		}
	}
	if _, err := s.Create(t.Context(), Agent{ID: "ok", AdditionalDirs: []string{"relative/path"}}); err == nil {
		t.Error("a relative additional dir was accepted")
	}
}

func TestListIsOrderedByID(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"charlie", "alpha", "bravo"} {
		if _, err := s.Create(t.Context(), Agent{ID: id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	agents, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, a := range agents {
		if a.ID != want[i] {
			t.Fatalf("list = %v, want %v", agents, want)
		}
	}
}
