package adapter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/roundtable/roundclaw/internal/config"
	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/store"
)

// twoToolHarness gives pm two conversations: one heard in Discord, one in
// Slack. Which sender a message reaches is decided against these.
func twoToolHarness(t *testing.T, connect ...core.OriginType) (*httptest.Server, map[core.OriginType]*fakeSender) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundclaw.yaml")
	body := "workspace_root: ws\ncontainer:\n  image: test\nhttp:\n  wait_timeout: 300ms\n  max_sse_per_agent: 2\nagents:\n  - id: pm\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
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
	if _, err := reg.Seed(context.Background(), []registry.Agent{{ID: "pm"}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	pm, err := stores.Get("pm")
	if err != nil {
		t.Fatalf("open pm store: %v", err)
	}
	for _, seed := range []struct {
		conversation string
		origin       core.Origin
	}{
		{"discord-thread", core.DiscordOrigin("chan-1", "msg-1")},
		{"1712345678-000100", core.SlackOrigin("C0123ABCD", "1712345678.000100")},
	} {
		if _, _, err := pm.CreateTurn(context.Background(), store.NewTurn{
			Request: "시작", Origin: seed.origin, Conversation: seed.conversation,
		}); err != nil {
			t.Fatalf("seed %s: %v", seed.conversation, err)
		}
	}

	disp := NewDispatcher(cfg, &fakeTemporal{}, stores, reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := NewHTTP(disp, log, []string{testToken}, nil, cfg.HTTP.WaitTimeout, cfg.HTTP.MaxSSEPerAgent)

	senders := map[core.OriginType]*fakeSender{}
	for _, tool := range connect {
		s := &fakeSender{}
		senders[tool] = s
		api.SetMessageSender(tool, s)
	}

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, senders
}

// An answer returns to the tool that asked, and to no other. With both
// connected, the routing is what decides — not whichever sender was installed
// first.
func TestSayReachesTheToolTheConversationLivesIn(t *testing.T) {
	srv, senders := twoToolHarness(t, core.OriginDiscord, core.OriginSlack)

	resp := post(t, srv, "/v1/agents/pm/messages", testToken, "", map[string]any{
		"text": "빌드 중", "conversation": "1712345678-000100",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}

	if got := senders[core.OriginSlack].messages(); len(got) != 1 || got[0] != "C0123ABCD: 빌드 중" {
		t.Errorf("slack got %v, want the message", got)
	}
	if got := senders[core.OriginDiscord].messages(); len(got) != 0 {
		t.Errorf("discord also heard it: %v", got)
	}
}

func TestSayReachesDiscordForADiscordConversation(t *testing.T) {
	srv, senders := twoToolHarness(t, core.OriginDiscord, core.OriginSlack)

	resp := post(t, srv, "/v1/agents/pm/messages", testToken, "", map[string]any{
		"text": "빌드 중", "conversation": "discord-thread",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if got := senders[core.OriginDiscord].messages(); len(got) != 1 {
		t.Errorf("discord got %v, want the message", got)
	}
	if got := senders[core.OriginSlack].messages(); len(got) != 0 {
		t.Errorf("slack also heard it: %v", got)
	}
}

// Running with one chat tool and not the other is supported, so the endpoint
// has to say which connection is missing rather than pretend it delivered.
func TestSayReportsTheMissingConnectionForThatTool(t *testing.T) {
	srv, senders := twoToolHarness(t, core.OriginDiscord)

	resp := post(t, srv, "/v1/agents/pm/messages", testToken, "", map[string]any{
		"text": "빌드 중", "conversation": "1712345678-000100",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %s, want 503", resp.Status)
	}
	if got := senders[core.OriginDiscord].messages(); len(got) != 0 {
		t.Errorf("it fell back to the other tool: %v", got)
	}
}

// Discord being unconfigured must not stop a Slack conversation being spoken
// into. The check is per-tool, not "is anything connected".
func TestSayWorksWithOnlySlackConnected(t *testing.T) {
	srv, senders := twoToolHarness(t, core.OriginSlack)

	resp := post(t, srv, "/v1/agents/pm/messages", testToken, "", map[string]any{
		"text": "빌드 중", "conversation": "1712345678-000100",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if got := senders[core.OriginSlack].messages(); len(got) != 1 {
		t.Errorf("slack got %v, want the message", got)
	}
}
