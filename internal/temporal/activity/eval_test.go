package activity

import (
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
