package activity

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/temporal/contract"
)

// EnqueueScheduledInput names the schedule that fired.
type EnqueueScheduledInput struct {
	ScheduleID string `json:"schedule_id"`
}

// TemporalSignaller is the slice of the Temporal client this activity needs.
type TemporalSignaller interface {
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg any,
		options client.StartWorkflowOptions, workflow any, workflowArgs ...any) (client.WorkflowRun, error)
}

// SetSignaller installs the client used to queue scheduled requests.
func (a *Activities) signaller() TemporalSignaller { return a.signal }

// EnqueueScheduled turns a schedule firing into a queued request.
//
// The definition is read here, at fire time, rather than being baked into the
// Temporal schedule: editing a schedule's prompt then takes effect on the next
// run without recreating anything in Temporal.
//
// The request joins the agent's ordinary queue, so scheduled work shares one
// session, one ordering rule and one set of spend limits with everything else.
func (a *Activities) EnqueueScheduled(ctx context.Context, in EnqueueScheduledInput) error {
	log := activity.GetLogger(ctx)

	sched, err := a.reg.GetSchedule(ctx, in.ScheduleID)
	if err != nil {
		// A deleted schedule will not reappear; retrying would just spin until
		// the next firing.
		return newNonRetryable(fmt.Errorf("read schedule %s: %w", in.ScheduleID, err))
	}
	if !sched.Enabled {
		log.Info("schedule is disabled; skipping this firing", "schedule", sched.ID)
		return nil
	}

	agent, err := a.reg.Get(ctx, sched.AgentID)
	if err != nil {
		return newNonRetryable(fmt.Errorf("schedule %s: %w", sched.ID, err))
	}
	if !agent.Enabled {
		// Disabling an agent should quiet its schedules too, without having to
		// remember to pause each one.
		log.Info("agent is disabled; skipping this firing",
			"schedule", sched.ID, "agent", agent.ID)
		return nil
	}

	st, err := a.stores.Get(sched.AgentID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	origin := core.HTTPPollOrigin()
	if sched.ChannelID != "" {
		origin = core.DiscordOrigin(sched.ChannelID, "")
	}

	// The firing time is the idempotency key: Temporal may retry this activity,
	// and without it a retry would queue the same run twice.
	fireKey := fmt.Sprintf("schedule:%s:%d", sched.ID,
		activity.GetInfo(ctx).ScheduledTime.Truncate(time.Second).Unix())

	turnID, existed, err := st.CreateTurn(ctx, sched.Prompt, origin, fireKey)
	if err != nil {
		return err
	}
	if existed {
		log.Info("this firing was already queued", "schedule", sched.ID, "turn_id", turnID)
		return nil
	}

	req := core.Request{
		AgentID:    sched.AgentID,
		RequestID:  fireKey,
		TurnID:     turnID,
		Text:       sched.Prompt,
		Origin:     origin,
		SuppressIf: sched.SuppressIf,
		ReceivedAt: time.Now().UTC(),
	}

	workflowID := contract.WorkflowID(sched.AgentID)
	_, err = a.signaller().SignalWithStartWorkflow(ctx, workflowID, contract.SignalEnqueue, req,
		client.StartWorkflowOptions{ID: workflowID, TaskQueue: a.cfg.Temporal.TaskQueue},
		// The workflow type by name, not by function reference: importing the
		// package that defines it would close an import cycle.
		contract.AgentWorkflowType, contract.AgentInput{AgentID: sched.AgentID})
	if err != nil {
		return fmt.Errorf("queue scheduled request for %s: %w", sched.AgentID, err)
	}

	log.Info("queued a scheduled request",
		"schedule", sched.ID, "agent", sched.AgentID, "turn_id", turnID)
	return nil
}
