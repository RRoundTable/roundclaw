package registry

import "testing"

func run(id int64, version int, score float64) EvalRun {
	return EvalRun{
		ID: id, EvalSetID: "dev-basic", AgentID: "dev",
		Version: version, Status: EvalDone, Score: score,
	}
}

func result(name string, score float64, passed bool) EvalResult {
	return EvalResult{CaseName: name, Score: score, Passed: passed}
}

// The rule the whole loop rests on: a case that used to pass and now fails is a
// regression, and one regression is enough to stop calling the candidate better
// however much the average score improved.
func TestOneRegressionOutweighsAHigherScore(t *testing.T) {
	base := run(1, 3, 0.5)
	candidate := run(2, 4, 0.95)

	c := compareRuns(base, candidate,
		[]EvalResult{result("a", 0.2, false), result("b", 1, true)},
		[]EvalResult{result("a", 1, true), result("b", 0.9, false)},
	)

	if c.Regressions != 1 || c.Improvements != 1 {
		t.Fatalf("regressions = %d, improvements = %d; want 1 and 1", c.Regressions, c.Improvements)
	}
	if c.Verdict != RunMixed {
		t.Errorf("verdict = %q, want %q — the score went up but something broke", c.Verdict, RunMixed)
	}
	// And the broken case is first, because that is what the reader came for.
	if c.Cases[0].CaseName != "b" || c.Cases[0].Verdict != VerdictRegression {
		t.Errorf("first case = %+v, want the regression", c.Cases[0])
	}
}

func TestVerdicts(t *testing.T) {
	cases := []struct {
		name       string
		base, cand []EvalResult
		want       string
	}{
		{
			name: "only improvements",
			base: []EvalResult{result("a", 0, false)},
			cand: []EvalResult{result("a", 1, true)},
			want: RunBetter,
		},
		{
			name: "only regressions",
			base: []EvalResult{result("a", 1, true)},
			cand: []EvalResult{result("a", 0, false)},
			want: RunWorse,
		},
		{
			// A score that drifted without changing any pass/fail is noise, not a
			// result. Calling it "worse" would have a meta-agent rolling versions
			// back over judge variance.
			name: "same passes, different scores",
			base: []EvalResult{result("a", 0.9, true)},
			cand: []EvalResult{result("a", 0.7, true)},
			want: RunUnchanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compareRuns(run(1, 3, 0), run(2, 4, 0), tc.base, tc.cand)
			if got.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q", got.Verdict, tc.want)
			}
		})
	}
}

// A case the candidate never ran is not a pass. It usually means the case was
// dropped from the set, which is worth seeing next to whatever proposed that.
func TestACaseMissingFromTheCandidateIsReported(t *testing.T) {
	c := compareRuns(run(1, 3, 1), run(2, 4, 1),
		[]EvalResult{result("kept", 1, true), result("dropped", 1, true)},
		[]EvalResult{result("kept", 1, true), result("added", 1, true)},
	)

	byName := map[string]CaseDelta{}
	for _, d := range c.Cases {
		byName[d.CaseName] = d
	}
	if byName["dropped"].Verdict != VerdictBaseOnly {
		t.Errorf("dropped case = %q, want %q", byName["dropped"].Verdict, VerdictBaseOnly)
	}
	if byName["added"].Verdict != VerdictNewCase {
		t.Errorf("added case = %q, want %q", byName["added"].Verdict, VerdictNewCase)
	}
	// A dropped case is not counted as a regression: nothing got worse, the
	// question simply stopped being asked.
	if c.Regressions != 0 {
		t.Errorf("regressions = %d, want 0", c.Regressions)
	}
}

// Comparing runs of different sets, agents, or an unfinished run still produces
// numbers — but saying they are not comparable is the difference between a
// verdict and a misleading one.
func TestIncomparableRunsAreFlagged(t *testing.T) {
	other := run(2, 4, 1)
	other.EvalSetID = "something-else"
	if c := compareRuns(run(1, 3, 1), other, nil, nil); c.Comparable || c.Note == "" {
		t.Errorf("different eval sets: comparable = %v, note = %q", c.Comparable, c.Note)
	}

	unfinished := run(2, 4, 0)
	unfinished.Status = EvalRunning
	if c := compareRuns(run(1, 3, 1), unfinished, nil, nil); c.Comparable {
		t.Error("a run still in progress was reported as comparable")
	}

	// Same version twice is comparable — it is a valid thing to measure — but it
	// is variance, not progress, and the report says so.
	if c := compareRuns(run(1, 3, 0.8), run(2, 3, 0.9), nil, nil); !c.Comparable || c.Note == "" {
		t.Errorf("same version: comparable = %v, note = %q; want comparable with a warning",
			c.Comparable, c.Note)
	}
}

func TestCompareEvalRunsReadsFromTheStore(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Enabled: true}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := s.PutEvalSet(t.Context(), EvalSet{
		ID: "dev-basic", AgentID: "dev", Enabled: true,
		Cases: []EvalCase{{Name: "a", Prompt: "p"}},
	}); err != nil {
		t.Fatalf("put eval set: %v", err)
	}

	var ids []int64
	for i, passed := range []bool{true, false} {
		r, err := s.StartEvalRun(t.Context(), EvalRun{
			EvalSetID: "dev-basic", AgentID: "dev", Version: 3 + i, Total: 1,
		})
		if err != nil {
			t.Fatalf("start run: %v", err)
		}
		score := 0.0
		if passed {
			score = 1
		}
		if err := s.RecordEvalResult(t.Context(), EvalResult{
			RunID: r.ID, CaseName: "a", Score: score, Passed: passed,
		}); err != nil {
			t.Fatalf("record result: %v", err)
		}
		if err := s.FinishEvalRun(t.Context(), r.ID, EvalDone, score, boolToInt(passed), 0.01, ""); err != nil {
			t.Fatalf("finish run: %v", err)
		}
		ids = append(ids, r.ID)
	}

	c, err := s.CompareEvalRuns(t.Context(), ids[0], ids[1])
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c.Verdict != RunWorse || c.Regressions != 1 {
		t.Errorf("comparison = %+v; want one regression and a worse verdict", c)
	}
	if c.Base.ID != ids[0] || c.Candidate.ID != ids[1] {
		t.Errorf("compared runs %d and %d, want %d and %d", c.Base.ID, c.Candidate.ID, ids[0], ids[1])
	}
}

// Case names are how two runs are lined up, so a duplicate would silently
// overwrite a result and then look like a case that vanished.
func TestEvalSetRejectsDuplicateCaseNames(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(t.Context(), Agent{ID: "dev", Enabled: true}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	_, err := s.PutEvalSet(t.Context(), EvalSet{
		ID: "dupes", AgentID: "dev", Enabled: true,
		Cases: []EvalCase{{Name: "same", Prompt: "a"}, {Name: "same", Prompt: "b"}},
	})
	if err == nil {
		t.Fatal("a set with two cases of the same name was accepted")
	}
}
