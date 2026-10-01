package lifecycle

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShapeKeepsActingCallsOnly(t *testing.T) {
	s := Shape([]ToolCall{
		{Name: "Read"}, {Name: "Grep"},
		{Name: "Bash", Input: `{"command":"go vet ./...","description":"vet"}`},
		{Name: "Edit"}, {Name: "Edit"}, {Name: "Read"},
		{Name: "Bash", Input: `{"command":"/usr/bin/git push origin x"}`},
	})
	assert.Equal(t, "Bash:go > Edit > Bash:git", s)
	assert.Equal(t, 3, ActingCalls(s))
	assert.Equal(t, 0, ActingCalls(""))
}

func turns(shape string, n, convs int, at time.Time) []Turn {
	var out []Turn
	for i := 0; i < n; i++ {
		out = append(out, Turn{Agent: "dev", TurnID: int64(i + 1), Conversation: fmt.Sprintf("c%d", i%convs), Shape: shape, At: at})
	}
	return out
}

func TestRecurringNeedsEnoughTurnsAcrossConversations(t *testing.T) {
	r := Defaults()
	now := time.Now()
	shape := "Bash:go > Edit > Bash:git"
	got := Recurring(turns(shape, 5, 2, now.Add(-time.Hour)), now, nil, r)
	require.Len(t, got, 1)
	assert.Equal(t, shape, got[0].Shape)

	assert.Empty(t, Recurring(turns(shape, 4, 2, now), now, nil, r), "too few turns")
	assert.Empty(t, Recurring(turns(shape, 9, 1, now), now, nil, r), "one conversation is a loop, not recurrence")
	assert.Empty(t, Recurring(turns("Edit > Bash:go", 9, 3, now), now, nil, r), "too few acting calls")
	assert.Empty(t, Recurring(turns(shape, 9, 3, now.Add(-30*24*time.Hour)), now, nil, r), "outside the window")
	assert.Empty(t, Recurring(turns(shape, 9, 3, now), now, map[string]bool{shape: true}, r), "covered or cooling down")
	assert.Len(t, Recurring(turns(shape, 30, 3, now), now, nil, r)[0].Turns, r.MaxTrialCases)
}

func pass(cost, base float64) CaseResult {
	return CaseResult{Passed: true, CostUSD: cost, HasBaseline: true, BaselinePassed: true, BaselineCost: base}
}

func TestAdmit(t *testing.T) {
	r := Defaults()
	good := []CaseResult{pass(0.1, 0.4), pass(0.1, 0.4), pass(0.1, 0.4), pass(0.2, 0.4)}
	d := Admit(good, r)
	assert.True(t, d.Accept, d.Rule)

	assert.False(t, Admit(good[:2], r).Accept, "too few cases")

	dear := []CaseResult{pass(0.5, 0.1), pass(0.5, 0.1), pass(0.5, 0.1)}
	assert.Contains(t, Admit(dear, r).Rule, "median cost")

	worse := []CaseResult{pass(0.1, 0.4), pass(0.1, 0.4), {CostUSD: 0.1, HasBaseline: true, BaselinePassed: true, BaselineCost: 0.4},
		pass(0.1, 0.4), pass(0.1, 0.4)}
	d = Admit(worse, r)
	assert.False(t, d.Accept)
	assert.Contains(t, d.Rule, "below the work it came from")

	infra := []CaseResult{pass(0.1, 0.4), pass(0.1, 0.4), {InfraError: true}, {InfraError: true}}
	assert.Contains(t, Admit(infra, r).Rule, "infrastructure")

	unmarked := []CaseResult{pass(0.1, 0.4), pass(0.1, 0.4), {Unmarked: true}, {Unmarked: true}}
	assert.Contains(t, Admit(unmarked, r).Rule, "could not mark")

	// A submitted routine has no recorded work behind it: only the floor applies.
	submitted := []CaseResult{{Passed: true}, {Passed: true}, {Passed: true}}
	assert.True(t, Admit(submitted, r).Accept)
}

