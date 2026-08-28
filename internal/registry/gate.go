package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The measurement a self-made change has to pass.
//
// An agent that can change itself and judge the result by reading its own output
// is an agent that drifts, because reading outputs and forming an impression is
// how a regression gets talked away. The verdict here is the arithmetic
// compareRuns already does — a case that passed before and fails now is a
// regression, and any regression makes the change worse.
//
// The gate is asynchronous by necessity. A run takes minutes, far longer than the
// write that triggered it, so the change applies and is then judged: kept if the
// measurement does not say it got worse, put back without being asked if it does.
// Both the change and its reversal stay in the history, so what happened is
// legible rather than looking like nothing changed.
//
// An agent cannot move this gate, because it cannot reach its own evaluation
// cases at all — /v1/evals is outside the surface a per-agent credential opens
// (adr/003). The cases are what "better" means, and an agent that could rewrite
// them would be grading its own homework.

// GateOutcome is what settling a gating run decided.
type GateOutcome struct {
	// Gated is false when the run was an ordinary one, judging nothing.
	Gated bool
	// Verdict is the comparison's, or empty when there was nothing to compare.
	Verdict string
	// Reverted is true when the agent was put back.
	Reverted bool
	// RevertedTo is the version restored, and Version the one that was judged.
	Version    int
	RevertedTo int
	// Regressed names the cases that went from passing to failing. This is the
	// evidence: an agent told only "no" makes the same change again.
	Regressed []string
}

// SettleGate judges a finished run and puts the agent back if it regressed.
//
// Called once a run's aggregate is written. An ordinary run settles to nothing,
// which is what makes this safe to call on every completion rather than on a
// path somebody has to remember to take.
func (s *Store) SettleGate(ctx context.Context, runID int64) (GateOutcome, error) {
	run, err := s.GetEvalRun(ctx, runID)
	if err != nil {
		return GateOutcome{}, err
	}
	if run.GatesVersion == 0 {
		return GateOutcome{}, nil
	}
	out := GateOutcome{Gated: true, Version: run.GatesVersion}

	// A run that never produced results judges nothing. Reverting on a failed
	// run would undo a change because the harness broke, which is a marking
	// failure being scored as a regression — the thing evaluation.md refuses.
	if run.Status != EvalDone {
		out.Verdict = "not measured: the run did not complete"
		return out, nil
	}
	if run.BaselineRun == 0 {
		out.Verdict = "not measured: no baseline to compare against"
		return out, nil
	}

	cmp, err := s.CompareEvalRuns(ctx, run.BaselineRun, runID)
	if err != nil {
		return out, fmt.Errorf("compare gate run %d: %w", runID, err)
	}
	if !cmp.Comparable {
		// evaluation.md: two runs that do not measure the same thing say so. A
		// gate that reverted on an incomparable pair would undo a change on the
		// strength of a verdict the comparison itself disowns.
		out.Verdict = "not measured: " + cmp.Note
		return out, nil
	}
	out.Verdict = cmp.Verdict
	for _, c := range cmp.Cases {
		if c.Verdict == VerdictRegression {
			out.Regressed = append(out.Regressed, c.CaseName)
		}
	}
	sort.Strings(out.Regressed)

	// Only "worse" reverts. "Mixed" already means something regressed, and
	// compareRuns is what decides that — the rule lives there, once.
	if cmp.Verdict != RunWorse && cmp.Verdict != RunMixed {
		return out, nil
	}

	target := run.GatesVersion - 1
	if target < 1 {
		out.Verdict += " (nothing to revert to)"
		return out, nil
	}
	old, err := s.GetVersion(ctx, run.AgentID, target)
	if err != nil {
		return out, fmt.Errorf("read version to revert to: %w", err)
	}
	if _, err := s.Update(ctx, old.Definition, Change{
		Author: "roundclaw",
		Note:   revertNote(run.GatesVersion, target, cmp.Verdict, out.Regressed),
	}); err != nil {
		return out, fmt.Errorf("revert %s to v%d: %w", run.AgentID, target, err)
	}
	out.Reverted, out.RevertedTo = true, target
	return out, nil
}

func revertNote(from, to int, verdict string, regressed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "reverted v%d to v%d: measured %s", from, to, verdict)
	if len(regressed) > 0 {
		fmt.Fprintf(&b, "; regressed: %s", strings.Join(regressed, ", "))
	}
	return b.String()
}

// PendingRevertsFor returns the automatic reversions an agent has not been told
// about, newest first.
//
// An agent told only that its change is gone proposes the same change again. It
// has to see which cases regressed, which is the one thing the history holds that
// its own reasoning does not.
func (s *Store) PendingRevertsFor(ctx context.Context, agentID string, since int) ([]AgentVersion, error) {
	versions, err := s.ListVersions(ctx, agentID, 8)
	if err != nil {
		return nil, err
	}
	var out []AgentVersion
	for _, v := range versions {
		if v.Version <= since {
			break
		}
		if v.Author == "roundclaw" && strings.HasPrefix(v.Note, "reverted v") {
			out = append(out, v)
		}
	}
	return out, nil
}

// ErrUngateable is returned when a self-made change cannot be measured, and so
// cannot be allowed to stick.
//
// The invariant is that no self-made change takes effect permanently without
// having been measured. An agent with nothing to measure it by cannot satisfy
// that, and the honest answer is to refuse the change rather than to keep it and
// quietly call it unmeasured — which would make the invariant a slogan.
//
// It is also the right pressure. The cases are what "better" means, so an agent
// that has none has not yet been told what improving would look like, and
// somebody has to say that before it starts rewriting itself.
var ErrUngateable = errors.New("this change cannot be measured")

// GatePlan is what a self-made change will be judged by.
type GatePlan struct {
	EvalSetID string
	Baseline  int64
}

// PlanGate finds what would judge a change to this agent, or says why nothing
// could.
//
// Both halves have to exist up front. A baseline started alongside the candidate
// would race it — the gate settles when the candidate finishes, and a baseline
// still running compares to nothing — so the baseline has to be a run that
// already completed against the version being replaced.
func (s *Store) PlanGate(ctx context.Context, agentID string, currentVersion int) (GatePlan, error) {
	sets, err := s.ListEvalSets(ctx, agentID)
	if err != nil {
		return GatePlan{}, err
	}
	var set EvalSet
	for _, c := range sets {
		if c.Enabled && len(c.Cases) > 0 {
			set = c
			break
		}
	}
	if set.ID == "" {
		return GatePlan{}, fmt.Errorf("%w: %s has no enabled evaluation set, so there is no definition of better to hold the change to",
			ErrUngateable, agentID)
	}

	runs, err := s.ListEvalRuns(ctx, set.ID, agentID, 50)
	if err != nil {
		return GatePlan{}, err
	}
	for _, r := range runs {
		if r.Status == EvalDone && r.Version == currentVersion {
			return GatePlan{EvalSetID: set.ID, Baseline: r.ID}, nil
		}
	}
	return GatePlan{}, fmt.Errorf("%w: no completed run of %s v%d to compare against; run %s against the current version first",
		ErrUngateable, agentID, currentVersion, set.ID)
}
