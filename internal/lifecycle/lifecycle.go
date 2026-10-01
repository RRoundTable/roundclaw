// Package lifecycle holds the rules that admit, promote, evolve and archive
// routines (adr/013). Agents propose — the steward drafts a routine or a fix —
// and arithmetic decides: every function here takes recorded figures and returns
// a decision with the rule and the figures that made it. Nothing here does I/O
// and nothing asks a model.
package lifecycle

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Rules are the thresholds. Every one is configuration; Defaults are the values
// ADR-013 records.
type Rules struct {
	// Recurrence: when repeated work is worth drafting a routine from.
	MinActingCalls     int           `yaml:"min_acting_calls"`
	MinTurns           int           `yaml:"min_turns"`
	MinConversations   int           `yaml:"min_conversations"`
	RecurrenceWindow   time.Duration `yaml:"recurrence_window"`
	RejectCooldown     time.Duration `yaml:"reject_cooldown"`
	MaxDraftsPerShape  int           `yaml:"max_drafts_per_shape"`
	MaxTrialCases      int           `yaml:"max_trial_cases"`
	MinTrialCases      int           `yaml:"min_trial_cases"`
	AdmitMinPassRate   float64       `yaml:"admit_min_pass_rate"`
	AdmitMaxInfraError int           `yaml:"admit_max_infra_errors"`
	AdmitMaxUnmarked   float64       `yaml:"admit_max_unmarked_share"`

	// Evolution.
	HealthWindow             int           `yaml:"health_window"`
	HealthMinFailures        int           `yaml:"health_min_failures"`
	HotspotShare             float64       `yaml:"hotspot_share"`
	RemeasureEvery           time.Duration `yaml:"remeasure_every"`
	RejectionsBeforeCooldown int           `yaml:"rejections_before_cooldown"`
	EvolveCooldown           time.Duration `yaml:"evolve_cooldown"`
	MaxRecentFailures        int           `yaml:"max_recent_failures"`
	PromoteMaxCostRatio      float64       `yaml:"promote_max_cost_ratio"`
	CostOnlyMinSaving        float64       `yaml:"cost_only_min_saving"`

	// Archival.
	UnusedAfter       time.Duration `yaml:"unused_after"`
	FailingRate       float64       `yaml:"failing_rate"`
	FailingMinRuns    int           `yaml:"failing_min_runs"`
	FailingAfterFixes int           `yaml:"failing_after_rejected_fixes"`
	SupersedeMinRuns  int           `yaml:"supersede_min_runs"`
	UnrunnableAfter   time.Duration `yaml:"unrunnable_after"`

	// DailySpendUSD caps what the lifecycle itself spends per day.
	DailySpendUSD float64       `yaml:"daily_spend_usd"`
	SweepEvery    time.Duration `yaml:"sweep_every"`
}

// Defaults are the thresholds ADR-013 records.
func Defaults() Rules {
	day := 24 * time.Hour
	return Rules{
		MinActingCalls: 3, MinTurns: 5, MinConversations: 2, RecurrenceWindow: 14 * day,
		RejectCooldown: 30 * day, MaxDraftsPerShape: 2, MaxTrialCases: 10, MinTrialCases: 3,
		AdmitMinPassRate: 0.8, AdmitMaxInfraError: 1, AdmitMaxUnmarked: 0.2,
		HealthWindow: 20, HealthMinFailures: 3, HotspotShare: 0.5, RemeasureEvery: 7 * day,
		RejectionsBeforeCooldown: 2, EvolveCooldown: 7 * day, MaxRecentFailures: 5,
		PromoteMaxCostRatio: 1.1, CostOnlyMinSaving: 0.1,
		UnusedAfter: 30 * day, FailingRate: 0.5, FailingMinRuns: 10, FailingAfterFixes: 2,
		SupersedeMinRuns: 10, UnrunnableAfter: 7 * day,
		DailySpendUSD: 5, SweepEvery: 15 * time.Minute,
	}
}

