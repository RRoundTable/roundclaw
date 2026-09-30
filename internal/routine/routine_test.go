package routine

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	agents   map[string]bool
	images   map[string]bool
	routines map[string][]string
}

func (f fakeResolver) AgentUsable(a string) (bool, error) { return f.agents[a], nil }
func (f fakeResolver) ImageAllowed(i string) bool         { return f.images[i] }
func (f fakeResolver) RoutineSteps(id string) ([]string, bool, error) {
	uses, ok := f.routines[id]
	return uses, ok, nil
}

var resolver = fakeResolver{
	agents:   map[string]bool{"dev": true},
	images:   map[string]bool{"golang:1.26": true},
	routines: map[string][]string{"lint": nil, "outer": {"fix-vet"}},
}

func exit(n int) *int { return &n }

func valid() Source {
	return Source{
		ID: "fix-vet",
		Contract: Contract{
			Intent:  "make go vet pass",
			Inputs:  []string{"repo"},
			Done:    Condition{MustContain: []string{"branch:"}},
			Effects: EffectWorkspace,
		},
		Steps: []Step{
			{ID: "vet", Kind: KindProcess, Image: "golang:1.26", Argv: []string{"go", "vet", "./..."},
				Done: Condition{ExitCode: exit(0)}},
			{ID: "fix", Kind: KindAgent, Agent: "dev", After: []string{"vet"}, Effects: EffectWorkspace,
				Goal: "fix what vet reported in {{inputs.repo}}: {{steps.vet.output}}",
				Done: Condition{MustContain: []string{"fixed"}}},
		},
	}
}

func TestCompileAcceptsAValidRoutine(t *testing.T) {
	plan, err := Compile(valid(), resolver, DefaultCaps)
	require.NoError(t, err)
	assert.Equal(t, []string{"vet", "fix"}, plan.Order)
	assert.Len(t, plan.Hash, 64)
	fix, _ := plan.Step("fix")
	assert.Equal(t, DefaultAttemptsOpen, fix.MaxAttempts, "an open step gets retries by default")
	vet, _ := plan.Step("vet")
	assert.Equal(t, 1, vet.MaxAttempts, "a fixed program gets one attempt")
}

func TestCompileHashIsStableAndSensitive(t *testing.T) {
	a, err := Compile(valid(), resolver, DefaultCaps)
	require.NoError(t, err)
	b, err := Compile(valid(), resolver, DefaultCaps)
	require.NoError(t, err)
	assert.Equal(t, a.Hash, b.Hash)

	changed := valid()
	changed.Steps[1].Goal += " carefully"
	c, err := Compile(changed, resolver, DefaultCaps)
	require.NoError(t, err)
	assert.NotEqual(t, a.Hash, c.Hash)
}

