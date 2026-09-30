package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/store"
)

// Routine activities. The interpreter workflow decides what runs next; these do
// the I/O: load the pinned plan, run one attempt of one step, admit an inner
// run, and close the run out. Every step attempt is written to the registry when
// it starts and when it finishes, so the trace is readable while a run is stuck
// and never depends on Temporal's history (adr/006).

// maxStepOutput bounds a step output carried through the workflow for bindings.
// The full output is kept on the step's row.
const maxStepOutput = 32 << 10

// RoutineRunPlan is what a run executes: its row and the plan its version pins.
type RoutineRunPlan struct {
	Run   registry.RoutineRun `json:"run"`
	Plan  routine.Plan        `json:"plan"`
	Owner string              `json:"owner"`
}

// LoadRoutineRun reads a run and the plan of the version it was admitted on.
// Versions are immutable, so the plan a replay sees is the plan the run began
// with.
func (a *Activities) LoadRoutineRun(ctx context.Context, runID int64) (RoutineRunPlan, error) {
	run, err := a.reg.GetRoutineRun(ctx, runID)
	if err != nil {
		return RoutineRunPlan{}, newNonRetryable(err)
	}
	v, err := a.reg.GetRoutineVersion(ctx, run.RoutineID, run.Version)
	if err != nil {
		return RoutineRunPlan{}, newNonRetryable(err)
	}
	r, err := a.reg.GetRoutine(ctx, run.RoutineID)
	if err != nil {
		return RoutineRunPlan{}, newNonRetryable(err)
	}
	return RoutineRunPlan{Run: run, Plan: v.Plan, Owner: r.Owner}, nil
}

// RoutineStepInput is one attempt of one step.
type RoutineStepInput struct {
	RunID    int64             `json:"run_id"`
	Owner    string            `json:"owner"`
	Step     routine.Step      `json:"step"`
	Attempt  int               `json:"attempt"`
	Feedback string            `json:"feedback,omitempty"`
	Inputs   map[string]string `json:"inputs,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"`
	// AgentVersion is the version the run's manifest pinned for Step.Agent.
	AgentVersion int `json:"agent_version,omitempty"`
}

