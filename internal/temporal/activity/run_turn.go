// Package activity holds roundclaw's non-deterministic work: running the
// `claude` container and delivering responses. Everything that touches the
// filesystem, the network, or a clock lives here rather than in the workflow.
package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/workspace"
)

// heartbeatInterval is how often the activity reports progress.
//
// This is not cosmetic. An activity only learns it has been cancelled through a
// heartbeat response, so this bounds how long /stop and /steer take to reach a
// running container.
//
// Calling RecordHeartbeat this often is necessary but not sufficient: the SDK
// throttles outbound heartbeats to about 80% of HeartbeatTimeout, so a 30s
// timeout would collapse these one-second calls into one network round trip
// every ~24s and make cancellation feel broken. The worker therefore also caps
// MaxHeartbeatThrottleInterval — see cmd/worker/main.go. Both halves are
// required.
const heartbeatInterval = time.Second

// Activities carries the dependencies shared by every activity.
//
// Every exported method on this type is registered as a Temporal activity, so
// it must expose nothing but activities — a stray exported setter makes worker
// registration panic at startup. Dependencies are therefore constructor
// arguments, never setters.
type Activities struct {
	cfg     *config.Config
	stores  *store.Registry
	reg     *registry.Store
	discord DiscordSender
	slack   SlackSender
	// signal queues scheduled requests. Nil when the worker was built without
	// a Temporal client, which only happens in tests.
	signal TemporalSignaller
}

// NewActivities builds the activity set. The store registry must be opened in
// ReadWrite mode: this is the only writer.
//
// Either chat sender may be nil when that tool is not configured; a delivery to
// it then fails as non-retryable rather than blocking startup. Running with one
// chat tool and not the other is a supported configuration, so a missing sender
// is an ordinary state and not a misconfiguration to refuse at boot.
func NewActivities(cfg *config.Config, stores *store.Registry, reg *registry.Store,
	discord DiscordSender, slack SlackSender, signal TemporalSignaller) *Activities {
	return &Activities{cfg: cfg, stores: stores, reg: reg, discord: discord, slack: slack, signal: signal}
}

// RunTurnInput is one agent turn.
type RunTurnInput struct {
	AgentID    string `json:"agent_id"`
	TurnID     int64  `json:"turn_id"`
	WorkflowID string `json:"workflow_id"`
	// ConversationID selects the workspace. The session follows the workflow
	// ID, which already encodes it.
	ConversationID string `json:"conversation_id,omitempty"`
	Prompt         string `json:"prompt"`
}

// recapTurns is how many past turns are summarised when a session is lost.
// Enough to re-establish what the agent was working on, few enough that the
// recap does not crowd out the actual request.
const recapTurns = 10

// maxRecapField bounds each quoted request and result.
const maxRecapField = 400

// TurnProgress is the heartbeat payload. It is visible through
// DescribeWorkflowExecution, which makes a stuck turn diagnosable without
// reading the database.
type TurnProgress struct {
	TurnID    int64  `json:"turn_id"`
	Events    int    `json:"events"`
	LastTool  string `json:"last_tool,omitempty"`
	StartedAt string `json:"started_at"`
}

