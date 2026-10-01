// Package routine is the pure core of routines: the shape of a routine, the
// compiler that admits one, and the rules that decide whether a step is done.
//
// Nothing here does I/O. The interpreter workflow calls it during replay, so it
// must stay deterministic: no clock, no map iteration that decides anything, no
// randomness (adr/006). What the compiler needs to know about the fleet — which
// agents and routines exist, which images are allowed — arrives through the
// Resolver interface.
package routine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kind is what executes a step. The set is closed; adding one is an ADR.
type Kind string

const (
	// KindAgent is a one-shot turn of a registered agent.
	KindAgent Kind = "agent"
	// KindProcess is an allowlisted image and a literal argv, no model (adr/007).
	KindProcess Kind = "process"
	// KindRoutine runs another routine as an inner run.
	KindRoutine Kind = "routine"
)

// Effect says what a step, or at most a whole routine, may do.
type Effect string

const (
	EffectNone      Effect = "none"      // reads its inputs, writes its output
	EffectWorkspace Effect = "workspace" // may change the run's work area
	EffectExternal  Effect = "external"  // acts outside roundclaw
)

func (e Effect) rank() int {
	switch e {
	case EffectNone, "":
		return 0
	case EffectWorkspace:
		return 1
	case EffectExternal:
		return 2
	}
	return -1
}

// Condition is a completion rule. Every field is an exact rule decided without
// asking anything to judge (SPEC-004: done is decided by rules before opinion).
type Condition struct {
	// MustContain lists text the output must include, case-insensitively.
	MustContain []string `json:"must_contain,omitempty"`
	// MustNotContain lists text the output must not include.
	MustNotContain []string `json:"must_not_contain,omitempty"`
	// ExitCode is the status a process step must exit with.
	ExitCode *int `json:"exit_code,omitempty"`
}

// Render substitutes bindings in the condition's text, so a step's condition
// can name what this run was given, as its instruction does.
func (c Condition) Render(inputs, outputs map[string]string) Condition {
	r := Condition{ExitCode: c.ExitCode}
	for _, t := range c.MustContain {
		r.MustContain = append(r.MustContain, Render(t, inputs, outputs))
	}
	for _, t := range c.MustNotContain {
		r.MustNotContain = append(r.MustNotContain, Render(t, inputs, outputs))
	}
	return r
}

// Empty reports whether the condition states nothing.
func (c Condition) Empty() bool {
	return len(c.MustContain) == 0 && len(c.MustNotContain) == 0 && c.ExitCode == nil
}

// Contract is what a routine promises. It is fixed when the routine is created:
// whoever improves the routine cannot change it (adr/009).
type Contract struct {
	Intent string `json:"intent"`
	// Inputs names what a caller must supply.
	Inputs []string `json:"inputs,omitempty"`
	// Done is checked against the routine's final output.
	Done Condition `json:"done,omitempty"`
	// Effects is the ceiling: no step may do more.
	Effects Effect `json:"effects,omitempty"`
}

// Step is one node of the graph.
type Step struct {
	ID    string   `json:"id"`
	Kind  Kind     `json:"kind"`
	After []string `json:"after,omitempty"`

	// Agent steps: Agent names the registered agent. Task is a fixed
	// instruction; Goal is an intent the agent pursues however it chooses until
	// Done holds. Exactly one of the two.
	Agent string `json:"agent,omitempty"`
	Task  string `json:"task,omitempty"`
	Goal  string `json:"goal,omitempty"`

	// Process steps.
	Image string   `json:"image,omitempty"`
	Argv  []string `json:"argv,omitempty"`

	// Routine steps.
	Routine string `json:"routine,omitempty"`

	Done        Condition `json:"done,omitempty"`
	MaxAttempts int       `json:"max_attempts,omitempty"`
	TimeoutSec  int       `json:"timeout_sec,omitempty"`
	Effects     Effect    `json:"effects,omitempty"`
}

// Open reports whether the step states an outcome and leaves the method to the
// agent — the kind that must say how to tell it is done.
func (s Step) Open() bool { return s.Kind == KindAgent && s.Goal != "" }

// Source is a routine as submitted.
type Source struct {
	ID       string   `json:"id"`
	Contract Contract `json:"contract"`
	Steps    []Step   `json:"steps"`
}

// Plan is a compiled routine: the normalised source, the order its steps run
// in, and the hash of its canonical bytes.
type Plan struct {
	Source Source   `json:"source"`
	Order  []string `json:"order"`
	Hash   string   `json:"hash"`
}