// RoutineStepOutcome is how an attempt ended.
type RoutineStepOutcome struct {
	// Status is passed, failed (its conditions did not hold), or error (it could
	// not be judged at all — the container or the agent failed).
	Status   string            `json:"status"`
	Output   string            `json:"output"`
	Verdicts []routine.Verdict `json:"verdicts,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	CostUSD  float64           `json:"cost_usd"`
}

// RunRoutineStep runs one attempt of an agent or process step and marks it by
// its completion conditions. It records the attempt before anything runs, and
// heartbeats from the start, because preparing a step can outlast the heartbeat
// timeout before any container exists.
func (a *Activities) RunRoutineStep(ctx context.Context, in RoutineStepInput) (RoutineStepOutcome, error) {
	stop := heartbeatUntilDone(ctx)
	defer stop()

	if err := a.reg.StartStepRun(ctx, in.RunID, in.Step.ID, in.Attempt, string(in.Step.Kind), in.Feedback); err != nil {
		return RoutineStepOutcome{}, err
	}
	if err := os.MkdirAll(a.cfg.RoutineWorkDir(in.RunID), 0o750); err != nil {
		return RoutineStepOutcome{}, fmt.Errorf("create run work area: %w", err)
	}

	started := time.Now()
	var (
		row      registry.StepRun
		exitCode *int
		runErr   error
	)
	switch in.Step.Kind {
	case routine.KindAgent:
		row, runErr = a.runAgentStep(ctx, in)
	case routine.KindProcess:
		row, exitCode, runErr = a.runProcessStep(ctx, in)
	default:
		runErr = newNonRetryable(fmt.Errorf("step kind %q does not run as an activity", in.Step.Kind))
	}
	if runErr != nil && ctx.Err() == nil {
		return RoutineStepOutcome{}, runErr
	}

	row.RunID, row.StepID, row.Attempt, row.Kind = in.RunID, in.Step.ID, in.Attempt, string(in.Step.Kind)
	row.Feedback = in.Feedback
	row.ExitCode = exitCode
	row.DurationMS = time.Since(started).Milliseconds()

	out := RoutineStepOutcome{Output: row.Output, CostUSD: row.CostUSD}
	switch {
	case ctx.Err() != nil:
		row.Status = registry.RunStopped
		out.Status, out.Reason = registry.RunStopped, "stopped"
	case row.Error != "":
		row.Status = registry.RunError
		out.Status, out.Reason = registry.RunError, row.Error
	default:
		out.Verdicts = routine.Check(in.Step.Done, row.Output, exitCode)
		row.Verdicts = out.Verdicts
		if routine.Held(out.Verdicts) {
			row.Status, out.Status = registry.RunPassed, registry.RunPassed
		} else {
			row.Status, out.Status = registry.RunFailed, registry.RunFailed
			out.Reason = routine.Shortfall(out.Verdicts)
		}
	}
	if err := a.reg.FinishStepRun(context.WithoutCancel(ctx), row); err != nil {
		return RoutineStepOutcome{}, err
	}
	if len(out.Output) > maxStepOutput {
		out.Output = out.Output[:maxStepOutput]
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, nil
}

// heartbeatUntilDone heartbeats every second until the returned func is called.
// A cancellation reaches an activity only through a heartbeat response.
func heartbeatUntilDone(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

// stepPrompt is what an agent step is told. A task is its instruction; a goal is
// its intent and the conditions that decide it is done, leaving the method to
// the agent. A retry is told why the last attempt fell short.
func stepPrompt(in RoutineStepInput) string {
	var b strings.Builder
	b.WriteString("You are one step of a routine. Work in the current directory, which is this run's work area.\n\n")
	if in.Step.Task != "" {
		b.WriteString("Your task:\n")
		b.WriteString(routine.Render(in.Step.Task, in.Inputs, in.Outputs))
	} else {
		b.WriteString("Your goal:\n")
		b.WriteString(routine.Render(in.Step.Goal, in.Inputs, in.Outputs))
		b.WriteString("\n\nChoose your own method. The step is done only when all of these hold for your final message:\n")
		for _, want := range in.Step.Done.MustContain {
			fmt.Fprintf(&b, "- it contains %q\n", want)
		}
		for _, bad := range in.Step.Done.MustNotContain {
			fmt.Fprintf(&b, "- it does not contain %q\n", bad)
		}
	}
	if in.Feedback != "" {
		fmt.Fprintf(&b, "\n\nAttempt %d. Your previous attempt fell short: %s\nFix that; you can see what you already tried.", in.Attempt, in.Feedback)
	}
	b.WriteString("\n\nEnd with your answer as your final message. Do not ask questions; nobody will answer.")
	return b.String()
}

// runAgentStep runs a one-shot turn of the step's agent at the version the run
// pinned. The step carries no roundclaw credential: it cannot hand work on,
// start a routine or change the fleet, so the graph is the whole of what the
// routine did (adr/009). Credentials reach it only when it declares an external
// effect and runs the routine owner's own agent.
func (a *Activities) runAgentStep(ctx context.Context, in RoutineStepInput) (registry.StepRun, error) {
	var row registry.StepRun
	cred, err := a.cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		return row, newNonRetryable(err)
	}

	agent, persona, err := a.pinnedAgent(ctx, in.Step.Agent, in.AgentVersion)
	if err != nil {
		return row, err
	}

	home := a.cfg.RoutineClaudeHome(in.RunID, in.Step.ID)
	if err := os.MkdirAll(home, 0o750); err != nil {
		return row, fmt.Errorf("create step claude home: %w", err)
	}
	// The persona is the agent's user-level instructions in this step's own
	// home, not a CLAUDE.md in the shared work area, where the next step — often
	// another agent — would read it or overwrite it. Rewritten every attempt.
	if err := os.WriteFile(filepath.Join(home, "CLAUDE.md"), []byte(persona), 0o640); err != nil {
		return row, fmt.Errorf("write persona: %w", err)
	}

	secrets := map[string]string{}
	if in.Step.Effects == routine.EffectExternal && in.Step.Agent == in.Owner {
		if secrets, err = a.reg.SecretsForAgent(ctx, agent.ID); err != nil {
			return row, fmt.Errorf("load secrets for %s: %w", agent.ID, err)
		}
		if secrets == nil {
			secrets = map[string]string{}
		}
	}
	toolDirs, toolNote, err := a.resolveTools(ctx, agent.Tools, secrets)
	if err != nil {
		return row, err
	}
	delete(secrets, cred.EnvName)
	skills, err := a.resolveSkills(ctx, agent.Skills)
	if err != nil {
		return row, err
	}

	image := a.cfg.Container.Image
	if agent.Image != "" {
		image = agent.Image
	}
	model := a.cfg.Container.Model
	if agent.Model != "" {
		model = agent.Model
	}
	permission := agent.PermissionMode
	if permission == "" || permission == "default" {
		permission = "bypassPermissions"
	}

	// One session per (run, step): a retried activity reattaches, and a further
	// attempt resumes it, so the agent remembers what it already tried.
	sessionID := claude.SessionID(fmt.Sprintf("routine-%d-%s", in.RunID, in.Step.ID))
	exists, err := claude.SessionExists(home, sessionID)
	if err != nil {
		return row, fmt.Errorf("check step session: %w", err)
	}
	prompt := stepPrompt(in)
	spec := claude.RunSpec{
		Runtime:         a.cfg.Container.Runtime,
		Image:           image,
		ContainerName:   fmt.Sprintf("roundclaw-rt-%d-%s-%d", in.RunID, slug(in.Step.ID), in.Attempt),
		WorkDir:         a.cfg.RoutineWorkDir(in.RunID),
		DenyPaths:       agent.DenyPaths,
		ClaudeHome:      home,
		AdditionalDirs:  append(append([]string{}, agent.AdditionalDirs...), toolDirs...),
		CredentialEnv:   cred.EnvName,
		CredentialValue: cred.Value,
		SessionID:       sessionID,
		Resume:          exists,
		AgentName:       agent.AgentName,
		PermissionMode:  permission,
		AllowedTools:    agent.AllowedTools,
		DisallowedTools: evalDeniedTools,
		Model:           model,
		Network:         a.cfg.Container.Network,
		Secrets:         secrets,
		Skills:          skills,
		Prompt:          toolNote + prompt,
	}

	st, err := store.Open(a.cfg.RoutineDBPath(in.RunID), fmt.Sprintf("routine-run-%d", in.RunID), store.ReadWrite)
	if err != nil {
		return row, fmt.Errorf("open run store: %w", err)
	}
	defer st.Close()
	turnID, _, err := st.CreateTurn(ctx, store.NewTurn{
		Request: prompt, Origin: core.HTTPPollOrigin(), Conversation: in.Step.ID,
		IdempotencyKey: fmt.Sprintf("step:%s:%d", in.Step.ID, in.Attempt),
	})
	if err != nil {
		return row, fmt.Errorf("record step turn: %w", err)
	}
	row.TurnID = turnID

	result, err := a.runTurn(ctx, st, spec, RunTurnInput{
		AgentID: fmt.Sprintf("routine-run-%d", in.RunID), TurnID: turnID, ConversationID: in.Step.ID, Prompt: prompt,
	})
	if ferr := st.FinishTurn(context.WithoutCancel(ctx), turnID, result); ferr != nil {
		activity.GetLogger(ctx).Warn("could not close out the step turn", "turn", turnID, "error", ferr)
	}
	row.Output, row.CostUSD, row.Usage = result.Text, result.CostUSD, result.Usage
	if result.Status == core.TurnError {
		row.Error = result.ErrorMessage
	}
	var nonRetryable interface{ NonRetryable() bool }
	if err != nil && errors.As(err, &nonRetryable) && nonRetryable.NonRetryable() {
		row.Error = err.Error()
		return row, nil
	}
	return row, err
}

// pinnedAgent returns the agent definition and persona at the pinned version, or
// what is live when nothing was pinned.
func (a *Activities) pinnedAgent(ctx context.Context, id string, version int) (registry.Agent, string, error) {
	if version > 0 {
		v, err := a.reg.GetVersion(ctx, id, version)
		if err != nil {
			return registry.Agent{}, "", newNonRetryable(err)
		}
		return v.Definition, v.Persona, nil
	}
	agent, err := a.reg.Get(ctx, id)
	if err != nil {
		return registry.Agent{}, "", newNonRetryable(err)
	}
	persona := ""
	if latest, err := a.reg.LatestVersion(ctx, id); err == nil {
		persona = latest.Persona
	} else if !errors.Is(err, registry.ErrNotFound) {
		return registry.Agent{}, "", err
	}
	return agent, persona, nil
}

// runProcessStep runs an allowlisted image with a literal argv (adr/007): no
// shell, no socket, only the run's work area mounted, and no network unless the
// step declares an external effect.
func (a *Activities) runProcessStep(ctx context.Context, in RoutineStepInput) (registry.StepRun, *int, error) {
	var row registry.StepRun
	if !a.cfg.Routines.ImageAllowed(in.Step.Image) {
		row.Error = fmt.Sprintf("image %q is not on the process allowlist", in.Step.Image)
		return row, nil, nil
	}
	name := fmt.Sprintf("roundclaw-rp-%d-%s-%d", in.RunID, slug(in.Step.ID), in.Attempt)
	runtime := a.cfg.Container.Runtime
	_ = exec.CommandContext(context.WithoutCancel(ctx), runtime, "rm", "-f", name).Run()

	args := []string{"run", "--rm", "--name", name, "--workdir", "/workspace",
		"-v", a.cfg.RoutineWorkDir(in.RunID) + ":/workspace"}
	if in.Step.Effects == routine.EffectExternal {
		if a.cfg.Container.Network != "" {
			args = append(args, "--network", a.cfg.Container.Network)
		}
	} else {
		args = append(args, "--network", "none")
	}
	args = append(args, in.Step.Image)
	args = append(args, in.Step.Argv...)

	// Not CommandContext: a cancelled step is stopped through the runtime, so
	// the container is not orphaned by killing only its client.
	cmd := exec.Command(runtime, args...)
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		row.Error = "start process step: " + err.Error()
		return row, nil, nil
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = exec.CommandContext(stopCtx, runtime, "stop", "--time", "10", name).Run()
		cancel()
		<-waited
		row.Output = out.String()
		return row, nil, ctx.Err()
	}
	row.Output = out.String()
	if len(row.Output) > 1<<20 {
		row.Output = row.Output[len(row.Output)-1<<20:]
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		code := 0
		return row, &code, nil
	case errors.As(waitErr, &exitErr):
		code := exitErr.ExitCode()
		return row, &code, nil
	default:
		row.Error = "process step: " + waitErr.Error()
		return row, nil, nil
	}
}

// SkipRoutineStep records a step that never ran, with the reason.
func (a *Activities) SkipRoutineStep(ctx context.Context, row registry.StepRun) error {
	row.Status = registry.StepSkipped
	return a.reg.FinishStepRun(ctx, row)
}

// ChildRunInput admits an inner run for a routine step.
type ChildRunInput struct {
	ParentRunID int64             `json:"parent_run_id"`
	Step        routine.Step      `json:"step"`
	Attempt     int               `json:"attempt"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Depth       int               `json:"depth"`
}

