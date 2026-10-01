package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.temporal.io/sdk/client"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
	"github.com/roundtable/roundclaw/internal/temporal/contract"
	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

// Errors a routine request can be refused with.
var (
	ErrRoutineArchived  = errors.New("routine is archived")
	ErrRoutineInput     = errors.New("routine input missing")
	ErrRoutineAuthority = errors.New("not permitted to run a routine that acts on the outside world")
)

// fleetResolver answers the compiler's questions about the fleet.
type fleetResolver struct {
	ctx context.Context
	d   *Dispatcher
}

func (f fleetResolver) AgentUsable(id string) (bool, error) {
	a, err := f.d.reg.Get(f.ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.Enabled, nil
}

func (f fleetResolver) ImageAllowed(image string) bool { return f.d.cfg.Routines.ImageAllowed(image) }

func (f fleetResolver) RoutineSteps(id string) ([]string, bool, error) {
	return f.d.reg.RoutineUses(f.ctx, id)
}

// CompileRoutine checks a routine against the fleet as it is now.
func (d *Dispatcher) CompileRoutine(ctx context.Context, src routine.Source) (routine.Plan, error) {
	return routine.Compile(src, fleetResolver{ctx: ctx, d: d}, routine.DefaultCaps)
}

// RoutineRunRequest is a hand-off to a routine.
type RoutineRunRequest struct {
	RoutineID string
	Inputs    map[string]string
	Origin    core.Origin
	// RequestedBy names who asked — an agent, or empty for a person.
	RequestedBy string
	// Caller is the agent the credential established, and Full says the caller
	// holds a full-scope token (a person, or an agent trusted like one).
	Caller    string
	Full      bool
	Expect    routine.Condition
	InsteadOf int64
}

// StartRoutineRun admits a run: the row first, then the execution, whose ID is
// derived from the row so a retried start cannot run the work twice.
func (d *Dispatcher) StartRoutineRun(ctx context.Context, req RoutineRunRequest) (registry.RoutineRun, error) {
	r, err := d.reg.GetRoutine(ctx, req.RoutineID)
	if err != nil {
		return registry.RoutineRun{}, err
	}
	if r.State != registry.RoutineActive {
		return registry.RoutineRun{}, fmt.Errorf("%w: %s", ErrRoutineArchived, r.ID)
	}
	for _, name := range r.Contract.Inputs {
		if _, ok := req.Inputs[name]; !ok {
			return registry.RoutineRun{}, fmt.Errorf("%w: %s needs %q", ErrRoutineInput, r.ID, name)
		}
	}
	// Running a routine that can act outside is its owner's or a person's to do
	// (adr/009). Anybody may run one that cannot.
	if r.Contract.Effects == routine.EffectExternal && !req.Full && (req.Caller == "" || req.Caller != r.Owner) {
		return registry.RoutineRun{}, fmt.Errorf("%w: %s", ErrRoutineAuthority, r.ID)
	}
	manifest, err := d.reg.RoutineManifest(ctx, r.Plan)
	if err != nil {
		return registry.RoutineRun{}, err
	}
	run, err := d.reg.StartRoutineRun(ctx, registry.RoutineRun{
		RoutineID: r.ID, Version: r.Version, PlanHash: r.Plan.Hash, Manifest: manifest,
		Inputs: req.Inputs, Origin: req.Origin, RequestedBy: req.RequestedBy,
		Expect: req.Expect, InsteadOf: req.InsteadOf,
	})
	if err != nil {
		return registry.RoutineRun{}, err
	}
	if _, err := d.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        contract.RoutineRunID(run.ID),
		TaskQueue: d.cfg.Temporal.TaskQueue,
	}, contract.RoutineRunWorkflowType, rcworkflow.RoutineRunInput{RunID: run.ID, StepQueue: d.cfg.RoutineStepQueue()}); err != nil {
		// The row would otherwise read as running forever.
		if _, finErr := d.reg.FinishRoutineRun(ctx, run.ID, registry.RunOutcome{
			Status: registry.RunError, Reason: "could not start the run: " + err.Error(),
		}); finErr != nil {
			slog.Error("could not mark an unstartable routine run as failed", "run", run.ID, "error", finErr)
		}
		return registry.RoutineRun{}, fmt.Errorf("start routine run %d: %w", run.ID, err)
	}
	return run, nil
}

// StopRoutineRun cancels a run. Cancellation — never termination — so the run
// closes itself out and still reports; it reaches every running step and inner
// run. It never involves a model.
func (d *Dispatcher) StopRoutineRun(ctx context.Context, runID int64) error {
	if _, err := d.reg.GetRoutineRun(ctx, runID); err != nil {
		return err
	}
	return d.tc.CancelWorkflow(ctx, contract.RoutineRunID(runID), "")
}