// RunClaudeTurn runs one turn to completion inside a container.
//
// On cancellation it stops the container gracefully, records the turn as
// stopped, and returns a cancelled error. The partial transcript stays in
// SQLite either way, so /status and the HTTP API can still show what happened.
func (a *Activities) RunClaudeTurn(ctx context.Context, in RunTurnInput) (core.TurnResult, error) {
	log := activity.GetLogger(ctx)

	st, err := a.stores.Get(in.AgentID)
	if err != nil {
		return core.TurnResult{}, fmt.Errorf("open store: %w", err)
	}

	// Read at the start of each turn, so an edit to the definition takes effect
	// on the next turn rather than the one already running.
	agentCfg, err := a.reg.Get(ctx, in.AgentID)
	if errors.Is(err, registry.ErrNotFound) {
		// A deleted agent will not reappear on retry, so this must not spin.
		return core.TurnResult{}, newNonRetryable(fmt.Errorf("unknown agent %q", in.AgentID))
	}
	if err != nil {
		return core.TurnResult{}, fmt.Errorf("read agent %s: %w", in.AgentID, err)
	}

	cred, err := a.cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		return core.TurnResult{}, newNonRetryable(err)
	}

	if err := a.ensureAgentDirs(in.AgentID); err != nil {
		return core.TurnResult{}, err
	}

	// Parallel conversations cannot share a working tree, so each gets its own
	// — a directory, or a git worktree when there is a repository to branch.
	workspace, err := a.resolveWorkspace(ctx, agentCfg, in.ConversationID)
	if err != nil {
		return core.TurnResult{}, err
	}

	// Uploads were staged at admission, before this workspace was known or even
	// created. Now that it is, they go where the prompt already told the agent to
	// look — and a failure fails the turn, because answering a question about a
	// document nobody managed to deliver is worse than saying so.
	staged, err := st.TurnAttachments(ctx, in.TurnID)
	if err != nil {
		return core.TurnResult{}, err
	}
	if err := placeAttachments(workspace, staged); err != nil {
		return core.TurnResult{}, err
	}
	if len(staged) > 0 {
		log.Info("placed uploads in the conversation's workspace",
			"agent", in.AgentID, "conversation", in.ConversationID,
			"workspace", workspace, "files", len(staged))
	}

	// Registered secrets to inject as container env vars. Nil when no master key
	// is configured, so an agent that uses none is unaffected. The credential is
	// injected separately and must win, so a same-named secret is dropped rather
	// than allowed to shadow it.
	secrets, err := a.reg.SecretsForAgent(ctx, in.AgentID)
	if err != nil {
		return core.TurnResult{}, fmt.Errorf("load secrets for %s: %w", in.AgentID, err)
	}
	delete(secrets, cred.EnvName)

	// Registered tools the agent is granted: each adds a read-only mount and the
	// env it needs, and a note prepended to the prompt so the agent knows the
	// capability is there. Resolved here rather than baked into the definition so a
	// tool's path or env can be corrected without touching every agent that uses
	// it. The credential still wins over any same-named tool env.
	if secrets == nil {
		secrets = map[string]string{}
	}
	toolDirs, toolNote, err := a.resolveTools(ctx, agentCfg.Tools, secrets)
	if err != nil {
		return core.TurnResult{}, err
	}
	delete(secrets, cred.EnvName)

	// Granted Claude Code skills: each mounts into ~/.claude/skills/<id>, where the
	// CLI discovers it. Resolved here, like tools, so a skill's path can change
	// without editing every agent that has it.
	skills, err := a.resolveSkills(ctx, agentCfg.Skills)
	if err != nil {
		return core.TurnResult{}, err
	}

	// A per-agent image override wins over the global default, so one agent can
	// run a purpose-built image (a dev agent with the docker CLI) without
	// changing the image the rest of the fleet shares. Empty keeps the default.
	image := a.cfg.Container.Image
	if agentCfg.Image != "" {
		image = agentCfg.Image
	}

	// Same shape for the model: the fleet runs one model unless an agent names
	// its own. Both empty leaves the choice to the CLI's default.
	model := a.cfg.Container.Model
	if agentCfg.Model != "" {
		model = agentCfg.Model
	}

	// Who and where this turn is, so the roundclaw CLI inside the container can
	// name itself instead of making the model guess an ID. Set last, and
	// deliberately after the secrets and tool env: a registered secret must not
	// be able to shadow the agent's own identity, which `say` and `--notify-me`
	// are authorised against.
	for name, value := range a.identityEnv(ctx, st, in) {
		secrets[name] = value
	}

	spec := claude.RunSpec{
		Runtime:         a.cfg.Container.Runtime,
		Image:           image,
		ContainerName:   claude.ContainerName(in.AgentID, in.TurnID),
		WorkDir:         workspace,
		DenyPaths:       agentCfg.DenyPaths,
		ClaudeHome:      a.cfg.ClaudeHomeDir(in.AgentID),
		AdditionalDirs:  append(append([]string{}, agentCfg.AdditionalDirs...), toolDirs...),
		CredentialEnv:   cred.EnvName,
		CredentialValue: cred.Value,
		SessionID:       claude.SessionID(in.WorkflowID),
		AgentName:       agentCfg.AgentName,
		PermissionMode:  agentCfg.PermissionMode,
		AllowedTools:    agentCfg.AllowedTools,
		Model:           model,
		Network:         a.cfg.Container.Network,
		GroupAdd:        agentCfg.GroupAdd,
		Secrets:         secrets,
		Skills:          skills,
		Prompt:          toolNote + in.Prompt,
	}

	// Whether this turn continues the session or opens one is read off the disk
	// the CLI writes to, every turn, rather than remembered between turns. The
	// workflow used to carry it, and a belief nothing ever re-checked is exactly
	// what got stuck: one turn that failed before the container started left it
	// claiming a live session was gone, permanently.
	exists, err := claude.SessionExists(spec.ClaudeHome, spec.SessionID)
	if err != nil {
		// Proceeding as though there were no session is safe — the run corrects
		// a wrong guess — but an unreadable claude-home is worth saying out loud.
		log.Warn("could not check for an existing session; assuming there is none",
			"agent", in.AgentID, "error", err)
	}
	spec.Resume = exists

	_ = st.SetRuntime(ctx, core.AgentRunning, in.TurnID, spec.SessionID)

	result, err := a.runTurn(ctx, st, spec, in)

	// Persistence must survive cancellation, so it runs on a context that the
	// cancellation does not reach.
	persistCtx := context.WithoutCancel(ctx)
	if finErr := st.FinishTurn(persistCtx, in.TurnID, result); finErr != nil {
		log.Error("failed to record turn outcome", "turn_id", in.TurnID, "error", finErr)
	}
	agentStatus := core.AgentIdle
	if result.Status == core.TurnError {
		agentStatus = core.AgentFailed
	}
	_ = st.SetRuntime(persistCtx, agentStatus, in.TurnID, spec.SessionID)

	return result, err
}