// MaxRoutineDepth bounds how deeply routines may nest at run time; the compiler
// has already refused a routine that contains itself.
const MaxRoutineDepth = 4

// AdmitChildRun writes the inner run's row. It is unique per parent run, step
// and attempt, so a retried activity finds the run it already admitted.
func (a *Activities) AdmitChildRun(ctx context.Context, in ChildRunInput) (int64, error) {
	if in.Depth > MaxRoutineDepth {
		return 0, newNonRetryable(fmt.Errorf("routines nest deeper than %d", MaxRoutineDepth))
	}
	r, err := a.reg.GetRoutine(ctx, in.Step.Routine)
	if err != nil {
		return 0, newNonRetryable(err)
	}
	if r.State != registry.RoutineActive {
		return 0, newNonRetryable(fmt.Errorf("routine %s is archived", r.ID))
	}
	manifest, err := a.reg.RoutineManifest(ctx, r.Plan)
	if err != nil {
		return 0, err
	}
	run, err := a.reg.StartRoutineRun(ctx, registry.RoutineRun{
		RoutineID: r.ID, Version: r.Version, PlanHash: r.Plan.Hash, Manifest: manifest,
		Inputs: in.Inputs, Origin: core.HTTPPollOrigin(),
		RequestedBy: fmt.Sprintf("routine-run:%d", in.ParentRunID),
		ParentRunID: in.ParentRunID, ParentStep: in.Step.ID, ParentAttempt: in.Attempt, Depth: in.Depth,
	})
	if err != nil {
		return 0, err
	}
	if err := a.reg.StartStepRun(ctx, in.ParentRunID, in.Step.ID, in.Attempt, string(routine.KindRoutine), ""); err != nil {
		return 0, err
	}
	return run.ID, nil
}

