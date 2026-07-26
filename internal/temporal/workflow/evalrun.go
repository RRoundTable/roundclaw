package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// EvalRunType is the workflow type a trigger starts to run an eval set.
const EvalRunType = "EvalRun"

// EvalRunInput names which run to execute. Only the ID travels: the row exists
// before the workflow starts, so the caller has something to poll and the
// workflow has somewhere to write.
type EvalRunInput struct {
	RunID int64 `json:"run_id"`
}

// EvalRun executes an eval set's cases against a pinned agent version, marks
// each answer, and records the aggregate.
//
// Cases run one at a time. They are container starts against a shared host and a
// shared concurrency budget, and an eval is background work — running ten at
// once to save a few minutes would starve the agents doing the actual job. The
// durability is what matters here rather than the latency: a worker lost halfway
// resumes at the case it was on, because each case's result is written as it
// completes rather than at the end.
//
// A case that fails is not a failed run. An agent that errors on one question
// has answered the other nine, and those answers are exactly what a comparison
// needs; the failure is recorded as a zero and the run carries on.
func EvalRun(ctx workflow.Context, in EvalRunInput) error {
	log := workflow.GetLogger(ctx)

	shortCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	var plan activity.EvalPlan
	if err := workflow.ExecuteActivity(shortCtx, (*activity.Activities).LoadEvalPlan, in.RunID).Get(shortCtx, &plan); err != nil {
		// Nothing to record against: the run row could not even be read.
		return err
	}
	if !plan.Set.Enabled {
		log.Info("eval set is disabled; not running it", "run", in.RunID, "set", plan.Set.ID)
		return finish(ctx, in.RunID, plan, "the eval set is disabled")
	}

	caseCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 35 * time.Minute,
		HeartbeatTimeout:    10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			// One retry, not the default forever. A case that fails twice is a
			// result — "this agent cannot do this" — and retrying it into a third
			// container is spending money to reach the same answer.
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	judgeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})

	names := make([]string, 0, len(plan.Set.Cases))
	for _, c := range plan.Set.Cases {
		names = append(names, c.Name)

		result := registry.EvalResult{RunID: in.RunID, CaseName: c.Name}
		var outcome activity.CaseOutcome
		err := workflow.ExecuteActivity(caseCtx, (*activity.Activities).RunEvalCase, activity.RunCaseInput{
			RunID:      in.RunID,
			Case:       c,
			Agent:      plan.Agent,
			Persona:    plan.Persona,
			FullGrants: plan.Set.FullGrants,
		}).Get(caseCtx, &outcome)

		switch {
		case err != nil:
			// The case never produced an answer. Recorded as a zero with the
			// reason, because a missing row would read as a case that was never
			// asked — and a comparison would then call it "removed".
			result.Reason = "the case could not be run: " + err.Error()
			log.Warn("eval case did not run", "run", in.RunID, "case", c.Name, "error", err)

		case outcome.Failed:
			result.Output, result.CostUSD, result.DurationMS = outcome.Output, outcome.CostUSD, outcome.DurationMS
			result.Reason = "the agent errored: " + outcome.Error

		case outcome.AssertionFailure != "":
			// An exact rule was broken, so no judge is asked. The rules exist
			// precisely to be beyond argument.
			result.Output, result.CostUSD, result.DurationMS = outcome.Output, outcome.CostUSD, outcome.DurationMS
			result.Reason = outcome.AssertionFailure

		case c.Rubric == "":
			// Nothing to judge against: the case only asked whether the agent
			// could answer at all, and it did.
			result.Output, result.CostUSD, result.DurationMS = outcome.Output, outcome.CostUSD, outcome.DurationMS
			result.Score, result.Passed = 1, true
			result.Reason = "answered without error; this case sets no rubric"

		default:
			result.Output, result.CostUSD, result.DurationMS = outcome.Output, outcome.CostUSD, outcome.DurationMS
			var j activity.Judgement
			jErr := workflow.ExecuteActivity(judgeCtx, (*activity.Activities).JudgeEvalCase, activity.JudgeInput{
				RunID: in.RunID, Case: c.Name, Rubric: c.Rubric, Prompt: c.Prompt, Output: outcome.Output,
			}).Get(judgeCtx, &j)
			if jErr != nil {
				// Unjudged, not failed. The answer exists and was not marked, and
				// saying so is more useful than a zero nobody can act on.
				result.Reason = "the answer was not marked: " + jErr.Error()
				log.Warn("eval judge did not run", "run", in.RunID, "case", c.Name, "error", jErr)
				break
			}
			result.Score, result.Passed, result.Reason = j.Score, j.Passed, j.Reason
		}

		if err := workflow.ExecuteActivity(shortCtx,
			(*activity.Activities).RecordEvalCase, result).Get(shortCtx, nil); err != nil {
			// Losing a result would corrupt the aggregate, so this one does stop
			// the run rather than carrying on with a hole in it.
			return finish(ctx, in.RunID, plan, fmt.Sprintf("could not record case %q: %v", c.Name, err))
		}
	}

	// Scratch workspaces go before the notification, so an agent told the run is
	// done does not find half of it still on disk.
	cleanCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	})
	_ = workflow.ExecuteActivity(cleanCtx, (*activity.Activities).CleanEvalWorkspaces,
		activity.CleanEvalWorkspacesInput{RunID: in.RunID, AgentID: plan.Agent.ID, Cases: names}).Get(cleanCtx, nil)

	return finish(ctx, in.RunID, plan, "")
}

// finish aggregates, records and notifies. It runs on every exit path that got
// far enough to have a run row, so a failed run still reports rather than going
// quiet — an eval nobody hears about is one nobody acts on.
func finish(ctx workflow.Context, runID int64, plan activity.EvalPlan, runErr string) error {
	log := workflow.GetLogger(ctx)
	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})

	var run registry.EvalRun
	if err := workflow.ExecuteActivity(actCtx, (*activity.Activities).FinishEvalRun,
		activity.FinishEvalInput{RunID: runID, Error: runErr}).Get(actCtx, &run); err != nil {
		return err
	}
	log.Info("eval run finished", "run", runID, "set", plan.Set.ID,
		"agent", run.AgentID, "version", run.Version,
		"passed", run.Passed, "total", run.Total, "score", run.Score)

	if err := workflow.ExecuteActivity(actCtx,
		(*activity.Activities).NotifyEvalRun, runID).Get(actCtx, nil); err != nil {
		// The results are recorded and readable; failing the workflow over the
		// notification would make a completed run look like a broken one.
		log.Warn("could not tell the requester the eval finished", "run", runID, "error", err)
	}
	if runErr != "" {
		return fmt.Errorf("eval run %d: %s", runID, runErr)
	}
	return nil
}
