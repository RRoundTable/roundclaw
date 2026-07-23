package adapter

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/roundtable/roundclaw/internal/core"
)

// Inbound webhooks: POST /v1/webhooks/{agent}
//
// A separate door from the rest of the API, because the callers are different.
// A GitHub or CI webhook cannot be given a bearer token and will not be updated
// when one rotates; it signs its body with a shared secret instead. So this
// route sits outside the bearer-auth middleware and authenticates by signature.
//
// The signature is over the exact bytes received. Parsing first and re-encoding
// would verify something the sender never sent.

const (
	// MaxWebhookBytes bounds a payload. Event bodies are small; a large one is
	// either a mistake or an attempt to make roundclaw hold it in memory.
	MaxWebhookBytes = 1 << 20

	signatureHeader = "X-Roundclaw-Signature"
	// GitHub's header, accepted so its webhooks work without a proxy.
	githubSignatureHeader = "X-Hub-Signature-256"
	eventHeader           = "X-Roundclaw-Event"
	githubEventHeader     = "X-GitHub-Event"
)

// ErrWebhookUnverified is returned when a payload's signature does not match.
var ErrWebhookUnverified = errors.New("signature does not match")

// registerWebhookRoutes mounts the webhook endpoint. It is registered outside
// the authenticated mux: signature verification is its authentication.
func (h *HTTP) registerWebhookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/webhooks/{agent}", h.postWebhook)
}

func (h *HTTP) postWebhook(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")

	secret := os.Getenv(h.disp.Config().HTTP.WebhookSecretEnv)
	if secret == "" {
		// Refusing is the only safe answer: without a secret every caller on
		// the network could queue billable work.
		writeError(w, http.StatusServiceUnavailable,
			"webhooks are not configured: "+h.disp.Config().HTTP.WebhookSecretEnv+" is empty")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload is too large")
		return
	}

	if err := verifySignature(r.Header, body, secret); err != nil {
		// Deliberately terse. Telling an unauthenticated caller which part of
		// the check failed helps them more than it helps anyone else.
		h.log.Warn("rejected an unverified webhook", "agent", agentID, "error", err)
		writeError(w, http.StatusUnauthorized, "signature verification failed")
		return
	}

	event := firstHeader(r.Header, eventHeader, githubEventHeader)
	prompt := webhookPrompt(agentID, event, body)

	// The delivery ID makes a redelivery — which senders do routinely on a
	// timeout — land on the original turn instead of running the work twice.
	key := firstHeader(r.Header, "X-Roundclaw-Delivery", "X-GitHub-Delivery")
	if key != "" {
		key = "webhook:" + agentID + ":" + key
	}

	sub, err := h.disp.Submit(r.Context(), agentID, prompt, core.HTTPPollOrigin(), key)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownAgent):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	h.log.Info("accepted a webhook", "agent", agentID, "event", event, "turn_id", sub.TurnID)
	writeJSON(w, http.StatusAccepted, submitResponse{
		AgentID: agentID, TurnID: sub.TurnID, Status: "queued",
		QueuePosition: sub.QueuePosition, Duplicate: sub.Duplicate,
	})
}

// verifySignature checks an HMAC-SHA256 over the raw body.
func verifySignature(header http.Header, body []byte, secret string) error {
	presented := firstHeader(header, signatureHeader, githubSignatureHeader)
	if presented == "" {
		return fmt.Errorf("%w: no signature header", ErrWebhookUnverified)
	}
	presented = strings.TrimPrefix(presented, "sha256=")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Constant time, so a wrong signature cannot be recovered a byte at a time.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
		return ErrWebhookUnverified
	}
	return nil
}

// webhookPrompt renders a payload for the agent.
//
// The body is handed over as-is rather than summarised: roundclaw has no idea
// what any given sender's schema means, and guessing would drop the field that
// mattered. The agent can read JSON.
func webhookPrompt(agentID, event string, body []byte) string {
	var b strings.Builder
	b.WriteString("A webhook arrived")
	if event != "" {
		fmt.Fprintf(&b, " for the event %q", event)
	}
	b.WriteString(".\n\nPayload:\n```json\n")

	// Indented when it parses, so the agent reads a structure rather than one
	// long line; passed through untouched when it does not, because a sender
	// may legitimately post something other than JSON.
	var pretty any
	if json.Unmarshal(body, &pretty) == nil {
		if formatted, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			body = formatted
		}
	}
	b.Write(body)
	b.WriteString("\n```\n")
	return b.String()
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}
