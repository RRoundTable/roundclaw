package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/roundtable/roundclaw/internal/core"
)

// This is the only way an agent speaks outside its own turn.
//
// Every other outbound path is the tail of a turn: the workflow finishes, delivery
// runs once, and that is the agent's single opportunity to say anything. That is
// fine for an answer and useless for "this will take 30 minutes" — by the time the
// turn ends the wait is over. A long delegated run is therefore silent from the
// outside no matter how much is happening inside it.
//
// This endpoint posts a message into a conversation without creating a turn: no
// session, no container, no LLM call, nothing to schedule. It is deliberately
// cheap, and deliberately unreliable in the sense that matters — nothing retries
// it and nothing remembers it. A final result must never travel this way; that is
// what a notify origin is for (see core.OriginAgent). Progress, findings and
// "still working" belong here.

// messageBody is one out-of-band message.
type messageBody struct {
	Text string `json:"text"`
	// Conversation names which of the agent's conversations to speak into. Empty
	// is the default conversation. The target is not a channel ID on purpose: an
	// agent may only be heard where it is already spoken to, so an injected
	// prompt cannot turn one agent into a fleet-wide megaphone.
	Conversation string `json:"conversation,omitempty"`
}

type messageResponse struct {
	AgentID      string `json:"agent_id"`
	Conversation string `json:"conversation,omitempty"`
	Delivered    bool   `json:"delivered"`
	// Target is where it went, for a caller that wants to log it.
	Target string `json:"target,omitempty"`
}

// MessageSender posts a message to a Discord channel. The gateway already owns a
// session for receiving, so speaking costs no new connection.
type MessageSender interface {
	ChannelMessageSend(channelID, text string) error
}

// SetMessageSender installs the sender used by POST /v1/agents/{id}/messages.
// Left unset, the endpoint reports 503 rather than pretending to deliver: an
// agent that believes it announced progress and did not is worse than one that
// knows it cannot.
func (h *HTTP) SetMessageSender(s MessageSender) { h.sender = s }

func (h *HTTP) postMessage(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")

	var body messageBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBytes))
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error())
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, "text is empty")
		return
	}

	if _, err := h.disp.requireAgent(r.Context(), agentID); err != nil {
		h.writeLookupError(w, err)
		return
	}
	if h.sender == nil {
		writeError(w, http.StatusServiceUnavailable,
			"no discord connection is configured, so there is nowhere to speak")
		return
	}

	target, err := h.conversationChannel(r.Context(), agentID, body.Conversation)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Chunked the same way a turn's result is: Discord rejects anything over its
	// per-message limit, and a progress note can still carry a diff.
	for _, part := range chunk(body.Text, 1990) {
		if err := h.sender.ChannelMessageSend(target, part); err != nil {
			writeError(w, http.StatusBadGateway, "send failed: "+err.Error())
			return
		}
	}

	h.log.Info("agent spoke out of band",
		"agent", agentID, "conversation", body.Conversation, "channel", target, "bytes", len(body.Text))
	writeJSON(w, http.StatusOK, messageResponse{
		AgentID: agentID, Conversation: body.Conversation, Delivered: true, Target: target,
	})
}

// conversationChannel resolves which channel a conversation is heard in, by
// reading the address its own turns answered to.
//
// This is the whole authorisation model for speaking: the answer comes from the
// conversation's history, so an agent can only reach an audience it already has.
// There is no way to name an arbitrary channel, which is what keeps a
// prompt-injected agent from broadcasting.
func (h *HTTP) conversationChannel(ctx context.Context, agentID, conversation string) (string, error) {
	st, err := h.disp.Store(ctx, agentID)
	if err != nil {
		return "", err
	}
	turns, err := st.RecentTurnsIn(ctx, conversation, messageLookback)
	if err != nil {
		return "", err
	}
	for _, t := range turns {
		if t.Origin.Type == core.OriginDiscord && t.Origin.ChannelID != "" {
			return t.Origin.ChannelID, nil
		}
	}
	if len(turns) == 0 {
		return "", fmt.Errorf("agent %s has no conversation %q to speak into", agentID, conversation)
	}
	return "", fmt.Errorf(
		"conversation %q of agent %s has no discord audience; it was driven by the API, "+
			"so its caller reads results rather than being told them", conversation, agentID)
}

// messageLookback bounds the search for the conversation's audience. A run of
// notification turns can sit between now and the last human-facing turn.
const messageLookback = 20

// maxMessageBytes caps an out-of-band message. Generous next to Discord's 2000
// characters per message, small enough that this endpoint cannot be used to move
// bulk data into a channel.
const maxMessageBytes = 64 << 10
