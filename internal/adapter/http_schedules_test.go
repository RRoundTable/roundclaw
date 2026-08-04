package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.temporal.io/sdk/client"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// fakeSchedules stands in for Temporal's schedule API, remembering enough to
// answer Describe so the joined view is exercised too.
type fakeSchedules struct {
	mu      sync.Mutex
	created map[string]bool
	paused  map[string]bool
}

func newFakeSchedules() *fakeSchedules {
	return &fakeSchedules{created: map[string]bool{}, paused: map[string]bool{}}
}

func (f *fakeSchedules) Create(_ context.Context, opts client.ScheduleOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[opts.ID] = true
	f.paused[opts.ID] = opts.Paused
	return nil
}

func (f *fakeSchedules) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.created, id)
	delete(f.paused, id)
	return nil
}

func (f *fakeSchedules) Pause(_ context.Context, id, _ string) error   { return f.setPaused(id, true) }
func (f *fakeSchedules) Unpause(_ context.Context, id, _ string) error { return f.setPaused(id, false) }

func (f *fakeSchedules) setPaused(id string, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.created[id] {
		return fmt.Errorf("no schedule %s", id)
	}
	f.paused[id] = paused
	return nil
}

func (f *fakeSchedules) Describe(_ context.Context, id string) (*client.ScheduleDescription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.created[id] {
		return nil, fmt.Errorf("no schedule %s", id)
	}
	return &client.ScheduleDescription{
		Schedule: client.Schedule{State: &client.ScheduleState{Paused: f.paused[id]}},
	}, nil
}

