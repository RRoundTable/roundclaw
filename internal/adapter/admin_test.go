package adapter

import (
	"strings"
	"testing"

	"github.com/roundtable/roundclaw/internal/claude"
	"github.com/roundtable/roundclaw/internal/config"
)

func adminDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	return NewDispatcher(&config.Config{}, &fakeTemporal{}, newStores(t), testRegistry(t))
}

func TestExecuteAdminCreatesAgent(t *testing.T) {
	d := adminDispatcher(t)
	mention := false
	msg, err := d.ExecuteAdmin(t.Context(), claude.AdminAction{
		Action: claude.AdminCreateAgent,
		Agent:  &claude.AdminAgentSpec{ID: "helper", Description: "Helps", RequireMention: &mention},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(msg, "helper") {
		t.Errorf("result = %q, want it to name the created agent", msg)
	}
	got, err := d.reg.Get(t.Context(), "helper")
	if err != nil {
		t.Fatalf("the agent was not actually created: %v", err)
	}
	// Defaults: an unspecified reply_in_thread should default on; require_mention
	// was explicitly false.
	if got.RequireMention {
		t.Error("require_mention should honour the explicit false")
	}
	if !got.ReplyInThread {
		t.Error("reply_in_thread should default to true")
	}
	if got.PermissionMode != "acceptEdits" {
		t.Errorf("permission_mode = %q, want the acceptEdits default", got.PermissionMode)
	}
}

func TestExecuteAdminScheduleRejectsUnknownAgent(t *testing.T) {
	d := adminDispatcher(t)
	msg, err := d.ExecuteAdmin(t.Context(), claude.AdminAction{
		Action: claude.AdminCreateSchedule,
		Schedule: &claude.AdminScheduleSpec{
			ID: "daily", AgentID: "ghost", Cron: "0 9 * * *", Prompt: "report",
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(msg, "ghost") || !strings.Contains(msg, "no agent") {
		t.Errorf("result = %q, want it to reject the unknown agent", msg)
	}
}

func TestExecuteAdminClarify(t *testing.T) {
	d := adminDispatcher(t)
	msg, err := d.ExecuteAdmin(t.Context(), claude.AdminAction{
		Action: claude.AdminClarify, Reason: "Which agent?",
	})
	if err != nil || !strings.Contains(msg, "Which agent?") {
		t.Errorf("clarify = %q, %v; want the reason surfaced", msg, err)
	}
}