func TestPromote(t *testing.T) {
	r := Defaults()
	fix := []PairResult{
		{Name: "c1", CurrentPassed: true, CandidatePassed: true, CurrentCost: 0.1, CandidateCost: 0.1},
		{Name: "f1", Targeted: true, CandidatePassed: true, CurrentCost: 0.1, CandidateCost: 0.1},
	}
	d := Promote(fix, r)
	assert.True(t, d.Accept, d.Rule)

	regress := append(fix, PairResult{Name: "c2", CurrentPassed: true, CandidatePassed: false})
	d = Promote(regress, r)
	assert.False(t, d.Accept)
	assert.Contains(t, d.Rule, "regressed on c2")

	nofix := []PairResult{
		{Name: "c1", CurrentPassed: true, CandidatePassed: true, CurrentCost: 0.1, CandidateCost: 0.1},
		{Name: "f1", Targeted: true, CandidatePassed: false},
	}
	assert.Contains(t, Promote(nofix, r).Rule, "fixed none")

	dear := []PairResult{
		{Name: "c1", CurrentPassed: true, CandidatePassed: true, CurrentCost: 0.1, CandidateCost: 0.5},
		{Name: "f1", Targeted: true, CandidatePassed: true, CandidateCost: 0.5},
	}
	assert.Contains(t, Promote(dear, r).Rule, "costs")

	cheaper := []PairResult{{Name: "c1", CurrentPassed: true, CandidatePassed: true, CurrentCost: 1, CandidateCost: 0.5}}
	assert.True(t, Promote(cheaper, r).Accept, "a cost-only change must save enough")
}

func TestHotspot(t *testing.T) {
	r := Defaults()
	runs := []RunSummary{
		{ID: 1, FailedStep: "fix"}, {ID: 2, FailedStep: "fix"}, {ID: 3, Passed: true, Redone: true},
		{ID: 4, Passed: true}, {ID: 5, FailedStep: "vet"},
	}
	step, f := Hotspot(runs, r)
	assert.Equal(t, "fix", step, "half of the not-passed runs failed at fix (share %v)", f["share"])

	spread := []RunSummary{{ID: 1, FailedStep: "a"}, {ID: 2, FailedStep: "b"}, {ID: 3, FailedStep: "c"}, {ID: 4, FailedStep: "a"}, {ID: 5, FailedStep: "d"}}
	step, _ = Hotspot(spread, r)
	assert.Equal(t, "", step, "failures spread across steps name no hotspot")

	step, _ = Hotspot([]RunSummary{{ID: 1, FailedStep: "fix"}, {ID: 2, FailedStep: "fix"}}, r)
	assert.Equal(t, "", step, "too few failures")
}

func f64(v float64) *float64 { return &v }

func TestArchive(t *testing.T) {
	r := Defaults()
	now := time.Now()
	fresh := ArchiveFacts{Now: now, CreatedAt: now.Add(-48 * time.Hour), LastRunAt: now.Add(-time.Hour)}
	assert.False(t, Archive(fresh, r).Accept)

	unused := fresh
	unused.LastRunAt = now.Add(-31 * 24 * time.Hour)
	assert.Contains(t, Archive(unused, r).Rule, "unused")

	broken := fresh
	broken.UnrunnableSince = now.Add(-8 * 24 * time.Hour)
	assert.Contains(t, Archive(broken, r).Rule, "unable to run")

	failing := fresh
	failing.Runs, failing.PassRate, failing.RejectedFixes = 12, f64(0.3), 2
	assert.Contains(t, Archive(failing, r).Rule, "rejected fixes")
	failing.RejectedFixes = 1
	assert.False(t, Archive(failing, r).Accept, "fixes still being tried")

	rivalled := fresh
	rivalled.Own = RivalFacts{ID: "a", Runs: 10, IndependentRate: f64(0.8), CostPerPassedUSD: f64(0.5)}
	rivalled.Rival = &RivalFacts{ID: "b", Runs: 10, IndependentRate: f64(0.9), CostPerPassedUSD: f64(0.4)}
	assert.Contains(t, Archive(rivalled, r).Rule, "superseded by b")
	rivalled.Rival.CostPerPassedUSD = f64(0.9)
	assert.False(t, Archive(rivalled, r).Accept, "better but dearer is not superseding")
}
