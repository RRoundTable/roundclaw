package adapter

import (
	"errors"
	"testing"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
)

// With no ceilings configured nothing is refused, so an operator who has not
// opted in is never surprised by an invented default.
func TestNoLimitsConfiguredAdmitsEverything(t *testing.T) {
	srv, _, _ := newHarness(t)
	for i := range 5 {
		resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "go"})
		if resp.StatusCode != 202 {
			t.Fatalf("request %d: status = %d, want 202", i, resp.StatusCode)
		}
	}
}

func TestTurnsPerHourRefusesWith429(t *testing.T) {
	srv, tc, _ := newHarnessWithLimits(t, "turns_per_hour: 2")

	for i := range 2 {
		if resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "go"}); resp.StatusCode != 202 {
			t.Fatalf("request %d: status = %d, want 202", i, resp.StatusCode)
		}
	}

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "go"})
	// 429 rather than 400: the request is fine, it just has to wait.
	if resp.StatusCode != 429 {
		t.Fatalf("over the limit: status = %d, want 429", resp.StatusCode)
	}
	// Nothing may be queued for a refused request.
	if got := tc.sent(); len(got) != 2 {
		t.Errorf("signalled %d times, want 2 — a refused request was still queued", len(got))
	}
}

func TestDailyCostCeilingRefuses(t *testing.T) {
	srv, _, st := newHarnessWithLimits(t, "cost_per_day_usd: 1.0")

	turnID, _, err := st.CreateTurn(t.Context(), "expensive", core.HTTPPollOrigin(), "")
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if err := st.FinishTurn(t.Context(), turnID, core.TurnResult{
		TurnID: turnID, Status: core.TurnDone, CostUSD: 1.50,
	}); err != nil {
		t.Fatalf("finish turn: %v", err)
	}

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "more"})
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 once the day's spend is over the ceiling", resp.StatusCode)
	}
	body := decode[map[string]string](t, resp)
	if got := body["error"]; got == "" || !errors.Is(ErrLimitReached, ErrLimitReached) {
		t.Errorf("error message was unhelpful: %q", got)
	}
}

// Usage is counted from when a turn was queued, not when it finished, or a
// burst could slip past the rate limit while its turns were still running.
func TestUsageCountsQueuedNotFinished(t *testing.T) {
	_, _, st := newHarness(t)

	if _, _, err := st.CreateTurn(t.Context(), "still running", core.HTTPPollOrigin(), ""); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	usage, err := st.UsageSince(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.Turns != 1 {
		t.Errorf("turns = %d, want 1 — an unfinished turn was not counted", usage.Turns)
	}
	if usage.CostUSD != 0 {
		t.Errorf("cost = %v, want 0 — cost only accrues on completion", usage.CostUSD)
	}
}
