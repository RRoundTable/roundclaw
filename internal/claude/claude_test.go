package claude

import (
	"slices"
	"strings"
	"testing"
)

// The session ID must be a pure function of the workflow ID. If this ever
// changes, every existing agent silently loses its conversation history on the
// next turn, so it is pinned to a literal.
func TestSessionIDIsStableAndDistinct(t *testing.T) {
	const wf = "roundclaw-agent-pr-reviewer"
	got := SessionID(wf)
	const want = "d2855af2-c3d0-51c4-8388-585ab61fc7dd"
	if got != want {
		t.Errorf("SessionID(%q) = %q, want %q (changing this orphans every live session)", wf, got, want)
	}
	if SessionID(wf) != got {
		t.Error("SessionID is not deterministic")
	}
	if SessionID("roundclaw-agent-other") == got {
		t.Error("different workflow IDs produced the same session ID")
	}
}

func TestArgsFirstTurnCreatesSession(t *testing.T) {
	args, err := baseSpec().Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if !hasPair(args, "--session-id", "11111111-2222-3333-4444-555555555555") {
		t.Errorf("first turn must pass --session-id, got %v", args)
	}
	if slices.Contains(args, "--resume") {
		t.Error("first turn must not pass --resume")
	}
}

func TestArgsResumeContinuesSession(t *testing.T) {
	s := baseSpec()
	s.Resume = true
	args, err := s.Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if !hasPair(args, "--resume", "11111111-2222-3333-4444-555555555555") {
		t.Errorf("resume turn must pass --resume, got %v", args)
	}
	// Passing both is an error at the CLI level, so it must never happen here.
	if slices.Contains(args, "--session-id") {
		t.Error("resume turn must not also pass --session-id")
	}
}

func TestArgsMountsSessionHomeAndSetsWorkdir(t *testing.T) {
	args, err := baseSpec().Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	// Losing either of these breaks --resume in a way that only shows up on the
	// second turn, so they are asserted explicitly.
	if !hasPair(args, "-v", "/srv/agent/claude-home:"+ContainerClaudeHome) {
		t.Errorf("claude home must be mounted for session persistence, got %v", args)
	}
	if !hasPair(args, "--workdir", ContainerWorkspace) {
		t.Errorf("workdir must be pinned so --resume can find the session, got %v", args)
	}
}

func TestArgsPassesAPIKeyByNameNotValue(t *testing.T) {
	s := baseSpec()
	s.CredentialValue = "sk-ant-secret-value"
	args, err := s.Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "sk-ant-secret-value") {
			t.Fatalf("api key leaked into argv: %v", args)
		}
	}
	if !hasPair(args, "-e", "ANTHROPIC_API_KEY") {
		t.Errorf("credential must be inherited by name, got %v", args)
	}

	// A setup-token is selected by variable name, so the flag must follow it.
	s.CredentialEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	args, err = s.Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if !hasPair(args, "-e", "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("oauth token env not passed through, got %v", args)
	}
}

func TestArgsAdditionalDirsAreReadOnlyAndAdvertised(t *testing.T) {
	s := baseSpec()
	s.AdditionalDirs = []string{"/srv/shared/docs"}
	args, err := s.Args()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if !hasPair(args, "-v", "/srv/shared/docs:/mnt/docs:ro") {
		t.Errorf("additional dirs must mount read-only, got %v", args)
	}
	if !hasPair(args, "--add-dir", "/mnt/docs") {
		t.Errorf("additional dirs must be passed via --add-dir, got %v", args)
	}
}

func TestArgsRejectsOversizedPrompt(t *testing.T) {
	s := baseSpec()
	s.Prompt = strings.Repeat("x", MaxPromptBytes+1)
	if _, err := s.Args(); err == nil {
		t.Fatal("oversized prompt was accepted")
	}
}

func TestDecodeInit(t *testing.T) {
	events := mustDecode(t, `{"type":"system","subtype":"init","session_id":"abc","model":"claude-opus-4-8"}`)
	if len(events) != 1 || events[0].Kind != KindInit {
		t.Fatalf("got %+v, want one init event", events)
	}
	if events[0].SessionID != "abc" {
		t.Errorf("session id = %q, want abc", events[0].SessionID)
	}
}