// RecordRoutineStep records how an inner-run step ended, from its run's row.
func (a *Activities) RecordRoutineStep(ctx context.Context, row registry.StepRun) error {
	if row.ChildRunID != 0 {
		child, err := a.reg.GetRoutineRun(ctx, row.ChildRunID)
		if err != nil {
			return err
		}
		row.CostUSD, row.Usage = child.CostUSD, child.Usage
		if row.Output == "" {
			row.Output = child.Output
		}
	}
	return a.reg.FinishStepRun(ctx, row)
}

// RoutineRunStatus reads a run's row: how an inner run ended.
func (a *Activities) RoutineRunStatus(ctx context.Context, runID int64) (registry.RoutineRun, error) {
	return a.reg.GetRoutineRun(ctx, runID)
}

// FinishRoutineRunInput closes out a run.
type FinishRoutineRunInput struct {
	RunID   int64               `json:"run_id"`
	Outcome registry.RunOutcome `json:"outcome"`
}

// FinishRoutineRun records a run's outcome and totals.
func (a *Activities) FinishRoutineRun(ctx context.Context, in FinishRoutineRunInput) (registry.RoutineRun, error) {
	return a.reg.FinishRoutineRun(ctx, in.RunID, in.Outcome)
}

// DeliverRoutineRun delivers a finished top-level run to whoever asked. It
// reads the return address from the run's row, so a run that failed before its
// plan loaded still reports. An inner run delivers nothing: its parent reads it.
func (a *Activities) DeliverRoutineRun(ctx context.Context, runID int64) error {
	run, err := a.reg.GetRoutineRun(ctx, runID)
	if err != nil {
		return newNonRetryable(err)
	}
	if run.ParentRunID != 0 {
		return nil
	}
	if run.Origin.Type == core.OriginAgent {
		head, kept, err := a.keepResult(ctx, run.Origin.Agent, run.Origin.Conversation,
			fmt.Sprintf("routine-%s-run-%d", run.RoutineID, run.ID), run.Output)
		if err != nil {
			return err
		}
		return a.wakeAgent(ctx, run.Origin.Agent, run.Origin.Conversation,
			fmt.Sprintf("routine:%d", run.ID), routineWakePrompt(run, head, kept))
	}
	status := core.TurnDone
	if run.Status == registry.RunError || run.Status == registry.RunFailed {
		status = core.TurnError
	}
	return a.DeliverResponse(ctx, DeliverInput{
		AgentID: "routine:" + run.RoutineID,
		Origin:  run.Origin,
		Result: core.TurnResult{Status: status, Text: routineWakePrompt(run, run.Output, ""),
			CostUSD: run.CostUSD, Usage: run.Usage},
	})
}

