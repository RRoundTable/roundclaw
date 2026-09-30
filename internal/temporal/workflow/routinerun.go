package workflow

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
	"github.com/roundtable/roundclaw/internal/temporal/contract"
)

// RoutineRunInput names the run; everything else is read from its row.
type RoutineRunInput struct {
	RunID int64 `json:"run_id"`
	// StepQueue is the task queue routine step activities run on (adr/010).
	// Empty uses the workflow's own queue, which is what tests do.
	StepQueue string `json:"step_queue,omitempty"`
}

// RoutineRun is the one interpreter every routine runs on (adr/006).
//
// It loads the plan pinned by the run's version and walks it in the compiled
// order: one step at a time, each attempt an activity keyed by step and attempt,
// an inner routine as a child run. An open step that falls short runs again,
// told why, until its attempts are spent; a fixed program is not retried for
// giving the same answer. A step whose predecessor did not pass is recorded as
// skipped with the reason rather than left without a row.
//
// Whatever happens — a failed load, a failed step, a stop — the run is closed
// out and its result delivered, on a disconnected context, because a result
// outlives whoever asked for it. Stop is a cancellation, never a termination, so
// this path always runs.
//
// Everything that decides control flow here is pure (routine.Check, the
// compiled order), so replay reaches the same decisions.
func RoutineRun(ctx workflow.Context, in RoutineRunInput) error {
	log := workflow.GetLogger(ctx)
	short := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})

	var plan activity.RoutineRunPlan
	if err := workflow.ExecuteActivity(short, (*activity.Activities).LoadRoutineRun, in.RunID).Get(short, &plan); err != nil {
		return closeRun(ctx, in.RunID, registry.RunOutcome{
			Status: registry.RunError, Reason: "could not load the run: " + err.Error(),
		})
	}

	var (
		outputs = map[string]string{}
		passed  = map[string]bool{}
		outcome = registry.RunOutcome{Status: registry.RunPassed}
		last    string
		stopped bool
	)
	for _, id := range plan.Plan.Order {
		step, _ := plan.Plan.Step(id)

		if stopped || ctx.Err() != nil {
			stopped = true
			skip(ctx, in.RunID, step, "the run was stopped")
			continue
		}
		if blocker := firstUnpassed(step.After, passed); blocker != "" {
			skip(ctx, in.RunID, step, fmt.Sprintf("step %q did not pass", blocker))
			continue
		}

		res, err := runStep(ctx, in, plan, step, outputs)
		if err != nil && (errors.Is(err, workflow.ErrCanceled) || temporal.IsCanceledError(err) || ctx.Err() != nil) {
			stopped = true
			continue
		}
		if err != nil {
			res = activity.RoutineStepOutcome{Status: registry.RunError, Reason: err.Error()}
		}
		outputs[id] = res.Output
		last = res.Output
		if res.Status == registry.RunPassed {
			passed[id] = true
			continue
		}
		if outcome.FailedStep == "" {
			outcome.Status = res.Status
			if res.Status == registry.RunStopped {
				outcome.Status = registry.RunFailed
			}
			outcome.FailedStep, outcome.Reason = id, res.Reason
		}
	}

	switch {
	case stopped:
		outcome = registry.RunOutcome{Status: registry.RunStopped, Reason: "stopped", FailedStep: outcome.FailedStep}
	case outcome.FailedStep == "":
		// Every step passed; the routine is held to its own promise last.
		verdicts := routine.Check(plan.Plan.Source.Contract.Done, last, nil)
		if !routine.Held(verdicts) {
			outcome.Status = registry.RunFailed
			outcome.Reason = "the routine's own conditions did not hold: " + routine.Shortfall(verdicts)
		}
	}
	outcome.Output = last
	if !plan.Run.Expect.Empty() {
		outcome.ExpectVerdicts = routine.Check(plan.Run.Expect, last, nil)
	}
	log.Info("routine run finished", "run", in.RunID, "routine", plan.Run.RoutineID, "status", outcome.Status)
	return closeRun(ctx, in.RunID, outcome)
}

