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

// Flags written after the positional arguments must still be parsed. Go's flag
// package stops at the first non-flag token, so before parseFlags this exact
// command line queued a *waiting* turn and appended "--notify-me" to the prompt
// text — silently doing the opposite of what was asked, which is the failure this
// whole return-address mechanism exists to prevent.
func TestSendAcceptsFlagsAfterPositionals(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "pm")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "thread-1")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") == "true" {
			t.Error("--notify-me must not wait")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTestJSON(w, http.StatusAccepted, map[string]any{
			"turn_id": 3, "status": "queued", "conversation": "worker-1",
		})
	}))
	defer srv.Close()

	code := cmdSend([]string{
		"--url", srv.URL, "--token", "t",
		"dev", "QA 버튼 만들어줘", // positionals first, as anyone would write it
		"--notify-me", "--conversation", "worker-1",
	})
	if code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if text, _ := got["text"].(string); text != "QA 버튼 만들어줘" {
		t.Errorf("text = %q; a trailing flag leaked into the prompt", text)
	}
	if got["conversation_id"] != "worker-1" {
		t.Errorf("conversation_id = %v, want worker-1", got["conversation_id"])
	}
	notify, ok := got["notify"].(map[string]any)
	if !ok {
		t.Fatalf("no notify in body: %v", got)
	}
	if notify["agent"] != "pm" || notify["conversation"] != "thread-1" {
		t.Errorf("notify = %v, want the caller's own identity from the environment", notify)
	}
}

// Delegated work belongs to the conversation that asked for it. Without this the
// request lands in the delegate's default conversation — the one /ask, schedules
// and webhooks share — so every thread delegating to the same agent piles into a
// single Claude session and reads the others' history.
func TestSendNotifyMeRunsInTheCallersConversation(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "pm")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "1529396903836651540")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTestJSON(w, http.StatusAccepted, map[string]any{"turn_id": 4, "status": "queued"})
	}))
	defer srv.Close()

	code := cmdSend([]string{"--url", srv.URL, "--token", "t", "dev", "x", "--notify-me"})
	if code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if got["conversation_id"] != "1529396903836651540" {
		t.Errorf("conversation_id = %v, want the caller's own conversation", got["conversation_id"])
	}
}

// An explicit --conversation is the caller saying where the work goes, so
// inheriting must not override it.
func TestSendNotifyMeKeepsAnExplicitConversation(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "pm")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "thread-1")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTestJSON(w, http.StatusAccepted, map[string]any{"turn_id": 5, "status": "queued"})
	}))
	defer srv.Close()

	code := cmdSend([]string{
		"--url", srv.URL, "--token", "t", "dev", "x", "--notify-me", "--conversation", "worker-1",
	})
	if code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if got["conversation_id"] != "worker-1" {
		t.Errorf("conversation_id = %v, want worker-1", got["conversation_id"])
	}
}

// A delegator that is itself in a default conversation has no thread to hand
// down, and inventing one would strand the work in a conversation nobody is
// watching. The delegate's default conversation is the right answer there.
func TestSendNotifyMeFromTheDefaultConversationNamesNothing(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "pm")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTestJSON(w, http.StatusAccepted, map[string]any{"turn_id": 6, "status": "queued"})
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "dev", "x", "--notify-me"}); code != 0 {
		t.Fatalf("cmdSend exit = %d, want 0", code)
	}
	if _, ok := got["conversation_id"]; ok {
		t.Errorf("conversation_id = %v, want it absent so the delegate uses its default", got["conversation_id"])
	}
}

// Without an injected identity there is nobody to notify, and guessing would
// wake the wrong agent.
func TestSendRefusesNotifyMeOutsideAContainer(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be sent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if code := cmdSend([]string{"--url", srv.URL, "--token", "t", "dev", "x", "--notify-me"}); code == 0 {
		t.Error("--notify-me succeeded with no identity to return to")
	}
}

