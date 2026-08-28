package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/workspace"
)

// Running an eval case.
//
// A case runs the *pinned version* of an agent — its definition and its persona
// as they were at that version — against one prompt, in a throwaway workspace,
// and marks the answer. That pinning is the whole point: comparing two runs is
// only meaningful if each ran a known configuration rather than "the agent, at
// some point".
//
// Three deliberate differences from a real turn:
//
//   - No secrets, unless the set opts in with full_grants. A scheduled eval
//     otherwise runs the agent's real credentials against scratch files, and a
//     test that can push, deploy or post is not a test.
//   - No supplementary groups. group_add exists to hand an agent the docker
//     socket, which is host root; an automated eval is not where that belongs.
//   - No identity environment. Without ROUNDCLAW_AGENT_ID and the API token an
//     eval container cannot speak into a channel or delegate, so a case cannot
//     be mistaken for the agent doing real work.
//
// Tools and skills *are* mounted, because an agent stripped of its capabilities
// is not the agent anyone wants measured.

// EvalPlan is everything a run needs, read once at the start.
type EvalPlan struct {
	Run     registry.EvalRun `json:"run"`
	Set     registry.EvalSet `json:"set"`
	Agent   registry.Agent   `json:"agent"`
	Persona string           `json:"persona,omitempty"`
	// Version is the agent version actually evaluated, resolved from the run
	// (which may have asked for 0, meaning whatever was live).
	Version int `json:"version"`
}

// LoadEvalPlan reads the run, its set, and the pinned agent version.
func (a *Activities) LoadEvalPlan(ctx context.Context, runID int64) (EvalPlan, error) {
	run, err := a.reg.GetEvalRun(ctx, runID)
	if err != nil {
		return EvalPlan{}, newNonRetryable(err)
	}
	set, err := a.reg.GetEvalSet(ctx, run.EvalSetID)
	if err != nil {
		// A deleted set will not come back on retry.
		return EvalPlan{}, newNonRetryable(err)
	}

	plan := EvalPlan{Run: run, Set: set, Version: run.Version}
	if run.Version > 0 {
		v, err := a.reg.GetVersion(ctx, run.AgentID, run.Version)
		if err != nil {
			return EvalPlan{}, newNonRetryable(err)
		}
		plan.Agent, plan.Persona = v.Definition, v.Persona
		return plan, nil
	}

	// Version 0 means "whatever is live", which is resolved to a concrete version
	// here rather than left as 0 — a run that cannot say what it measured is
	// useless the moment the agent changes again.
	agent, err := a.reg.Get(ctx, run.AgentID)
	if err != nil {
		return EvalPlan{}, newNonRetryable(err)
	}
	plan.Agent = agent
	if latest, err := a.reg.LatestVersion(ctx, run.AgentID); err == nil {
		plan.Persona, plan.Version = latest.Persona, latest.Version
	} else if !errors.Is(err, registry.ErrNotFound) {
		return EvalPlan{}, err
	}
	return plan, nil
}

// RunCaseInput is one case to run under a pinned configuration.
type RunCaseInput struct {
	RunID   int64             `json:"run_id"`
	Case    registry.EvalCase `json:"case"`
	Agent   registry.Agent    `json:"agent"`
	Persona string            `json:"persona,omitempty"`
	// FullGrants injects the agent's registered secrets and supplementary
	// groups. Off unless the eval set asked for it.
	FullGrants bool `json:"full_grants,omitempty"`
}

