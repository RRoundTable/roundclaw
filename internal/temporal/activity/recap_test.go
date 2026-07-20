package activity

import (
	"strings"
	"testing"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
)

func seedTurn(t *testing.T, st *store.Store, request string, result core.TurnResult) {
	t.Helper()
	id, _, err := st.CreateTurn(t.Context(), request, core.HTTPPollOrigin(), "")
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if result.Status == "" {
		return // leave it running
	}
	result.TurnID = id
	if err := st.FinishTurn(t.Context(), id, result); err != nil {
		t.Fatalf("finish turn: %v", err)
	}
}

func TestBuildRecapReadsOldestFirstAndLabelsItself(t *testing.T) {
	dir := t.TempDir()
	_, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	seedTurn(t, st, "what is in the repo", core.TurnResult{Status: core.TurnDone, Text: "a Go module"})
	seedTurn(t, st, "run the tests", core.TurnResult{Status: core.TurnError, ErrorMessage: "no test files"})
	seedTurn(t, st, "long job", core.TurnResult{Status: core.TurnStopped})

	recap, err := buildRecap(t.Context(), st)
	if err != nil {
		t.Fatalf("build recap: %v", err)
	}

	// The preamble has to do two things at once, and an earlier version got the
	// balance wrong: it stressed that the recap was not memory so heavily that
	// the agent treated its contents as unreliable and answered "I don't know"
	// to a fact it had recorded itself. It must say the record is accurate and
	// usable, while being clear the surrounding context is gone.
	for _, want := range []string{"previous session was lost", "reliable", "use them"} {
		if !strings.Contains(recap, want) {
			t.Errorf("recap does not present the record as usable (%q):\n%s", want, recap)
		}
	}
	if strings.Contains(recap, "not as memory") {
		t.Error("the recap discourages using its own contents")
	}
	for _, want := range []string{
		"what is in the repo", "a Go module",
		"run the tests", "no test files",
		"long job", "stopped before finishing",
	} {
		if !strings.Contains(recap, want) {
			t.Errorf("recap is missing %q:\n%s", want, recap)
		}
	}

	// Oldest first, so it reads like a conversation rather than backwards.
	first := strings.Index(recap, "what is in the repo")
	last := strings.Index(recap, "long job")
	if first < 0 || last < 0 || first > last {
		t.Errorf("recap is not in chronological order:\n%s", recap)
	}
}

// The turn being started now is still running, and must not appear in its own
// recap as though it had already happened.
func TestBuildRecapSkipsTheRunningTurn(t *testing.T) {
	dir := t.TempDir()
	_, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	seedTurn(t, st, "finished work", core.TurnResult{Status: core.TurnDone, Text: "done"})
	seedTurn(t, st, "the request being asked right now", core.TurnResult{})

	recap, err := buildRecap(t.Context(), st)
	if err != nil {
		t.Fatalf("build recap: %v", err)
	}
	if strings.Contains(recap, "being asked right now") {
		t.Errorf("the running turn appeared in its own recap:\n%s", recap)
	}
	if !strings.Contains(recap, "finished work") {
		t.Errorf("recap lost the finished turn:\n%s", recap)
	}
}

// With nothing to recap the prompt is left alone, rather than gaining a
// preamble that explains an absence.
func TestBuildRecapIsEmptyWithNoHistory(t *testing.T) {
	dir := t.TempDir()
	_, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	recap, err := buildRecap(t.Context(), st)
	if err != nil {
		t.Fatalf("build recap: %v", err)
	}
	if recap != "" {
		t.Errorf("recap for an agent with no history = %q", recap)
	}
}

// A recap of ten turns of full transcripts would crowd out the actual request,
// so each field is bounded.
func TestBuildRecapTruncatesLongFields(t *testing.T) {
	dir := t.TempDir()
	_, st, _ := newActivities(t, fakeRuntime(t, dir, false))

	seedTurn(t, st, strings.Repeat("q", 5000),
		core.TurnResult{Status: core.TurnDone, Text: strings.Repeat("a", 5000)})

	recap, err := buildRecap(t.Context(), st)
	if err != nil {
		t.Fatalf("build recap: %v", err)
	}
	if len(recap) > 2*maxRecapField+600 {
		t.Errorf("recap is %d bytes; long fields were not truncated", len(recap))
	}
	if !strings.Contains(recap, "…") {
		t.Error("truncation left no marker, so the agent cannot tell it is partial")
	}
}
