package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/roundtable/roundclaw/internal/core"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), "test-agent", ReadWrite)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateTurnIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	origin := core.HTTPPollOrigin()

	first, existed, err := s.CreateTurn(ctx, "do the thing", origin, "key-1")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if existed {
		t.Fatal("first create reported existed=true")
	}

	// A client retry must land on the same turn, not start a second agent run.
	second, existed, err := s.CreateTurn(ctx, "do the thing", origin, "key-1")
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if !existed {
		t.Error("retry reported existed=false")
	}
	if second != first {
		t.Errorf("retry produced turn %d, want %d", second, first)
	}

	turns, err := s.RecentTurns(ctx, 10)
	if err != nil {
		t.Fatalf("recent turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want exactly 1", len(turns))
	}
}

func TestCreateTurnIdempotencyUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const racers = 8
	ids := make([]int64, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := s.CreateTurn(ctx, "concurrent", core.HTTPPollOrigin(), "same-key")
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()

	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("racer %d got turn %d, racer 0 got %d: idempotency is not atomic", i, id, ids[0])
		}
	}
	turns, err := s.RecentTurns(ctx, 10)
	if err != nil {
		t.Fatalf("recent turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want exactly 1", len(turns))
	}
}

func TestFinishTurnKeepsFirstTerminalOutcome(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, _, err := s.CreateTurn(ctx, "req", core.DiscordOrigin("chan-1", "msg-1"), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.FinishTurn(ctx, id, core.TurnResult{Status: core.TurnStopped, Text: "stopped by user"}); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	// A retried delivery must not resurrect a stopped turn as done.
	if err := s.FinishTurn(ctx, id, core.TurnResult{Status: core.TurnDone, Text: "late success"}); err != nil {
		t.Fatalf("second finish: %v", err)
	}

	turn, err := s.GetTurn(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if turn.Status != core.TurnStopped {
		t.Errorf("status = %q, want %q", turn.Status, core.TurnStopped)
	}
	if turn.Result != "stopped by user" {
		t.Errorf("result = %q, want the first outcome", turn.Result)
	}
}

func TestOriginRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := core.HTTPCallbackOrigin("https://example.test/hook", "default")
	id, _, err := s.CreateTurn(ctx, "req", want, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	turn, err := s.GetTurn(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if turn.Origin != want {
		t.Errorf("origin = %+v, want %+v", turn.Origin, want)
	}
}

// A reader in a separate connection must see committed rows while the writer
// keeps working. This is the /status path: gateway reads, worker writes.
func TestConcurrentReaderSeesWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	ctx := context.Background()

	writer, err := Open(path, "test-agent", ReadWrite)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close()

	turnID, _, err := writer.CreateTurn(ctx, "req", core.HTTPPollOrigin(), "")
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	reader, err := Open(path, "test-agent", ReadOnly)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()

	for i := range 50 {
		if err := writer.AppendLog(ctx, turnID, core.LogText, "chunk"); err != nil {
			t.Fatalf("append log %d: %v", i, err)
		}
	}

	logs, err := reader.LogsAfter(ctx, turnID, 0, 100)
	if err != nil {
		t.Fatalf("reader logs: %v", err)
	}
	if len(logs) != 50 {
		t.Fatalf("reader saw %d logs, want 50", len(logs))
	}

	tail, err := reader.TailLogs(ctx, turnID, 5)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(tail) != 5 {
		t.Fatalf("tail returned %d, want 5", len(tail))
	}
	// TailLogs must return oldest-first within the tail window.
	if tail[0].ID >= tail[len(tail)-1].ID {
		t.Errorf("tail is not ascending: first=%d last=%d", tail[0].ID, tail[len(tail)-1].ID)
	}
}

func TestGetRuntimeDefaultsToIdle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rt, err := s.GetRuntime(ctx)
	if err != nil {
		t.Fatalf("get runtime on empty db: %v", err)
	}
	if rt.Status != core.AgentIdle {
		t.Errorf("status = %q, want %q", rt.Status, core.AgentIdle)
	}

	if err := s.SetRuntime(ctx, core.AgentRunning, 7, "session-uuid"); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	rt, err = s.GetRuntime(ctx)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.Status != core.AgentRunning || rt.CurrentTurn != 7 || rt.SessionID != "session-uuid" {
		t.Errorf("runtime = %+v, want running/7/session-uuid", rt)
	}
}
