package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// fakeTemporal records signals instead of sending them, so the admission path
// can be exercised without a Temporal server.
type fakeTemporal struct {
	mu      sync.Mutex
	signals []string
	starts  int
}

func (f *fakeTemporal) SignalWithStartWorkflow(_ context.Context, _, signalName string, _ any,
	_ client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, signalName)
	f.starts++
	return nil, nil
}

func (f *fakeTemporal) SignalWorkflow(_ context.Context, _, _, signalName string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, signalName)
	return nil
}

func (f *fakeTemporal) QueryWorkflow(_ context.Context, _, _, _ string, _ ...any) (converter.EncodedValue, error) {
	// Status queries must survive an unreachable workflow, so this always
	// fails and callers are expected to degrade to a zero queue depth.
	return nil, io.EOF
}

func (f *fakeTemporal) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.signals...)
}

const testToken = "test-token-value"

func newHarness(t *testing.T) (*httptest.Server, *fakeTemporal, *store.Store) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	err := os.WriteFile(configPath, []byte(`
workspace_root: ws
container:
  image: roundclaw/claude:test
http:
  wait_timeout: 300ms
  max_sse_per_agent: 2
agents:
  - id: pr-reviewer
    discord_channels: ["chan-1"]
`), 0o600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })

	st, err := stores.Get("pr-reviewer")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Seed(context.Background(), []registry.Agent{
		{ID: "pr-reviewer", Description: "Reviews pull requests", DiscordChannels: []string{"chan-1"}},
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	tc := &fakeTemporal{}
	disp := NewDispatcher(cfg, tc, stores, reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := NewHTTP(disp, log, []string{testToken}, cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, tc, st
}

func post(t *testing.T, srv *httptest.Server, path, token, idempotencyKey string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

func TestRequestRequiresBearerToken(t *testing.T) {
	srv, _, _ := newHarness(t)

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests", "", "", submitBody{Text: "hi"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", resp.StatusCode)
	}

	resp = post(t, srv, "/v1/agents/pr-reviewer/requests", "wrong-token", "", submitBody{Text: "hi"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", resp.StatusCode)
	}
}

func TestRequestReturns202WithTurnID(t *testing.T) {
	srv, tc, _ := newHarness(t)

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "review the diff"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	body := decode[submitResponse](t, resp)
	if body.TurnID <= 0 {
		t.Errorf("turn_id = %d, want a positive id", body.TurnID)
	}
	if body.AgentID != "pr-reviewer" {
		t.Errorf("agent_id = %q", body.AgentID)
	}
	if got := tc.sent(); len(got) != 1 || got[0] != "enqueue" {
		t.Errorf("signals = %v, want one enqueue", got)
	}
}

// A client retrying with the same Idempotency-Key must land on the same turn
// and must not signal the workflow a second time.
func TestIdempotencyKeyPreventsDuplicateRuns(t *testing.T) {
	srv, tc, st := newHarness(t)

	first := decode[submitResponse](t,
		post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "abc-123", submitBody{Text: "do it"}))

	for range 2 {
		resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "abc-123", submitBody{Text: "do it"})
		body := decode[submitResponse](t, resp)
		if body.TurnID != first.TurnID {
			t.Fatalf("retry got turn %d, want %d", body.TurnID, first.TurnID)
		}
		if !body.Duplicate {
			t.Error("retry was not flagged as a duplicate")
		}
	}

	if got := tc.sent(); len(got) != 1 {
		t.Errorf("sent %d signals (%v), want exactly 1 — retries started extra runs", len(got), got)
	}
	turns, err := st.RecentTurns(context.Background(), 10)
	if err != nil {
		t.Fatalf("recent turns: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("created %d turns, want 1", len(turns))
	}
}

// Distinct keys are distinct requests, each getting its own turn and reply.
func TestDistinctKeysProduceDistinctTurns(t *testing.T) {
	srv, tc, _ := newHarness(t)

	a := decode[submitResponse](t, post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "k1", submitBody{Text: "one"}))
	b := decode[submitResponse](t, post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "k2", submitBody{Text: "two"}))

	if a.TurnID == b.TurnID {
		t.Fatalf("distinct requests shared turn %d", a.TurnID)
	}
	if got := tc.sent(); len(got) != 2 {
		t.Errorf("sent %d signals, want 2", len(got))
	}
}

func TestSteerUsesSteerSignal(t *testing.T) {
	srv, tc, _ := newHarness(t)

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "do X instead", Steer: true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := tc.sent(); len(got) != 1 || got[0] != "steer" {
		t.Errorf("signals = %v, want one steer", got)
	}
}

