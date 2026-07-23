package adapter

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
)

// Turn IDs come from a per-agent SQLite AUTOINCREMENT, so they are unique only
// within an agent. Every turn route is therefore agent-scoped; a bare
// /v1/turns/{id} would be ambiguous across agents.
//
// Routes:
//
//	POST /v1/agents/{agent}/requests             queue a request
//	GET  /v1/agents/{agent}                      agent status
//	GET  /v1/agents/{agent}/turns/{turn}         turn state and result
//	GET  /v1/agents/{agent}/turns/{turn}/stream  SSE live transcript
type HTTP struct {
	disp *Dispatcher
	log  *slog.Logger

	tokens         [][32]byte
	delegateTokens [][32]byte
	waitTimeout    time.Duration

	sse sseLimiter
}

// NewHTTP builds the API adapter. tokens are full-access bearer credentials;
// delegateTokens are restricted to sending requests and reading agent status
// (see delegateAllowed). An empty tokens list rejects every request rather than
// running open.
func NewHTTP(disp *Dispatcher, log *slog.Logger, tokens, delegateTokens []string, waitTimeout time.Duration, maxSSEPerAgent int) *HTTP {
	h := &HTTP{
		disp:        disp,
		log:         log,
		waitTimeout: waitTimeout,
		sse:         sseLimiter{max: maxSSEPerAgent, active: map[string]int{}},
	}
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			h.tokens = append(h.tokens, sha256.Sum256([]byte(t)))
		}
	}
	for _, t := range delegateTokens {
		if t = strings.TrimSpace(t); t != "" {
			h.delegateTokens = append(h.delegateTokens, sha256.Sum256([]byte(t)))
		}
	}
	return h
}

// Handler builds the router with auth applied to every route.
func (h *HTTP) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents/{agent}/requests", h.postRequest)
	mux.HandleFunc("GET /v1/agents/{agent}", h.getStatus)
	mux.HandleFunc("GET /v1/agents/{agent}/turns/{turn}", h.getTurn)
	mux.HandleFunc("GET /v1/agents/{agent}/turns/{turn}/stream", h.streamTurn)
	mux.HandleFunc("GET /v1/agents/{agent}/workflow", h.getWorkflow)
	h.registerAgentRoutes(mux)
	h.registerScheduleRoutes(mux)
	h.registerSecretRoutes(mux)
	h.registerWorkflowRoutes(mux)
	h.registerToolRoutes(mux)
	h.registerSkillRoutes(mux)

	// Webhooks sit outside the bearer middleware: their callers cannot hold a
	// token and would not be updated when one rotates, so a per-payload
	// signature is their authentication instead.
	outer := http.NewServeMux()
	h.registerWebhookRoutes(outer)
	outer.Handle("/", h.authenticate(mux))
	return outer
}

type submitBody struct {
	Text        string `json:"text"`
	CallbackURL string `json:"callback_url,omitempty"`
	// Steer interrupts the running turn instead of queueing behind it. It is
	// opt-in and explicit: nothing infers a steer from message content, so a
	// misread can never destroy work in progress.
	Steer bool `json:"steer,omitempty"`
}

