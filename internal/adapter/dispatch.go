// Package adapter contains roundclaw's inbound edges: Discord and the HTTP
// API. Both funnel into Dispatcher, so the two event sources share one
// admission path and cannot drift apart in how they queue work.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	rcworkflow "github.com/roundtable/roundclaw/internal/temporal/workflow"
)

// ErrUnknownAgent is returned when a request names an agent that is not
// configured, or arrives on an unbound Discord channel.
var ErrUnknownAgent = errors.New("unknown agent")

// TemporalClient is the slice of client.Client the dispatcher actually uses.
// Narrowing it keeps the admission path testable without a running Temporal
// server, which matters because that path carries the idempotency and SSRF
// checks.
type TemporalClient interface {
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg any,
		options client.StartWorkflowOptions, workflow any, workflowArgs ...any) (client.WorkflowRun, error)
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg any) error
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...any) (converter.EncodedValue, error)
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// Dispatcher admits requests and answers status questions.
//
// It deliberately splits two paths that must not share a failure mode:
// Submit/Stop/Steer go through Temporal, while Status reads SQLite directly.
// Status must keep working when Temporal is unreachable or an agent is wedged,
// because "what is it doing?" is the question a user asks precisely when
// something is wrong.
type Dispatcher struct {
	cfg       *config.Config
	tc        TemporalClient
	stores    *store.Registry
	reg       *registry.Store
	schedules ScheduleBackend
}

// NewDispatcher builds a dispatcher. stores must be opened ReadWrite: the
// adapter inserts the turn row so HTTP can return a turn_id immediately. reg
// holds the agent definitions, which may change while the process runs.
func NewDispatcher(cfg *config.Config, tc TemporalClient, stores *store.Registry, reg *registry.Store) *Dispatcher {
	return &Dispatcher{cfg: cfg, tc: tc, stores: stores, reg: reg}
}

// Registry exposes the agent registry to adapters.
func (d *Dispatcher) Registry() *registry.Store { return d.reg }

// requireAgent looks up an agent and refuses one that is disabled.
//
// Disabling is the reversible way to take an agent out of service: its
// workspace, database and Claude session all survive, so re-enabling picks the
// conversation back up rather than starting over.
func (d *Dispatcher) requireAgent(ctx context.Context, agentID string) (registry.Agent, error) {
	agent, err := d.reg.Get(ctx, agentID)
	if errors.Is(err, registry.ErrNotFound) {
		return registry.Agent{}, fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	}
	if err != nil {
		return registry.Agent{}, err
	}
	if !agent.Enabled {
		return registry.Agent{}, fmt.Errorf("agent %s is disabled", agentID)
	}
	return agent, nil
}

// Submission is the outcome of admitting a request.
type Submission struct {
	TurnID int64
	// QueuePosition is how many requests are ahead of this one, best-effort.
	// Discord and HTTP both share an agent's queue, so a value above zero
	// usually means the other source is mid-turn.
	QueuePosition int
	// Duplicate is true when an idempotency key matched an earlier request and
	// no new turn was created.
	Duplicate bool
}

// Submit queues a request for an agent.
//
// The turn row is written before the signal is sent so that a caller always has
// a durable handle, even if the signal or the worker fails immediately
// afterwards.
func (d *Dispatcher) Submit(ctx context.Context, agentID, text string, origin core.Origin, idempotencyKey string) (Submission, error) {
	return d.SubmitIn(ctx, agentID, "", text, origin, idempotencyKey)
}

// SubmitIn queues a request in a named conversation.
//
// A conversation owns a Claude session, a queue and a workspace, so two of them
// run in parallel without sharing any of the three. Empty means the agent's
// default conversation, which is what /ask, schedules and webhooks use — none
// of them arrive with a thread.
func (d *Dispatcher) SubmitIn(ctx context.Context, agentID, conversationID, text string, origin core.Origin, idempotencyKey string) (Submission, error) {
	return d.submit(ctx, agentID, conversationID, text, origin, idempotencyKey, rcworkflow.SignalEnqueue)
}