// WithDefaults fills every unset threshold from Defaults.
func (r Rules) WithDefaults() Rules {
	d := Defaults()
	fillI := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	fillF := func(v *float64, def float64) {
		if *v == 0 {
			*v = def
		}
	}
	fillD := func(v *time.Duration, def time.Duration) {
		if *v == 0 {
			*v = def
		}
	}
	fillI(&r.MinActingCalls, d.MinActingCalls)
	fillI(&r.MinTurns, d.MinTurns)
	fillI(&r.MinConversations, d.MinConversations)
	fillD(&r.RecurrenceWindow, d.RecurrenceWindow)
	fillD(&r.RejectCooldown, d.RejectCooldown)
	fillI(&r.MaxDraftsPerShape, d.MaxDraftsPerShape)
	fillI(&r.MaxTrialCases, d.MaxTrialCases)
	fillI(&r.MinTrialCases, d.MinTrialCases)
	fillF(&r.AdmitMinPassRate, d.AdmitMinPassRate)
	fillI(&r.AdmitMaxInfraError, d.AdmitMaxInfraError)
	fillF(&r.AdmitMaxUnmarked, d.AdmitMaxUnmarked)
	fillI(&r.HealthWindow, d.HealthWindow)
	fillI(&r.HealthMinFailures, d.HealthMinFailures)
	fillF(&r.HotspotShare, d.HotspotShare)
	fillD(&r.RemeasureEvery, d.RemeasureEvery)
	fillI(&r.RejectionsBeforeCooldown, d.RejectionsBeforeCooldown)
	fillD(&r.EvolveCooldown, d.EvolveCooldown)
	fillI(&r.MaxRecentFailures, d.MaxRecentFailures)
	fillF(&r.PromoteMaxCostRatio, d.PromoteMaxCostRatio)
	fillF(&r.CostOnlyMinSaving, d.CostOnlyMinSaving)
	fillD(&r.UnusedAfter, d.UnusedAfter)
	fillF(&r.FailingRate, d.FailingRate)
	fillI(&r.FailingMinRuns, d.FailingMinRuns)
	fillI(&r.FailingAfterFixes, d.FailingAfterFixes)
	fillI(&r.SupersedeMinRuns, d.SupersedeMinRuns)
	fillD(&r.UnrunnableAfter, d.UnrunnableAfter)
	fillF(&r.DailySpendUSD, d.DailySpendUSD)
	fillD(&r.SweepEvery, d.SweepEvery)
	return r
}

// Decision is what a rule concluded, and on what.
type Decision struct {
	Accept  bool           `json:"accept"`
	Rule    string         `json:"rule"`
	Figures map[string]any `json:"figures"`
}

// ---- shape and recurrence --------------------------------------------------

// readOnly tools are left out of a shape: exploration differs from run to run
// even when the work is the same.
var readOnly = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "LS": true, "WebFetch": true, "WebSearch": true,
	"TodoWrite": true, "NotebookRead": true, "BashOutput": true, "ToolSearch": true,
}

// ToolCall is one recorded tool use: its name and the preview of its input.
type ToolCall struct {
	Name  string
	Input string
}