// runTurn runs the container, and runs it once more with the other session flag
// if the first attempt failed because the flag was wrong.
//
// This is what makes the activity safe under Temporal's at-least-once execution.
// A turn that opens the session and then loses its worker before reporting
// completion is retried with the same input; the session now exists, so
// --session-id is refused — and every later turn was refused with it, because
// nothing ever revisited the decision. removeOrphan already makes the
// deterministic container name reusable across exactly that retry. The
// deterministic session ID needs the same treatment, and this is it.
//
// It also collapses session-loss recovery into one turn. A transcript cleaned up
// underneath a conversation used to cost the turn that discovered it; now that
// turn resumes, finds nothing, and opens a new session with a recap instead.
func (a *Activities) runTurn(
	ctx context.Context,
	st *store.Store,
	spec claude.RunSpec,
	in RunTurnInput,
) (core.TurnResult, error) {
	log := activity.GetLogger(ctx)
	base := spec.Prompt

	spec.Prompt = a.sessionPrompt(ctx, st, base, spec.Resume, in)
	result, sawInit, err := a.attempt(ctx, st, spec, in)
	if err != nil {
		return result, err
	}
	if !a.sessionFlagWrong(ctx, spec, in, result, sawInit) {
		return result, nil
	}

	spec.Resume = !spec.Resume
	log.Info("the session flag disagreed with what is on disk; running the turn again with the other one",
		"agent", in.AgentID, "turn_id", in.TurnID, "resume", spec.Resume)

	spec.Prompt = a.sessionPrompt(ctx, st, base, spec.Resume, in)
	result, _, err = a.attempt(ctx, st, spec, in)
	return result, err
}

// attempt runs the container once and reports whether the CLI got as far as
// opening a session — the only positive evidence that the flag it was given was
// the right one.
func (a *Activities) attempt(
	ctx context.Context,
	st *store.Store,
	spec claude.RunSpec,
	in RunTurnInput,
) (core.TurnResult, bool, error) {
	args, err := spec.Args()
	if err != nil {
		// A terminal result, not a bare error: this return reaches FinishTurn,
		// and a turn closed out with the zero value would leave a row with no
		// status at all — neither running, nor done, nor failed.
		return errorResult(in.TurnID, err), false, newNonRetryable(err)
	}

	// A worker that died mid-turn leaves the container running under the same
	// deterministic name. Clearing it is what makes the name safe to reuse on
	// retry, and it is also how a crashed turn's orphan gets reaped.
	a.removeOrphan(ctx, spec)

	return a.stream(ctx, st, spec, args, in)
}

