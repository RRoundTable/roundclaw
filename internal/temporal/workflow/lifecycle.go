package workflow

import (
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// RoutineTrialInput names the trial; everything else is on its row.
type RoutineTrialInput struct {
	TrialID   int64  `json:"trial_id"`
	StepQueue string `json:"step_queue,omitempty"`
}

// RoutineTrialID is a trial's execution ID, derived from its row.
func RoutineTrialID(trialID int64) string { return fmt.Sprintf("roundclaw-routine-trial-%d", trialID) }

// RoutineTrial runs one trial (adr/013). An admission trial first has the
// examiner write each case's checks from the work it came from — never seeing
// the routine — then runs every case on each version it compares, as
// measurements, one at a time, and hands the runs to DecideTrial, where the
// lifecycle's rule decides and the decision is recorded with its figures.
func RoutineTrial(ctx workflow.Context, in RoutineTrialInput) error {
	short := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	examineCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           in.StepQueue,
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})

	var t registry.Trial
	if err := workflow.ExecuteActivity(short, (*activity.Activities).LoadTrial, in.TrialID).Get(short, &t); err != nil {
		return err
	}

	if t.Kind == registry.TrialAdmission {
		var cost float64
		for i := range t.Cases {
			c := &t.Cases[i]
			if !c.Checks.Empty() || c.Unmarked {
				continue
			}
			var ex activity.ExamineResult
			if err := workflow.ExecuteActivity(examineCtx, (*activity.Activities).ExamineCase, activity.ExamineInput{
				TrialID: t.ID, Case: c.Name, Request: c.Source.Request, Outcome: c.Source.Outcome,
			}).Get(examineCtx, &ex); err != nil {
				ex = activity.ExamineResult{Unmarked: true, Reason: err.Error()}
			}
			c.Checks, c.Unmarked, c.UnmarkedReason = ex.Checks, ex.Unmarked, ex.Reason
			cost += ex.CostUSD
		}
		if err := workflow.ExecuteActivity(short, (*activity.Activities).SaveTrialCases,
			activity.SaveTrialCasesInput{TrialID: t.ID, Cases: t.Cases, CostUSD: cost}).Get(short, nil); err != nil {
			return err
		}
	}

	versions := []int{t.Version}
	if t.Kind == registry.TrialVersion {
		versions = []int{t.BaseVersion, t.Version}
	}
	for i := range t.Cases {
		c := &t.Cases[i]
		if c.Runs == nil {
			c.Runs = map[int]int64{}
		}
		for _, v := range versions {
			if c.Unmarked && t.Kind == registry.TrialAdmission {
				continue
			}
			var runID int64
			if err := workflow.ExecuteActivity(short, (*activity.Activities).StartTrialRun, activity.TrialRunInput{
				TrialID: t.ID, Case: c.Name, Version: v, Inputs: c.Inputs,
			}).Get(short, &runID); err != nil {
				return err
			}
			c.Runs[v] = runID
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				WorkflowID:          fmt.Sprintf("roundclaw-routine-%d", runID),
				ParentClosePolicy:   enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
				WaitForCancellation: true,
			})
			// A run's own failure is recorded on its row, which is what the
			// decision reads; it does not fail the trial.
			_ = workflow.ExecuteChildWorkflow(childCtx, RoutineRun,
				RoutineRunInput{RunID: runID, StepQueue: in.StepQueue, Measurement: true}).Get(childCtx, nil)
		}
	}
	if err := workflow.ExecuteActivity(short, (*activity.Activities).SaveTrialCases,
		activity.SaveTrialCasesInput{TrialID: t.ID, Cases: t.Cases}).Get(short, nil); err != nil {
		return err
	}
	var decided registry.Trial
	if err := workflow.ExecuteActivity(detached(short), (*activity.Activities).DecideTrial, t.ID).Get(short, &decided); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("routine trial decided", "trial", t.ID, "routine", t.RoutineID,
		"kind", t.Kind, "state", decided.State, "rule", decided.Rule)
	return nil
}

// RoutineLifecycleInput carries the loop's position across Continue-As-New.
type RoutineLifecycleInput struct {
	StepQueue string        `json:"step_queue,omitempty"`
	Every     time.Duration `json:"every"`
}

// RoutineLifecycleID is the one lifecycle loop's execution ID.
const RoutineLifecycleID = "roundclaw-routine-lifecycle"

// lifecycleSweepsPerRun bounds one run's history before Continue-As-New.
const lifecycleSweepsPerRun = 100

// RoutineLifecycle is the durable loop that runs the lifecycle: sweep, start
// the trials the sweep created, sleep, repeat. Trials run as their own
// executions and outlive the loop's Continue-As-New.
func RoutineLifecycle(ctx workflow.Context, in RoutineLifecycleInput) error {
	if in.Every <= 0 {
		in.Every = 15 * time.Minute
	}
	sweepCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	for i := 0; i < lifecycleSweepsPerRun; i++ {
		var res activity.SweepResult
		if err := workflow.ExecuteActivity(sweepCtx, (*activity.Activities).LifecycleSweep).Get(sweepCtx, &res); err != nil {
			workflow.GetLogger(ctx).Warn("lifecycle sweep failed; trying again next time", "error", err)
		}
		for _, id := range res.Trials {
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				WorkflowID: RoutineTrialID(id),
				// A trial is its own work: it keeps running across the loop's
				// Continue-As-New.
				ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
			})
			fut := workflow.ExecuteChildWorkflow(childCtx, RoutineTrial, RoutineTrialInput{TrialID: id, StepQueue: in.StepQueue})
			if err := fut.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
				workflow.GetLogger(ctx).Warn("could not start a trial", "trial", id, "error", err)
			}
		}
		if err := workflow.Sleep(ctx, in.Every); err != nil {
			return err
		}
	}
	return workflow.NewContinueAsNewError(ctx, RoutineLifecycle, in)
}
