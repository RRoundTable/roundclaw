package store

import (
	"testing"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// seedTurns creates turns and finishes them in the given states, returning the
// ids in creation order.
func seedTurns(t *testing.T, s *Store, states []core.TurnStatus, conversation string) []int64 {
	t.Helper()
	var ids []int64
	for i, st := range states {
		id, _, err := s.CreateTurn(t.Context(), NewTurn{
			Request: "request " + string(rune('a'+i)), Origin: core.HTTPPollOrigin(), Conversation: conversation,
		})
		if err != nil {
			t.Fatalf("create turn: %v", err)
		}
		if st != core.TurnRunning {
			if err := s.FinishTurn(t.Context(), id, core.TurnResult{Status: st, Text: "result"}); err != nil {
				t.Fatalf("finish turn: %v", err)
			}
		}
		ids = append(ids, id)
	}
	return ids
}

func TestTurnsAreNewestFirstAndLimited(t *testing.T) {
	s := newTestStore(t)
	ids := seedTurns(t, s, []core.TurnStatus{core.TurnDone, core.TurnDone, core.TurnDone}, "")

	got, err := s.Turns(t.Context(), TurnFilter{Limit: 2})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("turns = %d, want 2", len(got))
	}
	if got[0].ID != ids[2] || got[1].ID != ids[1] {
		t.Errorf("order = %d, %d; want newest first (%d, %d)", got[0].ID, got[1].ID, ids[2], ids[1])
	}
}

// The usual caller is a model paying for every row, so an absurd limit must be
// clamped rather than honoured, and no limit at all must not mean "everything".
func TestTurnsClampsTheLimit(t *testing.T) {
	s := newTestStore(t)
	states := make([]core.TurnStatus, MaxHistoryLimit+5)
	for i := range states {
		states[i] = core.TurnDone
	}
	seedTurns(t, s, states, "")

	got, err := s.Turns(t.Context(), TurnFilter{Limit: 100000})
	if err != nil {
		t.Fatalf("Turns with a huge limit: %v", err)
	}
	if len(got) != MaxHistoryLimit {
		t.Errorf("limit=100000 returned %d rows, want the ceiling of %d", len(got), MaxHistoryLimit)
	}

	got, err = s.Turns(t.Context(), TurnFilter{})
	if err != nil {
		t.Fatalf("Turns with the zero filter: %v", err)
	}
	if len(got) != DefaultHistoryLimit {
		t.Errorf("zero filter returned %d rows, want the default of %d", len(got), DefaultHistoryLimit)
	}
}

// "What went wrong lately" is the question this exists to answer, so filtering
// to failures has to be exact.
func TestTurnsFilterByStatus(t *testing.T) {
	s := newTestStore(t)
	seedTurns(t, s, []core.TurnStatus{core.TurnDone, core.TurnError, core.TurnDone, core.TurnError}, "")

	got, err := s.Turns(t.Context(), TurnFilter{Status: core.TurnError})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("errored turns = %d, want 2", len(got))
	}
	for _, turn := range got {
		if turn.Status != core.TurnError {
			t.Errorf("turn %d status = %s", turn.ID, turn.Status)
		}
	}
}

// The empty string is itself a conversation — the agent's default one — so a nil
// filter and a filter for "" must mean different things.
func TestTurnsFilterByConversationDistinguishesTheDefault(t *testing.T) {
	s := newTestStore(t)
	seedTurns(t, s, []core.TurnStatus{core.TurnDone, core.TurnDone}, "")
	seedTurns(t, s, []core.TurnStatus{core.TurnDone}, "thread-a")

	all, err := s.Turns(t.Context(), TurnFilter{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("nil conversation filter = %d turns, want all 3", len(all))
	}

	def := ""
	only, err := s.Turns(t.Context(), TurnFilter{Conversation: &def})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(only) != 2 {
		t.Errorf("default conversation = %d turns, want 2", len(only))
	}
	for _, turn := range only {
		if turn.Conversation != "" {
			t.Errorf("turn %d is in conversation %q", turn.ID, turn.Conversation)
		}
	}
}

func TestTurnsFilterBySince(t *testing.T) {
	s := newTestStore(t)
	seedTurns(t, s, []core.TurnStatus{core.TurnDone, core.TurnDone}, "")

	// Everything was queued just now, so a cutoff in the future excludes it all
	// and one in the past keeps it.
	if got, err := s.Turns(t.Context(), TurnFilter{Since: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Turns: %v", err)
	} else if len(got) != 0 {
		t.Errorf("since=+1h returned %d turns, want none", len(got))
	}
	if got, err := s.Turns(t.Context(), TurnFilter{Since: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Turns: %v", err)
	} else if len(got) != 2 {
		t.Errorf("since=-1h returned %d turns, want 2", len(got))
	}
}