// sessionPrompt composes the prompt for one attempt.
//
// A turn that opens a session is owed the record of what came before it; a turn
// that continues one is not, because the session still holds it. The recap is
// empty for a conversation with no history, which is what makes this right for
// an agent's very first turn as well as for one whose session went missing.
func (a *Activities) sessionPrompt(
	ctx context.Context,
	st *store.Store,
	base string,
	resume bool,
	in RunTurnInput,
) string {
	if resume {
		return base
	}

	// The conversation itself is gone; this is a reconstruction from the turn
	// record, and it is labelled as one so the agent does not treat a summary as
	// though it remembered the exchange.
	recap, err := buildRecap(ctx, st, in.ConversationID)
	if err != nil {
		// Worth a turn without context rather than no turn at all.
		activity.GetLogger(ctx).Warn("could not rebuild context for a new session",
			"agent", in.AgentID, "error", err)
		return base
	}
	if recap == "" {
		return base
	}
	activity.GetLogger(ctx).Info("rebuilt context from the turn history for a new session",
		"agent", in.AgentID, "turn_id", in.TurnID)
	return recap + base
}

// sessionFlagWrong reports whether a failed turn failed over the session flag it
// was given rather than over the work it was asked to do.
//
// Every condition here is positive evidence, on purpose. A bare failure means
// nothing about the session: the container can fail to start, the credential can
// expire, the agent can exhaust its quota, and none of that says whether the
// session exists. Reading an uninformative failure as "the session is gone" is
// the mistake this path exists to stop making — it is how one oversized prompt
// took a conversation out for good.
func (a *Activities) sessionFlagWrong(
	ctx context.Context,
	spec claude.RunSpec,
	in RunTurnInput,
	result core.TurnResult,
	sawInit bool,
) bool {
	// The CLI attached to a session, so the flag was right whatever failed after.
	if sawInit || result.Status != core.TurnError {
		return false
	}

	exists, err := claude.SessionExists(spec.ClaudeHome, spec.SessionID)
	if err != nil {
		activity.GetLogger(ctx).Warn("could not check for an existing session after a failed turn",
			"agent", in.AgentID, "error", err)
	}
	// The filesystem and the CLI's own complaint are independent answers to the
	// same question, and either is enough, so each direction survives one of the
	// two going wrong.
	if spec.Resume {
		// Asked to continue a session that is not there.
		return !exists || claude.SessionNotFound(result.ErrorMessage)
	}
	// Asked to create one that is.
	return exists || claude.SessionTaken(result.ErrorMessage)
}