// say takes its target from the environment, so an agent needs no arguments to
// speak where it already is.
func TestSayUsesInjectedIdentity(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "thread-9")

	var path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeTestJSON(w, http.StatusOK, map[string]any{"delivered": true, "target": "chan-1"})
	}))
	defer srv.Close()

	if code := cmdSay([]string{"--url", srv.URL, "--token", "t", "빌드 중, 5분쯤 더"}); code != 0 {
		t.Fatalf("cmdSay exit = %d, want 0", code)
	}
	if path != "/v1/agents/dev/messages" {
		t.Errorf("path = %q, want /v1/agents/dev/messages", path)
	}
	if got["conversation"] != "thread-9" {
		t.Errorf("conversation = %v, want thread-9", got["conversation"])
	}
}

// Speaking where I am names the turn I am running, so the server reads my
// audience off that row. A delegated turn has no conversation history to infer
// one from — before this, an argument-less say inside one failed outright.
func TestSayNamesTheTurnItIsRunning(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "")
	t.Setenv("ROUNDCLAW_TURN_ID", "41")
	t.Setenv("ROUNDCLAW_REPLY_TO", "pm")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeTestJSON(w, http.StatusOK, map[string]any{"delivered": true, "target": "chan-1"})
	}))
	defer srv.Close()

	if code := cmdSay([]string{"--url", srv.URL, "--token", "t", "빌드 중"}); code != 0 {
		t.Fatalf("cmdSay exit = %d, want 0", code)
	}
	if got["turn_id"] != float64(41) {
		t.Errorf("turn_id = %v, want 41", got["turn_id"])
	}
}

// Speaking into someone else's conversation must not carry my turn id: their
// audience is theirs to resolve, and mine names a thread that is not theirs.
func TestSayToAnotherAgentDoesNotNameMyTurn(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "")
	t.Setenv("ROUNDCLAW_TURN_ID", "41")
	t.Setenv("ROUNDCLAW_REPLY_TO", "pm")
	t.Setenv("ROUNDCLAW_REPLY_TO_CONVERSATION", "thread-1")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeTestJSON(w, http.StatusOK, map[string]any{"delivered": true, "target": "chan-1"})
	}))
	defer srv.Close()

	if code := cmdSay([]string{"--url", srv.URL, "--token", "t", "PRD와 다른 부분", "--to", "pm"}); code != 0 {
		t.Fatalf("cmdSay exit = %d, want 0", code)
	}
	if _, ok := got["turn_id"]; ok {
		t.Errorf("turn_id = %v, want none when speaking in another agent's conversation", got["turn_id"])
	}
}

// --to the delegator resolves that agent's waiting conversation, not this one.
func TestSayToDelegatorUsesReplyToConversation(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")
	t.Setenv("ROUNDCLAW_CONVERSATION_ID", "worker-1")
	t.Setenv("ROUNDCLAW_REPLY_TO", "pm")
	t.Setenv("ROUNDCLAW_REPLY_TO_CONVERSATION", "thread-1")

	var path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeTestJSON(w, http.StatusOK, map[string]any{"delivered": true, "target": "chan-1"})
	}))
	defer srv.Close()

	if code := cmdSay([]string{"--url", srv.URL, "--token", "t", "PRD와 다른 부분 발견", "--to", "pm"}); code != 0 {
		t.Fatalf("cmdSay exit = %d, want 0", code)
	}
	if path != "/v1/agents/pm/messages" {
		t.Errorf("path = %q, want /v1/agents/pm/messages", path)
	}
	if got["conversation"] != "thread-1" {
		t.Errorf("conversation = %v, want thread-1 (the delegator's, not mine)", got["conversation"])
	}
}