// newScheduleHarness serves two agents: dev, bound to a channel, and ops, whose
// schedules dev must not be able to touch.
func newScheduleHarness(t *testing.T) (*httptest.Server, *fakeSchedules) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	if err := os.WriteFile(configPath, []byte(`
workspace_root: ws
container:
  image: roundclaw/claude:test
http:
  wait_timeout: 300ms
  max_sse_per_agent: 2
agents:
  - id: dev
    discord_channels: ["chan-dev"]
  - id: ops
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	stores := store.NewRegistry(store.ReadWrite, cfg.DBPath)
	t.Cleanup(func() { stores.Close() })

	reg, err := registry.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Seed(context.Background(), []registry.Agent{
		{ID: "dev", DiscordChannels: []string{"chan-dev"}},
		{ID: "ops"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sched := newFakeSchedules()
	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	disp.SetSchedules(sched)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := NewHTTP(disp, log, []string{testToken}, []string{delegateToken},
		cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, sched
}

// scheduleReq sends a request with the given token and returns the response.
func scheduleReq(t *testing.T, srv *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The point of the whole feature: the token an agent container carries is enough
// to define, read and drop its own recurring work.
func TestAgentManagesItsOwnScheduleWithDelegateToken(t *testing.T) {
	srv, sched := newScheduleHarness(t)

	resp := scheduleReq(t, srv, http.MethodPut, "/v1/agents/dev/schedules/standup", delegateToken,
		map[string]any{"cron": "0 9 * * *", "prompt": "summarise yesterday", "timezone": "Asia/Seoul"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT own schedule = %d, want 200", resp.StatusCode)
	}
	view := decode[ScheduleView](t, resp)
	if view.AgentID != "dev" {
		t.Errorf("agent_id = %q, want dev (taken from the path)", view.AgentID)
	}
	if !sched.created["roundclaw-schedule-standup"] {
		t.Error("no temporal schedule was created")
	}

	resp = scheduleReq(t, srv, http.MethodGet, "/v1/agents/dev/schedules", delegateToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET own schedules = %d, want 200", resp.StatusCode)
	}
	list := decode[struct {
		Schedules []ScheduleView `json:"schedules"`
	}](t, resp)
	if len(list.Schedules) != 1 || list.Schedules[0].ID != "standup" {
		t.Fatalf("listed %+v, want just standup", list.Schedules)
	}

	resp = scheduleReq(t, srv, http.MethodPost, "/v1/agents/dev/schedules/standup/pause", delegateToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d, want 200", resp.StatusCode)
	}
	if !sched.paused["roundclaw-schedule-standup"] {
		t.Error("pause did not reach the trigger")
	}

	resp = scheduleReq(t, srv, http.MethodDelete, "/v1/agents/dev/schedules/standup", delegateToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", resp.StatusCode)
	}
	if sched.created["roundclaw-schedule-standup"] {
		t.Error("trigger outlived the definition")
	}
}

// The body naming another agent must not move the work: the path is the only
// thing that decides who runs it.
func TestScheduleBodyCannotReassignTheAgent(t *testing.T) {
	srv, _ := newScheduleHarness(t)

	resp := scheduleReq(t, srv, http.MethodPut, "/v1/agents/dev/schedules/nightly", delegateToken,
		map[string]any{"agent_id": "ops", "cron": "0 3 * * *", "prompt": "run it"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", resp.StatusCode)
	}
	if got := decode[ScheduleView](t, resp).AgentID; got != "dev" {
		t.Errorf("agent_id = %q, want dev — the body must not hand work to another agent", got)
	}
}

// Schedule ids are unique fleet-wide, so one agent could otherwise replace
// another's recurring work by guessing its name.
func TestScheduleOfAnotherAgentIsInvisibleAndUntouchable(t *testing.T) {
	srv, _ := newScheduleHarness(t)

	resp := scheduleReq(t, srv, http.MethodPut, "/v1/agents/ops/schedules/backup", testToken,
		map[string]any{"cron": "0 2 * * *", "prompt": "back up the database"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed ops schedule = %d, want 200", resp.StatusCode)
	}

	cases := []struct {
		name, method, path string
		want               int
	}{
		{"read", http.MethodGet, "/v1/agents/dev/schedules/backup", http.StatusNotFound},
		{"delete", http.MethodDelete, "/v1/agents/dev/schedules/backup", http.StatusNotFound},
		{"pause", http.MethodPost, "/v1/agents/dev/schedules/backup/pause", http.StatusNotFound},
	}
	for _, c := range cases {
		resp := scheduleReq(t, srv, c.method, c.path, delegateToken, nil)
		if resp.StatusCode != c.want {
			t.Errorf("%s another agent's schedule = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}

	// Overwriting is a name collision, not a missing schedule: say so.
	resp = scheduleReq(t, srv, http.MethodPut, "/v1/agents/dev/schedules/backup", delegateToken,
		map[string]any{"cron": "* * * * *", "prompt": "mine now"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("overwrite = %d, want 409", resp.StatusCode)
	}

	resp = scheduleReq(t, srv, http.MethodGet, "/v1/agents/ops/schedules/backup", testToken, nil)
	if got := decode[ScheduleView](t, resp).Prompt; got != "back up the database" {
		t.Errorf("ops schedule prompt = %q, want it untouched", got)
	}
}

// A schedule names where its result is announced, and the caller's identity here
// is a claim rather than a proof. Without this check, naming a channel id would
// be enough to post into any channel the bot can see, on a timer.
func TestScheduleChannelMustBelongToTheAgent(t *testing.T) {
	srv, _ := newScheduleHarness(t)

	resp := scheduleReq(t, srv, http.MethodPut, "/v1/agents/dev/schedules/report", delegateToken,
		map[string]any{"cron": "0 9 * * *", "prompt": "report", "channel_id": "chan-somebody-else"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign channel = %d, want 400", resp.StatusCode)
	}

	resp = scheduleReq(t, srv, http.MethodPut, "/v1/agents/dev/schedules/report", delegateToken,
		map[string]any{"cron": "0 9 * * *", "prompt": "report", "channel_id": "chan-dev"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("own channel = %d, want 200", resp.StatusCode)
	}
}

// The fleet-wide routes stay privileged: they take the owner from the body, so
// nothing about them is scoped to the caller.
func TestFleetWideSchedulesStayOffTheDelegateSurface(t *testing.T) {
	srv, _ := newScheduleHarness(t)

	resp := scheduleReq(t, srv, http.MethodPut, "/v1/schedules/anything", delegateToken,
		map[string]any{"agent_id": "ops", "cron": "0 9 * * *", "prompt": "x"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delegate token on /v1/schedules = %d, want 403", resp.StatusCode)
	}
}

// Editing takes effect on the next run, with nothing to recreate — the
// definition is read at fire time, and only the trigger is replaced.
func TestEditingAScheduleKeepsItsIdentity(t *testing.T) {
	srv, sched := newScheduleHarness(t)

	put := func(body map[string]any) ScheduleView {
		t.Helper()
		resp := scheduleReq(t, srv, http.MethodPut, "/v1/agents/dev/schedules/standup", delegateToken, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT = %d, want 200", resp.StatusCode)
		}
		return decode[ScheduleView](t, resp)
	}

	put(map[string]any{"cron": "0 9 * * *", "prompt": "first"})
	resp := scheduleReq(t, srv, http.MethodPost, "/v1/agents/dev/schedules/standup/pause", delegateToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d, want 200", resp.StatusCode)
	}

	// An edit that says nothing about enabled must not resume a paused schedule.
	view := put(map[string]any{"cron": "0 10 * * *", "prompt": "second"})
	if view.Cron != "0 10 * * *" || view.Prompt != "second" {
		t.Errorf("edit did not take: %+v", view)
	}
	if view.Enabled {
		t.Error("editing a paused schedule resumed it")
	}
	if !sched.paused["roundclaw-schedule-standup"] {
		t.Error("the replaced trigger came back unpaused")
	}
}