// stream starts the container and pumps its stream-json output into SQLite,
// heartbeating throughout. It reports whether the CLI emitted its init event,
// which is what tells the caller the session flag was accepted.
func (a *Activities) stream(
	ctx context.Context,
	st *store.Store,
	spec claude.RunSpec,
	args []string,
	in RunTurnInput,
) (core.TurnResult, bool, error) {
	log := activity.GetLogger(ctx)
	persistCtx := context.WithoutCancel(ctx)

	// Not exec.CommandContext: the default kill-on-cancel would SIGKILL the
	// runtime client and orphan the container. Shutdown is handled explicitly
	// below so `claude` gets a chance to flush its session transcript.
	cmd := exec.Command(spec.Runtime, args...)
	// The credential and every registered secret are set here, on the subprocess
	// environment, and referenced by name in argv — so no value appears in the
	// process table or a Temporal history event.
	cmd.Env = append(os.Environ(), spec.CredentialEnv+"="+spec.CredentialValue)
	for name, value := range spec.Secrets {
		cmd.Env = append(cmd.Env, name+"="+value)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errorResult(in.TurnID, fmt.Errorf("stdout pipe: %w", err)), false, nil
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return errorResult(in.TurnID, fmt.Errorf("start container: %w", err)), false, nil
	}

	var (
		mu       sync.Mutex
		progress = TurnProgress{TurnID: in.TurnID, StartedAt: time.Now().UTC().Format(time.RFC3339)}
		final    core.TurnResult
		sawFinal bool
		sawInit  bool
	)

	onEvent := func(ev claude.Event) error {
		mu.Lock()
		progress.Events++
		if ev.Kind == claude.KindToolUse {
			progress.LastTool = ev.ToolName
		}
		if ev.Kind == claude.KindResult {
			sawFinal = true
			final = core.TurnResult{
				TurnID:  in.TurnID,
				Status:  core.TurnDone,
				Text:    ev.Text,
				CostUSD: ev.CostUSD,
			}
			if ev.IsError {
				final.Status = core.TurnError
				final.ErrorMessage = ev.Text
			}
		}
		if ev.Kind == claude.KindInit {
			sawInit = true
		}
		if ev.Kind == claude.KindInit && ev.SessionID != "" && ev.SessionID != spec.SessionID {
			// Not fatal, but it means resume did not attach to the session we
			// expected and this turn has lost its history.
			log.Warn("claude reported an unexpected session id",
				"want", spec.SessionID, "got", ev.SessionID)
		}
		mu.Unlock()

		// Persisted on a cancellation-immune context so the tail of a stopped
		// turn is not lost between the stop request and the process exiting.
		return persistEvent(persistCtx, st, in.TurnID, ev)
	}

	scanDone := make(chan error, 1)
	go func() {
		scanDone <- claude.Scan(stdout, onEvent, func(err error) {
			log.Warn("skipped an undecodable output line", "error", err)
		})
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mu.Lock()
			snapshot := progress
			mu.Unlock()
			activity.RecordHeartbeat(ctx, snapshot)

		case scanErr := <-scanDone:
			waitErr := cmd.Wait()
			mu.Lock()
			defer mu.Unlock()
			result := a.finish(in.TurnID, final, sawFinal, scanErr, waitErr, stderr.String())
			return result, sawInit, nil

		case <-ctx.Done():
			// Cancellation arrives only because a heartbeat carried it back,
			// which is why the ticker above is mandatory.
			log.Info("turn cancelled; stopping container", "container", spec.ContainerName)
			a.stopContainer(spec)
			<-scanDone
			_ = cmd.Wait()
			mu.Lock()
			stopped := core.TurnResult{
				TurnID:  in.TurnID,
				Status:  core.TurnStopped,
				Text:    final.Text,
				CostUSD: final.CostUSD,
			}
			mu.Unlock()
			return stopped, sawInit, ctx.Err()
		}
	}
}

// finish converts process outcome into a TurnResult. A container that exits
// without emitting a terminal `result` event is an error even if its exit code
// was zero, because that means the turn produced no answer.
func (a *Activities) finish(turnID int64, final core.TurnResult, sawFinal bool, scanErr, waitErr error, stderr string) core.TurnResult {
	if scanErr != nil {
		return errorResult(turnID, fmt.Errorf("reading claude output: %w", scanErr))
	}
	if waitErr != nil && !sawFinal {
		return errorResult(turnID, fmt.Errorf("claude exited: %w%s", waitErr, stderrSuffix(stderr)))
	}
	if !sawFinal {
		return errorResult(turnID, fmt.Errorf("claude produced no result event%s", stderrSuffix(stderr)))
	}
	return final
}

// stopContainer asks the runtime to stop the container gracefully, then forces
// it. The grace period matters: `claude` writes its session transcript on the
// way out, and killing it outright can cost the next turn its history.
func (a *Activities) stopContainer(spec claude.RunSpec) {
	grace := int(a.cfg.Container.StopGrace.Seconds())
	if grace < 1 {
		grace = 1
	}

	// Detached from the cancelled activity context, with its own bound.
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Container.StopGrace+10*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, spec.Runtime, spec.StopArgs(grace)...).Run(); err == nil {
		return
	}
	forceCtx, forceCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer forceCancel()
	_ = exec.CommandContext(forceCtx, spec.Runtime, spec.RemoveArgs()...).Run()
}