// A callback URL pointing into the private network must be rejected at
// admission, before any tokens are spent.
func TestCallbackURLSSRFIsRejectedAtAdmission(t *testing.T) {
	srv, tc, _ := newHarness(t)

	for _, bad := range []string{
		// Bad scheme or shape.
		"file:///etc/passwd",
		"ftp://example.com/hook",
		"not-a-url",
		// Addresses inside the host's own network. An earlier version passed
		// every one of these with 202 because the address check only ran at
		// delivery time, and the cases above all tripped the scheme check
		// first — so the test looked green while the hole was wide open.
		"http://127.0.0.1:9999/hook",
		"http://localhost:9999/hook",
		"http://10.0.0.5/hook",
		"http://192.168.1.10/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:9999/hook",
		"http://100.64.0.1/hook",
	} {
		resp := post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "",
			submitBody{Text: "hi", CallbackURL: bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("callback %q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
	if got := tc.sent(); len(got) != 0 {
		t.Errorf("rejected callbacks still signalled the workflow: %v", got)
	}
}

func TestUnknownAgentIs404(t *testing.T) {
	srv, _, _ := newHarness(t)

	resp := post(t, srv, "/v1/agents/nope/requests", testToken, "", submitBody{Text: "hi"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ?wait=true must not hang forever. When the turn is still running at the
// deadline it demotes to 202 and hands back a turn_id the caller can poll.
func TestWaitDemotesTo202OnTimeout(t *testing.T) {
	srv, _, _ := newHarness(t)

	start := time.Now()
	resp := post(t, srv, "/v1/agents/pr-reviewer/requests?wait=true", testToken, "", submitBody{Text: "long job"})
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 after the wait budget expired", resp.StatusCode)
	}
	if elapsed > 3*time.Second {
		t.Errorf("waited %v; the wait budget was not honoured", elapsed)
	}
	body := decode[submitResponse](t, resp)
	if body.TurnID <= 0 {
		t.Error("demoted response carried no turn_id to poll")
	}
}

// When the turn finishes inside the budget, ?wait=true returns the result.
func TestWaitReturnsCompletedResult(t *testing.T) {
	srv, _, st := newHarness(t)

	// Finish the turn shortly after it is created, simulating a fast worker.
	go func() {
		time.Sleep(50 * time.Millisecond)
		turns, err := st.RecentTurns(context.Background(), 1)
		if err != nil || len(turns) == 0 {
			return
		}
		_ = st.FinishTurn(context.Background(), turns[0].ID, core.TurnResult{
			TurnID: turns[0].ID, Status: core.TurnDone, Text: "all done", CostUSD: 0.02,
		})
	}()

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests?wait=true", testToken, "", submitBody{Text: "quick job"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decode[submitResponse](t, resp)
	if body.Result != "all done" {
		t.Errorf("result = %q, want %q", body.Result, "all done")
	}
	if body.Status != string(core.TurnDone) {
		t.Errorf("status = %q, want %q", body.Status, core.TurnDone)
	}
}

// Turn IDs are only unique within an agent, so the route has to be
// agent-scoped. This pins that shape.
func TestTurnLookupIsAgentScoped(t *testing.T) {
	srv, _, st := newHarness(t)

	created := decode[submitResponse](t,
		post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "hello"}))
	_ = st.FinishTurn(context.Background(), created.TurnID, core.TurnResult{
		TurnID: created.TurnID, Status: core.TurnDone, Text: "answer",
	})

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/v1/agents/pr-reviewer/turns/"+itoa(created.TurnID), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decode[turnResponse](t, resp)
	if body.Result != "answer" || body.AgentID != "pr-reviewer" {
		t.Errorf("turn response = %+v", body)
	}
}

// Status must answer even though the workflow query fails, because it is the
// path a user reaches for when things are broken.
func TestStatusSurvivesUnreachableWorkflow(t *testing.T) {
	srv, _, _ := newHarness(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/agents/pr-reviewer", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite the workflow query failing", resp.StatusCode)
	}
	report := decode[StatusReport](t, resp)
	if report.AgentID != "pr-reviewer" {
		t.Errorf("agent_id = %q", report.AgentID)
	}
	if report.State != core.AgentIdle {
		t.Errorf("state = %q, want idle", report.State)
	}
	if report.QueueLength != 0 {
		t.Errorf("queue_length = %d, want 0 when the query fails", report.QueueLength)
	}
}

func TestSSEStreamIsCappedPerAgent(t *testing.T) {
	srv, _, st := newHarness(t)

	created := decode[submitResponse](t,
		post(t, srv, "/v1/agents/pr-reviewer/requests", testToken, "", submitBody{Text: "stream me"}))
	if err := st.AppendLog(context.Background(), created.TurnID, core.LogText, "working"); err != nil {
		t.Fatalf("append log: %v", err)
	}

	url := srv.URL + "/v1/agents/pr-reviewer/turns/" + itoa(created.TurnID) + "/stream"

	// max_sse_per_agent is 2 in the harness config; the third must be refused
	// rather than quietly consuming another file descriptor.
	var open []*http.Response
	defer func() {
		for _, r := range open {
			r.Body.Close()
		}
	}()

	for i := range 2 {
		resp := openStream(t, srv, url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: status = %d, want 200", i, resp.StatusCode)
		}
		open = append(open, resp)
	}

	resp := openStream(t, srv, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("third stream: status = %d, want 429", resp.StatusCode)
	}
}

func openStream(t *testing.T, srv *httptest.Server, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// Read the first chunk so the handler is definitely inside the stream loop
	// and holding its slot.
	if resp.StatusCode == http.StatusOK {
		buf := make([]byte, 1)
		go func() { _, _ = resp.Body.Read(buf) }()
		time.Sleep(20 * time.Millisecond)
	}
	return resp
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
