package registry

import (
	"context"
	"fmt"
	"sort"
)

// Comparing two eval runs.
//
// This is arithmetic on purpose. Deciding whether a new version is better is the
// step a meta-agent most wants to do by reading the outputs and forming an
// impression — which is exactly how a regression gets talked away. So the
// verdict is computed here, from case-level pass/fail, and the agent's job is to
// explain the result rather than to reach it.
//
// Cases are lined up by name, which is why a set may not reuse one.

// CaseVerdicts.
const (
	VerdictRegression  = "regression"  // passed before, fails now
	VerdictImprovement = "improvement" // failed before, passes now
	VerdictUnchanged   = "unchanged"   // same pass/fail either way
	VerdictBaseOnly    = "base_only"   // the candidate run has no such case
	VerdictNewCase     = "new_case"    // the base run had no such case
)

// Run verdicts.
const (
	RunBetter    = "better"
	RunWorse     = "worse"
	RunMixed     = "mixed"
	RunUnchanged = "unchanged"
)

// CaseDelta is one case across two runs.
type CaseDelta struct {
	CaseName        string  `json:"case_name"`
	Verdict         string  `json:"verdict"`
	BaseScore       float64 `json:"base_score"`
	CandidateScore  float64 `json:"candidate_score"`
	ScoreDelta      float64 `json:"score_delta"`
	BasePassed      bool    `json:"base_passed"`
	CandidatePassed bool    `json:"candidate_passed"`
	// Reason is the candidate's reason — why it scored what it did — because the
	// question being asked is what the new version did, not what the old one did.
	Reason string `json:"reason,omitempty"`
}

// Comparison is the full report.
type Comparison struct {
	Base      EvalRun     `json:"base"`
	Candidate EvalRun     `json:"candidate"`
	Cases     []CaseDelta `json:"cases"`

	Regressions  int     `json:"regressions"`
	Improvements int     `json:"improvements"`
	ScoreDelta   float64 `json:"score_delta"`
	CostDelta    float64 `json:"cost_delta"`
	// Verdict follows one rule, stated so nobody has to infer it: any regression
	// makes the candidate worse (or mixed, if it also improved something).
	// Improvements with no regressions make it better. Neither makes it
	// unchanged, whatever the scores did.
	Verdict string `json:"verdict"`
	// Comparable is false when the two runs do not evaluate the same thing, in
	// which case the verdict is not worth much. Stated rather than hidden.
	Comparable bool   `json:"comparable"`
	Note       string `json:"note,omitempty"`
}

// CompareEvalRuns lines two runs up case by case and computes the verdict.
func (s *Store) CompareEvalRuns(ctx context.Context, baseID, candidateID int64) (Comparison, error) {
	base, err := s.GetEvalRun(ctx, baseID)
	if err != nil {
		return Comparison{}, err
	}
	candidate, err := s.GetEvalRun(ctx, candidateID)
	if err != nil {
		return Comparison{}, err
	}
	baseResults, err := s.EvalResults(ctx, baseID)
	if err != nil {
		return Comparison{}, err
	}
	candidateResults, err := s.EvalResults(ctx, candidateID)
	if err != nil {
		return Comparison{}, err
	}
	return compareRuns(base, candidate, baseResults, candidateResults), nil
}

// compareRuns is the whole judgement, kept pure so it can be tested without a
// database and read without following a query.
func compareRuns(base, candidate EvalRun, baseResults, candidateResults []EvalResult) Comparison {
	c := Comparison{
		Base:       base,
		Candidate:  candidate,
		ScoreDelta: candidate.Score - base.Score,
		CostDelta:  candidate.CostUSD - base.CostUSD,
		Comparable: true,
	}

	// The comparison still runs when the runs disagree about what they measured —
	// the numbers are real either way — but saying so is the difference between a
	// verdict and a misleading one.
	switch {
	case base.EvalSetID != candidate.EvalSetID:
		c.Comparable = false
		c.Note = fmt.Sprintf("different eval sets (%s vs %s): the cases are not the same questions",
			base.EvalSetID, candidate.EvalSetID)
	case base.AgentID != candidate.AgentID:
		c.Comparable = false
		c.Note = fmt.Sprintf("different agents (%s vs %s)", base.AgentID, candidate.AgentID)
	case base.Status != EvalDone || candidate.Status != EvalDone:
		c.Comparable = false
		c.Note = fmt.Sprintf("a run did not finish (%s: %s, %s: %s)",
			shortID(base.ID), base.Status, shortID(candidate.ID), candidate.Status)
	case base.Version == candidate.Version && base.Version != 0:
		c.Note = fmt.Sprintf("both runs evaluated version %d, so any difference is run-to-run "+
			"variance rather than a change in the agent", base.Version)
	}

	byName := make(map[string]EvalResult, len(baseResults))
	for _, r := range baseResults {
		byName[r.CaseName] = r
	}
	seen := make(map[string]bool, len(candidateResults))

	for _, cand := range candidateResults {
		seen[cand.CaseName] = true
		d := CaseDelta{
			CaseName:        cand.CaseName,
			CandidateScore:  cand.Score,
			CandidatePassed: cand.Passed,
			Reason:          cand.Reason,
		}
		prev, ok := byName[cand.CaseName]
		if !ok {
			d.Verdict = VerdictNewCase
			c.Cases = append(c.Cases, d)
			continue
		}
		d.BaseScore, d.BasePassed = prev.Score, prev.Passed
		d.ScoreDelta = cand.Score - prev.Score
		switch {
		case prev.Passed && !cand.Passed:
			d.Verdict = VerdictRegression
			c.Regressions++
		case !prev.Passed && cand.Passed:
			d.Verdict = VerdictImprovement
			c.Improvements++
		default:
			d.Verdict = VerdictUnchanged
		}
		c.Cases = append(c.Cases, d)
	}

	// A case the candidate never ran is not a pass. It is usually a case removed
	// from the set, which is worth seeing next to a proposal that removed it.
	for _, prev := range baseResults {
		if seen[prev.CaseName] {
			continue
		}
		c.Cases = append(c.Cases, CaseDelta{
			CaseName:   prev.CaseName,
			Verdict:    VerdictBaseOnly,
			BaseScore:  prev.Score,
			BasePassed: prev.Passed,
		})
	}

	// Regressions first, then improvements, then the rest: the reader is looking
	// for what broke, and it should not be on page two.
	sort.SliceStable(c.Cases, func(i, j int) bool {
		return verdictRank(c.Cases[i].Verdict) < verdictRank(c.Cases[j].Verdict)
	})

	switch {
	case c.Regressions > 0 && c.Improvements > 0:
		c.Verdict = RunMixed
	case c.Regressions > 0:
		c.Verdict = RunWorse
	case c.Improvements > 0:
		c.Verdict = RunBetter
	default:
		c.Verdict = RunUnchanged
	}
	return c
}

func verdictRank(v string) int {
	switch v {
	case VerdictRegression:
		return 0
	case VerdictBaseOnly:
		return 1
	case VerdictImprovement:
		return 2
	case VerdictNewCase:
		return 3
	default:
		return 4
	}
}

func shortID(id int64) string { return fmt.Sprintf("run %d", id) }
