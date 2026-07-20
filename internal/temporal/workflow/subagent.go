// Package workflow holds roundclaw's deterministic orchestration. Nothing here
// may read a clock, touch the filesystem, hit the network, or open SQLite —
// all of that lives in activities. The workflow's job is to own the request
// queue and the turn lifecycle, and to survive worker restarts while doing it.
package workflow

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/temporal/activity"
)

// Signal and query names. These are part of the wire contract with the
// gateway, so renaming one breaks in-flight workflows.
const (
	SignalEnqueue = "enqueue"
	SignalStop    = "stop"
	SignalSteer   = "steer"
	QueryStatus   = "status"
)

// maxTurnsPerRun bounds workflow history before Continue-As-New. Temporal also
// suggests continuing on its own; whichever fires first wins.
const maxTurnsPerRun = 100

// WorkflowID builds the deterministic workflow ID for an agent. The Claude
// session UUID is derived from this string, so it must stay stable for the life
// of the agent — changing the format orphans every existing session.
func WorkflowID(agentID string) string {
	return "roundclaw-agent-" + agentID
}

// Input starts or continues a SubAgentWorkflow.
type Input struct {
	AgentID string `json:"agent_id"`

	// Queue carries requests across Continue-As-New so nothing is dropped at
	// the boundary.
	Queue []core.Request `json:"queue,omitempty"`

	// TurnCount is cumulative across Continue-As-New. It is what decides
	// --session-id versus --resume, so it must never reset.
	TurnCount int `json:"turn_count,omitempty"`
}

// StopSignal asks the agent to abandon its current turn.
type StopSignal struct {
	// ClearQueue drops queued requests too. /stop sets it; /steer does not,
	// because steering means "do this instead", not "forget everything".
	ClearQueue bool   `json:"clear_queue"`
	Reason     string `json:"reason,omitempty"`
}

// Status is the query response. It reports only what the workflow actually
// owns; live transcript output comes from SQLite, which the gateway reads
// directly because a query handler cannot legally do I/O.
type Status struct {
	AgentID       string           `json:"agent_id"`
	State         core.AgentStatus `json:"state"`
	CurrentTurnID int64            `json:"current_turn_id"`
	QueueLength   int              `json:"queue_length"`
	TurnCount     int              `json:"turn_count"`
}

// SubAgent is one persistent agent. Exactly one execution exists per agent, it
// lives indefinitely via Continue-As-New, and every request for that agent
// flows through its queue regardless of which adapter produced it.
func SubAgent(ctx workflow.Context, in Input) error {
	log := workflow.GetLogger(ctx)

	state := &agentState{
		agentID:   in.AgentID,
		queue:     append([]core.Request(nil), in.Queue...),
		turnCount: in.TurnCount,
		lifecycle: core.AgentIdle,
	}

	if err := workflow.SetQueryHandler(ctx, QueryStatus, state.status); err != nil {
		return fmt.Errorf("register status query: %w", err)
	}

	enqueueCh := workflow.GetSignalChannel(ctx, SignalEnqueue)
	stopCh := workflow.GetSignalChannel(ctx, SignalStop)
	steerCh := workflow.GetSignalChannel(ctx, SignalSteer)

	for {
		// Idle: wait for anything to arrive. A stop that lands while idle is
		// drained and discarded — there is nothing to cancel.
		if len(state.queue) == 0 {
			sel := workflow.NewSelector(ctx)
			sel.AddReceive(enqueueCh, func(c workflow.ReceiveChannel, _ bool) {
				state.receive(ctx, c, false)
			})
			sel.AddReceive(steerCh, func(c workflow.ReceiveChannel, _ bool) {
				state.receive(ctx, c, true)
			})
			sel.AddReceive(stopCh, func(c workflow.ReceiveChannel, _ bool) {
				var sig StopSignal
				c.Receive(ctx, &sig)
				if sig.ClearQueue {
					state.abandonQueue(ctx, sig.Reason)
				}
			})
			sel.Select(ctx)
			continue
		}

		// Continue-As-New only at a quiet point: never mid-turn, and only once
		// the signal channels are drained, so no request is lost at the seam.
		if state.shouldContinue(ctx) && !hasBuffered(enqueueCh, steerCh, stopCh) {
			log.Info("continuing as new", "turn_count", state.turnCount, "queued", len(state.queue))
			return workflow.NewContinueAsNewError(ctx, SubAgent, Input{
				AgentID:   state.agentID,
				Queue:     state.queue,
				TurnCount: state.turnCount,
			})
		}

		req := state.queue[0]
		state.queue = state.queue[1:]
		state.runTurn(ctx, req, enqueueCh, stopCh, steerCh)
	}
}