// removeOrphan clears a container left behind by a crashed worker. A failure
// here is expected and ignored: the usual case is that no such container
// exists.
func (a *Activities) removeOrphan(ctx context.Context, spec claude.RunSpec) {
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(rmCtx, spec.Runtime, spec.RemoveArgs()...).Run()
}

func (a *Activities) ensureAgentDirs(agentID string) error {
	for _, dir := range []string{a.cfg.WorkDir(agentID), a.cfg.ClaudeHomeDir(agentID)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return a.seedClaudeSettings(agentID)
}

// seedClaudeSettings writes the minimum settings a non-interactive run needs.
//
// A fresh ~/.claude makes the CLI treat the run as a first launch and ask for
// theme selection. There is no terminal to answer, so the turn stalls or dies
// with nothing useful in the transcript. Declaring onboarding complete is what
// keeps an agent's very first turn from being its worst one.
//
// Written only when absent, so an operator can edit the file and keep their
// changes across turns.
func (a *Activities) seedClaudeSettings(agentID string) error {
	path := filepath.Join(a.cfg.ClaudeHomeDir(agentID), "settings.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	body, err := json.MarshalIndent(map[string]any{
		"hasCompletedOnboarding": true,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode claude settings: %w", err)
	}
	// World-readable: the container runs as its own user, which will not match
	// the worker's uid.
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func persistEvent(ctx context.Context, st *store.Store, turnID int64, ev claude.Event) error {
	var kind core.LogKind
	var content string

	switch ev.Kind {
	case claude.KindText:
		kind, content = core.LogText, ev.Text
	case claude.KindToolUse:
		kind, content = core.LogToolUse, ev.ToolName+" "+ev.ToolInput
	case claude.KindToolResult:
		kind, content = core.LogToolResult, ev.ToolResult
	case claude.KindAPIRetry:
		kind = core.LogAPIRetry
		content = fmt.Sprintf("retry %d/%d: %s", ev.RetryAttempt, ev.RetryMax, ev.RetryError)
	case claude.KindInit:
		kind, content = core.LogSystem, "session started"
	case claude.KindRateLimit:
		// The CLI emits one of these per turn, nearly always "allowed". Logging
		// them all would bury the actual transcript under boilerplate, so only
		// the states a human can act on are recorded.
		if ev.RateLimitStatus == "" || ev.RateLimitStatus == "allowed" {
			return nil
		}
		kind = core.LogSystem
		content = fmt.Sprintf("rate limit %s (%s)", ev.RateLimitStatus, ev.RateLimitType)
	case claude.KindResult:
		// The final text is stored on the turn row itself; duplicating it in
		// the live log would show the answer twice in /status.
		return nil
	default:
		// An event this binary does not understand. It must not reach the live
		// log: that log is read by humans in /status, and a raw JSON blob from
		// a newer CLI is noise, not progress.
		return nil
	}
	if strings.TrimSpace(content) == "" {
		return nil
	}
	return st.AppendLog(ctx, turnID, kind, content)
}

func errorResult(turnID int64, err error) core.TurnResult {
	return core.TurnResult{TurnID: turnID, Status: core.TurnError, ErrorMessage: err.Error()}
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	const limit = 500
	if len(stderr) > limit {
		stderr = stderr[len(stderr)-limit:]
	}
	return ": " + stderr
}

// lockedBuffer is a concurrency-safe stderr sink. exec writes to it from its
// own goroutine while the heartbeat loop may read it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// NonRetryableType is the error type carried by a non-retryable activity
// failure. Workflows may list it in RetryPolicy.NonRetryableErrorTypes, though
// the flag on the error is enough on its own.
const NonRetryableType = "roundclaw: non-retryable"

// newNonRetryable marks configuration errors so Temporal stops retrying them.
//
// It must build an ApplicationError, not a wrapped ordinary error. Temporal
// matches NonRetryableErrorTypes against the failure's *type*, and an ordinary
// error's type is its Go type name — `wrapErrors` for anything from fmt.Errorf.
// A sentinel in the message therefore never matched, and every "no retry will
// fix this" error was retried the full policy anyway: a missing agent, an
// unknown origin, a malformed spec. The flag set here is honoured directly,
// which also covers callers whose policy names no types at all.
func newNonRetryable(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), NonRetryableType, err)
}

