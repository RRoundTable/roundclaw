package activity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
)

// sessionRuntime writes a stand-in container runtime that keeps session
// transcripts where the real CLI keeps them and refuses a session flag the way
// the real CLI refuses it.
//
// refuse forces one of those refusals regardless of what is on disk, which is
// the only way to drive the case the fallback exists for: roundclaw's reading of
// the session disagreeing with the CLI's. Left empty, the fake goes by the
// transcript, like the real thing.
//
//	create — the CLI holds a session roundclaw cannot see
//	resume — roundclaw sees a session the CLI does not hold
//	always — the run fails for a reason that says nothing about the session
func sessionRuntime(t *testing.T, dir, refuse string) string {
	t.Helper()

	// One line per invocation whatever the prompt contains: a recap runs to
	// several lines, and a log that splits on those cannot be read back as calls.
	body := `#!/bin/sh
{ printf '%s' "$*" | tr '\n' ' '; printf '\n'; } >> ` + filepath.Join(dir, "calls.log") + `
[ "$1" = run ] || exit 0

REFUSE="` + refuse + `"
HOME_MOUNT=""; MODE=""; SESSION=""; prev=""
for arg in "$@"; do
    case "$arg" in
        *:` + claude.ContainerClaudeHome + `) HOME_MOUNT="${arg%:` + claude.ContainerClaudeHome + `}" ;;
    esac
    case "$prev" in
        --session-id) MODE=create; SESSION="$arg" ;;
        --resume)     MODE=resume; SESSION="$arg" ;;
    esac
    prev="$arg"
done
T="$HOME_MOUNT/projects/-workspace/$SESSION.jsonl"

case "$REFUSE:$MODE" in
    always:*)
        echo "the container would not start" >&2; exit 1 ;;
    create:create)
        echo "Error: Session ID $SESSION is already in use." >&2; exit 1 ;;
    resume:resume)
        echo "Error: No conversation found with session ID: $SESSION" >&2; exit 1 ;;
esac

if [ -z "$REFUSE" ]; then
    if [ "$MODE" = create ] && [ -f "$T" ]; then
        echo "Error: Session ID $SESSION is already in use." >&2; exit 1
    fi
    if [ "$MODE" = resume ] && [ ! -f "$T" ]; then
        echo "Error: No conversation found with session ID: $SESSION" >&2; exit 1
    fi
fi

mkdir -p "$(dirname "$T")"
: >> "$T"
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$SESSION"
printf '{"type":"result","subtype":"success","result":"done","total_cost_usd":0.01,"is_error":false}\n'
`

	path := filepath.Join(dir, "session-runtime")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write session runtime: %v", err)
	}
	return path
}

// runs returns the runtime invocations that started a container, in order, with
// the flag each one carried. removeOrphan calls the runtime too, so counting
// lines would count those as well.
func runs(t *testing.T, dir string) []string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatalf("read runtime calls: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "run ") {
			out = append(out, line)
		}
	}
	return out
}

func flagOf(t *testing.T, call string) string {
	t.Helper()
	switch {
	case strings.Contains(call, "--session-id"):
		return "--session-id"
	case strings.Contains(call, "--resume"):
		return "--resume"
	default:
		t.Fatalf("run carried no session flag: %s", call)
		return ""
	}
}

func queueTurn(t *testing.T, st *store.Store, conversation, text string) int64 {
	t.Helper()
	turnID, _, err := st.CreateTurn(t.Context(), store.NewTurn{
		Request: text, Origin: core.HTTPPollOrigin(), Conversation: conversation,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return turnID
}

// The first turn opens the session and every later turn continues it — decided
// from the transcript the CLI leaves behind, not from anything remembered
// between turns.
func TestSessionFlagFollowsTheTranscriptOnDisk(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, sessionRuntime(t, dir, ""))

	for i := 0; i < 2; i++ {
		if _, err := runTurn(t, a, RunTurnInput{
			AgentID: "tester", TurnID: queueTurn(t, st, "", "hi"),
			WorkflowID: "roundclaw-tester-default", Prompt: "hi",
		}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}

	calls := runs(t, dir)
	if len(calls) != 2 {
		t.Fatalf("started %d containers, want 2", len(calls))
	}
	if got := flagOf(t, calls[0]); got != "--session-id" {
		t.Errorf("first turn used %s; with no transcript it must open a session", got)
	}
	if got := flagOf(t, calls[1]); got != "--resume" {
		t.Errorf("second turn used %s; the first turn left a transcript to continue", got)
	}
}

// The wedge this replaces. A turn that opens the session and then loses its
// worker before reporting completion is retried by Temporal with the same input,
// so the retry meets a session it created itself. Refusing that and stopping
// there is what took a conversation out permanently: every later turn was the
// same retry.
func TestATurnResumesASessionItIsRefusedPermissionToCreate(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, sessionRuntime(t, dir, "create"))

	result, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: queueTurn(t, st, "", "hi"),
		WorkflowID: "roundclaw-tester-default", Prompt: "hi",
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if result.Status != core.TurnDone {
		t.Fatalf("turn = %s (%s); a session that already exists must be resumed, not fatal",
			result.Status, result.ErrorMessage)
	}

	calls := runs(t, dir)
	if len(calls) != 2 {
		t.Fatalf("started %d containers, want 2 — one refused, one that recovered", len(calls))
	}
	if got := flagOf(t, calls[1]); got != "--resume" {
		t.Errorf("the second attempt used %s; a refused create must fall back to a resume", got)
	}
}

// A transcript cleaned up underneath a live conversation — the CLI ages them out
// on its own schedule, so this is ordinary rather than exceptional. The turn that
// meets it opens a new session and carries the record of what came before,
// without a failed attempt in between: there is nothing on disk to mistake for a
// session, so the first attempt is already the right one.
func TestALostSessionIsRebuiltOnTheFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, sessionRuntime(t, dir, ""))

	done := queueTurn(t, st, "", "what did we decide about the schema?")
	if err := st.FinishTurn(t.Context(), done, core.TurnResult{
		TurnID: done, Status: core.TurnDone, Text: "we decided to keep it flat",
	}); err != nil {
		t.Fatalf("finish turn: %v", err)
	}

	if _, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: queueTurn(t, st, "", "and the index?"),
		WorkflowID: "roundclaw-tester-default", Prompt: "and the index?",
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	calls := runs(t, dir)
	if len(calls) != 1 {
		t.Fatalf("started %d containers, want 1 — a missing transcript is read before "+
			"the run, not discovered by failing one", len(calls))
	}
	if got := flagOf(t, calls[0]); got != "--session-id" {
		t.Errorf("used %s; with no transcript there is nothing to continue", got)
	}
	if !strings.Contains(calls[0], "keep it flat") {
		t.Error("the new session was opened without the record of what came before it")
	}
}