type agentState struct {
	agentID       string
	queue         []core.Request
	turnCount     int
	currentTurnID int64
	lifecycle     core.AgentStatus
}

func (s *agentState) status() (Status, error) {
	return Status{
		AgentID:       s.agentID,
		State:         s.lifecycle,
		CurrentTurnID: s.currentTurnID,
		QueueLength:   len(s.queue),
		TurnCount:     s.turnCount,
	}, nil
}

// receive appends an incoming request. A steer jumps the queue; a normal
// request goes to the back. Requests are never coalesced — one request always
// produces one turn and one reply, which is the behaviour nanoclaw's boolean
// pending flag could not provide.
func (s *agentState) receive(ctx workflow.Context, c workflow.ReceiveChannel, front bool) {
	var req core.Request
	c.Receive(ctx, &req)
	if req.TurnID == 0 {
		workflow.GetLogger(ctx).Warn("dropping request with no turn id", "agent", s.agentID)
		return
	}
	if front {
		s.queue = append([]core.Request{req}, s.queue...)
	} else {
		s.queue = append(s.queue, req)
	}
}

// abandonQueue drops everything waiting and closes out its turn rows.
//
// Dropping the slice alone would leave those rows open forever: they would show
// as running in /status and keep counting against the hourly rate limit, so one
// /stop would permanently eat part of the agent's budget.
func (s *agentState) abandonQueue(ctx workflow.Context, reason string) {
	if len(s.queue) == 0 {
		return
	}
	ids := make([]int64, 0, len(s.queue))
	for _, req := range s.queue {
		ids = append(ids, req.TurnID)
	}
	s.queue = nil

	// Disconnected so a stop that arrives with the workflow itself being
	// cancelled still records the outcome.
	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	actCtx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumAttempts: 3,
		},
	})
	err := workflow.ExecuteActivity(actCtx, (*activity.Activities).AbandonTurns, activity.AbandonInput{
		AgentID: s.agentID,
		TurnIDs: ids,
		Reason:  reason,
	}).Get(actCtx, nil)
	if err != nil {
		workflow.GetLogger(ctx).Error("failed to close out abandoned turns",
			"agent", s.agentID, "turns", len(ids), "error", err)
	}
}

func (s *agentState) shouldContinue(ctx workflow.Context) bool {
	return s.turnCount >= maxTurnsPerRun || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
}