// AbandonInput names turns that were queued but will never run.
type AbandonInput struct {
	AgentID string  `json:"agent_id"`
	TurnIDs []int64 `json:"turn_ids"`
	Reason  string  `json:"reason"`
}

// AbandonTurns closes out turns dropped from an agent's queue.
//
// Clearing the queue on /stop leaves their rows open otherwise: they stay
// "running" forever and show as in-flight in /status long after the turn is
// gone.
//
// No response is delivered for them. Whoever ran /stop was already told, and
// fifteen separate cancellation notices would be worse than none.
func (a *Activities) AbandonTurns(ctx context.Context, in AbandonInput) error {
	if len(in.TurnIDs) == 0 {
		return nil
	}
	st, err := a.stores.Get(in.AgentID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	for _, id := range in.TurnIDs {
		result := core.TurnResult{
			TurnID:  id,
			Status:  core.TurnStopped,
			Text:    "",
			CostUSD: 0,
		}
		if err := st.FinishTurn(ctx, id, result); err != nil {
			return fmt.Errorf("abandon turn %d: %w", id, err)
		}
	}
	activity.GetLogger(ctx).Info("abandoned queued turns",
		"agent", in.AgentID, "turns", len(in.TurnIDs), "reason", in.Reason)
	return nil
}

// workDirFor picks the directory mounted at the container's /workspace.
//
// An agent that names a project directory works in it directly, read-write;
// otherwise it gets the empty one roundclaw manages. The managed directory
// stays the default because pointing an agent at a real repository hands it
// everything in that repository.
func workDirFor(cfg *config.Config, agent registry.Agent) string {
	return workspace.Base(cfg, agent)
}

// identityEnv is what the container is told about the turn it is running.
//
// Without it the roundclaw CLI inside the container cannot name itself, and an
// agent asked to "reply in this conversation" would have to guess an ID out of
// its prompt. With it, `say` and `send --notify-me` need no arguments to know
// where they are.
//
// ROUNDCLAW_REPLY_TO is set only when this turn arrived by delegation, and names
// the agent (and conversation) waiting on the result — it is read from the turn's
// own origin, which is the durable record of where the answer goes.
func (a *Activities) identityEnv(ctx context.Context, st *store.Store, in RunTurnInput) map[string]string {
	env := map[string]string{
		"ROUNDCLAW_AGENT_ID":        in.AgentID,
		"ROUNDCLAW_TURN_ID":         strconv.FormatInt(in.TurnID, 10),
		"ROUNDCLAW_CONVERSATION_ID": in.ConversationID,
	}
	// The credential that says which agent this is, derived rather than looked
	// up so the worker and the gateway agree without sharing anything but the
	// key. Absent when no key is configured, which leaves the container with the
	// shared delegate token it has always had and no way to change itself.
	if token := core.DeriveAgentToken(in.AgentID, os.Getenv(a.cfg.HTTP.SelfTokenKeyEnv)); token != "" {
		env["ROUNDCLAW_SELF_TOKEN"] = token
	}
	// A failed read is not worth failing the turn over: the agent simply runs
	// without knowing who delegated to it, which is how every turn ran before
	// this existed.
	turn, err := st.GetTurn(ctx, in.TurnID)
	if err != nil {
		activity.GetLogger(ctx).Warn("could not read the turn's origin for identity env",
			"agent", in.AgentID, "turn_id", in.TurnID, "error", err)
		return env
	}
	if turn.Origin.Type == core.OriginAgent {
		env["ROUNDCLAW_REPLY_TO"] = turn.Origin.Agent
		env["ROUNDCLAW_REPLY_TO_CONVERSATION"] = turn.Origin.Conversation
	}
	return env
}

// resolveTools turns an agent's granted tool IDs into what a container needs:
// the host directories to mount read-only, the env vars each tool uses (merged
// into env, the same map that carries secrets), and a note prepended to the
// prompt so the agent knows the capability exists and how to drive it.
//
// A tool that no longer exists is skipped with a warning rather than failing the
// turn: an agent should still answer even if one of its tools was deleted out
// from under it.
func (a *Activities) resolveTools(ctx context.Context, ids []string, env map[string]string) ([]string, string, error) {
	if len(ids) == 0 {
		return nil, "", nil
	}
	log := activity.GetLogger(ctx)
	var dirs []string
	var resolved []registry.Tool
	var note strings.Builder
	for _, id := range ids {
		t, err := a.reg.GetTool(ctx, id)
		if errors.Is(err, registry.ErrNotFound) {
			log.Warn("agent grants a tool that no longer exists", "tool", id)
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("load tool %s: %w", id, err)
		}
		resolved = append(resolved, t)
		dirs = append(dirs, t.HostPath)
		for k, v := range t.Env {
			env[k] = v
		}
		if strings.TrimSpace(t.Instructions) != "" {
			if note.Len() == 0 {
				note.WriteString("You have these local tools available:\n\n")
			}
			fmt.Fprintf(&note, "## %s\n%s\n\n", t.ID, strings.TrimSpace(t.Instructions))
		}
	}
	if note.Len() > 0 {
		note.WriteString("---\n\n")
	}
	// The state report leads, ahead of the instructions: an agent has to know its
	// database is gone before it reads how to query it, not after.
	return dirs, toolStateNote(a.resolveToolStates(ctx, resolved)) + note.String(), nil
}

// resolveSkills turns an agent's granted skill IDs into a map of id to host
// directory, which the RunSpec mounts into ~/.claude/skills/<id>. A skill that
// no longer exists is skipped with a warning rather than failing the turn.
func (a *Activities) resolveSkills(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	log := activity.GetLogger(ctx)
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		sk, err := a.reg.GetSkill(ctx, id)
		if errors.Is(err, registry.ErrNotFound) {
			log.Warn("agent grants a skill that no longer exists", "skill", id)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load skill %s: %w", id, err)
		}
		out[sk.ID] = sk.HostPath
	}
	return out, nil
}

