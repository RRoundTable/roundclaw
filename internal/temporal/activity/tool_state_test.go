package activity

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roundtable/roundclaw/internal/registry"
)

func TestNothingDeclaredIsNotProbed(t *testing.T) {
	// A tool that says nothing has nothing to check, so the check passes
	// trivially. What it must not do is get restored — see Restorable.
	var r registry.Reachability
	if r.Declared() {
		t.Error("an empty declaration reported itself as declared")
	}
	if r.Restorable() {
		t.Error("an empty declaration reported itself as restorable; the dangerous default is the optimistic one")
	}
	if err := probe(t.Context(), r, time.Second); err != nil {
		t.Errorf("probing an empty declaration failed: %v", err)
	}
}

// Only a container is something roundclaw can act on. Everything else it can
// check and not fix, which is the honest limit of a declaration.
func TestOnlyAContainerIsRestorable(t *testing.T) {
	if (registry.Reachability{Address: "127.0.0.1:1"}).Restorable() {
		t.Error("an address alone claimed to be restorable")
	}
	if !(registry.Reachability{Container: "pg"}).Restorable() {
		t.Error("a named container is the one thing that is restorable")
	}
}

func TestProbeChecksAFileThatMustExist(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "sock")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := probe(t.Context(), registry.Reachability{File: present}, time.Second); err != nil {
		t.Errorf("a file that exists failed the probe: %v", err)
	}
	err := probe(t.Context(), registry.Reachability{File: filepath.Join(dir, "gone")}, time.Second)
	if err == nil {
		t.Error("a missing file passed the probe")
	}
}

func TestProbeChecksAnAddressAndAnEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	if err := probe(t.Context(), registry.Reachability{Address: ln.Addr().String()}, time.Second); err != nil {
		t.Errorf("a listening address failed the probe: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := probe(t.Context(), registry.Reachability{Endpoint: srv.URL}, time.Second); err != nil {
		t.Errorf("a 200 endpoint failed the probe: %v", err)
	}

	sad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sad.Close()
	if err := probe(t.Context(), registry.Reachability{Endpoint: sad.URL}, time.Second); err == nil {
		t.Error("a 500 endpoint passed the probe")
	}
}

// Every declared condition has to hold. A tool naming two is saying both, and
// answering half of it is not being reachable.
func TestEveryDeclaredConditionMustHold(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	r := registry.Reachability{Address: ln.Addr().String(), File: "/definitely/not/here"}
	if err := probe(t.Context(), r, time.Second); err == nil {
		t.Error("a half-satisfied declaration passed the probe")
	}
}

// A closed port must not hang the session. Session start does I/O now, and an
// unreachable tool that could wait forever would let one dead dependency stop
// every turn behind it.
func TestAnUnreachableToolIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening there now

	start := time.Now()
	if err := waitFor(t.Context(), registry.Reachability{Address: addr}, 300*time.Millisecond); err == nil {
		t.Error("waiting on a dead address reported success")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waited %v for a 300ms bound", elapsed)
	}
}

// What the agent actually reads. A tool that is fine says nothing — the note
// exists to surface what is not fine, before the agent acts on it.
func TestNoteIsSilentWhenEverythingWorks(t *testing.T) {
	note := toolStateNote([]ToolState{
		{ID: "deploy", Condition: ToolUsable},
		{ID: "db", Condition: ToolUsable},
	})
	if note != "" {
		t.Errorf("note = %q, want empty when every tool is usable", note)
	}
}

func TestNoteSaysUnavailableWithTheReasonAndKeepsTheGrant(t *testing.T) {
	note := toolStateNote([]ToolState{
		{ID: "db", Condition: ToolUnavailable, Reason: "127.0.0.1:5432 did not accept a connection"},
	})
	if !strings.Contains(note, "UNAVAILABLE") {
		t.Error("the note does not say the tool is unavailable")
	}
	if !strings.Contains(note, "did not accept a connection") {
		t.Error("the note does not carry the reason")
	}
	// Unavailable is not the same as ungranted, and the agent has to be able to
	// tell them apart: the grant stands, the tool is down.
	if !strings.Contains(note, "down, not withdrawn") {
		t.Error("the note does not distinguish a down tool from a withdrawn one")
	}
}

// Restoring never invents state. An agent told only "restored" would assume it
// got back what it left.
func TestNoteWarnsThatRestoredStateMayBePartial(t *testing.T) {
	note := toolStateNote([]ToolState{{ID: "db", Condition: ToolRestored}})
	if !strings.Contains(note, "whatever actually came back") {
		t.Errorf("a restored tool is reported without warning that its state may be partial: %q", note)
	}
}

func TestNoteSaysDriftMakesResultsIncomparable(t *testing.T) {
	note := toolStateNote([]ToolState{
		{ID: "deploy", Condition: ToolDrifted, Reason: "it does not match the identity its newest version recorded"},
	})
	if !strings.Contains(note, "not comparable") {
		t.Errorf("drift is reported without saying what it costs: %q", note)
	}
}

// Stable ordering, so the same fleet state reads the same way twice rather than
// in whatever order the grants happened to resolve.
func TestNoteOrderingIsStable(t *testing.T) {
	states := []ToolState{
		{ID: "zebra", Condition: ToolUnavailable, Reason: "x"},
		{ID: "alpha", Condition: ToolUnavailable, Reason: "y"},
	}
	first := toolStateNote(states)
	if strings.Index(first, "alpha") > strings.Index(first, "zebra") {
		t.Error("the note is not ordered by tool id")
	}
	if toolStateNote([]ToolState{states[1], states[0]}) != first {
		t.Error("the same states produced two different notes")
	}
}