// routineWakePrompt is what the delegator reads when its routine finishes: the
// outcome, both verdicts when the caller stated conditions, the bounded result
// and where the rest is — never the run's progress (adr/014).
func routineWakePrompt(run registry.RoutineRun, head, kept string) string {
	var b strings.Builder
	label := "[routine " + run.Status
	if !run.Expect.Empty() {
		if routine.Held(run.ExpectVerdicts) {
			label += " · your conditions met"
		} else {
			label += " · your conditions NOT met"
		}
	}
	dur := ""
	if !run.FinishedAt.IsZero() {
		dur = " · " + run.FinishedAt.Sub(run.StartedAt).Round(time.Second).String()
	}
	fmt.Fprintf(&b, "%s] %s v%d · run %d%s · $%.4f\n", label, run.RoutineID, run.Version, run.ID, dur, run.CostUSD)
	if run.FailedStep != "" {
		fmt.Fprintf(&b, "step %q: %s\n", run.FailedStep, run.Reason)
	} else if run.Reason != "" {
		fmt.Fprintf(&b, "%s\n", run.Reason)
	}
	if !run.Expect.Empty() && !routine.Held(run.ExpectVerdicts) {
		fmt.Fprintf(&b, "your checks: %s\n", routine.Shortfall(run.ExpectVerdicts))
	}
	if head != "" {
		fmt.Fprintf(&b, "\nresult:\n%s\n", head)
	}
	if kept != "" {
		fmt.Fprintf(&b, "\nfull result: %s (open it only if you need it)\n", kept)
	}
	fmt.Fprintf(&b, "\ninspect: roundclaw routine trace %d · redo: roundclaw routine run %s --instead-of %d\n",
		run.ID, run.RoutineID, run.ID)
	return b.String()
}