// buildRecap summarises recent turns for an agent whose session was lost.
//
// This is the one place the turn window is fed back into a prompt. On an
// ordinary turn it is not: the live Claude session already holds that history,
// and repeating it would pay for the same context twice. Here the session is
// gone, so the record is all there is.
//
// Only finished turns are included, and each field is truncated: the point is
// to re-establish what the agent was working on, not to replay it.
func buildRecap(ctx context.Context, st *store.Store, conversationID string) (string, error) {
	// Scoped to the conversation: recapping a thread with another thread's
	// history would hand the agent a conversation it never had.
	turns, err := st.RecentTurnsIn(ctx, conversationID, recapTurns)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	var included int
	// RecentTurns is newest first; the recap reads oldest first, like a
	// conversation.
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.Status == core.TurnRunning {
			continue // the turn being started now
		}
		fmt.Fprintf(&b, "- asked: %s\n", truncateField(t.Request))
		switch {
		case t.Status == core.TurnDone && t.Result != "":
			fmt.Fprintf(&b, "  answered: %s\n", truncateField(t.Result))
		case t.Status == core.TurnError:
			fmt.Fprintf(&b, "  failed: %s\n", truncateField(t.Error))
		case t.Status == core.TurnStopped:
			b.WriteString("  stopped before finishing\n")
		}
		included++
	}
	if included == 0 {
		return "", nil
	}

	// The wording matters more than it looks. An earlier version said to treat
	// the recap "as background, not as memory", and the agent read that as "this
	// is unreliable" — it was handed a fact it had recorded itself and still
	// answered that it did not know. The distinction to draw is between what is
	// accurate (the record) and what is missing (everything else), not between
	// trustworthy and untrustworthy.
	return "Your previous session was lost and this is a fresh one. Below is an" +
		" accurate record of what you were asked and what you answered. The" +
		" details in it are reliable — use them. What is gone is everything" +
		" around them: your reasoning, files you had read, tool results. So" +
		" answer from this record where it covers the question, and say what" +
		" you no longer have where it does not.\n\n" +
		b.String() + "\n---\n\n", nil
}

func truncateField(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxRecapField {
		return s
	}
	return s[:maxRecapField] + "…"
}
