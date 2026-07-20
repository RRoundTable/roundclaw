package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// ScheduledRequestType is the workflow type a Temporal schedule starts.
const ScheduledRequestType = "ScheduledRequest"

// ScheduledInput names which schedule fired.
//
// Only the ID travels. The definition is read at fire time by the activity, so
// editing a schedule's prompt takes effect on its next run rather than
// requiring the Temporal schedule to be recreated.
type ScheduledInput struct {
	ScheduleID string `json:"schedule_id"`
}

// ScheduledRequest is what a Temporal schedule starts on each firing.
//
// It is deliberately thin: read the definition, queue a request, finish. The
// work itself happens in the agent's own long-lived workflow, which the request
// joins through the same queue as any human request. Running the turn here
// instead would give scheduled work its own execution path, its own session and
// its own ordering — three things to keep in step with the normal one.
func ScheduledRequest(ctx workflow.Context, in ScheduledInput) error {
	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			// Bounded: a schedule that cannot be enqueued will fire again on
			// its own, and retrying until then would stack duplicates.
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{"roundclaw: non-retryable"},
		},
	})

	return workflow.ExecuteActivity(actCtx,
		(*activity.Activities).EnqueueScheduled,
		activity.EnqueueScheduledInput{ScheduleID: in.ScheduleID},
	).Get(actCtx, nil)
}