// Steer cancels whatever the agent is doing and puts this request at the front
// of its queue. The Claude session is preserved, so the new turn keeps the
// conversation's context; only the interrupted turn's unfinished work is lost.
func (d *Dispatcher) Steer(ctx context.Context, agentID, text string, origin core.Origin, idempotencyKey string) (Submission, error) {
	return d.SteerIn(ctx, agentID, "", text, origin, idempotencyKey)
}

// SteerIn interrupts one conversation. A steer is scoped to its conversation:
// interrupting a thread must not disturb what another thread is doing.
func (d *Dispatcher) SteerIn(ctx context.Context, agentID, conversationID, text string, origin core.Origin, idempotencyKey string) (Submission, error) {
	return d.submit(ctx, agentID, conversationID, text, origin, idempotencyKey, rcworkflow.SignalSteer)
}

func (d *Dispatcher) submit(ctx context.Context, agentID, conversationID, text string, origin core.Origin, idempotencyKey, signal string) (Submission, error) {
	if _, err := d.requireAgent(ctx, agentID); err != nil {
		return Submission{}, err
	}
	// Before the turn row is written, so a refused request leaves no trace and
	// the caller is told rather than left waiting on work that will not run.
	if err := d.checkLimits(ctx, agentID); err != nil {
		return Submission{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Submission{}, fmt.Errorf("request text is empty")
	}
	if len(text) > claude.MaxPromptBytes {
		return Submission{}, fmt.Errorf("request text is %d bytes, limit is %d", len(text), claude.MaxPromptBytes)
	}
	if err := origin.Validate(); err != nil {
		return Submission{}, err
	}

	st, err := d.stores.Get(agentID)
	if err != nil {
		return Submission{}, fmt.Errorf("open store: %w", err)
	}

	turnID, existed, err := st.CreateTurn(ctx, store.NewTurn{
		Request: text, Origin: origin,
		Conversation: conversationID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return Submission{}, err
	}
	if existed {
		// A retry of an already-accepted request. Returning the original turn
		// is what stops a client retry from becoming a second agent run.
		return Submission{TurnID: turnID, Duplicate: true}, nil
	}

	req := core.Request{
		AgentID:        agentID,
		ConversationID: conversationID,
		RequestID:      idempotencyKey,
		TurnID:         turnID,
		Text:           text,
		Origin:         origin,
		ReceivedAt:     time.Now().UTC(),
	}
	if req.RequestID == "" {
		req.RequestID = fmt.Sprintf("turn-%d", turnID)
	}

	workflowID := rcworkflow.WorkflowID(agentID, conversationID)
	// SignalWithStart is what makes an agent self-starting: the first request
	// creates the workflow, every later one just signals the running instance.
	_, err = d.tc.SignalWithStartWorkflow(ctx, workflowID, signal, req,
		client.StartWorkflowOptions{
			ID:        workflowID,
			TaskQueue: d.cfg.Temporal.TaskQueue,
			// The agent is meant to outlive any single request.
			WorkflowExecutionTimeout: 0,
		},
		rcworkflow.SubAgent, rcworkflow.Input{AgentID: agentID, ConversationID: conversationID})
	if err != nil {
		return Submission{TurnID: turnID}, fmt.Errorf("signal agent %s: %w", agentID, err)
	}

	return Submission{TurnID: turnID, QueuePosition: d.queueDepth(ctx, agentID, conversationID)}, nil
}

// StopIn cancels one conversation's current turn and drops its queue. Scoped to
// the conversation: stopping a thread must not stop the others.
func (d *Dispatcher) StopIn(ctx context.Context, agentID, conversationID, reason string) error {
	// Stop deliberately does not require the agent to be enabled: taking an
	// agent out of service must not also take away the ability to stop it.
	if _, err := d.reg.Get(ctx, agentID); errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	} else if err != nil {
		return err
	}
	err := d.tc.SignalWorkflow(ctx, rcworkflow.WorkflowID(agentID, conversationID), "",
		rcworkflow.SignalStop, rcworkflow.StopSignal{ClearQueue: true, Reason: reason})
	if err != nil {
		return fmt.Errorf("signal stop to %s: %w", agentID, err)
	}
	return nil
}

// StopAll cancels every conversation the agent has — the default one and each
// Discord thread — draining their queues. This is what disable and delete need:
// stopping only the default conversation (as plain Stop does) would leave every
// thread's turn running, and after a delete they would fail one turn at a time
// once the definition is gone.
//
// Conversations are enumerated from the agent's own state.db, not by matching
// workflow-ID prefixes. Prefix matching is unsafe: the agent/conversation
// separator is '-', which is also legal inside an agent ID, so a prefix of
// agent "foo" would also catch agent "foo-bar"'s workflows. Comparing stored
// identity avoids the ambiguity entirely.
//
// A per-conversation failure is logged and does not stop the others: a signal
// to an idle conversation whose workflow has already completed is expected, and
// must not prevent stopping the ones that are still running.
func (d *Dispatcher) StopAll(ctx context.Context, agentID, reason string) error {
	if _, err := d.reg.Get(ctx, agentID); errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	} else if err != nil {
		return err
	}

	// Always attempt the default conversation, even if no turn row exists for it
	// yet, so StopAll is never weaker than Stop was.
	targets := map[string]struct{}{"": {}}
	if st, err := d.stores.Get(agentID); err != nil {
		slog.Warn("stop-all: could not open store; stopping only the default conversation",
			"agent", agentID, "error", err)
	} else if convs, err := st.Conversations(ctx); err != nil {
		slog.Warn("stop-all: could not list conversations; stopping only the default conversation",
			"agent", agentID, "error", err)
	} else {
		for _, c := range convs {
			targets[c] = struct{}{}
		}
	}

	var firstErr error
	for conv := range targets {
		if err := d.StopIn(ctx, agentID, conv, reason); err != nil {
			slog.Info("stop-all: could not stop a conversation",
				"agent", agentID, "conversation", conv, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// RunWorkflow starts one run of a workflow — the agent-less, multi-step kind.
// Each manual run is its own Temporal execution, so two runs of the same
// workflow do not collide. Returns the started execution's ID.
func (d *Dispatcher) RunWorkflow(ctx context.Context, id string) (string, error) {
	if _, err := d.reg.GetWorkflow(ctx, id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", ErrUnknownAgent, id)
		}
		return "", err
	}
	execID := fmt.Sprintf("roundclaw-workflow-%s-%d", id, time.Now().UnixNano())
	_, err := d.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        execID,
		TaskQueue: d.cfg.Temporal.TaskQueue,
	}, rcworkflow.RunWorkflowType, rcworkflow.RunWorkflowInput{WorkflowID: id})
	if err != nil {
		return "", fmt.Errorf("start workflow %s: %w", id, err)
	}
	return execID, nil
}

// StatusReport is what /status and GET /v1/agents/{id} return.
type StatusReport struct {
	AgentID     string           `json:"agent_id"`
	State       core.AgentStatus `json:"state"`
	CurrentTurn int64            `json:"current_turn,omitempty"`
	QueueLength int              `json:"queue_length"`
	Recent      []store.LogEntry `json:"recent,omitempty"`
	UpdatedAt   time.Time        `json:"updated_at,omitempty"`
	Budget      Budget           `json:"budget"`
}

// Status answers "what is this agent doing right now?" straight from SQLite.
//
// This path never calls an LLM and never blocks on the workflow, which is the
// point: it has to stay fast and available exactly when an agent is busy or
// stuck. Queue depth is the one field that only the workflow knows, so it is
// fetched separately and degrades to zero rather than failing the whole report.
func (d *Dispatcher) Status(ctx context.Context, agentID string, tail int) (StatusReport, error) {
	if _, err := d.reg.Get(ctx, agentID); errors.Is(err, registry.ErrNotFound) {
		return StatusReport{}, fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	} else if err != nil {
		return StatusReport{}, err
	}

	st, err := d.stores.Get(agentID)
	if err != nil {
		return StatusReport{}, fmt.Errorf("open store: %w", err)
	}

	rt, err := st.GetRuntime(ctx)
	if err != nil {
		return StatusReport{}, err
	}

	report := StatusReport{
		AgentID:     agentID,
		State:       rt.Status,
		CurrentTurn: rt.CurrentTurn,
		UpdatedAt:   rt.UpdatedAt,
		QueueLength: d.queueDepth(ctx, agentID, ""),
	}
	// Best-effort: a status report is more useful without spend figures than
	// not at all.
	if budget, err := d.Budget(ctx, agentID); err == nil {
		report.Budget = budget
	}

	if rt.CurrentTurn > 0 && tail > 0 {
		if logs, err := st.TailLogs(ctx, rt.CurrentTurn, tail); err == nil {
			report.Recent = logs
		}
	}
	return report, nil
}

// queueDepth asks the workflow how much work is waiting. A failure here means
// the agent has never started or Temporal is unreachable; neither should make
// a status request fail, so it reports zero.
func (d *Dispatcher) queueDepth(ctx context.Context, agentID, conversationID string) int {
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	val, err := d.tc.QueryWorkflow(qctx, rcworkflow.WorkflowID(agentID, conversationID), "", rcworkflow.QueryStatus)
	if err != nil {
		return 0
	}
	var st rcworkflow.Status
	if err := val.Get(&st); err != nil {
		return 0
	}
	return st.QueueLength
}

// Store exposes an agent's store for read paths that need more than Status
// gives, such as the SSE stream and single-turn lookups.
func (d *Dispatcher) Store(ctx context.Context, agentID string) (*store.Store, error) {
	if _, err := d.reg.Get(ctx, agentID); errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownAgent, agentID)
	} else if err != nil {
		return nil, err
	}
	return d.stores.Get(agentID)
}

