package activity

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.temporal.io/sdk/activity"

	"github.com/roundtable/roundclaw/internal/core"
)

// DiscordMaxMessage is Discord's hard per-message character limit.
const DiscordMaxMessage = 2000

// DeliverInput is one response delivery.
type DeliverInput struct {
	AgentID string          `json:"agent_id"`
	Origin  core.Origin     `json:"origin"`
	Result  core.TurnResult `json:"result"`
	// SuppressIf stops delivery when the result contains it. The turn stays
	// recorded either way — this hides a message, it does not hide a run.
	SuppressIf string `json:"suppress_if,omitempty"`
}

// DiscordSender sends a message to a channel. The worker uses discordgo's REST
// API only — it never opens a gateway websocket, because the gateway process
// owns the single live connection and a second one would double-deliver every
// inbound event.
type DiscordSender interface {
	ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// DeliverResponse routes a finished turn back to wherever it came from.
//
// The result is already durable in SQLite before this runs, so delivery is a
// convenience rather than the system of record. That is deliberate: a deleted
// Discord channel or a dead callback host must not be able to fail an agent.
func (a *Activities) DeliverResponse(ctx context.Context, in DeliverInput) error {
	if in.SuppressIf != "" && strings.Contains(in.Result.Text, in.SuppressIf) {
		activity.GetLogger(ctx).Info("suppressed a scheduled result",
			"agent", in.AgentID, "turn_id", in.Result.TurnID, "match", in.SuppressIf)
		return nil
	}

	switch in.Origin.Type {
	case core.OriginHTTPPoll:
		// Nothing to push. The client collects the result via
		// GET /v1/turns/{id} or the SSE stream.
		return nil

	case core.OriginDiscord:
		return a.deliverDiscord(in)

	case core.OriginHTTPCallback:
		return a.deliverCallback(ctx, in)

	default:
		// Unknown origin types are dropped rather than retried forever: the
		// result is safe in SQLite and no retry will teach this binary a type
		// it does not implement.
		return newNonRetryable(fmt.Errorf("cannot deliver to unknown origin type %q", in.Origin.Type))
	}
}

func (a *Activities) deliverDiscord(in DeliverInput) error {
	if a.discord == nil {
		return newNonRetryable(fmt.Errorf("discord delivery requested but no discord client is configured"))
	}

	text := in.Result.Text
	if in.Result.Status == core.TurnError {
		text = "⚠️ " + in.Result.ErrorMessage
	}
	if text == "" {
		text = "(no output)"
	}

	for _, chunk := range chunkForDiscord(text) {
		if _, err := a.discord.ChannelMessageSend(in.Origin.ChannelID, chunk); err != nil {
			return fmt.Errorf("send to discord channel %s: %w", in.Origin.ChannelID, err)
		}
	}
	return nil
}

// callbackPayload is the JSON body POSTed to a callback URL.
type callbackPayload struct {
	AgentID   string  `json:"agent_id"`
	TurnID    int64   `json:"turn_id"`
	Status    string  `json:"status"`
	Text      string  `json:"text,omitempty"`
	Error     string  `json:"error,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	Timestamp string  `json:"timestamp"`
}

func (a *Activities) deliverCallback(ctx context.Context, in DeliverInput) error {
	// Re-validated at delivery time, not just at admission: DNS can be
	// re-pointed at a private address between the two.
	if err := core.AssertPublicCallbackHost(in.Origin.URL); err != nil {
		return newNonRetryable(err)
	}

	body, err := json.Marshal(callbackPayload{
		AgentID:   in.AgentID,
		TurnID:    in.Result.TurnID,
		Status:    string(in.Result.Status),
		Text:      in.Result.Text,
		Error:     in.Result.ErrorMessage,
		CostUSD:   in.Result.CostUSD,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return newNonRetryable(fmt.Errorf("encode callback payload: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.Origin.URL, bytes.NewReader(body))
	if err != nil {
		return newNonRetryable(fmt.Errorf("build callback request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "roundclaw/1")

	if secret := os.Getenv(a.cfg.HTTP.CallbackSecretEnv); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Roundclaw-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := callbackClient.Do(req)
	if err != nil {
		return fmt.Errorf("post callback: %w", err)
	}
	defer resp.Body.Close()

	// 4xx will not improve on retry; 5xx might.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return newNonRetryable(fmt.Errorf("callback rejected with %s", resp.Status))
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("callback failed with %s", resp.Status)
	}
	return nil
}

// callbackClient refuses redirects so a public URL cannot bounce the request
// into the internal network after the host check has already passed.
var callbackClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// chunkForDiscord splits text into messages Discord will accept, preferring
// line boundaries so code blocks and lists survive the split.
func chunkForDiscord(s string) []string {
	if len(s) <= DiscordMaxMessage {
		return []string{s}
	}

	var chunks []string
	for len(s) > DiscordMaxMessage {
		cut := bytes.LastIndexByte([]byte(s[:DiscordMaxMessage]), '\n')
		if cut <= 0 {
			cut = DiscordMaxMessage
		}
		chunks = append(chunks, s[:cut])
		s = s[cut:]
		for len(s) > 0 && s[0] == '\n' {
			s = s[1:]
		}
	}
	if s != "" {
		chunks = append(chunks, s)
	}
	return chunks
}