func TestCompileRefuses(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Source)
		want   string
	}{
		"missing intent":     {func(s *Source) { s.Contract.Intent = "" }, "intent"},
		"unknown agent":      {func(s *Source) { s.Steps[1].Agent = "ghost" }, `agent "ghost"`},
		"unknown dependency": {func(s *Source) { s.Steps[1].After = []string{"nope"} }, `"nope"`},
		"cycle":              {func(s *Source) { s.Steps[0].After = []string{"fix"} }, "cycle"},
		"open step, no done": {func(s *Source) { s.Steps[1].Done = Condition{} }, "how to tell it is done"},
		"unbound input":      {func(s *Source) { s.Steps[1].Goal = "{{inputs.other}}" }, "inputs.other"},
		"non-upstream binding": {func(s *Source) {
			s.Steps[0].Argv = []string{"x"}
			s.Steps[1].Goal = "ok"
			s.Steps = append(s.Steps, Step{ID: "late", Kind: KindAgent, Agent: "dev", Task: "{{steps.fix2.output}}"})
		}, "steps.fix2"},
		"effect over ceiling": {func(s *Source) { s.Steps[1].Effects = EffectExternal }, "beyond the routine's ceiling"},
		"image not allowed":   {func(s *Source) { s.Steps[0].Image = "evil:latest" }, "allowlist"},
		"binding in argv":     {func(s *Source) { s.Steps[0].Argv = []string{"echo", "{{inputs.repo}}"} }, "argv is literal"},
		"retrying a program":  {func(s *Source) { s.Steps[0].MaxAttempts = 3 }, "max_attempts must be 1"},
		"task and goal":       {func(s *Source) { s.Steps[1].Task = "also this" }, "exactly one of task or goal"},
		"duplicate id":        {func(s *Source) { s.Steps[1].ID = "vet" }, "used twice"},
		"attempts over cap":   {func(s *Source) { s.Steps[1].MaxAttempts = 99 }, "max_attempts must be between"},
		"unknown routine":     {func(s *Source) { s.Steps = append(s.Steps, Step{ID: "inner", Kind: KindRoutine, Routine: "ghost"}) }, `routine "ghost"`},
		"contains itself":     {func(s *Source) { s.Steps = append(s.Steps, Step{ID: "inner", Kind: KindRoutine, Routine: "outer"}) }, "contain itself"},
		"unknown kind":        {func(s *Source) { s.Steps[0].Kind = "magic" }, "unknown step kind"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := valid()
			tc.mutate(&src)
			_, err := Compile(src, resolver, DefaultCaps)
			var rerr *Error
			require.True(t, errors.As(err, &rerr), "want a refusal, got %v", err)
			assert.Contains(t, rerr.Error(), tc.want)
		})
	}
}

// A refusal names every problem, not the first one found.
func TestCompileReportsEveryProblem(t *testing.T) {
	src := valid()
	src.Contract.Intent = ""
	src.Steps[1].Agent = "ghost"
	src.Steps[0].Image = "evil:latest"
	_, err := Compile(src, resolver, DefaultCaps)
	var rerr *Error
	require.True(t, errors.As(err, &rerr))
	assert.GreaterOrEqual(t, len(rerr.Diagnostics), 3)
}

func TestCompileRefusesTheWorstCaseOverTheCap(t *testing.T) {
	src := valid()
	_, err := Compile(src, resolver, Caps{MaxSteps: 50, MaxAttempts: 5, MaxTimeoutSec: 7200, MaxTotalAttempt: 2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worst case")
}

func TestRenderSubstitutesBindings(t *testing.T) {
	out := Render("repo {{inputs.repo}} said {{ steps.vet.output }}",
		map[string]string{"repo": "r1"}, map[string]string{"vet": "3 issues"})
	assert.Equal(t, "repo r1 said 3 issues", out)
}

func TestCheckDecidesByRuleAndExplainsShortfall(t *testing.T) {
	c := Condition{MustContain: []string{"PASS"}, MustNotContain: []string{"panic"}, ExitCode: exit(0)}
	vs := Check(c, "tests: pass\nno panic here", exit(1))
	assert.False(t, Held(vs))
	short := Shortfall(vs)
	assert.Contains(t, short, `contains "panic"`)
	assert.Contains(t, short, "exit 1")
	assert.False(t, strings.Contains(short, "PASS"), "a rule that held is not reported as short")

	assert.True(t, Held(Check(c, "PASS", exit(0))))
	assert.True(t, Held(Check(Condition{}, "anything", nil)), "an empty condition holds")
}

func TestInputOrderDoesNotChangeTheHash(t *testing.T) {
	a := valid()
	a.Contract.Inputs = []string{"repo", "branch"}
	b := valid()
	b.Contract.Inputs = []string{"branch", "repo"}
	pa, err := Compile(a, resolver, DefaultCaps)
	require.NoError(t, err)
	pb, err := Compile(b, resolver, DefaultCaps)
	require.NoError(t, err)
	assert.Equal(t, pa.Hash, pb.Hash)
	ha, err := ContractHash(pa.Source.Contract)
	require.NoError(t, err)
	hb, err := ContractHash(pb.Source.Contract)
	require.NoError(t, err)
	assert.Equal(t, ha, hb)
}
