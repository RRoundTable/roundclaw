package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/temporal/contract"
)

// Schedules pair a definition in the registry with a trigger in Temporal.
//
// The registry owns what to run; Temporal owns when. Keeping the definition out
// of the Temporal schedule is what lets a prompt be edited without recreating
// anything — the firing reads the current row.
//
// Temporal is authoritative for timing, so pause and next-run-time are read
// back from it rather than mirrored into SQLite, where the two could disagree.

// ScheduleBackend is the slice of Temporal's schedule API roundclaw uses.
// Narrowing it keeps schedule handling testable without a server.
type ScheduleBackend interface {
	Create(ctx context.Context, opts client.ScheduleOptions) error
	Delete(ctx context.Context, scheduleID string) error
	Pause(ctx context.Context, scheduleID, note string) error
	Unpause(ctx context.Context, scheduleID, note string) error
	Describe(ctx context.Context, scheduleID string) (*client.ScheduleDescription, error)
}

// SetSchedules installs the Temporal schedule backend. Without it the schedule
// commands report that scheduling is unavailable rather than panicking.
func (d *Dispatcher) SetSchedules(b ScheduleBackend) { d.schedules = b }

// ScheduleView is a definition joined with its live trigger state.
type ScheduleView struct {
	registry.Schedule
	Paused      bool   `json:"paused"`
	NextRun     string `json:"next_run,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

func temporalScheduleID(id string) string { return "roundclaw-schedule-" + id }

// PutSchedule saves a definition and creates or updates its trigger.
func (d *Dispatcher) PutSchedule(ctx context.Context, s registry.Schedule) (ScheduleView, error) {
	if d.schedules == nil {
		return ScheduleView{}, fmt.Errorf("scheduling is not available: no temporal schedule client")
	}

	saved, err := d.reg.PutSchedule(ctx, s)
	if err != nil {
		return ScheduleView{}, err
	}

	// Temporal has no upsert for schedules, and updating one in place requires
	// a callback that re-derives the whole spec. Replacing is simpler and safe
	// here because the trigger holds no state worth keeping — the definition
	// and the turn history both live elsewhere.
	tid := temporalScheduleID(saved.ID)
	_ = d.schedules.Delete(ctx, tid)

	err = d.schedules.Create(ctx, client.ScheduleOptions{
		ID: tid,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{saved.Cron},
			TimeZoneName:    saved.Timezone,
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "roundclaw-scheduled-" + saved.ID,
			Workflow:  contract.ScheduledWorkflowType,
			Args:      []any{contract.ScheduledInput{ScheduleID: saved.ID}},
			TaskQueue: d.cfg.Temporal.TaskQueue,
		},
		// A run that is still queued when the next firing comes round is a sign
		// the agent is behind, not a reason to pile on another request.
		Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		Paused:  !saved.Enabled,
	})
	if err != nil {
		return ScheduleView{}, fmt.Errorf("create temporal schedule: %w", err)
	}
	return d.describeSchedule(ctx, saved), nil
}

// ListSchedules returns every definition with its live trigger state.
func (d *Dispatcher) ListSchedules(ctx context.Context) ([]ScheduleView, error) {
	defs, err := d.reg.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleView, 0, len(defs))
	for _, s := range defs {
		out = append(out, d.describeSchedule(ctx, s))
	}
	return out, nil
}

// GetSchedule returns one definition with its live trigger state.
func (d *Dispatcher) GetSchedule(ctx context.Context, id string) (ScheduleView, error) {
	s, err := d.reg.GetSchedule(ctx, id)
	if err != nil {
		return ScheduleView{}, err
	}
	return d.describeSchedule(ctx, s), nil
}

// SetSchedulePaused pauses or resumes a trigger, and records the intent in the
// definition so a redeploy does not silently resume something left paused.
func (d *Dispatcher) SetSchedulePaused(ctx context.Context, id string, paused bool, note string) (ScheduleView, error) {
	if d.schedules == nil {
		return ScheduleView{}, fmt.Errorf("scheduling is not available")
	}
	s, err := d.reg.GetSchedule(ctx, id)
	if err != nil {
		return ScheduleView{}, err
	}

	tid := temporalScheduleID(id)
	if paused {
		err = d.schedules.Pause(ctx, tid, note)
	} else {
		err = d.schedules.Unpause(ctx, tid, note)
	}
	if err != nil {
		return ScheduleView{}, fmt.Errorf("update temporal schedule: %w", err)
	}

	s.Enabled = !paused
	if s, err = d.reg.PutSchedule(ctx, s); err != nil {
		return ScheduleView{}, err
	}
	return d.describeSchedule(ctx, s), nil
}

// DeleteSchedule removes both the definition and its trigger.
func (d *Dispatcher) DeleteSchedule(ctx context.Context, id string) error {
	// Trigger first: a definition without a trigger is inert, whereas a trigger
	// without a definition would fire and fail every time.
	if d.schedules != nil {
		if err := d.schedules.Delete(ctx, temporalScheduleID(id)); err != nil {
			return fmt.Errorf("delete temporal schedule: %w", err)
		}
	}
	return d.reg.DeleteSchedule(ctx, id)
}

// describeSchedule joins a definition with Temporal's view of its trigger.
// Trigger state is best-effort: a definition is still worth showing when
// Temporal is unreachable.
func (d *Dispatcher) describeSchedule(ctx context.Context, s registry.Schedule) ScheduleView {
	v := ScheduleView{Schedule: s}
	if d.schedules == nil {
		v.Unavailable = "no temporal schedule client"
		return v
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	desc, err := d.schedules.Describe(ctx, temporalScheduleID(s.ID))
	if err != nil {
		v.Unavailable = err.Error()
		return v
	}
	v.Paused = desc.Schedule.State.Paused
	if len(desc.Info.NextActionTimes) > 0 {
		v.NextRun = desc.Info.NextActionTimes[0].UTC().Format(time.RFC3339)
	}
	return v
}

// ErrSchedulesUnavailable is returned when no schedule backend is configured.
var ErrSchedulesUnavailable = errors.New("scheduling is not available")