// Step returns the step with the given id.
func (p Plan) Step(id string) (Step, bool) {
	for _, s := range p.Source.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

// Resolver answers what the compiler needs to know about the fleet.
type Resolver interface {
	// AgentUsable reports whether the routine's owner may run steps as agent.
	AgentUsable(agent string) (bool, error)
	// ImageAllowed reports whether the operator allowlisted image for process steps.
	ImageAllowed(image string) bool
	// RoutineSteps returns the routines an existing routine uses as steps, and
	// whether it exists at all. It is how self-containment is found through
	// other routines.
	RoutineSteps(id string) (uses []string, exists bool, err error)
}

// Caps bound what a routine may ask for, so every run is bounded before it
// starts and its history stays under the engine's limits.
type Caps struct {
	MaxSteps        int
	MaxAttempts     int
	MaxTimeoutSec   int
	MaxTotalAttempt int // steps × attempts, summed: the worst case
}

// DefaultCaps are the bounds used when configuration names none.
var DefaultCaps = Caps{MaxSteps: 50, MaxAttempts: 5, MaxTimeoutSec: 2 * 60 * 60, MaxTotalAttempt: 200}

// Defaults applied when a step leaves them out.
const (
	DefaultAttemptsOpen  = 3
	DefaultAttemptsFixed = 1
	DefaultTimeoutSec    = 30 * 60
)

// Diagnostic is one reason a routine was refused, named by the step it is in.
type Diagnostic struct {
	Step    string `json:"step,omitempty"`
	Message string `json:"message"`
}

func (d Diagnostic) String() string {
	if d.Step == "" {
		return d.Message
	}
	return d.Step + ": " + d.Message
}

// Error is returned when compilation fails; it carries every diagnostic, not
// only the first, because a refusal that names one problem at a time costs a
// round trip per problem.
type Error struct{ Diagnostics []Diagnostic }

func (e *Error) Error() string {
	parts := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		parts[i] = d.String()
	}
	return "routine refused: " + strings.Join(parts, "; ")
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// bindingPattern matches {{inputs.name}} and {{steps.id.output}}.
var bindingPattern = regexp.MustCompile(`\{\{\s*(inputs|steps)\.([a-zA-Z0-9_-]+)(\.output)?\s*\}\}`)

// Compile checks a routine in full and returns its plan, or an *Error naming
// every problem found (SPEC-004: a routine is checked in full before its trial).
func Compile(src Source, r Resolver, caps Caps) (Plan, error) {
	var diags []Diagnostic
	add := func(step, format string, args ...any) {
		diags = append(diags, Diagnostic{Step: step, Message: fmt.Sprintf(format, args...)})
	}

	if !idPattern.MatchString(src.ID) {
		add("", "routine id %q must be lowercase letters, digits, - or _, at most 64", src.ID)
	}
	if strings.TrimSpace(src.Contract.Intent) == "" {
		add("", "the contract must state an intent")
	}
	if src.Contract.Effects.rank() < 0 {
		add("", "unknown effect ceiling %q", src.Contract.Effects)
	}
	if len(src.Steps) == 0 {
		add("", "a routine needs at least one step")
	}
	if len(src.Steps) > caps.MaxSteps {
		add("", "%d steps exceeds the limit of %d", len(src.Steps), caps.MaxSteps)
	}

	sort.Strings(src.Contract.Inputs)
	inputs := map[string]bool{}
	for _, in := range src.Contract.Inputs {
		if !idPattern.MatchString(in) {
			add("", "input name %q is not a valid name", in)
		}
		inputs[in] = true
	}

	steps := map[string]Step{}
	for i := range src.Steps {
		s := &src.Steps[i]
		if !idPattern.MatchString(s.ID) {
			add(s.ID, "step id %q is not a valid name", s.ID)
			continue
		}
		if _, dup := steps[s.ID]; dup {
			add(s.ID, "step id is used twice")
			continue
		}
		normalise(s)
		steps[s.ID] = *s
	}

	for _, s := range src.Steps {
		if _, ok := steps[s.ID]; !ok {
			continue
		}
		checkStep(s, steps, src, r, caps, add)
	}

	order, cycle := topoOrder(src.Steps, steps)
	if cycle != "" {
		add(cycle, "the steps form a cycle")
	}

	if cycle == "" {
		ancestors := ancestorSets(order, steps)
		for _, s := range src.Steps {
			for _, text := range bindable(s) {
				for _, m := range bindingPattern.FindAllStringSubmatch(text, -1) {
					switch m[1] {
					case "inputs":
						if !inputs[m[2]] {
							add(s.ID, "{{inputs.%s}} names an input the contract does not declare", m[2])
						}
					case "steps":
						if !ancestors[s.ID][m[2]] {
							add(s.ID, "{{steps.%s.output}} is not produced by a step this one waits for", m[2])
						}
					}
				}
			}
		}
		// The contract's condition is checked in the interpreter, which renders
		// nothing; a binding there would be checked as literal text.
		for _, text := range append(append([]string{}, src.Contract.Done.MustContain...), src.Contract.Done.MustNotContain...) {
			if bindingPattern.MatchString(text) {
				add("", "the contract's done condition %q cannot hold a binding", text)
			}
		}
		checkSelfContainment(src, r, add)
	}

	total := 0
	for _, s := range steps {
		total += s.MaxAttempts
	}
	if total > caps.MaxTotalAttempt {
		add("", "the worst case of %d attempts exceeds the limit of %d", total, caps.MaxTotalAttempt)
	}

	if len(diags) > 0 {
		sort.SliceStable(diags, func(i, j int) bool { return diags[i].Step < diags[j].Step })
		return Plan{}, &Error{Diagnostics: diags}
	}

	plan := Plan{Source: src, Order: order}
	h, err := Hash(src)
	if err != nil {
		return Plan{}, err
	}
	plan.Hash = h
	return plan, nil
}

func normalise(s *Step) {
	if s.Effects == "" {
		s.Effects = EffectNone
	}
	if s.MaxAttempts == 0 {
		s.MaxAttempts = DefaultAttemptsFixed
		if s.Open() {
			s.MaxAttempts = DefaultAttemptsOpen
		}
	}
	if s.TimeoutSec == 0 {
		s.TimeoutSec = DefaultTimeoutSec
	}
	sort.Strings(s.After)
}

func checkStep(s Step, steps map[string]Step, src Source, r Resolver, caps Caps, add func(string, string, ...any)) {
	for _, dep := range s.After {
		if dep == s.ID {
			add(s.ID, "a step cannot wait for itself")
		} else if _, ok := steps[dep]; !ok {
			add(s.ID, "waits for %q, which is not a step", dep)
		}
	}
	if s.Effects.rank() < 0 {
		add(s.ID, "unknown effect %q", s.Effects)
	} else if s.Effects.rank() > src.Contract.Effects.rank() {
		add(s.ID, "effect %q is beyond the routine's ceiling %q", s.Effects, orNone(src.Contract.Effects))
	}
	if s.MaxAttempts < 1 || s.MaxAttempts > caps.MaxAttempts {
		add(s.ID, "max_attempts must be between 1 and %d", caps.MaxAttempts)
	}
	if s.TimeoutSec < 1 || s.TimeoutSec > caps.MaxTimeoutSec {
		add(s.ID, "timeout_sec must be between 1 and %d", caps.MaxTimeoutSec)
	}

	switch s.Kind {
	case KindAgent:
		if s.Agent == "" {
			add(s.ID, "an agent step must name an agent")
		} else if ok, err := r.AgentUsable(s.Agent); err != nil {
			add(s.ID, "could not check agent %q: %v", s.Agent, err)
		} else if !ok {
			add(s.ID, "agent %q does not exist or is beyond what the owner may use", s.Agent)
		}
		if (s.Task == "") == (s.Goal == "") {
			add(s.ID, "an agent step states exactly one of task or goal")
		}
		if s.Goal != "" && s.Done.Empty() {
			add(s.ID, "an open step must state how to tell it is done")
		}
		if s.Done.ExitCode != nil {
			add(s.ID, "exit_code applies only to process steps")
		}
	case KindProcess:
		if s.Image == "" || len(s.Argv) == 0 {
			add(s.ID, "a process step names an image and an argv")
		} else if !r.ImageAllowed(s.Image) {
			add(s.ID, "image %q is not on the process allowlist", s.Image)
		}
		for _, arg := range s.Argv {
			if bindingPattern.MatchString(arg) {
				add(s.ID, "argv is literal; pass data through files in the work area")
			}
		}
		if s.MaxAttempts > 1 {
			add(s.ID, "a fixed program is not retried for giving the same answer; max_attempts must be 1")
		}
	case KindRoutine:
		if s.Routine == "" {
			add(s.ID, "a routine step must name a routine")
		} else if s.Routine != src.ID {
			if _, exists, err := r.RoutineSteps(s.Routine); err != nil {
				add(s.ID, "could not check routine %q: %v", s.Routine, err)
			} else if !exists {
				add(s.ID, "routine %q does not exist", s.Routine)
			}
		}
	default:
		add(s.ID, "unknown step kind %q", s.Kind)
	}
}

func orNone(e Effect) Effect {
	if e == "" {
		return EffectNone
	}
	return e
}

// bindable is every text field a binding may appear in.
func bindable(s Step) []string {
	return append(append([]string{s.Task, s.Goal}, s.Done.MustContain...), s.Done.MustNotContain...)
}

// topoOrder returns the steps in an order where every step follows what it
// waits for, ties broken by position in the source. The tie-break is what keeps
// the order deterministic, which the interpreter's replay depends on.
func topoOrder(list []Step, steps map[string]Step) ([]string, string) {
	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var order []string
	var cycle string
	var visit func(id string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			cycle = id
			return false
		case 2:
			return true
		}
		state[id] = 1
		for _, dep := range steps[id].After {
			if _, ok := steps[dep]; ok && !visit(dep) {
				return false
			}
		}
		state[id] = 2
		order = append(order, id)
		return true
	}
	for _, s := range list {
		if _, ok := steps[s.ID]; ok && !visit(s.ID) {
			return nil, cycle
		}
	}
	return order, ""
}

