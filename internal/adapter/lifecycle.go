package adapter

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/client"

	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

// StartTrial starts a trial's execution, whose ID is derived from its row.
func (d *Dispatcher) StartTrial(ctx context.Context, trialID int64) error {
	if _, err := d.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        rcworkflow.RoutineTrialID(trialID),
		TaskQueue: d.cfg.Temporal.TaskQueue,
	}, rcworkflow.RoutineTrial, rcworkflow.RoutineTrialInput{TrialID: trialID, StepQueue: d.cfg.RoutineStepQueue()}); err != nil {
		return fmt.Errorf("start trial %d: %w", trialID, err)
	}
	return nil
}