// The same loss seen from the other side: a transcript that is on disk but that
// the CLI will not open. The filesystem and the CLI disagree, the CLI wins, and
// the turn recovers inside itself rather than spending itself on the discovery.
func TestALostSessionIsRebuiltWithinTheSameTurn(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, sessionRuntime(t, dir, "resume"))

	// Something to recap, and a transcript on disk so the turn starts out
	// believing there is a session to continue.
	done := queueTurn(t, st, "", "what did we decide about the schema?")
	if err := st.FinishTurn(t.Context(), done, core.TurnResult{
		TurnID: done, Status: core.TurnDone, Text: "we decided to keep it flat",
	}); err != nil {
		t.Fatalf("finish turn: %v", err)
	}
	seedTranscript(t, a, "roundclaw-tester-default")

	result, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: queueTurn(t, st, "", "and the index?"),
		WorkflowID: "roundclaw-tester-default", Prompt: "and the index?",
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if result.Status != core.TurnDone {
		t.Fatalf("turn = %s (%s); a missing session must be reopened, not fatal",
			result.Status, result.ErrorMessage)
	}

	calls := runs(t, dir)
	if len(calls) != 2 {
		t.Fatalf("started %d containers, want 2 — one refused, one that recovered", len(calls))
	}
	if got := flagOf(t, calls[0]); got != "--resume" {
		t.Errorf("the first attempt used %s; a transcript on disk means continue", got)
	}
	if got := flagOf(t, calls[1]); got != "--session-id" {
		t.Errorf("the second attempt used %s; a refused resume must open a session", got)
	}
	if strings.Contains(calls[0], "keep it flat") {
		t.Error("the resume attempt carried a recap; the session it continues already holds it")
	}
	if !strings.Contains(calls[1], "keep it flat") {
		t.Error("the new session was opened without the record of what came before it")
	}
}

// The rule the whole path turns on. A failure that says nothing about the
// session — the container refusing to start, a credential expiring, a quota
// running out — must move nothing. Reading one of those as "the session is gone"
// is what wedged an agent for a day off the back of one oversized prompt.
func TestAFailureThatSaysNothingAboutTheSessionIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, sessionRuntime(t, dir, "always"))

	result, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: queueTurn(t, st, "", "hi"),
		WorkflowID: "roundclaw-tester-default", Prompt: "hi",
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if result.Status != core.TurnError {
		t.Fatalf("turn = %s; the run failed and the turn should say so", result.Status)
	}
	if calls := runs(t, dir); len(calls) != 1 {
		t.Fatalf("started %d containers, want 1 — an uninformative failure is not "+
			"evidence about the session and must not buy a second attempt", len(calls))
	}
}

// seedTranscript puts a session transcript where the CLI would have left one, so
// a test can start a turn from the state a live conversation is in.
func seedTranscript(t *testing.T, a *Activities, workflowID string) {
	t.Helper()

	path := claude.TranscriptPath(a.cfg.ClaudeHomeDir("tester"), claude.SessionID(workflowID))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create transcript dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

// A turn that cannot even be built into a command still closes out its row. The
// argv check runs inside the attempt now, so its failure returns through the
// same path as a failed run — and a turn closed with the zero value would leave
// a row with no status, showing as neither running nor finished for good.
func TestATurnThatCannotBeBuiltIsRecordedAsFailed(t *testing.T) {
	dir := t.TempDir()
	a, st, _ := newActivities(t, sessionRuntime(t, dir, ""))

	turnID := queueTurn(t, st, "", "hi")
	if _, err := runTurn(t, a, RunTurnInput{
		AgentID: "tester", TurnID: turnID, WorkflowID: "roundclaw-tester-default",
		Prompt: strings.Repeat("x", claude.MaxPromptBytes+1),
	}); err == nil {
		t.Fatal("an unbuildable turn succeeded")
	}

	turn, err := st.GetTurn(t.Context(), turnID)
	if err != nil {
		t.Fatalf("read turn: %v", err)
	}
	if turn.Status != core.TurnError {
		t.Errorf("turn status = %q, want %q", turn.Status, core.TurnError)
	}
	if turn.Error == "" {
		t.Error("the turn was recorded as failed without saying why")
	}
}
