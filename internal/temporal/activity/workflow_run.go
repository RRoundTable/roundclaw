package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.temporal.io/sdk/activity"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// LoadWorkflow reads a workflow definition at run time, so an edit to its steps
// takes effect on the next run rather than being baked into the trigger.
func (a *Activities) LoadWorkflow(ctx context.Context, id string) (registry.Workflow, error) {
	w, err := a.reg.GetWorkflow(ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		// A deleted workflow will not reappear on retry, so this must not spin.
		return registry.Workflow{}, newNonRetryable(fmt.Errorf("unknown workflow %q", id))
	}
	return w, err
}

// RunStepInput runs one step of a workflow. Context carries the earlier steps'
// outputs so a step can build on them.
type RunStepInput struct {
	WorkflowID string                `json:"workflow_id"`
	RunKey     string                `json:"run_key"`
	StepIndex  int                   `json:"step_index"`
	Step       registry.WorkflowStep `json:"step"`
	Context    string                `json:"context,omitempty"`
}

// StepResult is a step's output. Failed marks a step whose container reported an
// error result rather than the activity itself failing.
type StepResult struct {
	Name   string `json:"name"`
	Output string `json:"output"`
	Failed bool   `json:"failed"`
	Error  string `json:"error,omitempty"`
}

// RunWorkflowStep executes one step's prompt in the workflow's shared workspace.
//
// It reuses the same container execution as an agent turn — streaming,
// heartbeating, graceful stop — but the configuration comes from the step, not
// an agent, and there is no session to resume: each step is a one-shot run that
// receives the earlier steps' outputs as input. The workflow's own SQLite store
// records each step as a turn, so a run's transcript is inspectable.
func (a *Activities) RunWorkflowStep(ctx context.Context, in RunStepInput) (StepResult, error) {
	log := activity.GetLogger(ctx)

	cred, err := a.cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		return StepResult{}, newNonRetryable(err)
	}
	for _, dir := range []string{a.cfg.WorkflowWorkDir(in.WorkflowID), a.cfg.WorkflowClaudeHome(in.WorkflowID)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return StepResult{}, fmt.Errorf("create workflow dir %s: %w", dir, err)
		}
	}
	st, err := store.Open(a.cfg.WorkflowDBPath(in.WorkflowID), in.WorkflowID, store.ReadWrite)
	if err != nil {
		return StepResult{}, fmt.Errorf("open workflow store: %w", err)
	}
	defer st.Close()

	// A workflow is agent-less, so it gets the global secrets — the ones every
	// executor may need — injected as env vars, the same as an agent turn. The
	// credential is injected separately and must win over a same-named secret.
	secrets, err := a.reg.SecretsForAgent(ctx, "")
	if err != nil {
		return StepResult{}, fmt.Errorf("load workflow secrets: %w", err)
	}
	delete(secrets, cred.EnvName)

	prompt := in.Step.Prompt
	if strings.TrimSpace(in.Context) != "" {
		prompt = "You are one step in a multi-step workflow. The steps before you produced:\n\n" +
			in.Context + "\n---\n\nYour task now:\n" + in.Step.Prompt
	}

	// A workflow runs with no human to answer a permission prompt, so a step must
	// not block on one. Default to a mode that never prompts; a step can override.
	permission := in.Step.PermissionMode
	if permission == "" {
		permission = "bypassPermissions"
	}

	spec := claude.RunSpec{
		Runtime:         a.cfg.Container.Runtime,
		Image:           a.cfg.Container.Image,
		ContainerName:   fmt.Sprintf("roundclaw-wf-%s-%s-%d", in.WorkflowID, in.RunKey, in.StepIndex),
		WorkDir:         a.cfg.WorkflowWorkDir(in.WorkflowID),
		ClaudeHome:      a.cfg.WorkflowClaudeHome(in.WorkflowID),
		CredentialEnv:   cred.EnvName,
		CredentialValue: cred.Value,
		// A unique, deterministic session per step run: unique so steps never
		// collide, deterministic so a retried activity reconnects to the same one.
		SessionID:      claude.SessionID(fmt.Sprintf("wf-%s-%s-step-%d", in.WorkflowID, in.RunKey, in.StepIndex)),
		Resume:         false,
		PermissionMode: permission,
		AllowedTools:   in.Step.AllowedTools,
		Model:          in.Step.Model,
		Network:        a.cfg.Container.Network,
		Secrets:        secrets,
		Prompt:         prompt,
	}

	turnID, _, err := st.CreateTurn(ctx, store.NewTurn{Request: prompt, Origin: core.HTTPPollOrigin()})
	if err != nil {
		return StepResult{}, fmt.Errorf("record step turn: %w", err)
	}
	args, err := spec.Args()
	if err != nil {
		return StepResult{}, newNonRetryable(err)
	}

	a.removeOrphan(ctx, spec)
	result, streamErr := a.stream(ctx, st, spec, args, RunTurnInput{TurnID: turnID})
	_ = st.FinishTurn(context.WithoutCancel(ctx), turnID, result)
	if streamErr != nil {
		return StepResult{}, streamErr
	}

	out := StepResult{Name: in.Step.Name, Output: result.Text}
	if result.Status == core.TurnError {
		out.Failed = true
		out.Error = result.ErrorMessage
		log.Warn("workflow step reported an error", "workflow", in.WorkflowID, "step", in.StepIndex, "error", result.ErrorMessage)
	}
	return out, nil
}
