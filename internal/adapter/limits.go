package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrLimitReached is returned when a request would exceed a configured ceiling.
var ErrLimitReached = errors.New("limit reached")

// Spend limits are enforced at admission, before the turn row is written.
//
// Checking here rather than in the worker means a caller is told immediately
// and nothing is queued that will never run. It also means every source —
// Discord, the HTTP API, and later schedules — passes through one check, since
// they all funnel through Submit.
//
// The gateway is the door, not a vault: anything signalling Temporal directly
// bypasses this. That is an acceptable trade for a check that costs one indexed
// query per request instead of coordination on the hot path.

// Budget is what remains against the configured ceilings.
type Budget struct {
	TurnsThisHour int     `json:"turns_this_hour"`
	TurnsPerHour  int     `json:"turns_per_hour,omitempty"`
	CostTodayUSD  float64 `json:"cost_today_usd"`
	CostPerDayUSD float64 `json:"cost_per_day_usd,omitempty"`
	GlobalCostUSD float64 `json:"global_cost_today_usd"`
	GlobalCapUSD  float64 `json:"global_cost_per_day_usd,omitempty"`
}

// checkLimits refuses a request that would exceed a ceiling.
//
// Cost limits are checked against what has already completed, so a single turn
// can still carry the total past the line — the alternative would be predicting
// a turn's cost before running it, which is not knowable. The ceiling therefore
// stops the next request, not the one that crossed it.
func (d *Dispatcher) checkLimits(ctx context.Context, agentID string) error {
	limits := d.cfg.Limits
	if limits.TurnsPerHour == 0 && limits.CostPerDayUSD == 0 && limits.GlobalCostPerDayUSD == 0 {
		return nil
	}

	st, err := d.stores.Get(agentID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	now := time.Now()
	if limits.TurnsPerHour > 0 {
		usage, err := st.UsageSince(ctx, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if usage.Turns >= limits.TurnsPerHour {
			return fmt.Errorf("%w: %s has run %d turns in the last hour, limit is %d",
				ErrLimitReached, agentID, usage.Turns, limits.TurnsPerHour)
		}
	}

	if limits.CostPerDayUSD > 0 {
		usage, err := st.UsageSince(ctx, startOfDay(now))
		if err != nil {
			return err
		}
		if usage.CostUSD >= limits.CostPerDayUSD {
			return fmt.Errorf("%w: %s has spent $%.2f today, limit is $%.2f",
				ErrLimitReached, agentID, usage.CostUSD, limits.CostPerDayUSD)
		}
	}

	if limits.GlobalCostPerDayUSD > 0 {
		total, err := d.globalCostToday(ctx, now)
		if err != nil {
			return err
		}
		if total >= limits.GlobalCostPerDayUSD {
			return fmt.Errorf("%w: everything together has spent $%.2f today, limit is $%.2f",
				ErrLimitReached, total, limits.GlobalCostPerDayUSD)
		}
	}
	return nil
}

// globalCostToday sums spend across every registered agent.
//
// One query per agent. That is fine at this scale and avoids a shared counter
// that two processes would have to agree on; if the agent count ever grows
// enough to matter, this is the place to add a cached rollup.
func (d *Dispatcher) globalCostToday(ctx context.Context, now time.Time) (float64, error) {
	agents, err := d.reg.List(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, a := range agents {
		st, err := d.stores.Get(a.ID)
		if err != nil {
			// An agent that has never run has no database yet. Skipping it is
			// correct: it has spent nothing.
			continue
		}
		usage, err := st.UsageSince(ctx, startOfDay(now))
		if err != nil {
			return 0, err
		}
		total += usage.CostUSD
	}
	return total, nil
}

// Budget reports an agent's usage against the ceilings, for /status.
func (d *Dispatcher) Budget(ctx context.Context, agentID string) (Budget, error) {
	b := Budget{
		TurnsPerHour:  d.cfg.Limits.TurnsPerHour,
		CostPerDayUSD: d.cfg.Limits.CostPerDayUSD,
		GlobalCapUSD:  d.cfg.Limits.GlobalCostPerDayUSD,
	}

	st, err := d.stores.Get(agentID)
	if err != nil {
		return b, err
	}
	now := time.Now()

	hour, err := st.UsageSince(ctx, now.Add(-time.Hour))
	if err != nil {
		return b, err
	}
	b.TurnsThisHour = hour.Turns

	day, err := st.UsageSince(ctx, startOfDay(now))
	if err != nil {
		return b, err
	}
	b.CostTodayUSD = day.CostUSD

	if d.cfg.Limits.GlobalCostPerDayUSD > 0 {
		if total, err := d.globalCostToday(ctx, now); err == nil {
			b.GlobalCostUSD = total
		}
	}
	return b, nil
}

// startOfDay is local midnight. Local rather than UTC because the ceiling is a
// daily budget a person reasons about, and "today" should mean their today.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