type submitResponse struct {
	AgentID       string `json:"agent_id"`
	TurnID        int64  `json:"turn_id"`
	Status        string `json:"status"`
	QueuePosition int    `json:"queue_position"`
	Duplicate     bool   `json:"duplicate,omitempty"`

	// Populated only when ?wait=true ran to completion.
	Result  string  `json:"result,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	Error   string  `json:"error,omitempty"`
}

func (h *HTTP) postRequest(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")

	body, files, cleanup, err := readSubmission(w, r)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	origin := core.HTTPPollOrigin()
	if body.CallbackURL != "" {
		origin = core.HTTPCallbackOrigin(body.CallbackURL, "default")
		if err := origin.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// The SSRF check has to run here as well as at delivery. Structural
		// validation above only covers the scheme, so without this a loopback
		// or private-network callback would be accepted, and the caller would
		// not find out until after an agent turn had already been paid for.
		if err := core.AssertPublicCallbackHost(body.CallbackURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// A caller that supplies no Idempotency-Key gets no retry protection; that
	// is their choice, so it is allowed but the header is strongly advised.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey != "" {
		idempotencyKey = "http:" + agentID + ":" + idempotencyKey
	}

	prompt := body.Text
	if len(files) > 0 {
		paths, err := h.disp.SaveAttachments(agentID, files)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		prompt = PromptWithAttachments(prompt, paths)
	}

	submit := h.disp.Submit
	if body.Steer {
		submit = h.disp.Steer
	}

	sub, err := submit(r.Context(), agentID, prompt, origin, idempotencyKey)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownAgent):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	resp := submitResponse{
		AgentID:       agentID,
		TurnID:        sub.TurnID,
		Status:        "queued",
		QueuePosition: sub.QueuePosition,
		Duplicate:     sub.Duplicate,
	}

	if r.URL.Query().Get("wait") != "true" {
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	// ?wait=true is a convenience, not a durability guarantee: this connection
	// dies with the gateway. The turn is already durable, so a timeout demotes
	// to 202 and the caller falls back to polling the same turn ID. An agent's
	// queue is shared with Discord, so the wait can be long through no fault of
	// this request.
	turn, done := h.waitForTurn(r.Context(), agentID, sub.TurnID)
	if !done {
		writeJSON(w, http.StatusAccepted, resp)
		return
	}
	resp.Status = string(turn.Status)
	resp.Result = turn.Result
	resp.CostUSD = turn.CostUSD
	resp.Error = turn.Error
	writeJSON(w, http.StatusOK, resp)
}

// waitForTurn polls until the turn reaches a terminal state, the client goes
// away, or the wait budget expires.
func (h *HTTP) waitForTurn(ctx context.Context, agentID string, turnID int64) (store.Turn, bool) {
	st, err := h.disp.Store(ctx, agentID)
	if err != nil {
		return store.Turn{}, false
	}

	deadline := time.NewTimer(h.waitTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return store.Turn{}, false
		case <-deadline.C:
			return store.Turn{}, false
		case <-tick.C:
			turn, err := st.GetTurn(ctx, turnID)
			if err != nil {
				return store.Turn{}, false
			}
			if turn.Status != core.TurnRunning {
				return turn, true
			}
		}
	}
}

// getWorkflow reports the agent's Temporal execution: whether it is alive, what
// it is waiting on, and whether an activity is retrying.
func (h *HTTP) getWorkflow(w http.ResponseWriter, r *http.Request) {
	info, err := h.disp.Workflow(r.Context(), r.PathValue("agent"))
	if err != nil {
		h.writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *HTTP) getStatus(w http.ResponseWriter, r *http.Request) {
	tail := intParam(r, "tail", statusTailLines)
	report, err := h.disp.Status(r.Context(), r.PathValue("agent"), tail)
	if err != nil {
		h.writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

type turnResponse struct {
	AgentID    string  `json:"agent_id"`
	TurnID     int64   `json:"turn_id"`
	Status     string  `json:"status"`
	Request    string  `json:"request"`
	Result     string  `json:"result,omitempty"`
	Error      string  `json:"error,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	QueuedAt   string  `json:"queued_at"`
	FinishedAt string  `json:"finished_at,omitempty"`
}