// runStep runs every attempt a step is allowed, or its inner routine.
func runStep(ctx workflow.Context, in RoutineRunInput, plan activity.RoutineRunPlan, step routine.Step,
	outputs map[string]string) (activity.RoutineStepOutcome, error) {
	if step.Kind == routine.KindRoutine {
		return runChild(ctx, in, plan, step, outputs)
	}

	opts := workflow.ActivityOptions{
		TaskQueue:           in.StepQueue,
		StartToCloseTimeout: time.Duration(step.TimeoutSec)*time.Second + time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		// No ScheduleToStartTimeout: time spent queued behind other steps never
		// fails one (adr/010).
		WaitForCancellation: true,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			// Retries here are for the infrastructure, not the work: a step that
			// ran and fell short is a result, retried below with feedback.
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	}
	stepCtx := workflow.WithActivityOptions(ctx, opts)

	var (
		res      activity.RoutineStepOutcome
		feedback string
	)
	for attempt := 1; attempt <= step.MaxAttempts; attempt++ {
		err := workflow.ExecuteActivity(stepCtx, (*activity.Activities).RunRoutineStep, activity.RoutineStepInput{
			RunID: in.RunID, Owner: plan.Owner, Step: step, Attempt: attempt, Feedback: feedback,
			Inputs: plan.Run.Inputs, Outputs: outputs, AgentVersion: plan.Run.Manifest[step.Agent],
		}).Get(stepCtx, &res)
		if err != nil {
			return res, err
		}
		if res.Status != registry.RunFailed || !step.Open() {
			return res, nil
		}
		feedback = res.Reason
	}
	return res, nil
}

// runChild runs an inner routine as a child run and reads its outcome.
func runChild(ctx workflow.Context, in RoutineRunInput, plan activity.RoutineRunPlan, step routine.Step,
	outputs map[string]string) (activity.RoutineStepOutcome, error) {
	short := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	inputs := map[string]string{}
	for k, v := range plan.Run.Inputs {
		inputs[k] = v
	}
	var childID int64
	if err := workflow.ExecuteActivity(short, (*activity.Activities).AdmitChildRun, activity.ChildRunInput{
		ParentRunID: in.RunID, Step: step, Attempt: 1, Inputs: inputs, Depth: plan.Run.Depth + 1,
	}).Get(short, &childID); err != nil {
		return activity.RoutineStepOutcome{Status: registry.RunError, Reason: err.Error()}, nil
	}

	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID: contract.RoutineRunID(childID),
		// A stop is a cancellation that reaches the inner run too, so it closes
		// itself out rather than being killed mid-step.
		ParentClosePolicy:   enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		WaitForCancellation: true,
	})
	childErr := workflow.ExecuteChildWorkflow(childCtx, RoutineRun, RoutineRunInput{RunID: childID, StepQueue: in.StepQueue}).Get(childCtx, nil)

	var child registry.RoutineRun
	if err := workflow.ExecuteActivity(detached(short), (*activity.Activities).RoutineRunStatus, childID).Get(short, &child); err != nil {
		return activity.RoutineStepOutcome{Status: registry.RunError, Reason: err.Error()}, nil
	}
	res := activity.RoutineStepOutcome{Status: child.Status, Output: child.Output, Reason: child.Reason, CostUSD: child.CostUSD}
	if child.Status == registry.RunUnmarked {
		res.Status = registry.RunFailed
	}
	row := registry.StepRun{RunID: in.RunID, StepID: step.ID, Attempt: 1, Kind: string(step.Kind),
		Status: res.Status, ChildRunID: childID, Error: child.Reason}
	if res.Status == registry.RunPassed {
		row.Error = ""
	}
	_ = workflow.ExecuteActivity(detached(short), (*activity.Activities).RecordRoutineStep, row).Get(short, nil)
	if childErr != nil && ctx.Err() != nil {
		return res, childErr
	}
	return res, nil
}

func firstUnpassed(deps []string, passed map[string]bool) string {
	for _, d := range deps {
		if !passed[d] {
			return d
		}
	}
	return ""
}

func skip(ctx workflow.Context, runID int64, step routine.Step, why string) {
	dctx := workflow.WithActivityOptions(detached(ctx), workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	row := registry.StepRun{RunID: runID, StepID: step.ID, Attempt: 1, Kind: string(step.Kind), Error: why}
	if err := workflow.ExecuteActivity(dctx, (*activity.Activities).SkipRoutineStep, row).Get(dctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("could not record a skipped step", "run", runID, "step", step.ID, "error", err)
	}
}

// closeRun records the outcome and delivers it, on a context a stop cannot
// cancel.
func closeRun(ctx workflow.Context, runID int64, outcome registry.RunOutcome) error {
	dctx := workflow.WithActivityOptions(detached(ctx), workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        2 * time.Second,
			MaximumAttempts:        5,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	if err := workflow.ExecuteActivity(dctx, (*activity.Activities).FinishRoutineRun,
		activity.FinishRoutineRunInput{RunID: runID, Outcome: outcome}).Get(dctx, nil); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(dctx, (*activity.Activities).DeliverRoutineRun, runID).Get(dctx, nil); err != nil {
		// The result is recorded and readable; the run is not a failure because
		// telling someone about it failed.
		workflow.GetLogger(ctx).Warn("could not deliver a routine result", "run", runID, "error", err)
	}
	return nil
}

// detached is a context a stop cannot cancel, for closing work out.
func detached(ctx workflow.Context) workflow.Context {
	d, _ := workflow.NewDisconnectedContext(ctx)
	return d
}
