package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	// The real gap is seconds; a test must not wait that long between polls.
	pollInterval = 5 * time.Millisecond
}

// send now waits by default. When the server finishes the turn inside its wait
// budget it answers the POST directly, and one request is enough.
func TestSendWaitsInline(t *testing.T) {
	var posts, gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			atomic.AddInt32(&posts, 1)
			if r.URL.Query().Get("wait") != "true" {
				t.Errorf("send should wait by default, but POST carried no ?wait=true")
			}
			writeTestJSON(w, http.StatusOK, map[string]any{
				"turn_id": 1, "status": "done", "result": "delegated answer",
			})
		default:
			atomic.AddInt32(&gets, 1)
		}
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "dev", "do the thing"}); code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if gets != 0 {
		t.Errorf("gets = %d, want 0 (no polling needed when the POST already finished)", gets)
	}
}

// The important case: the turn outlives the server's wait budget, so the POST
// demotes to 202 with the turn still running. send must then poll the turn to
// completion rather than returning a bare queued id — the bug this fixes.
func TestSendPollsAfterDemotion(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeTestJSON(w, http.StatusAccepted, map[string]any{
				"turn_id": 7, "status": "queued", "queue_position": 0,
			})
			return
		}
		// A couple of reads still running, then a terminal result.
		if n := atomic.AddInt32(&gets, 1); n < 3 {
			writeTestJSON(w, http.StatusOK, map[string]any{"status": "running"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"status": "done", "result": "finished after polling", "cost_usd": 0.12,
		})
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "dev", "long job"}); code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if gets < 3 {
		t.Errorf("gets = %d, want >= 3 (should poll until the turn leaves running)", gets)
	}
}

// A turn that ends in error must surface as a non-zero exit even when reached
// through polling, so a delegating caller can tell success from failure.
func TestSendPollsToError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeTestJSON(w, http.StatusAccepted, map[string]any{"turn_id": 3, "status": "queued"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "tool blew up"})
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "dev", "boom"}); code != 1 {
		t.Fatalf("cmdSend exit = %d, want 1 for a failed turn", code)
	}
}

// --no-wait keeps the old fire-and-forget behaviour: return the queued id, no
// ?wait=true, no polling.
func TestSendNoWait(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Query().Get("wait") == "true" {
				t.Errorf("--no-wait must not ask the server to wait")
			}
			writeTestJSON(w, http.StatusAccepted, map[string]any{"turn_id": 9, "status": "queued"})
			return
		}
		atomic.AddInt32(&gets, 1)
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "--no-wait", "dev", "later"}); code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if gets != 0 {
		t.Errorf("gets = %d, want 0 (--no-wait must not poll)", gets)
	}
}

// A turn that never finishes within the timeout returns non-zero without
// hanging, and does not blow up.
func TestSendTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeTestJSON(w, http.StatusAccepted, map[string]any{"turn_id": 5, "status": "queued"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"status": "running"})
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "--timeout", "20ms", "dev", "never"}); code != 1 {
		t.Fatalf("cmdSend exit = %d, want 1 on timeout", code)
	}
}

func TestTurnFinished(t *testing.T) {
	for status, want := range map[string]bool{
		"": false, "running": false, "queued": false,
		"done": true, "error": true, "stopped": true,
	} {
		if got := turnFinished(status); got != want {
			t.Errorf("turnFinished(%q) = %v, want %v", status, got, want)
		}
	}
}

func writeTestJSON(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	raw, err := json.Marshal(body)
	if err != nil {
		panic(fmt.Sprintf("marshal test body: %v", err))
	}
	if _, err := w.Write(raw); err != nil && !strings.Contains(err.Error(), "closed") {
		panic(err)
	}
}
