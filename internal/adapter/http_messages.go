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
	"github.com/roundtable/roundclaw/internal/store"
	"github.com/roundtable/roundclaw/internal/workspace"
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
	// TurnID names the turn the speaker is running, so its audience is read off
	// that one row instead of inferred from the conversation's recent history.
	//
	// It matters for a delegated turn, where the audience lives on the row and
	// nowhere else: a second delegation arriving while this one works would be
	// the newer row in the same conversation, and searching would find that
	// stranger's thread instead. Zero means "infer", which is what a person
	// running the CLI by hand gets.
	TurnID int64 `json:"turn_id,omitempty"`
	// Files names things the agent wrote into outbox/ in its workspace, to send
	// as attachments. Names are relative to that directory and reach nowhere
	// else; see outbox.go for what that does and does not protect.
	//
	// This is how a large result leaves. Sent as text, a long report is twenty
	// Discord messages and it stays in the session context to be re-sent on every
	// later turn of the conversation; sent as a file it costs one path.
	Files []string `json:"files,omitempty"`
}

type messageResponse struct {
	AgentID      string `json:"agent_id"`
	Conversation string `json:"conversation,omitempty"`
	Delivered    bool   `json:"delivered"`
	// Target is where it went, for a caller that wants to log it.
	Target string `json:"target,omitempty"`
	// Files is how many attachments went with it.
	Files int `json:"files,omitempty"`
}

// MessageSender posts a message to a chat channel. The gateway already owns a
// session for receiving, so speaking costs no new connection.
//
// It takes the whole audience rather than a channel id because a channel id is
// no longer enough to reach a place: it does not say which chat tool, and for
// Slack it does not say which thread. Handing over the address roundclaw
// recorded is also what keeps the authorisation story intact — the sender is
// given somewhere the agent has already been heard, never a name from a request.
type MessageSender interface {
	SendMessage(to core.Origin, text string) error
	// SendFiles posts text with files attached. The sender closes every
	// reader, including when the send fails.
	SendFiles(to core.Origin, text string, files []OutFile) error
}

// SetMessageSender installs the sender for one chat tool, used by
// POST /v1/agents/{id}/messages. With no sender for the tool a conversation
// answers to, the endpoint reports 503 rather than pretending to deliver: an
// agent that believes it announced progress and did not is worse than one that
// knows it cannot.
func (h *HTTP) SetMessageSender(t core.OriginType, s MessageSender) {
	if s == nil {
		return
	}
	if h.senders == nil {
		h.senders = map[core.OriginType]MessageSender{}
	}
	h.senders[t] = s
}

func (h *HTTP) postMessage(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")

	var body messageBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBytes))
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error())
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	// A file needs no words with it — "here is the report" is what the file is —
	// so only a message carrying neither is empty.
	if body.Text == "" && len(body.Files) == 0 {
		writeError(w, http.StatusBadRequest, "text is empty")
		return
	}

	agent, err := h.disp.requireAgent(r.Context(), agentID)
	if err != nil {
		h.writeLookupError(w, err)
		return
	}
	target, err := h.conversationAudience(r.Context(), agentID, body.Conversation, body.TurnID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolved after the audience, not before: which chat tool has to be
	// connected depends on where this conversation is heard, and reporting "no
	// connection" for the tool it does not use would be a lie.
	sender := h.senders[target.Type]
	if sender == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
			"this conversation answers to %s, and no such connection is configured, "+
				"so there is nowhere to speak", target.Type))
		return
	}

	// Resolved against the workspace this conversation actually runs in — the
	// same directory the worker mounts, named by the same function, so an agent
	// that wrote a file in its thread finds it here rather than in some other
	// thread's outbox.
	files, err := openOutbound(workspace.Dir(h.disp.Config(), agent, body.Conversation), body.Files)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Chunked the same way a turn's result is, to the limit of the tool being
	// spoken into rather than a fixed one: a note that fits in one Slack message
	// should not arrive as three because Discord would have needed three.
	// Attachments ride on the last part so a short note and its file arrive as
	// one message.
	parts := chunk(body.Text, messageChunk(target.Type))
	for _, part := range parts[:len(parts)-1] {
		if err := sender.SendMessage(target, part); err != nil {
			closeOutbound(files)
			writeError(w, http.StatusBadGateway, "send failed: "+err.Error())
			return
		}
	}

	last := parts[len(parts)-1]
	if len(files) > 0 {
		// The sender closes the readers either way.
		err = sender.SendFiles(target, last, files)
	} else {
		err = sender.SendMessage(target, last)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "send failed: "+err.Error())
		return
	}

	h.log.Info("agent spoke out of band",
		"agent", agentID, "conversation", body.Conversation, "channel", target.String(),
		"bytes", len(body.Text), "files", len(files))
	writeJSON(w, http.StatusOK, messageResponse{
		AgentID: agentID, Conversation: body.Conversation, Delivered: true,
		Target: target.String(), Files: len(files),
	})
}

