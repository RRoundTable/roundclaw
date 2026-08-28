package registry

import (
	"strings"
	"testing"
)

// gateFixture puts an agent at v2 with a baseline run of v1 and a gating run of
// v2, so a test only has to say how the two runs scored.
func gateFixture(t *testing.T, baseCases, candCases map[string]bool) (*Store, int64) {
	t.Helper()
	s := newStore(t)

	if _, err := s.Create(t.Context(), Agent{ID: "dev", Description: "first", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Update(t.Context(), Agent{ID: "dev", Description: "second", Enabled: true},
		Change{Author: "agent:dev", Note: "a change it made to itself"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var names []string
	for name := range baseCases {
		names = append(names, name)
	}
	if _, err := s.PutEvalSet(t.Context(), EvalSet{
		ID: "cases", AgentID: "dev", Enabled: true,
		Cases: casesNamed(names),
	}); err != nil {
		t.Fatalf("put eval set: %v", err)
	}

	base := startRun(t, s, 1, 0, 0, baseCases)
	cand := startRun(t, s, 2, 2, base, candCases)
	return s, cand
}

func casesNamed(names []string) []EvalCase {
	out := make([]EvalCase, 0, len(names))
	for _, n := range names {
		out = append(out, EvalCase{Name: n, Prompt: "do the thing"})
	}
	return out
}

func startRun(t *testing.T, s *Store, version, gates int, baseline int64, cases map[string]bool) int64 {
	t.Helper()
	run, err := s.StartEvalRun(t.Context(), EvalRun{
		EvalSetID: "cases", AgentID: "dev", Version: version,
		GatesVersion: gates, BaselineRun: baseline, Total: len(cases),
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	passed := 0
	for name, ok := range cases {
		score := 0.0
		if ok {
			score, passed = 1, passed+1
		}
		if err := s.RecordEvalResult(t.Context(), EvalResult{
			RunID: run.ID, CaseName: name, Score: score, Passed: ok,
		}); err != nil {
			t.Fatalf("record result: %v", err)
		}
	}
	if err := s.FinishEvalRun(t.Context(), run.ID, EvalDone,
		float64(passed)/float64(len(cases)), passed, 0, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return run.ID
}

// The behaviour the whole slice exists for.
func TestARegressionIsPutBackWithoutBeingAsked(t *testing.T) {
	s, gating := gateFixture(t,
		map[string]bool{"a": true, "b": true},
		map[string]bool{"a": true, "b": false}) // b regressed

	out, err := s.SettleGate(t.Context(), gating)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !out.Gated {
		t.Fatal("the run did not settle as a gate")
	}
	if !out.Reverted {
		t.Fatalf("a regression was not reverted; verdict = %q", out.Verdict)
	}
	if out.RevertedTo != 1 {
		t.Errorf("reverted to v%d, want v1", out.RevertedTo)
	}

	live, err := s.Get(t.Context(), "dev")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.Description != "first" {
		t.Errorf("description = %q, want the configuration from before the change", live.Description)
	}
}

// The evidence, not just the verdict. An agent told only "no" makes the same
// change again.
func TestTheRevertNamesTheCasesThatRegressed(t *testing.T) {
	s, gating := gateFixture(t,
		map[string]bool{"keeps-context": true, "answers-briefly": true},
		map[string]bool{"keeps-context": false, "answers-briefly": true})

	out, err := s.SettleGate(t.Context(), gating)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(out.Regressed) != 1 || out.Regressed[0] != "keeps-context" {
		t.Fatalf("regressed = %v, want [keeps-context]", out.Regressed)
	}

	reverts, err := s.PendingRevertsFor(t.Context(), "dev", 0)
	if err != nil {
		t.Fatalf("pending reverts: %v", err)
	}
	if len(reverts) == 0 {
		t.Fatal("the reversal left nothing for the agent to read")
	}
	if !strings.Contains(reverts[0].Note, "keeps-context") {
		t.Errorf("the reversal note does not name the regressed case: %q", reverts[0].Note)
	}
}

// Both the change and its reversal stand in the history: a rollback applied as a
// rewind would read as though the change never happened.
func TestTheChangeAndItsReversalBothStandInTheHistory(t *testing.T) {
	s, gating := gateFixture(t,
		map[string]bool{"a": true},
		map[string]bool{"a": false})
	if _, err := s.SettleGate(t.Context(), gating); err != nil {
		t.Fatalf("settle: %v", err)
	}

	versions, err := s.ListVersions(t.Context(), "dev", 0)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want 3: v1, the change, and its reversal", len(versions))
	}
	if versions[1].Author != "agent:dev" {
		t.Errorf("the change it made is recorded as %q", versions[1].Author)
	}
}

// An improvement is kept. The gate is not a veto on change.
func TestAnImprovementIsKept(t *testing.T) {
	s, gating := gateFixture(t,
		map[string]bool{"a": false},
		map[string]bool{"a": true})

	out, err := s.SettleGate(t.Context(), gating)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if out.Reverted {
		t.Error("an improvement was reverted")
	}
	if out.Verdict != RunBetter {
		t.Errorf("verdict = %q, want better", out.Verdict)
	}
	live, _ := s.Get(t.Context(), "dev")
	if live.Description != "second" {
		t.Errorf("description = %q, want the change to have been kept", live.Description)
	}
}

// Mixed means something regressed, and any regression makes the change worse.
func TestMixedIsTreatedAsARegression(t *testing.T) {
	s, gating := gateFixture(t,
		map[string]bool{"a": true, "b": false},
		map[string]bool{"a": false, "b": true}) // one each way

	out, err := s.SettleGate(t.Context(), gating)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if out.Verdict != RunMixed {
		t.Fatalf("verdict = %q, want mixed", out.Verdict)
	}
	if !out.Reverted {
		t.Error("a mixed verdict was kept; any regression makes the change worse")
	}
}

// A run that broke is a marking failure, not a regression. Reverting on it would
// undo a change because the harness fell over.
func TestAFailedRunDoesNotRevert(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Description: "first", Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Update(t.Context(), Agent{ID: "dev", Description: "second", Enabled: true}); err != nil {
		t.Fatalf("update: %v", err)
	}
	run, err := s.StartEvalRun(t.Context(), EvalRun{
		EvalSetID: "cases", AgentID: "dev", Version: 2, GatesVersion: 2, BaselineRun: 1,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.FinishEvalRun(t.Context(), run.ID, EvalFailed, 0, 0, 0, "the worker died"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	out, err := s.SettleGate(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if out.Reverted {
		t.Error("a failed run reverted a change; that is a marking failure scored as a regression")
	}
	if !strings.Contains(out.Verdict, "not measured") {
		t.Errorf("verdict = %q, want it to say the change was not measured", out.Verdict)
	}
	live, _ := s.Get(t.Context(), "dev")
	if live.Description != "second" {
		t.Error("the change was undone by a run that measured nothing")
	}
}

// An ordinary run judges nothing, which is what makes settling on every
// completion safe rather than something to remember.
func TestAnOrdinaryRunSettlesToNothing(t *testing.T) {
	s := newStore(t)
	run, err := s.StartEvalRun(t.Context(), EvalRun{EvalSetID: "cases", AgentID: "dev"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.FinishEvalRun(t.Context(), run.ID, EvalDone, 1, 1, 0, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	out, err := s.SettleGate(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if out.Gated || out.Reverted {
		t.Errorf("an ordinary run settled as a gate: %+v", out)
	}
}