// runTurn executes one turn, staying responsive to signals throughout.
func (s *agentState) runTurn(
	ctx workflow.Context,
	req core.Request,
	enqueueCh, stopCh, steerCh workflow.ReceiveChannel,
) {
	log := workflow.GetLogger(ctx)

	s.lifecycle = core.AgentRunning
	s.currentTurnID = req.TurnID

	// A child context so a stop cancels only this turn. The workflow context
	// itself stays live, which is what lets the next turn start immediately
	// after a steer.
	turnCtx, cancelTurn := workflow.WithCancel(ctx)
	defer cancelTurn()

	actCtx := workflow.WithActivityOptions(turnCtx, workflow.ActivityOptions{
		StartToCloseTimeout: activityTimeout(ctx),
		// How long before Temporal declares a silent activity dead, and so the
		// dominant term in crash-recovery latency. The activity starts
		// heartbeating within a second of spawning the container — before any
		// image pull or container start can stall it — so a tight value here is
		// safe and buys much faster recovery. It only works because the worker
		// caps MaxHeartbeatThrottleInterval; with the SDK default this would be
		// shorter than the interval at which heartbeats are actually sent, and
		// every turn would be killed as unresponsive.
		HeartbeatTimeout: 10 * time.Second,
		// Cancellation must actually reach the activity rather than being
		// resolved optimistically, or the container would keep running.
		WaitForCancellation: true,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    3,
			// Configuration errors will fail identically on every retry.
			NonRetryableErrorTypes: []string{"roundclaw: non-retryable"},
		},
	})

	future := workflow.ExecuteActivity(actCtx, (*activity.Activities).RunClaudeTurn, activity.RunTurnInput{
		AgentID:    s.agentID,
		TurnID:     req.TurnID,
		WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,
		Prompt:     req.Text,
		// The first turn of an agent's life creates the session; every later
		// turn resumes it. TurnCount is cumulative across Continue-As-New, so
		// this stays correct after the history is truncated.
		Resume: s.turnCount > 0,
	})

	var (
		result core.TurnResult
		actErr error
		done   bool
	)

	for !done {
		sel := workflow.NewSelector(ctx)

		sel.AddFuture(future, func(f workflow.Future) {
			actErr = f.Get(ctx, &result)
			done = true
		})
		// Requests that arrive mid-turn queue up rather than being merged into
		// the running turn; each will get its own reply.
		sel.AddReceive(enqueueCh, func(c workflow.ReceiveChannel, _ bool) {
			s.receive(ctx, c, false)
		})
		sel.AddReceive(steerCh, func(c workflow.ReceiveChannel, _ bool) {
			s.receive(ctx, c, true)
			s.lifecycle = core.AgentStopping
			log.Info("steer received; cancelling current turn", "turn_id", req.TurnID)
			cancelTurn()
		})
		sel.AddReceive(stopCh, func(c workflow.ReceiveChannel, _ bool) {
			var sig StopSignal
			c.Receive(ctx, &sig)
			if sig.ClearQueue {
				s.abandonQueue(ctx, sig.Reason)
			}
			s.lifecycle = core.AgentStopping
			log.Info("stop received; cancelling current turn", "turn_id", req.TurnID, "reason", sig.Reason)
			cancelTurn()
		})

		sel.Select(ctx)
	}

	s.turnCount++
	s.currentTurnID = 0
	s.lifecycle = core.AgentIdle

	if actErr != nil {
		if temporal.IsCanceledError(actErr) {
			// The activity already recorded the turn as stopped and wrote the
			// partial transcript, so there is nothing to deliver.
			log.Info("turn cancelled", "turn_id", req.TurnID)
			return
		}
		log.Error("turn failed", "turn_id", req.TurnID, "error", actErr)
		s.lifecycle = core.AgentFailed
		result = core.TurnResult{
			TurnID:       req.TurnID,
			Status:       core.TurnError,
			ErrorMessage: actErr.Error(),
		}
	}

	s.deliver(ctx, req, result)
}

// deliver sends the result to wherever the request came from.
//
// It runs on a disconnected context so that a workflow-level cancellation still
// tells the caller what happened; without this, cancelling the workflow would
// leave a Discord user or an HTTP callback waiting forever.
func (s *agentState) deliver(ctx workflow.Context, req core.Request, result core.TurnResult) {
	disconnected, cancelDisconnected := workflow.NewDisconnectedContext(ctx)
	defer cancelDisconnected()

	deliverCtx := workflow.WithActivityOptions(
		disconnected,
		workflow.ActivityOptions{
			StartToCloseTimeout: 2 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:    2 * time.Second,
				BackoffCoefficient: 2,
				MaximumInterval:    30 * time.Second,
				MaximumAttempts:    5,
			},
		})

	err := workflow.ExecuteActivity(deliverCtx, (*activity.Activities).DeliverResponse, activity.DeliverInput{
		AgentID: s.agentID,
		Origin:  req.Origin,
		Result:  result,
	}).Get(deliverCtx, nil)
	if err != nil {
		// Delivery is best-effort by design: the result is already durable in
		// SQLite, so a permanently unreachable Discord channel or callback URL
		// must not fail the agent.
		workflow.GetLogger(ctx).Error("response delivery failed",
			"turn_id", req.TurnID, "origin", req.Origin.String(), "error", err)
	}
}

// activityTimeout bounds a single turn. It mirrors container.turn_timeout but
// is read from workflow memo-free config baked into the worker, so the workflow
// stays deterministic.
func activityTimeout(ctx workflow.Context) time.Duration {
	// Deliberately a constant rather than a config read: changing config must
	// not alter the behaviour of a workflow that is already replaying.
	_ = ctx
	return 35 * time.Minute
}

// hasBuffered reports whether any channel still holds an unconsumed signal.
// Continue-As-New with a buffered signal would silently drop it.
func hasBuffered(channels ...workflow.ReceiveChannel) bool {
	for _, c := range channels {
		if c.Len() > 0 {
			return true
		}
	}
	return false
}
