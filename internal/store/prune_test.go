package store

import (
	"testing"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// finishAt closes a turn and backdates it, since FinishTurn stamps "now".
func finishAt(t *testing.T, s *Store, turnID int64, when time.Time) {
	t.Helper()
	if err := s.FinishTurn(t.Context(), turnID, core.TurnResult{
		TurnID: turnID, Status: core.TurnDone, Text: "ok",
	}); err != nil {
		t.Fatalf("finish turn: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE turns SET finished_at = ? WHERE id = ?`, when.UnixMilli(), turnID); err != nil {
		t.Fatalf("backdate turn: %v", err)
	}
}

func TestPruneRemovesOldTranscriptsButKeepsTurns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	old, _, _ := s.CreateTurn(t.Context(), "old", core.HTTPPollOrigin(), "")
	recent, _, _ := s.CreateTurn(t.Context(), "recent", core.HTTPPollOrigin(), "")
	for _, id := range []int64{old, recent} {
		for range 3 {
			if err := s.AppendLog(t.Context(), id, core.LogText, "chunk"); err != nil {
				t.Fatalf("append log: %v", err)
			}
		}
	}
	finishAt(t, s, old, now.AddDate(0, 0, -30))
	finishAt(t, s, recent, now.AddDate(0, 0, -1))

	// Transcripts kept 7 days, turns kept 90 — the turn row is the small,
	// durable audit trail; its transcript is the bulky part.
	p, err := s.Prune(t.Context(), now.AddDate(0, 0, -7), now.AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if p.Logs != 3 {
		t.Errorf("pruned %d transcript rows, want 3", p.Logs)
	}
	if p.Turns != 0 {
		t.Errorf("pruned %d turns, want 0 — turns outlive their transcripts", p.Turns)
	}

	if logs, _ := s.LogsAfter(t.Context(), old, 0, 10); len(logs) != 0 {
		t.Errorf("the old turn kept %d transcript rows", len(logs))
	}
	if logs, _ := s.LogsAfter(t.Context(), recent, 0, 10); len(logs) != 3 {
		t.Errorf("the recent turn lost transcript rows: %d left", len(logs))
	}
	if _, err := s.GetTurn(t.Context(), old); err != nil {
		t.Errorf("the old turn row was removed too early: %v", err)
	}
}

// A running turn is the one someone is watching. However old the cutoff, its
// transcript must survive — otherwise a long turn goes blank mid-flight.
func TestPruneNeverTouchesRunningTurns(t *testing.T) {
	s := newTestStore(t)

	running, _, _ := s.CreateTurn(t.Context(), "still going", core.HTTPPollOrigin(), "")
	if err := s.AppendLog(t.Context(), running, core.LogText, "working"); err != nil {
		t.Fatalf("append log: %v", err)
	}

	// A cutoff in the future would match everything that is prunable at all.
	p, err := s.Prune(t.Context(), time.Now().AddDate(1, 0, 0), time.Now().AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if p.Logs != 0 || p.Turns != 0 {
		t.Errorf("pruned %d logs and %d turns from a running turn", p.Logs, p.Turns)
	}
	if logs, _ := s.TailLogs(t.Context(), running, 10); len(logs) != 1 {
		t.Errorf("a running turn lost its transcript")
	}
}

func TestPruneRemovesOldTurnsAndKeys(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	old, _, _ := s.CreateTurn(t.Context(), "ancient", core.HTTPPollOrigin(), "key-old")
	finishAt(t, s, old, now.AddDate(0, 0, -120))
	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE idempotency SET created_at = ?`, now.AddDate(0, 0, -120).UnixMilli()); err != nil {
		t.Fatalf("backdate key: %v", err)
	}

	p, err := s.Prune(t.Context(), now.AddDate(0, 0, -7), now.AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if p.Turns != 1 {
		t.Errorf("pruned %d turns, want 1", p.Turns)
	}
	if p.Keys != 1 {
		t.Errorf("pruned %d idempotency keys, want 1", p.Keys)
	}
}