// ancestorSets maps every step to all the steps it transitively waits for.
func ancestorSets(order []string, steps map[string]Step) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(order))
	for _, id := range order {
		set := map[string]bool{}
		for _, dep := range steps[id].After {
			set[dep] = true
			for a := range out[dep] {
				set[a] = true
			}
		}
		out[id] = set
	}
	return out
}

// checkSelfContainment refuses a routine that contains itself, directly or
// through the routines it uses.
func checkSelfContainment(src Source, r Resolver, add func(string, string, ...any)) {
	for _, s := range src.Steps {
		if s.Kind != KindRoutine || s.Routine == "" {
			continue
		}
		seen := map[string]bool{}
		queue := []string{s.Routine}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if id == src.ID {
				add(s.ID, "routine %q contains this routine, so it would contain itself", s.Routine)
				break
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			uses, _, err := r.RoutineSteps(id)
			if err != nil {
				add(s.ID, "could not check routine %q: %v", id, err)
				break
			}
			queue = append(queue, uses...)
		}
	}
}

// Hash is the sha256 of a source's canonical bytes. encoding/json writes struct
// fields in declaration order and the compiler sorts every list whose order does
// not matter, so equal routines hash equally.
func Hash(src Source) (string, error) {
	b, err := json.Marshal(src)
	if err != nil {
		return "", fmt.Errorf("canonical form: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ContractHash is the hash of a contract alone: what stays fixed for the life of
// a routine.
func ContractHash(c Contract) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("canonical contract: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Render substitutes bindings in text. A binding to something absent renders as
// empty; the compiler has already refused bindings that could be absent.
func Render(text string, inputs map[string]string, outputs map[string]string) string {
	return bindingPattern.ReplaceAllStringFunc(text, func(m string) string {
		parts := bindingPattern.FindStringSubmatch(m)
		if parts[1] == "inputs" {
			return inputs[parts[2]]
		}
		return outputs[parts[2]]
	})
}

// Verdict is the outcome of one completion rule.
type Verdict struct {
	Rule   string `json:"rule"`
	Held   bool   `json:"held"`
	Detail string `json:"detail,omitempty"`
}

// Check applies a condition to an output and, for process steps, an exit code.
// It returns one verdict per rule; the condition holds when every verdict does.
func Check(c Condition, output string, exitCode *int) []Verdict {
	var out []Verdict
	lower := strings.ToLower(output)
	for _, want := range c.MustContain {
		held := strings.Contains(lower, strings.ToLower(want))
		v := Verdict{Rule: "must_contain " + quote(want), Held: held}
		if !held {
			v.Detail = "missing " + quote(want)
		}
		out = append(out, v)
	}
	for _, bad := range c.MustNotContain {
		held := !strings.Contains(lower, strings.ToLower(bad))
		v := Verdict{Rule: "must_not_contain " + quote(bad), Held: held}
		if !held {
			v.Detail = "contains " + quote(bad)
		}
		out = append(out, v)
	}
	if c.ExitCode != nil {
		v := Verdict{Rule: fmt.Sprintf("exit_code %d", *c.ExitCode)}
		switch {
		case exitCode == nil:
			v.Detail = "no exit code was recorded"
		case *exitCode == *c.ExitCode:
			v.Held = true
		default:
			v.Detail = fmt.Sprintf("exit %d", *exitCode)
		}
		out = append(out, v)
	}
	return out
}

// Held reports whether every verdict held.
func Held(vs []Verdict) bool {
	for _, v := range vs {
		if !v.Held {
			return false
		}
	}
	return true
}

// Shortfall joins the details of the verdicts that did not hold: the feedback a
// retried step is told.
func Shortfall(vs []Verdict) string {
	var parts []string
	for _, v := range vs {
		if !v.Held {
			parts = append(parts, v.Rule+": "+v.Detail)
		}
	}
	return strings.Join(parts, "; ")
}

func quote(s string) string { return fmt.Sprintf("%q", s) }
