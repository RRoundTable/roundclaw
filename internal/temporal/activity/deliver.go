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
	"go.temporal.io/sdk/client"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/temporal/contract"
)

// DiscordMaxMessage is Discord's hard per-message character limit.
const DiscordMaxMessage = core.DiscordMaxMessage

// DeliverInput is one response delivery.
type DeliverInput struct {
	AgentID string `json:"agent_id"`
	// Conversation is the conversation the finished turn ran in — the sender's
	// side, not the destination's. It is what a delegator is given as the handle
	// to continue in, so a follow-up resumes the session that did the work
	// instead of briefing a fresh one.
	Conversation string          `json:"conversation,omitempty"`
	Origin       core.Origin     `json:"origin"`
	Result       core.TurnResult `json:"result"`
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

// SlackSender posts a message to a Slack channel, in a thread when threadTS is
// set. Same rule as DiscordSender for the same reason: the gateway owns the
// single Socket Mode connection and the worker speaks only over the Web API,
// because a second live connection would double-deliver every inbound event.
//
// Stated in roundclaw's own terms rather than slack-go's so this package does
// not depend on that library's option builders — the caller adapts.
type SlackSender interface {
	PostMessage(ctx context.Context, channelID, threadTS, text string) error
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

	case core.OriginSlack:
		return a.deliverSlack(ctx, in)

	case core.OriginHTTPCallback:
		return a.deliverCallback(ctx, in)

	case core.OriginAgent:
		return a.deliverToAgent(ctx, in)

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

	for _, chunk := range chunkForDiscord(deliveredText(in.Result)) {
		if _, err := a.discord.ChannelMessageSend(in.Origin.ChannelID, chunk); err != nil {
			return fmt.Errorf("send to discord channel %s: %w", in.Origin.ChannelID, err)
		}
	}
	return nil
}

func (a *Activities) deliverSlack(ctx context.Context, in DeliverInput) error {
	if a.slack == nil {
		return newNonRetryable(fmt.Errorf("slack delivery requested but no slack client is configured"))
	}

	// Every chunk goes to the same thread, not just the first. Posting the rest
	// bare would drop the tail of a long answer into the channel, out of the
	// conversation it belongs to.
	for _, chunk := range core.ChunkMessage(deliveredText(in.Result), core.SlackMaxMessage) {
		if err := a.slack.PostMessage(ctx, in.Origin.ChannelID, in.Origin.MessageID, chunk); err != nil {
			return fmt.Errorf("send to slack channel %s: %w", in.Origin.ChannelID, err)
		}
	}
	return nil
}

// deliveredText is what a chat channel shows for a finished turn: the agent's
// answer, or the reason there is none. Shared so the two chat adapters cannot
// disagree about what a failed turn looks like.
func deliveredText(r core.TurnResult) string {
	if r.Status == core.TurnError {
		return "⚠️ " + r.ErrorMessage
	}
	if r.Text == "" {
		return "(no output)"
	}
	return r.Text
}

// deliverToAgent hands a finished turn's result to the agent that delegated it,
// as an ordinary request in that agent's conversation.
//
// This is what makes "I'll tell you when it's done" keepable. The delegator does
// not have to stay alive waiting: the return address is on the delegated turn's
// row, so the result finds its way back even if the delegating process, its HTTP
// connection and the worker have all died in between.
//
// It deliberately queues a turn rather than posting the result to the channel
// itself. The delegator knows what the human asked and can answer in its own
// words; a raw dump of another agent's output is not an answer.
//
// The row write and the signal live in one activity so that a single idempotency
// key covers both: DeliverResponse is retried, and without it a retry would wake
// the delegator a second time with the same result.
func (a *Activities) deliverToAgent(ctx context.Context, in DeliverInput) error {
	parent, conversation := in.Origin.Agent, in.Origin.Conversation
	// Deterministic on purpose: the delegated turn can only finish once, so
	// (agent, turn) is a stable name for this notification however many times
	// the activity is retried.
	key := fmt.Sprintf("notify:%s:%d", in.AgentID, in.Result.TurnID)
	return a.wakeAgent(ctx, parent, conversation, key, notifyPrompt(in))
}

// wakeAgent queues text as a new turn for an agent and starts its workflow.
//
// This is what makes "I'll tell you when it's done" keepable, and it is shared
// by everything that finishes long after the turn that asked for it — a
// delegated request, an eval run. The caller does not have to stay alive: the
// address is on the row, so the result finds its way back even if the requester,
// its container and the worker have all died in between.
//
// idempotencyKey must name the event rather than the moment. The row write and
// the signal are one activity so that a single key covers both; without it a
// retry would wake the agent a second time with the same news.
func (a *Activities) wakeAgent(ctx context.Context, agentID, conversation, idempotencyKey, prompt string) error {
	log := activity.GetLogger(ctx)

	// An agent that has been deleted cannot be woken, and no retry will bring it
	// back. The result stays recorded either way.
	agent, err := a.reg.Get(ctx, agentID)
	if err != nil {
		return newNonRetryable(fmt.Errorf("notify %s: %w", agentID, err))
	}
	if !agent.Enabled {
		log.Info("agent is disabled; not waking it", "agent", agentID, "key", idempotencyKey)
		return nil
	}

	st, err := a.stores.Get(agentID)
	if err != nil {
		return fmt.Errorf("open %s store: %w", agentID, err)
	}
	replyTo, err := a.replyOriginFor(ctx, st, conversation)
	if err != nil {
		return err
	}

	turnID, existed, err := st.CreateTurn(ctx, store.NewTurn{
		Request: prompt, Origin: replyTo,
		Conversation: conversation, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return fmt.Errorf("queue notification for %s: %w", agentID, err)
	}
	if existed {
		log.Info("this notification was already delivered", "agent", agentID, "key", idempotencyKey, "turn_id", turnID)
		return nil
	}

	req := core.Request{
		AgentID:        agentID,
		ConversationID: conversation,
		RequestID:      idempotencyKey,
		TurnID:         turnID,
		Text:           prompt,
		Origin:         replyTo,
		ReceivedAt:     time.Now().UTC(),
	}

	workflowID := contract.WorkflowID(agentID, conversation)
	_, err = a.signaller().SignalWithStartWorkflow(ctx, workflowID, contract.SignalEnqueue, req,
		client.StartWorkflowOptions{ID: workflowID, TaskQueue: a.cfg.Temporal.TaskQueue},
		// By name, not by function reference: importing the package that defines
		// the workflow would close an import cycle.
		contract.AgentWorkflowType, contract.AgentInput{AgentID: agentID, ConversationID: conversation})
	if err != nil {
		return fmt.Errorf("wake %s: %w", agentID, err)
	}

	log.Info("woke an agent with a result it was waiting on",
		"agent", agentID, "conversation", conversation, "key", idempotencyKey, "turn_id", turnID)
	return nil
}

// replyOriginFor is where the woken agent's own answer should go: wherever that
// conversation last answered.
//
// A conversation has exactly one audience — the chat thread it lives in, in
// whichever tool that is — so the last human-facing turn is the authoritative
// address, and reading it here keeps a second copy of it out of the delegated
// turn's origin.
//
// A conversation whose only turns are notifications has no audience to answer
// to; recording the result is then all that can be done, which is what
// http_poll means.
func (a *Activities) replyOriginFor(ctx context.Context, st *store.Store, conversation string) (core.Origin, error) {
	turns, err := st.RecentTurnsIn(ctx, conversation, replyOriginLookback)
	if err != nil {
		return core.Origin{}, fmt.Errorf("read conversation %q: %w", conversation, err)
	}
	for _, t := range turns {
		switch t.Origin.Type {
		case core.OriginDiscord, core.OriginSlack, core.OriginHTTPCallback:
			return t.Origin, nil
		}
	}
	return core.HTTPPollOrigin(), nil
}

// replyOriginLookback bounds how far back the reply address is searched. A
// conversation can accumulate several notification turns in a row — one per
// delegated task — and the human-facing turn that started it all sits behind
// them.
const replyOriginLookback = 20

// notifyPrompt is what the delegator reads when it wakes. It says who finished,
// what it cost, and what to do now, and it hands over the conversation handle so
// a follow-up continues in the same session rather than briefing a fresh one.
func notifyPrompt(in DeliverInput) string {
	var b strings.Builder
	status := "완료"
	if in.Result.Status == core.TurnError {
		status = "실패"
	} else if in.Result.Status == core.TurnStopped {
		status = "중단됨"
	}

	fmt.Fprintf(&b, "[위임 %s] 에이전트 %s · turn %d · $%.4f\n",
		status, in.AgentID, in.Result.TurnID, in.Result.CostUSD)
	if in.Conversation != "" {
		fmt.Fprintf(&b, "이어서 시키려면: roundclaw send %s --conversation %s \"...\"\n",
			in.AgentID, in.Conversation)
	} else {
		fmt.Fprintf(&b, "이어서 시키려면: roundclaw send %s \"...\" (기본 대화)\n", in.AgentID)
	}
	b.WriteString("\n")

	if in.Result.ErrorMessage != "" {
		fmt.Fprintf(&b, "오류:\n%s\n\n", in.Result.ErrorMessage)
	}
	if in.Result.Text != "" {
		fmt.Fprintf(&b, "결과:\n%s\n\n", in.Result.Text)
	}
	b.WriteString("---\n" +
		"이 결과를 요청한 사람에게 당신의 말로 보고하세요. " +
		"추가 작업이 필요하면 지금 하거나 위 핸들로 이어서 위임하세요.")
	return b.String()
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

// chunkForDiscord splits text into messages Discord will accept.
//
// Shared with the gateway, which also sends messages: the limit counts
// characters rather than bytes, and a cut has to land on a rune boundary or a
// Korean reply carries a broken character at every seam.
func chunkForDiscord(s string) []string {
	return core.ChunkMessage(s, DiscordMaxMessage)
}