func (h *HTTP) getTurn(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")
	turnID, err := strconv.ParseInt(r.PathValue("turn"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "turn id must be an integer")
		return
	}

	st, err := h.disp.Store(r.Context(), agentID)
	if err != nil {
		h.writeLookupError(w, err)
		return
	}
	turn, err := st.GetTurn(r.Context(), turnID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such turn")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := turnResponse{
		AgentID:  agentID,
		TurnID:   turn.ID,
		Status:   string(turn.Status),
		Request:  turn.Request,
		Result:   turn.Result,
		Error:    turn.Error,
		CostUSD:  turn.CostUSD,
		QueuedAt: turn.QueuedAt.UTC().Format(time.RFC3339),
	}
	if !turn.FinishedAt.IsZero() {
		resp.FinishedAt = turn.FinishedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// streamTurn tails a turn's transcript as Server-Sent Events, reading the same
// SQLite rows /status uses. Nothing here touches Temporal.
func (h *HTTP) streamTurn(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")
	turnID, err := strconv.ParseInt(r.PathValue("turn"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "turn id must be an integer")
		return
	}

	st, err := h.disp.Store(r.Context(), agentID)
	if err != nil {
		h.writeLookupError(w, err)
		return
	}

	// SSE connections are long-lived. Without a cap, a handful of watchers per
	// agent would exhaust the gateway's file descriptors before anything else
	// showed strain.
	if !h.sse.acquire(agentID) {
		writeError(w, http.StatusTooManyRequests, "too many open streams for this agent")
		return
	}
	defer h.sse.release(agentID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is unsupported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Last-Event-ID lets a reconnecting client resume without replaying the
	// whole transcript.
	var cursor int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			logs, err := st.LogsAfter(r.Context(), turnID, cursor, 200)
			if err != nil {
				return
			}
			for _, e := range logs {
				cursor = e.ID
				payload, err := json.Marshal(map[string]any{
					"kind":       e.Kind,
					"content":    e.Content,
					"created_at": e.CreatedAt.UTC().Format(time.RFC3339),
				})
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", e.ID, payload)
			}

			turn, err := st.GetTurn(r.Context(), turnID)
			if err == nil && turn.Status != core.TurnRunning && len(logs) == 0 {
				// Terminal state reached and the transcript is drained.
				fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", turn.Status)
				flusher.Flush()
				return
			}
			flusher.Flush()
		}
	}
}

// authenticate enforces bearer auth with a constant-time comparison. Tokens are
// held hashed so a heap dump does not hand over live credentials.
func (h *HTTP) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(h.tokens) == 0 {
			writeError(w, http.StatusServiceUnavailable, "no API tokens are configured")
			return
		}

		header := r.Header.Get("Authorization")
		presented, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}

		sum := sha256.Sum256([]byte(strings.TrimSpace(presented)))
		for _, known := range h.tokens {
			if subtle.ConstantTimeCompare(sum[:], known[:]) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		// A delegate token is accepted only on the restricted surface — sending a
		// request to an agent and reading status. Everything else (managing agents,
		// secrets, tools, workflows, schedules) is 403, so a token that leaks into
		// an agent container cannot reconfigure the fleet.
		for _, known := range h.delegateTokens {
			if subtle.ConstantTimeCompare(sum[:], known[:]) == 1 {
				if !delegateAllowed(r.Method, r.URL.Path) {
					writeError(w, http.StatusForbidden,
						"this token may only send requests to agents and read their status")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid bearer token")
	})
}

// delegateAllowed is the whitelist a restricted (delegate-scoped) token may
// reach: list agents, read one agent's status/turns/workflow, and send it a
// request. It is a prefix/shape check on the raw path because it runs in the
// auth middleware, before the router has matched a pattern. Deny by default —
// any route not named here (agent CRUD, secrets, tools, workflows, schedules,
// even an agent's definition, which exposes host paths) is refused.
func delegateAllowed(method, path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 2 || segs[0] != "v1" || segs[1] != "agents" {
		return false
	}
	switch method {
	case http.MethodGet:
		switch len(segs) {
		case 2: // /v1/agents                          list
			return true
		case 3: // /v1/agents/{id}                     status
			return true
		case 4: // /v1/agents/{id}/workflow            execution state
			return segs[3] == "workflow"
		case 5: // /v1/agents/{id}/turns/{turn}        turn result
			return segs[3] == "turns"
		case 6: // /v1/agents/{id}/turns/{turn}/stream live transcript
			return segs[3] == "turns" && segs[5] == "stream"
		}
	case http.MethodPost:
		// /v1/agents/{id}/requests                    delegate a task
		return len(segs) == 4 && segs[3] == "requests"
	}
	return false
}

func (h *HTTP) writeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnknownAgent) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// sseLimiter caps concurrent streams per agent.
type sseLimiter struct {
	mu     sync.Mutex
	max    int
	active map[string]int
}

func (l *sseLimiter) acquire(agentID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[agentID] >= l.max {
		return false
	}
	l.active[agentID]++
	return true
}

func (l *sseLimiter) release(agentID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[agentID] > 0 {
		l.active[agentID]--
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func intParam(r *http.Request, name string, fallback int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// TokensFromEnv splits a comma-separated bearer token list.
func TokensFromEnv(raw string) []string {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
