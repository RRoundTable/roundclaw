package adapter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const webhookSecret = "webhook-shared-secret"

func sign(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, srv *httptest.Server, agent, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/"+agent, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The signature is the authentication: an unsigned or wrongly signed payload
// must not be able to queue billable work.
func TestWebhookRejectsBadSignatures(t *testing.T) {
	t.Setenv("ROUNDCLAW_WEBHOOK_SECRET", webhookSecret)
	srv, tc, _ := newHarness(t)
	body := `{"action":"opened"}`

	cases := map[string]map[string]string{
		"no signature":    {},
		"wrong secret":    {"X-Roundclaw-Signature": sign(body, "not-the-secret")},
		"wrong body":      {"X-Roundclaw-Signature": sign(`{"action":"closed"}`, webhookSecret)},
		"malformed value": {"X-Roundclaw-Signature": "sha256=zzzz"},
	}
	for name, headers := range cases {
		resp := postWebhook(t, srv, "pr-reviewer", body, headers)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
	if got := tc.sent(); len(got) != 0 {
		t.Errorf("an unverified webhook still queued work: %v", got)
	}
}

func TestWebhookAcceptsAValidSignature(t *testing.T) {
	t.Setenv("ROUNDCLAW_WEBHOOK_SECRET", webhookSecret)
	srv, tc, st := newHarness(t)
	body := `{"action":"opened","number":42}`

	resp := postWebhook(t, srv, "pr-reviewer", body, map[string]string{
		"X-Roundclaw-Signature": sign(body, webhookSecret),
		"X-Roundclaw-Event":     "pull_request",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := tc.sent(); len(got) != 1 || got[0] != "enqueue" {
		t.Errorf("signals = %v, want one enqueue", got)
	}

	turns, err := st.RecentTurns(t.Context(), 1)
	if err != nil || len(turns) != 1 {
		t.Fatalf("recent turns: %v (%d)", err, len(turns))
	}
	// The payload reaches the agent intact — roundclaw has no idea what a given
	// sender's schema means, so summarising would drop the field that mattered.
	for _, want := range []string{"pull_request", `"number": 42`, `"action": "opened"`} {
		if !strings.Contains(turns[0].Request, want) {
			t.Errorf("prompt is missing %q:\n%s", want, turns[0].Request)
		}
	}
}

// GitHub's own header names work, so its webhooks need no proxy in front.
func TestWebhookAcceptsGitHubHeaders(t *testing.T) {
	t.Setenv("ROUNDCLAW_WEBHOOK_SECRET", webhookSecret)
	srv, _, _ := newHarness(t)
	body := `{"zen":"Design for failure."}`

	resp := postWebhook(t, srv, "pr-reviewer", body, map[string]string{
		"X-Hub-Signature-256": sign(body, webhookSecret),
		"X-GitHub-Event":      "ping",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

// Senders redeliver on a timeout. The delivery ID must make that land on the
// original turn rather than running the work twice.
func TestWebhookRedeliveryIsIdempotent(t *testing.T) {
	t.Setenv("ROUNDCLAW_WEBHOOK_SECRET", webhookSecret)
	srv, tc, st := newHarness(t)
	body := `{"action":"opened"}`
	headers := map[string]string{
		"X-Roundclaw-Signature": sign(body, webhookSecret),
		"X-GitHub-Delivery":     "delivery-abc",
	}

	for range 3 {
		if resp := postWebhook(t, srv, "pr-reviewer", body, headers); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
	}
	if got := tc.sent(); len(got) != 1 {
		t.Errorf("sent %d signals, want 1 — a redelivery ran the work again", len(got))
	}
	turns, _ := st.RecentTurns(t.Context(), 10)
	if len(turns) != 1 {
		t.Errorf("created %d turns, want 1", len(turns))
	}
}

// With no secret configured every webhook is refused: the alternative is an
// open door to billable work.
func TestWebhookRefusedWithoutASecret(t *testing.T) {
	t.Setenv("ROUNDCLAW_WEBHOOK_SECRET", "")
	srv, _, _ := newHarness(t)

	resp := postWebhook(t, srv, "pr-reviewer", `{}`, map[string]string{
		"X-Roundclaw-Signature": sign(`{}`, webhookSecret),
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// The bearer-authenticated routes must still require a token; mounting webhooks
// outside that middleware must not have opened everything else up.
func TestWebhookMountDoesNotBypassBearerAuth(t *testing.T) {
	t.Setenv("ROUNDCLAW_WEBHOOK_SECRET", webhookSecret)
	srv, _, _ := newHarness(t)

	resp := post(t, srv, "/v1/agents/pr-reviewer/requests", "", "", submitBody{Text: "hi"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the authenticated routes were left open", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/agents", nil)
	r2, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/agents without a token = %d, want 401", r2.StatusCode)
	}
}
