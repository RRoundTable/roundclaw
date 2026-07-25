package workflow

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// RunWorkflowType is the workflow type a trigger starts to run a workflow.
const RunWorkflowType = "RunWorkflow"

// RunWorkflowInput names which workflow to run. Only the ID travels; the steps
// are read at run time, so an edit takes effect on the next run.
type RunWorkflowInput struct {
	WorkflowID string `json:"workflow_id"`
}

// RunWorkflow executes a workflow's steps in order, passing each step's output
// to the next, and delivers the final result to the workflow's channel.
//
// This is the agent-less execution path: no session, no queue, no channel
// binding — just a pipeline of prompts that Temporal runs durably. Each step is
// its own activity, so a worker crash resumes the pipeline at the step it was
// on rather than from the top.
func RunWorkflow(ctx workflow.Context, in RunWorkflowInput) error {
	log := workflow.GetLogger(ctx)

	loadCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})
	var def registry.Workflow
	if err := workflow.ExecuteActivity(loadCtx, (*activity.Activities).LoadWorkflow, in.WorkflowID).Get(loadCtx, &def); err != nil {
		return err
	}
	if !def.Enabled {
		log.Info("workflow is disabled; skipping this run", "workflow", in.WorkflowID)
		return nil
	}

	runKey := workflow.GetInfo(ctx).WorkflowExecution.RunID

	stepCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 35 * time.Minute,
		HeartbeatTimeout:    10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        5 * time.Second,
			BackoffCoefficient:     2,
			MaximumInterval:        time.Minute,
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{activity.NonRetryableType},
		},
	})

	var context strings.Builder
	var final string
	for i, step := range def.Steps {
		var res activity.StepResult
		err := workflow.ExecuteActivity(stepCtx, (*activity.Activities).RunWorkflowStep, activity.RunStepInput{
			WorkflowID: in.WorkflowID,
			RunKey:     runKey,
			StepIndex:  i,
			Step:       step,
			Context:    context.String(),
		}).Get(stepCtx, &res)
		if err != nil {
			log.Error("workflow step failed", "workflow", in.WorkflowID, "step", i, "error", err)
			deliverWorkflow(ctx, def, fmt.Sprintf("⚠️ 워크플로 `%s` 실패 — %d단계 (%s): %v",
				def.ID, i+1, stepLabel(step, i), err))
			return err
		}
		if res.Failed {
			deliverWorkflow(ctx, def, fmt.Sprintf("⚠️ 워크플로 `%s` — %d단계 (%s) 오류: %s",
				def.ID, i+1, stepLabel(step, i), res.Error))
			return nil
		}
		final = res.Output
		fmt.Fprintf(&context, "## %d단계 — %s\n%s\n\n", i+1, stepLabel(step, i), res.Output)
	}

	deliverWorkflow(ctx, def, fmt.Sprintf("✅ 워크플로 `%s` 완료.\n\n%s", def.ID, final))
	return nil
}

func stepLabel(s registry.WorkflowStep, i int) string {
	if s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("step %d", i+1)
}

// deliverWorkflow posts a result to the workflow's channel, if any, reusing the
// agent-turn delivery activity. On a disconnected context so a cancelled run
// still reports what happened.
func deliverWorkflow(ctx workflow.Context, def registry.Workflow, text string) {
	if def.ChannelID == "" {
		return
	}
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	actCtx := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 2 * time.Second,
			MaximumAttempts: 5,
		},
	})
	_ = workflow.ExecuteActivity(actCtx, (*activity.Activities).DeliverResponse, activity.DeliverInput{
		AgentID: "workflow:" + def.ID,
		Origin:  core.DiscordOrigin(def.ChannelID, ""),
		Result:  core.TurnResult{Text: text, Status: core.TurnDone},
	}).Get(actCtx, nil)
}
