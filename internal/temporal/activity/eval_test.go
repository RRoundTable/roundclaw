package activity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roundtable/roundclaw/internal/registry"
)

func TestAssertionsAreCheckedCaseInsensitively(t *testing.T) {
	c := registry.EvalCase{
		Name:           "cites",
		MustContain:    []string{"main.go"},
		MustNotContain: []string{"I cannot"},
	}

	// Present, but capitalised differently: failing this would be noise dressed
	// up as a regression.
	if got := checkAssertions(c, "Look at MAIN.GO line 12"); got != "" {
		t.Errorf("case-different match reported a failure: %q", got)
	}
	if got := checkAssertions(c, "no idea where that lives"); got == "" {
		t.Error("a missing must_contain was not reported")
	}
	if got := checkAssertions(c, "main.go — but I cannot read it"); got == "" {
		t.Error("a present must_not_contain was not reported")
	}
}

// An empty entry is what a hand-edited JSON file leaves behind. Treating "" as
// "must contain the empty string" would pass always; treating it as a rule that
// nothing satisfies would fail always. It is skipped.
func TestEmptyAssertionsAreIgnored(t *testing.T) {
	c := registry.EvalCase{MustContain: []string{""}, MustNotContain: []string{""}}
	if got := checkAssertions(c, "anything at all"); got != "" {
		t.Errorf("empty assertions produced %q", got)
	}
}

func TestParseJudgementToleratesFencedOutput(t *testing.T) {
	cases := []struct {
		name, text string
		wantScore  float64
		wantPassed bool
	}{
		{
			name:       "bare json",
			text:       `{"score": 0.8, "passed": true, "reason": "cites the file"}`,
			wantScore:  0.8,
			wantPassed: true,
		},
		{
			name:       "fenced and chatty",
			text:       "Here is my verdict:\n```json\n{\"score\": 0.2, \"passed\": false, \"reason\": \"vague\"}\n```\nHope that helps!",
			wantScore:  0.2,
			wantPassed: false,
		},
		{
			// A judge that says 1.5 meant "good"; losing the case over its
			// arithmetic would be worse than the imprecision.
			name:       "out of range score is clamped",
			text:       `{"score": 1.5, "passed": true, "reason": "excellent"}`,
			wantScore:  1,
			wantPassed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseJudgement(tc.text)
			if got.Score != tc.wantScore || got.Passed != tc.wantPassed {
				t.Errorf("parseJudgement = %+v, want score %v passed %v", got, tc.wantScore, tc.wantPassed)
			}
		})
	}
}

// Prose instead of a verdict must not be read as a zero-scoring fail: that would
// invent a regression out of a judge having a bad day.
func TestUnparseableJudgementSaysSo(t *testing.T) {
	got := parseJudgement("The answer was pretty good, I'd say about 70%.")
	if got.Passed {
		t.Error("unparseable output was read as a pass")
	}
	if got.Reason == "" {
		t.Error("unparseable output produced no explanation")
	}
}

// Case names become container names and directory names, and a case may be named
// in any language.
func TestSlugProducesAUsableName(t *testing.T) {
	cases := map[string]string{
		"basic recall":        "basic-recall",
		"한국어로 답하기":            "",
		"  ":                  "case",
		"Refactor: main.go!!": "refactor--main-go",
	}
	for in, want := range cases {
		got := slug(in)
		if got == "" {
			t.Errorf("slug(%q) is empty; a name is always needed", in)
			continue
		}
		if strings.ContainsAny(got, " /:!.") {
			t.Errorf("slug(%q) = %q, which is not safe as a container or directory name", in, got)
		}
		if want != "" && got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}

	if len(slug(strings.Repeat("long", 40))) > 40 {
		t.Error("a long case name produced a slug over the length limit")
	}
	// Two cases must not collide after slugging in a way that makes one
	// overwrite the other's workspace.
	if slug("case one") == slug("case two") {
		t.Error("distinct case names slugged to the same value")
	}
}

func TestEvalConversationIsStablePerCase(t *testing.T) {
	a := EvalConversation(7, "basic recall")
	if a != EvalConversation(7, "basic recall") {
		t.Error("the same case produced two different conversation names; a retry would lose its workspace")
	}
	if a == EvalConversation(8, "basic recall") {
		t.Error("two runs share a conversation name; their workspaces would collide")
	}
}

// share_workspace means the agent's threads all work in its real directory. An
// eval case must not: it writes the pinned persona over the workspace's
// CLAUDE.md and runs with permissions bypassed, so honouring the flag would
// roll a live agent's instructions back to whatever version was under test and
// leave the case editing the files the agent actually works in.
func TestEvalWorkspaceIsolatesDespiteShareWorkspace(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "gameart", Enabled: true, ShareWorkspace: true}
	base := workDirFor(a.cfg, agent)
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("create base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "CLAUDE.md"), []byte("live persona"), 0o600); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	dir, err := a.evalWorkspace(context.Background(), agent, EvalConversation(7, "some case"))
	if err != nil {
		t.Fatalf("evalWorkspace: %v", err)
	}
	if dir == base {
		t.Fatalf("an eval case would run in the live workspace %s", base)
	}

	// The live persona has to survive a case that overwrites the one in its own
	// workspace — that overwrite is what would otherwise roll the agent back.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("pinned persona"), 0o600); err != nil {
		t.Fatalf("write pinned persona: %v", err)
	}
	live, err := os.ReadFile(filepath.Join(base, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read live CLAUDE.md: %v", err)
	}
	if string(live) != "live persona" {
		t.Errorf("the live persona is now %q; an eval case overwrote it", live)
	}
}

// The ordinary agent path is untouched: a case for an agent without the flag
// still gets its own directory, as it always did.
func TestEvalWorkspaceIsolatesWithoutShareWorkspace(t *testing.T) {
	a, _, _ := notifyHarness(t)

	agent := registry.Agent{ID: "pm", Enabled: true}
	base := workDirFor(a.cfg, agent)
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("create base: %v", err)
	}

	dir, err := a.evalWorkspace(context.Background(), agent, EvalConversation(7, "some case"))
	if err != nil {
		t.Fatalf("evalWorkspace: %v", err)
	}
	if dir == base {
		t.Fatalf("an eval case would run in the live workspace %s", base)
	}
}
