package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/lifecycle"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/store"
)

// The routine lifecycle (adr/013). Agents propose — the steward drafts a
// routine from recurring work, or a fix for a step failures gather at — and
// arithmetic decides: every admission, promotion, rejection and archive here is
// a lifecycle rule applied to recorded figures, and is recorded with them.

// StewardConversation is where the steward is woken with lifecycle work, kept
// apart from anything it is asked by people.
const StewardConversation = "routine-lifecycle"

// SweepResult names the trials a sweep created, for the lifecycle workflow to
// run.
type SweepResult struct {
	Trials []int64  `json:"trials,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// LifecycleSweep is one pass of the lifecycle: find recurring work, notice
// routines whose failures gather at one step, re-measure routines that are due
// or whose dependencies moved, and archive routines that stopped earning their
// place. Past the daily spend cap it drafts and measures nothing, so the
// lifecycle cannot spend without bound; archival costs nothing and still runs.
func (a *Activities) LifecycleSweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	rules := a.cfg.Routines.Lifecycle
	spent, err := a.reg.LifecycleSpendToday(ctx)
	if err != nil {
		return res, err
	}
	withinBudget := spent < rules.DailySpendUSD
	if !withinBudget {
		res.Notes = append(res.Notes, fmt.Sprintf("daily lifecycle spend $%.2f reached the cap $%.2f", spent, rules.DailySpendUSD))
	}

	if withinBudget && a.cfg.Routines.Steward != "" {
		if err := a.mineRecurrence(ctx, &res); err != nil {
			return res, err
		}
	}

	routines, err := a.reg.ListRoutines(ctx, true)
	if err != nil {
		return res, err
	}
	for _, rt := range routines {
		if err := a.checkRunnable(ctx, rt); err != nil {
			return res, err
		}
		archived, err := a.maybeArchive(ctx, rt)
		if err != nil {
			return res, err
		}
		if archived || !withinBudget {
			continue
		}
		if err := a.maybeEvolve(ctx, rt, &res); err != nil {
			return res, err
		}
		if id, err := a.maybeRemeasure(ctx, rt); err != nil {
			return res, err
		} else if id != 0 {
			res.Trials = append(res.Trials, id)
		}
	}
	return res, nil
}

// mineRecurrence turns work that keeps succeeding the same way into candidates
// and wakes the steward to draft a routine from each.
func (a *Activities) mineRecurrence(ctx context.Context, res *SweepResult) error {
	rules := a.cfg.Routines.Lifecycle
	since := time.Now().Add(-rules.RecurrenceWindow)
	agents, err := a.reg.List(ctx)
	if err != nil {
		return err
	}
	var turns []lifecycle.Turn
	for _, ag := range agents {
		if ag.ID == a.cfg.Routines.Steward {
			continue // the steward's own drafting is not work to draft from
		}
		st, err := a.stores.Get(ag.ID)
		if err != nil {
			return fmt.Errorf("open %s store: %w", ag.ID, err)
		}
		shaped, err := st.SucceededWithShape(ctx, since, 500)
		if err != nil {
			return err
		}
		for _, t := range shaped {
			turns = append(turns, lifecycle.Turn{
				Agent: ag.ID, TurnID: t.ID, Conversation: t.Conversation, Shape: t.Shape,
				Request: t.Request, Result: t.Result, CostUSD: t.CostUSD, At: t.FinishedAt,
			})
		}
	}
	skip, err := a.reg.ShapesToSkip(ctx, rules.RejectCooldown)
	if err != nil {
		return err
	}
	for _, cl := range lifecycle.Recurring(turns, time.Now(), skip, rules) {
		cand, err := a.reg.CreateCandidate(ctx, cl.Shape, cl.Turns)
		if err != nil {
			return err
		}
		if err := a.wakeSteward(ctx, fmt.Sprintf("candidate:%d:%d", cand.ID, cand.Drafts), candidatePrompt(cand, cl, "")); err != nil {
			return err
		}
		res.Notes = append(res.Notes, fmt.Sprintf("candidate %d from %d turns shaped %q", cand.ID, len(cl.Turns), cl.Shape))
	}
	return nil
}

// wakeSteward queues lifecycle work for the steward, once per key.
func (a *Activities) wakeSteward(ctx context.Context, key, prompt string) error {
	if a.cfg.Routines.Steward == "" {
		return nil
	}
	// A steward that is not registered is a configuration mistake, not a reason
	// to stop the lifecycle: trials, re-measurement and archival go on.
	if _, err := a.reg.Get(ctx, a.cfg.Routines.Steward); errors.Is(err, registry.ErrNotFound) {
		activity.GetLogger(ctx).Warn("the configured steward is not a registered agent", "steward", a.cfg.Routines.Steward)
		return nil
	}
	return a.wakeAgent(ctx, a.cfg.Routines.Steward, StewardConversation, key, prompt)
}

func candidatePrompt(c registry.Candidate, cl lifecycle.Cluster, rejection string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[routine candidate %d] The same work has succeeded %d times across conversations, doing: %s\n\n",
		c.ID, len(cl.Turns), cl.Shape)
	if rejection != "" {
		fmt.Fprintf(&b, "Your previous draft was rejected by its trial: %s\nRedraft it once more, or leave it.\n\n", rejection)
	}
	b.WriteString("The work it came from (newest first):\n")
	for _, t := range cl.Turns {
		fmt.Fprintf(&b, "- agent %s · turn %d · $%.4f\n  asked: %s\n  answered: %s\n",
			t.Agent, t.TurnID, t.CostUSD, oneLine(t.Request, 300), oneLine(t.Result, 300))
	}
	fmt.Fprintf(&b, "\nDraft a routine that does this work, following the roundclaw-routine-steward skill, and submit it with one case per turn above:\n"+
		"POST /v1/routine-candidates/%d/draft  {\"routine\": {...}, \"cases\": [{\"agent\": \"...\", \"turn_id\": N, \"inputs\": {...}}]}\n"+
		"It is tried on that work and admitted only if it does it at least as well, for no more. If this is not worth a routine, do nothing.", c.ID)
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// checkRunnable records when a routine became unable to run, so archival can
// wait out a grace period from that moment.
func (a *Activities) checkRunnable(ctx context.Context, rt registry.Routine) error {
	runnable := true
	for _, st := range rt.Plan.Source.Steps {
		if st.Kind != routine.KindAgent {
			continue
		}
		ag, err := a.reg.Get(ctx, st.Agent)
		if errors.Is(err, registry.ErrNotFound) || (err == nil && !ag.Enabled) {
			runnable = false
			break
		}
		if err != nil {
			return err
		}
	}
	if runnable == rt.UnrunnableSince.IsZero() {
		return nil
	}
	return a.reg.SetRoutineUnrunnable(ctx, rt.ID, !runnable)
}

func (a *Activities) maybeArchive(ctx context.Context, rt registry.Routine) (bool, error) {
	rules := a.cfg.Routines.Lifecycle
	cur, _, err := a.reg.RoutineMetrics(ctx, rt.ID, rt.Version)
	if err != nil {
		return false, err
	}
	last, err := a.reg.LastRunAt(ctx, rt.ID)
	if err != nil {
		return false, err
	}
	rejections, _, err := a.reg.RecentRejections(ctx, rt.ID)
	if err != nil {
		return false, err
	}
	facts := lifecycle.ArchiveFacts{
		Now: time.Now(), CreatedAt: rt.CreatedAt, LastRunAt: last, Runs: cur.Runs,
		RejectedFixes: rejections, UnrunnableSince: rt.UnrunnableSince,
		Own: rivalFacts(rt, cur),
	}
	switch {
	case cur.CallerStated >= 5 && cur.CallerMetRate != nil:
		facts.PassRate = cur.CallerMetRate
	case rt.IndependentTotal > 0:
		rate := float64(rt.IndependentPassed) / float64(rt.IndependentTotal)
		facts.PassRate = &rate
	}
	rivals, err := a.reg.FindActiveByContract(ctx, rt.ContractHash, rt.ID)
	if err != nil {
		return false, err
	}
	for _, id := range rivals {
		other, err := a.reg.GetRoutine(ctx, id)
		if err != nil {
			return false, err
		}
		oc, _, err := a.reg.RoutineMetrics(ctx, id, other.Version)
		if err != nil {
			return false, err
		}
		rf := rivalFacts(other, oc)
		if facts.Rival == nil || better(rf, *facts.Rival) {
			facts.Rival = &rf
		}
	}
	d := lifecycle.Archive(facts, rules)
	if !d.Accept {
		return false, nil
	}
	if err := a.reg.SetRoutineState(ctx, rt.ID, registry.RoutineArchived, d.Rule, d.Figures, "lifecycle"); err != nil {
		return false, err
	}
	activity.GetLogger(ctx).Info("archived a routine", "routine", rt.ID, "rule", d.Rule)
	return true, nil
}

func rivalFacts(rt registry.Routine, f registry.RoutineFigures) lifecycle.RivalFacts {
	rf := lifecycle.RivalFacts{ID: rt.ID, Runs: f.Runs, CostPerPassedUSD: f.CostPerPassed}
	if rt.IndependentTotal > 0 {
		rate := float64(rt.IndependentPassed) / float64(rt.IndependentTotal)
		rf.IndependentRate = &rate
	}
	return rf
}

func better(a, b lifecycle.RivalFacts) bool {
	if a.IndependentRate == nil {
		return false
	}
	if b.IndependentRate == nil {
		return true
	}
	return *a.IndependentRate > *b.IndependentRate
}

// maybeEvolve wakes the steward when a routine's failures gather at one step.
func (a *Activities) maybeEvolve(ctx context.Context, rt registry.Routine, res *SweepResult) error {
	rules := a.cfg.Routines.Lifecycle
	if rt.PendingVersion != 0 || a.cfg.Routines.Steward == "" {
		return nil
	}
	n, last, err := a.reg.RecentRejections(ctx, rt.ID)
	if err != nil {
		return err
	}
	if n >= rules.RejectionsBeforeCooldown && time.Since(last) < rules.EvolveCooldown {
		return nil
	}
	runs, err := a.reg.ListRoutineRuns(ctx, registry.RunFilter{RoutineID: rt.ID, Limit: 200})
	if err != nil {
		return err
	}
	redone := map[int64]bool{}
	for _, r := range runs {
		if r.InsteadOf != 0 {
			redone[r.InsteadOf] = true
		}
	}
	var summaries []lifecycle.RunSummary
	var failing []registry.RoutineRun
	for _, r := range runs {
		if r.Version != rt.Version || r.Status == registry.RunRunning || strings.HasPrefix(r.RequestedBy, registry.TrialRequester) {
			continue
		}
		s := lifecycle.RunSummary{ID: r.ID, Passed: r.Status == registry.RunPassed, FailedStep: r.FailedStep,
			Redone: redone[r.ID], CallerUnmet: !r.Expect.Empty() && !routine.Held(r.ExpectVerdicts)}
		summaries = append(summaries, s)
		if s.NotPassed() {
			failing = append(failing, r)
		}
	}
	step, figures := lifecycle.Hotspot(summaries, rules)
	if step == "" {
		return nil
	}
	key := fmt.Sprintf("evolve:%s:v%d:%s", rt.ID, rt.Version, step)
	if err := a.wakeSteward(ctx, key, evolvePrompt(rt, step, figures, failing)); err != nil {
		return err
	}
	res.Notes = append(res.Notes, fmt.Sprintf("failures of %s gather at %q", rt.ID, step))
	return nil
}

func evolvePrompt(rt registry.Routine, step string, figures map[string]any, failing []registry.RoutineRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[routine %s v%d] Failures are gathering at step %q (%v of the last %v runs did not pass).\n\n",
		rt.ID, rt.Version, step, figures["not_passed"], figures["window"])
	b.WriteString("Recent failures:\n")
	for i, r := range failing {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "- run %d: %s %s\n", r.ID, r.FailedStep, oneLine(r.Reason, 300))
	}
	fmt.Fprintf(&b, "\nRead each failing step's verdicts (GET /v1/routine-runs/<run>) and draft a fix to %q only, following the "+
		"roundclaw-routine-steward skill. Submit it with PUT /v1/routines/%s. It serves only if it does no worse on the "+
		"routine's cases and fixes at least one of these failures. The contract cannot change.", step, rt.ID)
	return b.String()
}

// maybeRemeasure creates a re-measurement when a routine is due, or when an
// agent it uses has moved since it was last measured.
func (a *Activities) maybeRemeasure(ctx context.Context, rt registry.Routine) (int64, error) {
	cases, err := a.reg.ListRoutineCases(ctx, rt.ID)
	if err != nil || len(cases) == 0 {
		return 0, err
	}
	manifest, err := a.reg.RoutineManifest(ctx, rt.Plan)
	if err != nil {
		return 0, err
	}
	due := time.Since(rt.MeasuredAt) >= a.cfg.Routines.Lifecycle.RemeasureEvery
	moved := !sameManifest(manifest, rt.MeasuredManifest)
	if !due && !moved {
		return 0, nil
	}
	trials, err := a.reg.ListTrials(ctx, rt.ID, 5)
	if err != nil {
		return 0, err
	}
	for _, t := range trials {
		if t.State == registry.TrialRunning {
			return 0, nil
		}
	}
	tcs := make([]registry.TrialCase, 0, len(cases))
	for _, c := range cases {
		tcs = append(tcs, registry.TrialCase{Name: c.Name, Inputs: c.Inputs, Checks: c.Checks, Source: c.Source})
	}
	t, err := a.reg.CreateTrial(ctx, registry.Trial{Kind: registry.TrialRemeasure, RoutineID: rt.ID, Version: rt.Version, Cases: tcs})
	if err != nil {
		return 0, err
	}
	return t.ID, nil
}

func sameManifest(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// LoadTrial reads a trial.
func (a *Activities) LoadTrial(ctx context.Context, id int64) (registry.Trial, error) {
	t, err := a.reg.GetTrial(ctx, id)
	if err != nil {
		return t, newNonRetryable(err)
	}
	return t, nil
}

// ExamineInput is one trial case to write checks for.
type ExamineInput struct {
	TrialID int64  `json:"trial_id"`
	Case    string `json:"case"`
	Request string `json:"request"`
	Outcome string `json:"outcome"`
}

// ExamineResult is the examiner's checks for a case, or why there are none.
type ExamineResult struct {
	Checks   routine.Condition `json:"checks"`
	Unmarked bool              `json:"unmarked"`
	Reason   string            `json:"reason,omitempty"`
	CostUSD  float64           `json:"cost_usd"`
}

// ExamineCase writes the checks a trial case is marked by. It is a model call
// roundclaw runs itself, like the evaluation judge: a fixed prompt, no tools,
// and — the point of it — no sight of the routine being tried. Its checks come
// from the request and the recorded outcome of the work alone, so the routine's
// author never sets the standard it is admitted by (adr/012, adr/013). The
// prompt is code, outside anything an agent can edit.
func (a *Activities) ExamineCase(ctx context.Context, in ExamineInput) (ExamineResult, error) {
	stop := heartbeatUntilDone(ctx)
	defer stop()
	cred, err := a.cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		return ExamineResult{}, newNonRetryable(err)
	}
	dir := filepath.Join(a.cfg.WorkspaceRoot, "routines", "trials", fmt.Sprint(in.TrialID), "examiner")
	for _, d := range []string{dir, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return ExamineResult{}, fmt.Errorf("create examiner dir: %w", err)
		}
	}
	model := a.cfg.Router.Model
	if model == "" {
		model = a.cfg.Container.Model
	}
	spec := claude.RunSpec{
		Runtime: a.cfg.Container.Runtime, Image: a.cfg.Container.Image,
		ContainerName: fmt.Sprintf("roundclaw-exam-%d-%s", in.TrialID, slug(in.Case)),
		WorkDir:       dir, ClaudeHome: filepath.Join(dir, "home"),
		CredentialEnv: cred.EnvName, CredentialValue: cred.Value,
		SessionID:       claude.SessionID(fmt.Sprintf("examine-%d-%s", in.TrialID, in.Case)),
		PermissionMode:  "bypassPermissions",
		DisallowedTools: judgeDeniedTools,
		Model:           model,
		Prompt:          examinePrompt(in),
	}
	st, err := store.Open(filepath.Join(filepath.Dir(dir), "state.db"), fmt.Sprintf("trial-%d", in.TrialID), store.ReadWrite)
	if err != nil {
		return ExamineResult{}, fmt.Errorf("open trial store: %w", err)
	}
	defer st.Close()
	turnID, _, err := st.CreateTurn(ctx, store.NewTurn{
		Request: "examine " + in.Case, Origin: core.HTTPPollOrigin(), Conversation: "examiner",
		IdempotencyKey: "examine:" + in.Case,
	})
	if err != nil {
		return ExamineResult{}, fmt.Errorf("record examiner turn: %w", err)
	}
	args, err := spec.Args()
	if err != nil {
		_ = st.FinishTurn(context.WithoutCancel(ctx), turnID, errorResult(turnID, err))
		return ExamineResult{}, newNonRetryable(err)
	}
	a.removeOrphan(ctx, spec)
	result, _, streamErr := a.stream(ctx, st, spec, args, RunTurnInput{TurnID: turnID})
	_ = st.FinishTurn(context.WithoutCancel(ctx), turnID, result)
	if streamErr != nil {
		return ExamineResult{}, streamErr
	}
	out := ExamineResult{CostUSD: result.CostUSD}
	if result.Status == core.TurnError {
		out.Unmarked, out.Reason = true, "the examiner could not run: "+result.ErrorMessage
		return out, nil
	}
	checks, ok := parseChecks(result.Text)
	if !ok || checks.Empty() {
		out.Unmarked, out.Reason = true, "the examiner returned no exact checks"
		return out, nil
	}
	out.Checks = checks
	return out, nil
}

func examinePrompt(in ExamineInput) string {
	return "You are writing the checks that decide whether a piece of work was done well. " +
		"You will not see who does it or how. Write only exact, mechanical checks: text a good answer to this request " +
		"must contain, and text it must not contain. Take them from what the request asked for and what the recorded " +
		"good outcome actually delivered — facts, names, markers that any correct answer would include. " +
		"Do not check wording or style. Two to five checks is right.\n\n" +
		"## The request\n" + in.Request + "\n\n" +
		"## A recorded outcome that was accepted\n" + in.Outcome + "\n\n" +
		"Reply with ONLY a JSON object, no prose and no code fence:\n" +
		`{"must_contain": ["..."], "must_not_contain": ["..."]}`
}

func parseChecks(text string) (routine.Condition, bool) {
	raw := strings.TrimSpace(text)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var c routine.Condition
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return routine.Condition{}, false
	}
	c.ExitCode = nil
	return c, true
}

// SaveTrialCases records examiner checks and run ids on a trial as it goes.
type SaveTrialCasesInput struct {
	TrialID int64                `json:"trial_id"`
	Cases   []registry.TrialCase `json:"cases"`
	CostUSD float64              `json:"cost_usd"`
}

// SaveTrialCases writes a trial's cases and adds what examining cost to the
// lifecycle's spend.
func (a *Activities) SaveTrialCases(ctx context.Context, in SaveTrialCasesInput) error {
	if err := a.reg.SetTrialCases(ctx, in.TrialID, in.Cases); err != nil {
		return err
	}
	if in.CostUSD > 0 {
		t, err := a.reg.GetTrial(ctx, in.TrialID)
		if err != nil {
			return err
		}
		return a.reg.AddLifecycleSpend(ctx, t.RoutineID, in.CostUSD)
	}
	return nil
}

// TrialRunInput starts one run of a trial case on one version.
type TrialRunInput struct {
	TrialID int64             `json:"trial_id"`
	Case    string            `json:"case"`
	Version int               `json:"version"`
	Inputs  map[string]string `json:"inputs"`
}

// StartTrialRun writes a trial run's row. A trial's runs name the trial as their
// parent — by its negated id, so they can never collide with a routine run —
// which makes a retried start find the run it already made, and keeps them from
// delivering anything or counting in the routine's record.
func (a *Activities) StartTrialRun(ctx context.Context, in TrialRunInput) (int64, error) {
	t, err := a.reg.GetTrial(ctx, in.TrialID)
	if err != nil {
		return 0, newNonRetryable(err)
	}
	v, err := a.reg.GetRoutineVersion(ctx, t.RoutineID, in.Version)
	if err != nil {
		return 0, newNonRetryable(err)
	}
	manifest, err := a.reg.RoutineManifest(ctx, v.Plan)
	if err != nil {
		return 0, err
	}
	run, err := a.reg.StartRoutineRun(ctx, registry.RoutineRun{
		RoutineID: t.RoutineID, Version: in.Version, PlanHash: v.PlanHash, Manifest: manifest,
		Inputs: in.Inputs, Origin: core.HTTPPollOrigin(),
		RequestedBy: fmt.Sprintf("%s%d", registry.TrialRequester, t.ID),
		ParentRunID: -t.ID, ParentStep: in.Case, ParentAttempt: in.Version,
	})
	if err != nil {
		return 0, err
	}
	return run.ID, nil
}

// DecideTrial applies the lifecycle's rule to a finished trial and acts on it:
// admits or rejects a routine, promotes or rejects a version, or records a
// re-measurement. The decision and its figures are recorded either way.
func (a *Activities) DecideTrial(ctx context.Context, id int64) (registry.Trial, error) {
	t, err := a.reg.GetTrial(ctx, id)
	if err != nil {
		return t, newNonRetryable(err)
	}
	if t.State != registry.TrialRunning {
		return t, nil
	}
	rules := a.cfg.Routines.Lifecycle
	runs := map[int64]registry.RoutineRun{}
	var spend float64
	for _, c := range t.Cases {
		for _, runID := range c.Runs {
			r, err := a.reg.GetRoutineRun(ctx, runID)
			if err != nil {
				return t, err
			}
			runs[runID] = r
			spend += r.CostUSD
		}
	}
	if err := a.reg.AddLifecycleSpend(ctx, t.RoutineID, spend); err != nil {
		return t, err
	}

	switch t.Kind {
	case registry.TrialAdmission:
		return a.decideAdmission(ctx, t, runs, rules)
	case registry.TrialVersion:
		return a.decideVersion(ctx, t, runs, rules)
	case registry.TrialRemeasure:
		return a.decideRemeasure(ctx, t, runs)
	}
	return t, newNonRetryable(fmt.Errorf("unknown trial kind %q", t.Kind))
}

// passedChecks is whether a trial run did the work, by the case's own checks.
func passedChecks(c registry.TrialCase, r registry.RoutineRun) bool {
	if r.Status != registry.RunPassed {
		return false
	}
	return routine.Held(routine.Check(c.Checks, r.Output, nil))
}

func (a *Activities) decideAdmission(ctx context.Context, t registry.Trial, runs map[int64]registry.RoutineRun, rules lifecycle.Rules) (registry.Trial, error) {
	var results []lifecycle.CaseResult
	for _, c := range t.Cases {
		r := runs[c.Runs[t.Version]]
		cr := lifecycle.CaseResult{Name: c.Name, CostUSD: r.CostUSD, Unmarked: c.Unmarked,
			InfraError: r.Status == registry.RunError || r.Status == registry.RunStopped}
		if !c.Unmarked {
			cr.Passed = passedChecks(c, r)
		}
		if c.Source.Outcome != "" {
			cr.HasBaseline = true
			cr.BaselinePassed = routine.Held(routine.Check(c.Checks, c.Source.Outcome, nil))
			cr.BaselineCost = c.Source.CostUSD
		}
		results = append(results, cr)
	}
	d := lifecycle.Admit(results, rules)
	if d.Accept {
		if err := a.reg.PromoteRoutineVersion(ctx, t.RoutineID, t.Version, d.Rule, d.Figures, "lifecycle"); err != nil {
			return t, err
		}
		var cases []registry.RoutineCase
		passed := 0
		for i, c := range t.Cases {
			if c.Unmarked {
				continue
			}
			cases = append(cases, registry.RoutineCase{RoutineID: t.RoutineID, Name: c.Name, Inputs: c.Inputs, Checks: c.Checks, Source: c.Source})
			if results[i].Passed {
				passed++
			}
		}
		if err := a.reg.PutRoutineCases(ctx, cases); err != nil {
			return t, err
		}
		rt, err := a.reg.GetRoutine(ctx, t.RoutineID)
		if err != nil {
			return t, err
		}
		manifest, err := a.reg.RoutineManifest(ctx, rt.Plan)
		if err != nil {
			return t, err
		}
		if err := a.reg.RecordMeasurement(ctx, t.RoutineID, passed, len(cases), manifest); err != nil {
			return t, err
		}
		if t.CandidateID != 0 {
			if err := a.reg.UpdateCandidate(ctx, t.CandidateID, registry.CandidateAdmitted, t.RoutineID, d.Rule, false); err != nil {
				return t, err
			}
		}
		if err := a.reg.FinishTrial(ctx, t.ID, registry.TrialAccepted, d.Rule, d.Figures); err != nil {
			return t, err
		}
		if err := a.wakeSteward(ctx, fmt.Sprintf("trial:%d", t.ID),
			fmt.Sprintf("[routine %s admitted] trial %d: %s. It is now offered in the library.", t.RoutineID, t.ID, d.Rule)); err != nil {
			return t, err
		}
		return a.reg.GetTrial(ctx, t.ID)
	}

	if err := a.reg.RejectRoutineVersion(ctx, t.RoutineID, t.Version, d.Rule, d.Figures); err != nil {
		return t, err
	}
	if err := a.reg.FinishTrial(ctx, t.ID, registry.TrialRejected, d.Rule, d.Figures); err != nil {
		return t, err
	}
	if t.CandidateID != 0 {
		cand, err := a.reg.GetCandidate(ctx, t.CandidateID)
		if err != nil {
			return t, err
		}
		state := registry.CandidateRejected
		if cand.Drafts < rules.MaxDraftsPerShape {
			state = registry.CandidateWaiting
		}
		if err := a.reg.UpdateCandidate(ctx, cand.ID, state, t.RoutineID, d.Rule, false); err != nil {
			return t, err
		}
		if state == registry.CandidateWaiting {
			var turns []lifecycle.Turn
			for _, raw := range cand.Evidence {
				var tu lifecycle.Turn
				if json.Unmarshal(raw, &tu) == nil {
					turns = append(turns, tu)
				}
			}
			cl := lifecycle.Cluster{Shape: cand.Shape, Turns: turns}
			if err := a.wakeSteward(ctx, fmt.Sprintf("candidate:%d:%d", cand.ID, cand.Drafts),
				candidatePrompt(cand, cl, fmt.Sprintf("%s (%v)", d.Rule, compactFigures(d.Figures)))); err != nil {
				return t, err
			}
		}
	}
	return a.reg.GetTrial(ctx, t.ID)
}

func (a *Activities) decideVersion(ctx context.Context, t registry.Trial, runs map[int64]registry.RoutineRun, rules lifecycle.Rules) (registry.Trial, error) {
	var pairs []lifecycle.PairResult
	for _, c := range t.Cases {
		cur, cand := runs[c.Runs[t.BaseVersion]], runs[c.Runs[t.Version]]
		p := lifecycle.PairResult{Name: c.Name, Targeted: c.Targeted, CurrentCost: cur.CostUSD, CandidateCost: cand.CostUSD}
		p.CurrentPassed = passedChecks(c, cur)
		p.CandidatePassed = passedChecks(c, cand)
		pairs = append(pairs, p)
	}
	d := lifecycle.Promote(pairs, rules)
	state := registry.TrialRejected
	if d.Accept {
		state = registry.TrialAccepted
		if err := a.reg.PromoteRoutineVersion(ctx, t.RoutineID, t.Version, d.Rule, d.Figures, "lifecycle"); err != nil {
			return t, err
		}
	} else if err := a.reg.RejectRoutineVersion(ctx, t.RoutineID, t.Version, d.Rule, d.Figures); err != nil {
		return t, err
	}
	if err := a.reg.FinishTrial(ctx, t.ID, state, d.Rule, d.Figures); err != nil {
		return t, err
	}
	verdict := "promoted"
	if !d.Accept {
		verdict = "rejected"
	}
	if err := a.wakeSteward(ctx, fmt.Sprintf("trial:%d", t.ID), fmt.Sprintf(
		"[routine %s v%d %s] trial %d: %s (%v)", t.RoutineID, t.Version, verdict, t.ID, d.Rule, compactFigures(d.Figures))); err != nil {
		return t, err
	}
	return a.reg.GetTrial(ctx, t.ID)
}

func (a *Activities) decideRemeasure(ctx context.Context, t registry.Trial, runs map[int64]registry.RoutineRun) (registry.Trial, error) {
	rt, err := a.reg.GetRoutine(ctx, t.RoutineID)
	if err != nil {
		return t, err
	}
	passed := 0
	var failing []string
	for _, c := range t.Cases {
		if passedChecks(c, runs[c.Runs[t.Version]]) {
			passed++
		} else {
			failing = append(failing, c.Name)
		}
	}
	manifest, err := a.reg.RoutineManifest(ctx, rt.Plan)
	if err != nil {
		return t, err
	}
	previous := rt.IndependentPassed
	if err := a.reg.RecordMeasurement(ctx, t.RoutineID, passed, len(t.Cases), manifest); err != nil {
		return t, err
	}
	figures := map[string]any{"passed": passed, "total": len(t.Cases), "previously_passed": previous, "failing": failing, "manifest": manifest}
	rule := fmt.Sprintf("re-measured: %d of %d independent cases pass", passed, len(t.Cases))
	if err := a.reg.RecordRoutineDecision(ctx, t.RoutineID, "remeasured", rule, figures, "lifecycle"); err != nil {
		return t, err
	}
	if err := a.reg.FinishTrial(ctx, t.ID, registry.TrialAccepted, rule, figures); err != nil {
		return t, err
	}
	// A case that newly fails counts as a failure: the steward hears of it the
	// way it hears of a hotspot.
	if passed < previous {
		if err := a.wakeSteward(ctx, fmt.Sprintf("remeasure:%d", t.ID), fmt.Sprintf(
			"[routine %s v%d regressed on re-measurement] %d of %d independent cases pass, down from %d; failing: %s. "+
				"Something it depends on may have changed. Draft a fix with PUT /v1/routines/%s.",
			t.RoutineID, t.Version, passed, len(t.Cases), previous, strings.Join(failing, ", "), t.RoutineID)); err != nil {
			return t, err
		}
	}
	return a.reg.GetTrial(ctx, t.ID)
}

func compactFigures(f map[string]any) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return oneLine(string(b), 400)
}