func TestDecodeAssistantSplitsTextAndToolUse(t *testing.T) {
	events := mustDecode(t, `{"type":"assistant","session_id":"s","message":{"content":[
		{"type":"text","text":"Looking at the file"},
		{"type":"tool_use","name":"Read","input":{"file_path":"/workspace/main.go"}}]}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Kind != KindText || events[0].Text != "Looking at the file" {
		t.Errorf("first event = %+v, want text", events[0])
	}
	if events[1].Kind != KindToolUse || events[1].ToolName != "Read" {
		t.Errorf("second event = %+v, want tool_use Read", events[1])
	}
	if !strings.Contains(events[1].ToolInput, "main.go") {
		t.Errorf("tool input lost: %q", events[1].ToolInput)
	}
}

func TestDecodeAPIRetry(t *testing.T) {
	events := mustDecode(t, `{"type":"system","subtype":"api_retry","attempt":2,"max_retries":5,"error":"overloaded"}`)
	if len(events) != 1 || events[0].Kind != KindAPIRetry {
		t.Fatalf("got %+v, want one api_retry event", events)
	}
	if events[0].RetryAttempt != 2 || events[0].RetryMax != 5 || events[0].RetryError != "overloaded" {
		t.Errorf("retry detail lost: %+v", events[0])
	}
}

func TestDecodeResultCarriesCost(t *testing.T) {
	events := mustDecode(t, `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.0421,"is_error":false}`)
	if len(events) != 1 || events[0].Kind != KindResult {
		t.Fatalf("got %+v, want one result event", events)
	}
	if events[0].Text != "done" || events[0].CostUSD != 0.0421 || events[0].IsError {
		t.Errorf("result = %+v", events[0])
	}
}

// A newer CLI emitting an unknown event type must not break the decoder.
func TestDecodeUnknownTypeIsPreserved(t *testing.T) {
	line := `{"type":"some_future_event","session_id":"s","payload":42}`
	events := mustDecode(t, line)
	if len(events) != 1 || events[0].Kind != KindOther {
		t.Fatalf("got %+v, want one other event", events)
	}
	if events[0].Raw != line {
		t.Errorf("raw line not preserved: %q", events[0].Raw)
	}
}

func TestDecodeTruncatesHugeToolResult(t *testing.T) {
	huge := strings.Repeat("y", maxPreview*3)
	events := mustDecode(t, `{"type":"user","message":{"content":[{"type":"tool_result","content":"`+huge+`"}]}}`)
	if len(events) != 1 || events[0].Kind != KindToolResult {
		t.Fatalf("got %+v, want one tool_result event", events)
	}
	if len(events[0].ToolResult) > maxPreview+64 {
		t.Errorf("tool result not truncated: %d bytes", len(events[0].ToolResult))
	}
}

// A malformed line must be skipped, not abort the turn.
func TestScanSkipsMalformedLines(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`this is not json`,
		`{"type":"result","result":"finished","total_cost_usd":0.01}`,
	}, "\n")

	var kinds []EventKind
	var decodeErrors int
	err := Scan(strings.NewReader(input),
		func(e Event) error { kinds = append(kinds, e.Kind); return nil },
		func(error) { decodeErrors++ })
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if decodeErrors != 1 {
		t.Errorf("decode errors = %d, want 1", decodeErrors)
	}
	want := []EventKind{KindInit, KindResult}
	if !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
}

func baseSpec() RunSpec {
	return RunSpec{
		Runtime:         "docker",
		Image:           "roundclaw/claude:latest",
		ContainerName:   ContainerName("pr-reviewer", 7),
		WorkDir:         "/srv/agent/work",
		ClaudeHome:      "/srv/agent/claude-home",
		SessionID:       "11111111-2222-3333-4444-555555555555",
		CredentialEnv:   "ANTHROPIC_API_KEY",
		CredentialValue: "unused-in-argv",
		Prompt:          "review the diff",
	}
}

func mustDecode(t *testing.T, line string) []Event {
	t.Helper()
	events, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return events
}

func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// The real CLI emits one of these per turn. Before it was decoded, the raw JSON
// was landing in the live log and showing up as noise in /status.
func TestDecodeRateLimitEvent(t *testing.T) {
	events := mustDecode(t, `{"type":"rate_limit_event","session_id":"s","rate_limit_info":{
		"status":"allowed","rateLimitType":"five_hour","resetsAt":1784547000}}`)
	if len(events) != 1 || events[0].Kind != KindRateLimit {
		t.Fatalf("got %+v, want one rate_limit event", events)
	}
	if events[0].RateLimitStatus != "allowed" || events[0].RateLimitType != "five_hour" {
		t.Errorf("rate limit detail lost: %+v", events[0])
	}
}

// A rate_limit_event with no info block must not panic or mis-report.
func TestDecodeRateLimitEventWithoutInfo(t *testing.T) {
	events := mustDecode(t, `{"type":"rate_limit_event","session_id":"s"}`)
	if len(events) != 1 || events[0].Kind != KindRateLimit || events[0].RateLimitStatus != "" {
		t.Fatalf("got %+v", events)
	}
}