// CaseOutcome is what running the case produced, before any judging.
type CaseOutcome struct {
	Output     string  `json:"output"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMS int64   `json:"duration_ms"`
	// Failed marks a case whose container reported an error, as opposed to one
	// that answered badly. The two are scored the same — zero — but only one of
	// them is worth investigating as an agent problem.
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
	// AssertionFailure names the must_contain / must_not_contain rule the answer
	// broke, if any. It is decided here, in Go, before a judge is asked: an exact
	// requirement should not be subject to a model's opinion.
	AssertionFailure string `json:"assertion_failure,omitempty"`
}

// evalDeniedTools names what a case may not reach, whatever the agent is
// otherwise granted.
//
// The line is the container, not read-versus-write. A case writing files is not
// a side effect: it works in a throwaway directory, its extra mounts are
// read-only, and producing files is half of what several agents are for —
// stripping that would measure a crippled agent, which is the thing this
// package says elsewhere it will not do. What must not happen is a case
// reaching *out* of that directory and changing the fleet it belongs to: a run
// that schedules work, wakes another agent, or moves the real repository's
// worktrees has stopped being a measurement.
//
// The same reasoning as judgeDeniedTools applies to why this is a deny list and
// not an allow list. The agent's own AllowedTools travels with the spec, but it
// approves rather than restricts, so under bypassPermissions it withholds
// nothing — pm and dev grant it nothing at all and would still be handed every
// tool the CLI has.
//
// Unconditional, including under full_grants. That flag says a case needs real
// credentials, not that it may reschedule the fleet.
var evalDeniedTools = []string{
	// Fleet control: scheduling, delegation, and waking other agents.
	"CronCreate", "CronDelete", "CronList", "ScheduleWakeup",
	"Workflow", "Task", "TaskCreate", "TaskGet",
	"TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
	"SendMessage", "PushNotification", "RemoteTrigger", "Monitor",
	// The real repository, rather than the copy the case was given.
	"EnterWorktree", "ExitWorktree",
}

// evalWorkspace prepares the throwaway directory one case runs in.
//
// It clears share_workspace before asking. That flag means "every conversation
// works in the agent's own directory", which is a reasonable thing for an
// operator to want for the agent's own threads and ruinous for an eval: a case
// writes the *pinned* persona over the workspace's CLAUDE.md and then runs with
// permissions bypassed. Pointed at the live workspace, evaluating v2 would
// overwrite a v4 agent's instructions with v2's and leave them there — the next
// ordinary edit would then mint a version pairing the new definition with the
// rolled-back persona, a combination that never existed. The agent would also
// be editing its own real files under test.
//
// So isolation is not a preference here, and not the agent's to configure. The
// flag is overridden rather than honoured, and the result is checked: if it
// still comes back as the agent's own directory, the case is refused instead of
// run. Nothing downstream would notice the difference, which is exactly why the
// check is here and not left to a reviewer.
func (a *Activities) evalWorkspace(ctx context.Context, agent registry.Agent, conversationID string) (string, error) {
	isolated := agent
	isolated.ShareWorkspace = false

	dir, err := a.resolveWorkspace(ctx, isolated, conversationID)
	if err != nil {
		return "", err
	}
	if base := workspace.Base(a.cfg, agent); dir == base {
		return "", newNonRetryable(fmt.Errorf(
			"refusing to run an eval case for %s in its live workspace %s", agent.ID, base))
	}
	return dir, nil
}

// RunEvalCase executes one case and applies the deterministic assertions.
func (a *Activities) RunEvalCase(ctx context.Context, in RunCaseInput) (CaseOutcome, error) {
	log := activity.GetLogger(ctx)
	started := time.Now()

	cred, err := a.cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		return CaseOutcome{}, newNonRetryable(err)
	}
	for _, dir := range []string{a.cfg.EvalDir(in.RunID), a.cfg.EvalClaudeHome(in.RunID)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return CaseOutcome{}, fmt.Errorf("create eval dir %s: %w", dir, err)
		}
	}

	// The case runs in an isolated conversation of the agent, which is how it
	// gets a workspace shaped like the real one — a git worktree for a repo
	// agent, a fresh directory for a managed one — without touching the agent's
	// own working tree.
	caseDir, err := a.evalWorkspace(ctx, in.Agent, EvalConversation(in.RunID, in.Case.Name))
	if err != nil {
		return CaseOutcome{}, err
	}
	// The pinned persona, not the one on disk: it is half of what a version is,
	// and evaluating v3's definition against v7's instructions would measure
	// something that never existed.
	if err := os.WriteFile(filepath.Join(caseDir, "CLAUDE.md"), []byte(in.Persona), 0o640); err != nil {
		return CaseOutcome{}, fmt.Errorf("write pinned persona: %w", err)
	}

	st, err := store.Open(a.cfg.EvalDBPath(in.RunID), fmt.Sprintf("eval-%d", in.RunID), store.ReadWrite)
	if err != nil {
		return CaseOutcome{}, fmt.Errorf("open eval store: %w", err)
	}
	defer st.Close()

	secrets := map[string]string{}
	groupAdd := []string(nil)
	if in.FullGrants {
		secrets, err = a.reg.SecretsForAgent(ctx, in.Agent.ID)
		if err != nil {
			return CaseOutcome{}, fmt.Errorf("load secrets for %s: %w", in.Agent.ID, err)
		}
		if secrets == nil {
			secrets = map[string]string{}
		}
		delete(secrets, cred.EnvName)
		groupAdd = in.Agent.GroupAdd
	}

	// Tools and skills come along either way. Their env is injected — a tool
	// whose env is missing is a tool that does not work — but a tool that reads a
	// *secret* still finds nothing without full_grants, which is the intended
	// half-measure: the capability is present, the credentials are not.
	toolDirs, toolNote, err := a.resolveTools(ctx, in.Agent.Tools, secrets)
	if err != nil {
		return CaseOutcome{}, err
	}
	delete(secrets, cred.EnvName)
	skills, err := a.resolveSkills(ctx, in.Agent.Skills)
	if err != nil {
		return CaseOutcome{}, err
	}

	image := a.cfg.Container.Image
	if in.Agent.Image != "" {
		image = in.Agent.Image
	}
	model := a.cfg.Container.Model
	if in.Agent.Model != "" {
		model = in.Agent.Model
	}
	// An eval has nobody to answer a permission prompt, so a case must never
	// block on one; the agent's own mode is used only when it already cannot.
	permission := in.Agent.PermissionMode
	if permission == "" || permission == "default" {
		permission = "bypassPermissions"
	}

	spec := claude.RunSpec{
		Runtime:         a.cfg.Container.Runtime,
		Image:           image,
		ContainerName:   evalContainerName(in.RunID, in.Case.Name),
		WorkDir:         caseDir,
		DenyPaths:       in.Agent.DenyPaths,
		ClaudeHome:      a.cfg.EvalClaudeHome(in.RunID),
		AdditionalDirs:  append(append([]string{}, in.Agent.AdditionalDirs...), toolDirs...),
		CredentialEnv:   cred.EnvName,
		CredentialValue: cred.Value,
		// Deterministic, so a retried activity reattaches instead of starting a
		// second session; unique per case, so cases never see each other.
		SessionID:      claude.SessionID(fmt.Sprintf("eval-%d-%s", in.RunID, in.Case.Name)),
		Resume:         false,
		AgentName:      in.Agent.AgentName,
		PermissionMode: permission,
		AllowedTools:   in.Agent.AllowedTools,
		// The agent's grant says what may run without asking; this says what may
		// not run at all. See evalDeniedTools.
		DisallowedTools: evalDeniedTools,
		Model:           model,
		Network:         a.cfg.Container.Network,
		GroupAdd:        groupAdd,
		Secrets:         secrets,
		Skills:          skills,
		Prompt:          toolNote + in.Case.Prompt,
	}

	turnID, _, err := st.CreateTurn(ctx, store.NewTurn{
		Request: in.Case.Prompt, Origin: core.HTTPPollOrigin(), Conversation: in.Case.Name,
	})
	if err != nil {
		return CaseOutcome{}, fmt.Errorf("record eval turn: %w", err)
	}
	args, err := spec.Args()
	if err != nil {
		return CaseOutcome{}, newNonRetryable(err)
	}

	a.removeOrphan(ctx, spec)
	result, _, streamErr := a.stream(ctx, st, spec, args, RunTurnInput{TurnID: turnID})
	_ = st.FinishTurn(context.WithoutCancel(ctx), turnID, result)
	if streamErr != nil {
		return CaseOutcome{}, streamErr
	}

	out := CaseOutcome{
		Output:     result.Text,
		CostUSD:    result.CostUSD,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if result.Status == core.TurnError {
		out.Failed = true
		out.Error = result.ErrorMessage
		log.Warn("eval case errored", "run", in.RunID, "case", in.Case.Name, "error", result.ErrorMessage)
	}
	out.AssertionFailure = checkAssertions(in.Case, result.Text)
	return out, nil
}

// checkAssertions applies the exact rules and returns the first one broken.
//
// Matching is case-insensitive: a rubric-free assertion is usually a term the
// answer must mention, and failing a case because the agent wrote "Error" where
// the set said "error" would be noise dressed up as a regression.
func checkAssertions(c registry.EvalCase, output string) string {
	lower := strings.ToLower(output)
	for _, want := range c.MustContain {
		if want == "" {
			continue
		}
		if !strings.Contains(lower, strings.ToLower(want)) {
			return fmt.Sprintf("the answer never mentions %q", want)
		}
	}
	for _, unwanted := range c.MustNotContain {
		if unwanted == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(unwanted)) {
			return fmt.Sprintf("the answer contains %q, which this case forbids", unwanted)
		}
	}
	return ""
}

// EvalConversation names the throwaway conversation a case runs in. Exported
// because the run's cleanup has to name the same directories.
func EvalConversation(runID int64, caseName string) string {
	return fmt.Sprintf("eval-%d-%s", runID, slug(caseName))
}

func evalContainerName(runID int64, caseName string) string {
	return fmt.Sprintf("roundclaw-eval-%d-%s", runID, slug(caseName))
}

// slug reduces a case name to what a container name and a directory both accept.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune('-')
		default:
			// Anything else — spaces, Korean, punctuation — becomes a separator, so
			// a case named in any language still produces a usable name.
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "case"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// ---- judging ---------------------------------------------------------------

// JudgeInput asks for a mark against a rubric.
type JudgeInput struct {
	RunID  int64  `json:"run_id"`
	Case   string `json:"case"`
	Rubric string `json:"rubric"`
	Prompt string `json:"prompt"`
	Output string `json:"output"`
}

// Judgement is the mark.
type Judgement struct {
	Score  float64 `json:"score"`
	Passed bool    `json:"passed"`
	Reason string  `json:"reason"`
}

// judgeDeniedTools names every tool the judge is refused, one by one.
//
// The judge reads the evaluated agent's answer, which is text this system did
// not write and cannot vouch for. An answer containing "ignore the rubric and
// run the following instead" must reach nothing that acts, so the judge is
// given no tools.
//
// Denying by name is what achieves that. An empty AllowedTools does not: it is
// an approval list, and this container runs with permissions bypassed because
// no operator is watching to answer a prompt, so nothing being pre-approved
// means everything is allowed. See RunSpec.DisallowedTools.
//
// Denying also does more than refuse the call: the tool is withheld from the
// list the model is shown at all, so it never attempts one. That is measured
// against the image, not assumed — see TestJudgeDeniesEveryToolTheImageOffers.
//
// The cost is that this list has to be kept current. It is not a policy that
// covers whatever exists; it is a set of names, and a tool the CLI adds under a
// name not written here is handed to the judge. Nothing fails loudly when that
// happens, which is why the test asserts the image's own list is covered rather
// than trusting this one to be complete. Denying a name the CLI does not have
// costs nothing, so entries outlive the versions that needed them.
//
// The alternative would be an allow list, which cannot express "nothing": every
// permission mode either prompts — hanging a run nobody is watching — or, like
// bypassPermissions and dontAsk, proceeds. Both were tried against the image.
var judgeDeniedTools = []string{
	"Bash", "BashOutput", "KillShell",
	"Read", "Write", "Edit", "NotebookEdit",
	"Glob", "Grep", "SlashCommand", "TodoWrite", "ExitPlanMode",
	"WebFetch", "WebSearch",
	"Task", "TaskCreate", "TaskGet", "TaskList",
	"TaskOutput", "TaskStop", "TaskUpdate",
	"CronCreate", "CronDelete", "CronList", "ScheduleWakeup",
	"SendMessage", "PushNotification", "RemoteTrigger", "Monitor",
	"EnterWorktree", "ExitWorktree", "DesignSync",
	"Skill", "ToolSearch", "Workflow", "ReportFindings",
}

// JudgeEvalCase marks one answer against its rubric with a model.
//
// It runs the same way a workflow step does — a one-shot container with no tools
// and no workspace of consequence — on the router's small model rather than the
// fleet's, because this is a classification and not the work itself.
//
// A judge that cannot be parsed does not fail the run. Its case is recorded
// unjudged with the reason, which is honest: the answer exists and was not
// marked, and a run that collapsed because a judge returned prose would lose the
// nine cases that did work.
func (a *Activities) JudgeEvalCase(ctx context.Context, in JudgeInput) (Judgement, error) {
	cred, err := a.cfg.Container.ResolveCredential(os.LookupEnv)
	if err != nil {
		return Judgement{}, newNonRetryable(err)
	}
	dir := filepath.Join(a.cfg.EvalDir(in.RunID), "judge")
	for _, d := range []string{dir, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return Judgement{}, fmt.Errorf("create judge dir %s: %w", d, err)
		}
	}

	model := a.cfg.Router.Model
	if model == "" {
		model = a.cfg.Container.Model
	}

	spec := claude.RunSpec{
		Runtime:         a.cfg.Container.Runtime,
		Image:           a.cfg.Container.Image,
		ContainerName:   fmt.Sprintf("roundclaw-judge-%d-%s", in.RunID, slug(in.Case)),
		WorkDir:         dir,
		ClaudeHome:      filepath.Join(dir, "home"),
		CredentialEnv:   cred.EnvName,
		CredentialValue: cred.Value,
		SessionID:       claude.SessionID(fmt.Sprintf("judge-%d-%s", in.RunID, in.Case)),
		PermissionMode:  "bypassPermissions",
		// A judge reads text and returns a verdict. Giving it tools would let a
		// prompt in the answer it is marking do something, so every one is
		// denied by name — see judgeDeniedTools for why denying, rather than
		// allowing nothing, is what actually withholds them.
		DisallowedTools: judgeDeniedTools,
		Model:           model,
		// No configured network, deliberately: the default bridge carries the
		// internet the CLI needs to reach the model and nothing else. The
		// container network exists so an agent can reach services that live on
		// it — the fleet's runs alongside an identity provider and its database,
		// a docker management UI, a wiki. A judge reads text and returns a
		// verdict, so it has no call on any of that, and the answer it is
		// marking is text this system did not write.
		Prompt: judgePrompt(in),
	}

	st, err := store.Open(a.cfg.EvalDBPath(in.RunID), fmt.Sprintf("eval-%d", in.RunID), store.ReadWrite)
	if err != nil {
		return Judgement{}, fmt.Errorf("open eval store: %w", err)
	}
	defer st.Close()

	turnID, _, err := st.CreateTurn(ctx, store.NewTurn{
		Request: "judge " + in.Case, Origin: core.HTTPPollOrigin(), Conversation: "judge",
	})
	if err != nil {
		return Judgement{}, fmt.Errorf("record judge turn: %w", err)
	}
	args, err := spec.Args()
	if err != nil {
		return Judgement{}, newNonRetryable(err)
	}
	a.removeOrphan(ctx, spec)
	result, _, streamErr := a.stream(ctx, st, spec, args, RunTurnInput{TurnID: turnID})
	_ = st.FinishTurn(context.WithoutCancel(ctx), turnID, result)
	if streamErr != nil {
		return Judgement{}, streamErr
	}
	if result.Status == core.TurnError {
		return Judgement{Reason: "the judge could not run: " + result.ErrorMessage}, nil
	}
	return parseJudgement(result.Text), nil
}

func judgePrompt(in JudgeInput) string {
	return "You are marking one answer from an AI agent against a rubric. " +
		"Be strict: the point of this is to catch a regression, and a generous mark hides one.\n\n" +
		"## The question the agent was asked\n" + in.Prompt + "\n\n" +
		"## The rubric\n" + in.Rubric + "\n\n" +
		"## The agent's answer\n" + in.Output + "\n\n" +
		"Reply with ONLY a JSON object, no prose and no code fence:\n" +
		`{"score": <0.0-1.0>, "passed": <true|false>, "reason": "<one sentence>"}` + "\n" +
		"score is how well the answer meets the rubric; passed is whether it meets it at all."
}

// parseJudgement pulls the verdict out of the judge's reply, tolerating the
// fenced or chatty output a model produces despite being asked not to.
func parseJudgement(text string) Judgement {
	raw := strings.TrimSpace(text)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var j Judgement
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return Judgement{Reason: "the judge did not return a verdict this could read"}
	}
	// Clamp rather than reject: a judge that says 1.5 meant "good", and losing
	// the case over its arithmetic would be worse than the imprecision.
	if j.Score < 0 {
		j.Score = 0
	}
	if j.Score > 1 {
		j.Score = 1
	}
	return j
}

// ---- persistence and completion --------------------------------------------

// RecordEvalCase saves one case's outcome.
func (a *Activities) RecordEvalCase(ctx context.Context, r registry.EvalResult) error {
	return a.reg.RecordEvalResult(ctx, r)
}

// FinishEvalInput closes a run out.
type FinishEvalInput struct {
	RunID int64  `json:"run_id"`
	Error string `json:"error,omitempty"`
}

// FinishEvalRun aggregates the recorded results and marks the run finished.
//
// The aggregate is computed from the rows rather than carried through the
// workflow, so a run that lost a worker mid-flight still totals up whatever
// actually landed instead of reporting a number nothing supports.
func (a *Activities) FinishEvalRun(ctx context.Context, in FinishEvalInput) (registry.EvalRun, error) {
	results, err := a.reg.EvalResults(ctx, in.RunID)
	if err != nil {
		return registry.EvalRun{}, err
	}
	run, err := a.reg.GetEvalRun(ctx, in.RunID)
	if err != nil {
		return registry.EvalRun{}, newNonRetryable(err)
	}

	var weighted, weights, cost float64
	passed := 0
	set, setErr := a.reg.GetEvalSet(ctx, run.EvalSetID)
	weightOf := func(name string) float64 {
		if setErr != nil {
			return 1
		}
		for _, c := range set.Cases {
			if c.Name == name && c.Weight > 0 {
				return c.Weight
			}
		}
		return 1
	}
	for _, r := range results {
		w := weightOf(r.CaseName)
		weighted += r.Score * w
		weights += w
		cost += r.CostUSD
		if r.Passed {
			passed++
		}
	}
	score := 0.0
	if weights > 0 {
		score = weighted / weights
	}

	status := registry.EvalDone
	if in.Error != "" {
		status = registry.EvalFailed
	}
	if err := a.reg.FinishEvalRun(ctx, in.RunID, status, score, passed, cost, in.Error); err != nil {
		return registry.EvalRun{}, err
	}

	// A run that was gating a self-made change decides it here. Settling on every
	// completion rather than on a path somebody has to remember to take is what
	// keeps the gate from being skippable: an ordinary run settles to nothing.
	//
	// A settle failure does not fail the run. The measurement is real and
	// recorded; what failed is acting on it, and losing the aggregate too would
	// destroy the evidence somebody needs to act by hand.
	out, err := a.reg.SettleGate(ctx, in.RunID)
	if err != nil {
		activity.GetLogger(ctx).Error("could not settle a gating eval run",
			"run", in.RunID, "error", err)
	} else if out.Gated {
		activity.GetLogger(ctx).Info("gating eval run settled",
			"run", in.RunID, "version", out.Version, "verdict", out.Verdict,
			"reverted", out.Reverted, "regressed", out.Regressed)
	}
	return a.reg.GetEvalRun(ctx, in.RunID)
}

// CleanEvalWorkspacesInput names a finished run whose scratch workspaces can go.
type CleanEvalWorkspacesInput struct {
	RunID   int64    `json:"run_id"`
	AgentID string   `json:"agent_id"`
	Cases   []string `json:"cases"`
}

// CleanEvalWorkspaces removes the per-case workspaces a run created.
//
// Best-effort by design: a leftover directory costs disk, while a cleanup that
// fails the run would throw away results that are already correct. Whatever it
// cannot remove is logged and left for an operator.
func (a *Activities) CleanEvalWorkspaces(ctx context.Context, in CleanEvalWorkspacesInput) error {
	log := activity.GetLogger(ctx)
	for _, name := range in.Cases {
		err := a.RemoveConversationWorkspace(ctx, RemoveConversationInput{
			AgentID: in.AgentID, ConversationID: EvalConversation(in.RunID, name),
		})
		if err != nil {
			log.Warn("could not remove an eval workspace",
				"run", in.RunID, "case", name, "error", err)
		}
	}
	return nil
}

// NotifyEvalRun wakes the agent that asked for the run, with a summary.
//
// A run takes minutes to tens of minutes — far longer than the turn that started
// it — so the requester is not made to wait. The result comes back as a new turn
// in its conversation, exactly as a delegated request does, which means it
// arrives even if the requesting agent, its container and the worker have all
// been restarted in between.
func (a *Activities) NotifyEvalRun(ctx context.Context, runID int64) error {
	run, err := a.reg.GetEvalRun(ctx, runID)
	if err != nil {
		return newNonRetryable(err)
	}
	if run.NotifyAgent == "" {
		return nil
	}
	return a.wakeAgent(ctx, run.NotifyAgent, run.NotifyConversation,
		fmt.Sprintf("eval:%d", runID), evalSummary(run))
}

func evalSummary(run registry.EvalRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Eval run %d finished (%s).\n\n", run.ID, run.Status)
	fmt.Fprintf(&b, "- eval set: %s\n- agent: %s (version %d)\n", run.EvalSetID, run.AgentID, run.Version)
	fmt.Fprintf(&b, "- passed: %d/%d\n- score: %.2f\n- cost: $%.4f\n", run.Passed, run.Total, run.Score, run.CostUSD)
	if run.Error != "" {
		fmt.Fprintf(&b, "- error: %s\n", run.Error)
	}
	b.WriteString("\nRead the per-case results with `roundclaw eval show " +
		fmt.Sprint(run.ID) + "`, and compare it against another run with " +
		"`roundclaw eval compare <base> " + fmt.Sprint(run.ID) + "` before drawing any conclusion — " +
		"the comparison decides what counts as a regression, not you.")
	return b.String()
}