// Config exposes the loaded configuration to adapters.
func (d *Dispatcher) Config() *config.Config { return d.cfg }

// ResolveAgent picks the agent a Discord interaction is aimed at.
//
// An explicit agent argument always wins, which is what lets any agent be
// called from any channel — including channels bound to nothing. Falling back
// to the channel binding keeps the common case ("just talk in the team's
// channel") free of ceremony.
func ResolveAgent(ctx context.Context, reg *registry.Store, explicitID, channelID string) (registry.Agent, error) {
	if explicitID != "" {
		// Some Discord clients (notably mobile) submit an autocomplete choice's
		// display name — the agent's label, "id — description" — instead of its
		// value, the bare id. An agent id can never contain " — " (its charset is
		// [A-Za-z0-9_-]), so a leaked label is unambiguous: take the id in front
		// of it. The HTTP path never sees a label, so this is a no-op there.
		if i := strings.Index(explicitID, " — "); i >= 0 {
			explicitID = strings.TrimSpace(explicitID[:i])
		}
		agent, err := reg.Get(ctx, explicitID)
		if errors.Is(err, registry.ErrNotFound) {
			return registry.Agent{}, fmt.Errorf("%w: %s", ErrUnknownAgent, explicitID)
		}
		return agent, err
	}
	agent, err := reg.ByChannel(ctx, channelID)
	if errors.Is(err, registry.ErrNotFound) {
		return registry.Agent{}, fmt.Errorf("%w: this channel is bound to no agent — name one explicitly", ErrUnknownAgent)
	}
	return agent, err
}