// --agent written after the secret's name must still scope it. Losing that flag
// does not fail — it stores the credential globally, where every agent in the
// fleet reads it, and prints success either way. This is the one flag whose
// silent loss is a leak rather than an inconvenience.
func TestSecretSetAcceptsAgentAfterPositionals(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeTestJSON(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	code := cmdSecret([]string{
		"set", "GEMINI_API_KEY", "sk-test", // positionals first, as anyone would write it
		"--agent", "gameart", "--url", srv.URL, "--token", "t",
	})
	if code != 0 {
		t.Fatalf("cmdSecret exit = %d, want 0", code)
	}
	if path != "/v1/agents/gameart/secrets/GEMINI_API_KEY" {
		t.Errorf("path = %q; the secret was stored globally, not scoped to gameart", path)
	}
}

// rm reaches the same scope set does, so a secret can be removed from where it
// was put rather than silently deleting a global one that was never there.
func TestSecretRmAcceptsAgentAfterPositionals(t *testing.T) {
	var path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		writeTestJSON(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	code := cmdSecret([]string{
		"rm", "GEMINI_API_KEY", "--agent", "gameart",
		"--url", srv.URL, "--token", "t",
	})
	if code != 0 {
		t.Fatalf("cmdSecret exit = %d, want 0", code)
	}
	if method != http.MethodDelete || path != "/v1/agents/gameart/secrets/GEMINI_API_KEY" {
		t.Errorf("%s %s, want DELETE /v1/agents/gameart/secrets/GEMINI_API_KEY", method, path)
	}
}

// Inside a container an agent manages its own schedules without naming itself,
// through the agent-scoped routes its restricted token can reach.
func TestScheduleDefaultsToTheCallingAgent(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeTestJSON(w, http.StatusOK, map[string]any{"schedules": []any{}})
	}))
	defer srv.Close()

	if code := cmdSchedule([]string{"ls", "--url", srv.URL, "--token", "t"}); code != 0 {
		t.Fatalf("cmdSchedule exit = %d, want 0", code)
	}
	if path != "/v1/agents/dev/schedules" {
		t.Errorf("path = %q, want /v1/agents/dev/schedules", path)
	}
}

// A PUT replaces the whole definition, so an edit that names only the cron has
// to carry the rest of the current one back — otherwise "move it an hour later"
// silently drops the prompt and the channel.
func TestScheduleSetKeepsWhatWasNotNamed(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")

	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeTestJSON(w, http.StatusOK, map[string]any{
				"id": "standup", "agent_id": "dev", "cron": "0 9 * * *",
				"timezone": "Asia/Seoul", "prompt": "summarise yesterday",
				"channel_id": "chan-dev", "enabled": true,
			})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"id": "standup", "agent_id": "dev", "cron": "0 10 * * *",
			"timezone": "Asia/Seoul", "channel_id": "chan-dev", "enabled": true,
		})
	}))
	defer srv.Close()

	code := cmdSchedule([]string{"set", "standup", "--cron", "0 10 * * *", "--url", srv.URL, "--token", "t"})
	if code != 0 {
		t.Fatalf("cmdSchedule exit = %d, want 0", code)
	}
	if sent["cron"] != "0 10 * * *" {
		t.Errorf("cron = %v, want the new one", sent["cron"])
	}
	if sent["prompt"] != "summarise yesterday" {
		t.Errorf("prompt = %v, want the existing one carried over", sent["prompt"])
	}
	if sent["channel_id"] != "chan-dev" {
		t.Errorf("channel_id = %v, want the existing one carried over", sent["channel_id"])
	}
	// Nothing said about pausing means the schedule keeps the state it is in;
	// sending enabled would decide it here instead.
	if _, ok := sent["enabled"]; ok {
		t.Errorf("body carried enabled=%v; an edit that says nothing about it must leave it to the server", sent["enabled"])
	}
}

// A read that fails for any reason other than "no such schedule" must stop the
// edit. Treating it as a new schedule would write back only the flags that were
// typed, dropping the prompt and channel the caller never meant to touch.
func TestScheduleSetRefusesToGuessAfterAFailedRead(t *testing.T) {
	t.Setenv("ROUNDCLAW_AGENT_ID", "dev")

	var puts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "registry is down"})
			return
		}
		atomic.AddInt32(&puts, 1)
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "standup", "agent_id": "dev"})
	}))
	defer srv.Close()

	code := cmdSchedule([]string{"set", "standup", "--cron", "0 10 * * *", "--url", srv.URL, "--token", "t"})
	if code == 0 {
		t.Error("cmdSchedule exit = 0, want non-zero when the current definition cannot be read")
	}
	if puts != 0 {
		t.Errorf("puts = %d, want 0 — nothing should be written on a guess", puts)
	}
}