// Shape is the structure of what a turn did: its acting tool calls in order,
// reads left out, a shell call counted by its program, consecutive repeats
// collapsed. It is computed from what the agent did, never from what anybody
// wrote (SPEC-004).
func Shape(calls []ToolCall) string {
	var parts []string
	for _, c := range calls {
		if readOnly[c.Name] {
			continue
		}
		part := c.Name
		if c.Name == "Bash" {
			if prog := shellProgram(c.Input); prog != "" {
				part = "Bash:" + prog
			}
		}
		if len(parts) > 0 && parts[len(parts)-1] == part {
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " > ")
}

// ActingCalls counts the distinct entries of a shape.
func ActingCalls(shape string) int {
	if shape == "" {
		return 0
	}
	return len(strings.Split(shape, " > "))
}

// shellProgram reads the program a Bash call ran from its input preview, which
// is the JSON of the tool input ({"command":"go test ./...", ...}).
func shellProgram(input string) string {
	i := strings.Index(input, `"command"`)
	if i < 0 {
		return ""
	}
	rest := input[i+len(`"command"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	prog := strings.Trim(fields[0], `"\`)
	if k := strings.LastIndex(prog, "/"); k >= 0 {
		prog = prog[k+1:]
	}
	return prog
}

// Turn is one finished agent turn as recurrence mining sees it.
type Turn struct {
	Agent        string    `json:"agent"`
	TurnID       int64     `json:"turn_id"`
	Conversation string    `json:"conversation"`
	Shape        string    `json:"shape"`
	Request      string    `json:"request"`
	Result       string    `json:"result"`
	CostUSD      float64   `json:"cost_usd"`
	At           time.Time `json:"at"`
}

// Cluster is work that recurred: turns that share one shape.
type Cluster struct {
	Shape string `json:"shape"`
	Turns []Turn `json:"turns"`
}

// Recurring returns the shapes worth drafting a routine from: enough acting
// calls, enough successful turns from enough conversations inside the window,
// and neither covered by a routine nor rejected recently. Turns passed in must
// already be the successful ones.
func Recurring(turns []Turn, now time.Time, skip map[string]bool, r Rules) []Cluster {
	byShape := map[string][]Turn{}
	for _, t := range turns {
		if t.Shape == "" || now.Sub(t.At) > r.RecurrenceWindow || skip[t.Shape] {
			continue
		}
		if ActingCalls(t.Shape) < r.MinActingCalls {
			continue
		}
		byShape[t.Shape] = append(byShape[t.Shape], t)
	}
	shapes := make([]string, 0, len(byShape))
	for s := range byShape {
		shapes = append(shapes, s)
	}
	sort.Strings(shapes)
	var out []Cluster
	for _, s := range shapes {
		ts := byShape[s]
		convs := map[string]bool{}
		for _, t := range ts {
			convs[t.Agent+"/"+t.Conversation] = true
		}
		if len(ts) < r.MinTurns || len(convs) < r.MinConversations {
			continue
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].At.After(ts[j].At) })
		if len(ts) > r.MaxTrialCases {
			ts = ts[:r.MaxTrialCases]
		}
		out = append(out, Cluster{Shape: s, Turns: ts})
	}
	return out
}

// ---- admission -------------------------------------------------------------

// CaseResult is one trial case, scored by the examiner's checks on the trial
// output and on the recorded outcome of the work it came from.
type CaseResult struct {
	Name       string  `json:"name"`
	Passed     bool    `json:"passed"`
	Unmarked   bool    `json:"unmarked"`    // the examiner could not write checks
	InfraError bool    `json:"infra_error"` // the trial run could not be judged at all
	CostUSD    float64 `json:"cost_usd"`
	// Baseline is the same checks applied to the recorded outcome; HasBaseline
	// is false for a submitted routine, which has no recorded work behind it.
	HasBaseline    bool    `json:"has_baseline"`
	BaselinePassed bool    `json:"baseline_passed"`
	BaselineCost   float64 `json:"baseline_cost_usd"`
}

// Admit decides an admission trial: the candidate must have done the work at
// least as well as it was done before, and for no more than it cost.
func Admit(cases []CaseResult, r Rules) Decision {
	f := map[string]any{"cases": len(cases)}
	if len(cases) < r.MinTrialCases {
		return Decision{Rule: fmt.Sprintf("fewer than %d trial cases", r.MinTrialCases), Figures: f}
	}
	var passed, marked, unmarked, infra, basePassed, baseMarked int
	var costs, baseCosts []float64
	for _, c := range cases {
		switch {
		case c.InfraError:
			infra++
		case c.Unmarked:
			unmarked++
		default:
			marked++
			if c.Passed {
				passed++
			}
			if c.HasBaseline {
				baseMarked++
				if c.BaselinePassed {
					basePassed++
				}
			}
		}
		if !c.InfraError {
			costs = append(costs, c.CostUSD)
		}
		if c.HasBaseline {
			baseCosts = append(baseCosts, c.BaselineCost)
		}
	}
	rate := ratio(passed, marked)
	baseRate := ratio(basePassed, baseMarked)
	f["passed"], f["marked"], f["unmarked"], f["infra_errors"] = passed, marked, unmarked, infra
	f["pass_rate"], f["baseline_pass_rate"] = rate, baseRate
	medCost, medBase := median(costs), median(baseCosts)
	f["median_cost_usd"], f["baseline_median_cost_usd"] = medCost, medBase

	switch {
	case infra > r.AdmitMaxInfraError:
		return Decision{Rule: fmt.Sprintf("more than %d infrastructure errors", r.AdmitMaxInfraError), Figures: f}
	case float64(unmarked) > r.AdmitMaxUnmarked*float64(len(cases)):
		return Decision{Rule: "too many cases the examiner could not mark", Figures: f}
	case marked == 0:
		return Decision{Rule: "no case could be marked", Figures: f}
	case rate < r.AdmitMinPassRate:
		return Decision{Rule: fmt.Sprintf("pass rate %.2f below %.2f", rate, r.AdmitMinPassRate), Figures: f}
	case baseMarked > 0 && rate < baseRate:
		return Decision{Rule: fmt.Sprintf("pass rate %.2f below the work it came from (%.2f)", rate, baseRate), Figures: f}
	case len(baseCosts) > 0 && medCost > medBase:
		return Decision{Rule: fmt.Sprintf("median cost $%.4f above the work it came from ($%.4f)", medCost, medBase), Figures: f}
	}
	return Decision{Accept: true, Rule: "did the work at least as well as before, for no more", Figures: f}
}

// ---- promotion -------------------------------------------------------------

// PairResult is one case run on the current and the candidate version.
type PairResult struct {
	Name            string `json:"name"`
	CurrentPassed   bool   `json:"current_passed"`
	CandidatePassed bool   `json:"candidate_passed"`
	// Targeted marks a recent failure the candidate was drafted to fix.
	Targeted      bool    `json:"targeted"`
	CurrentCost   float64 `json:"current_cost_usd"`
	CandidateCost float64 `json:"candidate_cost_usd"`
}

// Promote decides a version trial with the same arithmetic evaluations use: a
// case that passed on the current version and fails on the candidate is a
// regression, and any regression refuses the version.
func Promote(pairs []PairResult, r Rules) Decision {
	var regressions, fixed, targeted, curPassed, candPassed []string
	var curCost, candCost float64
	for _, p := range pairs {
		curCost += p.CurrentCost
		candCost += p.CandidateCost
		if p.CurrentPassed {
			curPassed = append(curPassed, p.Name)
		}
		if p.CandidatePassed {
			candPassed = append(candPassed, p.Name)
		}
		if p.Targeted {
			targeted = append(targeted, p.Name)
			if p.CandidatePassed {
				fixed = append(fixed, p.Name)
			}
			continue
		}
		if p.CurrentPassed && !p.CandidatePassed {
			regressions = append(regressions, p.Name)
		}
	}
	curPer := perPassed(curCost, len(curPassed))
	candPer := perPassed(candCost, len(candPassed))
	f := map[string]any{
		"cases": len(pairs), "regressions": regressions, "targeted": len(targeted), "fixed": fixed,
		"current_cost_per_passed_usd": curPer, "candidate_cost_per_passed_usd": candPer,
	}
	if len(regressions) > 0 {
		return Decision{Rule: fmt.Sprintf("regressed on %s", strings.Join(regressions, ", ")), Figures: f}
	}
	if len(candPassed) == 0 {
		return Decision{Rule: "passed nothing", Figures: f}
	}
	if curPer > 0 && candPer > curPer*r.PromoteMaxCostRatio {
		return Decision{Rule: fmt.Sprintf("one success costs $%.4f, above %.0f%% of $%.4f", candPer, r.PromoteMaxCostRatio*100, curPer), Figures: f}
	}
	switch {
	case len(fixed) > 0:
		return Decision{Accept: true, Rule: fmt.Sprintf("no regression, fixed %s", strings.Join(fixed, ", ")), Figures: f}
	case len(targeted) == 0 && curPer > 0 && candPer <= curPer*(1-r.CostOnlyMinSaving):
		return Decision{Accept: true, Rule: "no regression, cheaper per success", Figures: f}
	}
	return Decision{Rule: "fixed none of the failures it was drafted for", Figures: f}
}

// ---- health ----------------------------------------------------------------

// RunSummary is one finished run of the current version, newest first.
type RunSummary struct {
	ID          int64  `json:"id"`
	Passed      bool   `json:"passed"`
	FailedStep  string `json:"failed_step,omitempty"`
	Redone      bool   `json:"redone"`
	CallerUnmet bool   `json:"caller_unmet"`
}

// NotPassed reports whether a run counts against the routine: it failed, a
// caller redid it, or it did not meet its caller's conditions.
func (s RunSummary) NotPassed() bool { return !s.Passed || s.Redone || s.CallerUnmet }

// Hotspot decides whether failures have gathered at one step enough to draft a
// fix for it. It returns the step, or "" when they have not.
func Hotspot(runs []RunSummary, r Rules) (string, map[string]any) {
	if len(runs) > r.HealthWindow {
		runs = runs[:r.HealthWindow]
	}
	counts := map[string]int{}
	var failing []int64
	for _, s := range runs {
		if !s.NotPassed() {
			continue
		}
		failing = append(failing, s.ID)
		if s.FailedStep != "" {
			counts[s.FailedStep]++
		}
	}
	f := map[string]any{"window": len(runs), "not_passed": len(failing), "runs": failing}
	if len(failing) < r.HealthMinFailures {
		return "", f
	}
	steps := make([]string, 0, len(counts))
	for s := range counts {
		steps = append(steps, s)
	}
	sort.Strings(steps)
	best, bestN := "", 0
	for _, s := range steps {
		if counts[s] > bestN {
			best, bestN = s, counts[s]
		}
	}
	share := ratio(bestN, len(failing))
	f["step"], f["share"] = best, share
	if best == "" || share < r.HotspotShare {
		return "", f
	}
	return best, f
}

// ---- archival --------------------------------------------------------------

// ArchiveFacts are what archival looks at for one active routine.
type ArchiveFacts struct {
	Now             time.Time
	CreatedAt       time.Time
	LastRunAt       time.Time // zero when it never ran
	Runs            int       // finished runs of the current version
	PassRate        *float64  // callers' where stated enough, else independent
	RejectedFixes   int       // consecutive rejected versions
	UnrunnableSince time.Time // zero while it can run
	// Rival is the best other active routine with the same contract, if any.
	Rival *RivalFacts
	Own   RivalFacts
}

// RivalFacts compare two routines with one contract.
type RivalFacts struct {
	ID               string
	Runs             int
	IndependentRate  *float64
	CostPerPassedUSD *float64
}

// Archive decides whether a routine has stopped earning its place.
func Archive(f ArchiveFacts, r Rules) Decision {
	fig := map[string]any{"runs": f.Runs, "rejected_fixes": f.RejectedFixes}
	last := f.LastRunAt
	if last.IsZero() {
		last = f.CreatedAt
	}
	if f.Now.Sub(last) >= r.UnusedAfter {
		fig["idle_days"] = int(f.Now.Sub(last).Hours() / 24)
		return Decision{Accept: true, Rule: fmt.Sprintf("unused for %s", r.UnusedAfter), Figures: fig}
	}
	if !f.UnrunnableSince.IsZero() && f.Now.Sub(f.UnrunnableSince) >= r.UnrunnableAfter {
		return Decision{Accept: true, Rule: fmt.Sprintf("unable to run for %s", r.UnrunnableAfter), Figures: fig}
	}
	if f.PassRate != nil {
		fig["pass_rate"] = *f.PassRate
		if f.Runs >= r.FailingMinRuns && *f.PassRate < r.FailingRate && f.RejectedFixes >= r.FailingAfterFixes {
			return Decision{Accept: true, Rule: fmt.Sprintf("passing %.2f over %d runs after %d rejected fixes", *f.PassRate, f.Runs, f.RejectedFixes), Figures: fig}
		}
	}
	if rv := f.Rival; rv != nil && f.Own.Runs >= r.SupersedeMinRuns && rv.Runs >= r.SupersedeMinRuns &&
		rv.IndependentRate != nil && f.Own.IndependentRate != nil &&
		rv.CostPerPassedUSD != nil && f.Own.CostPerPassedUSD != nil &&
		*rv.IndependentRate >= *f.Own.IndependentRate && *rv.CostPerPassedUSD <= *f.Own.CostPerPassedUSD &&
		(*rv.IndependentRate > *f.Own.IndependentRate || *rv.CostPerPassedUSD < *f.Own.CostPerPassedUSD) {
		fig["superseded_by"] = rv.ID
		return Decision{Accept: true, Rule: "superseded by " + rv.ID + ", which keeps the same promise better", Figures: fig}
	}
	return Decision{Rule: "still earning its place", Figures: fig}
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func perPassed(cost float64, passed int) float64 {
	if passed == 0 {
		return 0
	}
	return cost / float64(passed)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}