// messageChunk is the per-message limit to split an out-of-band note at, held
// just under the tool's hard limit so a chunk that gains a character in
// transit is not rejected outright.
func messageChunk(t core.OriginType) int {
	if t == core.OriginSlack {
		return core.SlackMaxMessage - 10
	}
	return core.DiscordMaxMessage - 10
}

// conversationChannel resolves which channel the speaker is heard in.
//
// Naming the turn is the exact answer and the one an agent always has: its
// audience was resolved when it was admitted and written on the row, so a
// delegated turn knows the thread its work is being waited on in even though its
// own conversation holds nothing but delegations. Without a turn the address is
// inferred from the conversation's recent history, which is what a person
// running the CLI by hand gets and what speaking into someone else's
// conversation has to fall back on.
//
// Either way the answer comes from turns roundclaw itself recorded, never from
// the request. That is the whole authorisation model for speaking: an agent can
// only reach an audience it already has, so there is no way to name an arbitrary
// channel and no way for a prompt-injected agent to broadcast.
func (h *HTTP) conversationAudience(ctx context.Context, agentID, conversation string, turnID int64) (core.Origin, error) {
	st, err := h.disp.Store(ctx, agentID)
	if err != nil {
		return core.Origin{}, err
	}

	if turnID > 0 {
		t, err := st.GetTurn(ctx, turnID)
		if err != nil {
			return core.Origin{}, fmt.Errorf("read turn %d of agent %s: %w", turnID, agentID, err)
		}
		audience, ok := t.Origin.Listening()
		if !ok {
			return core.Origin{}, fmt.Errorf(
				"turn %d of agent %s has nobody listening; it was driven by the API, "+
					"so its caller reads the result rather than being told it", turnID, agentID)
		}
		return speakableAudience(audience, fmt.Sprintf("turn %d of agent %s", turnID, agentID))
	}

	audience, ok, err := st.AudienceIn(ctx, conversation, store.AudienceLookback)
	if err != nil {
		return core.Origin{}, err
	}
	if ok {
		return speakableAudience(audience, fmt.Sprintf("conversation %q of agent %s", conversation, agentID))
	}

	turns, err := st.RecentTurnsIn(ctx, conversation, 1)
	if err != nil {
		return core.Origin{}, err
	}
	if len(turns) == 0 {
		return core.Origin{}, fmt.Errorf("agent %s has no conversation %q to speak into", agentID, conversation)
	}
	return core.Origin{}, fmt.Errorf(
		"conversation %q of agent %s has no chat audience; it was driven by the API, "+
			"so its caller reads results rather than being told them", conversation, agentID)
}

// speakableAudience narrows a resolved audience to somewhere this endpoint can
// actually post. A callback URL is a real audience for a *result* and no use
// here: there is no channel behind it, and answering as though a progress note
// had been delivered is the failure this whole path exists to avoid.
//
// The address is returned whole rather than reduced to a channel id, so the
// chat tool and — for Slack — the thread survive to the send. Reducing it here
// is what would put a threaded progress note in front of the whole channel.
func speakableAudience(audience core.Origin, who string) (core.Origin, error) {
	switch audience.Type {
	case core.OriginDiscord, core.OriginSlack:
		if audience.ChannelID != "" {
			return audience, nil
		}
	}
	return core.Origin{}, fmt.Errorf("%s answers to %s, which is not a channel that can be spoken into",
		who, audience.String())
}

// maxMessageBytes caps an out-of-band message. Generous next to Discord's 2000
// characters per message, small enough that this endpoint cannot be used to move
// bulk data into a channel.
const maxMessageBytes = 64 << 10
