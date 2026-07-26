package adapter

import (
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/store"
)

// Request history.
//
//	GET /v1/agents/{agent}/turns?limit=&since=&status=&conversation=&full=
//
// The single-turn route answers "how did my request go". This answers "what has
// this agent been asked, and what went wrong" — the question you start from when
// deciding whether an agent needs changing. It reads the agent's state.db
// directly, like /status, so it neither costs a model call nor depends on
// Temporal being healthy.
//
// Results are truncated by default. The usual caller is an agent reviewing the
// fleet, and a hundred untruncated transcripts would spend its context before it
// read the second one; ?full=true opts out for the few turns worth reading whole.
func (h *HTTP) registerHistoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents/{agent}/turns", h.listTurns)
}

// historyPreview caps a truncated request or result. Long enough to tell what a
// turn was about and how it ended, short enough that a full page of them stays
// readable.
const historyPreview = 600

type historyTurn struct {
	TurnID       int64   `json:"turn_id"`
	Status       string  `json:"status"`
	Request      string  `json:"request"`
	Result       string  `json:"result,omitempty"`
	Error        string  `json:"error,omitempty"`
	CostUSD      float64 `json:"cost_usd"`
	Origin       string  `json:"origin,omitempty"`
	Conversation string  `json:"conversation,omitempty"`
	QueuedAt     string  `json:"queued_at"`
	FinishedAt   string  `json:"finished_at,omitempty"`
	// Truncated says the request or result was cut; fetch the turn itself, or
	// pass ?full=true, to read the whole thing.
	Truncated bool `json:"truncated,omitempty"`
}

func (h *HTTP) listTurns(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")

	f, err := parseTurnFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	st, err := h.disp.Store(r.Context(), agentID)
	if err != nil {
		h.writeLookupError(w, err)
		return
	}
	turns, err := st.Turns(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	full := r.URL.Query().Get("full") == "true"
	out := make([]historyTurn, 0, len(turns))
	var spend float64
	for _, t := range turns {
		spend += t.CostUSD
		ht := historyTurn{
			TurnID:       t.ID,
			Status:       string(t.Status),
			Request:      t.Request,
			Result:       t.Result,
			Error:        t.Error,
			CostUSD:      t.CostUSD,
			Origin:       string(t.Origin.Type),
			Conversation: t.Conversation,
			QueuedAt:     t.QueuedAt.UTC().Format(time.RFC3339),
		}
		if !t.FinishedAt.IsZero() {
			ht.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339)
		}
		if !full {
			var cutReq, cutRes bool
			ht.Request, cutReq = preview(ht.Request, historyPreview)
			ht.Result, cutRes = preview(ht.Result, historyPreview)
			ht.Truncated = cutReq || cutRes
		}
		out = append(out, ht)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":  agentID,
		"turns":     out,
		"count":     len(out),
		"cost_usd":  spend,
		"truncated": !full,
	})
}

func parseTurnFilter(r *http.Request) (store.TurnFilter, error) {
	q := r.URL.Query()
	var f store.TurnFilter

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, errBadQuery("limit must be an integer")
		}
		f.Limit = n
	}
	if v := q.Get("since"); v != "" {
		// Both spellings are what a caller actually reaches for: an absolute
		// instant when comparing against a recorded time, a duration when asking
		// "since the last review".
		if d, err := time.ParseDuration(v); err == nil {
			f.Since = time.Now().Add(-d)
		} else if ts, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = ts
		} else {
			return f, errBadQuery("since must be RFC3339 (2026-07-01T00:00:00Z) or a duration back from now (72h)")
		}
	}
	if v := q.Get("status"); v != "" {
		switch core.TurnStatus(v) {
		// A queued turn is stored as running — the queue is the workflow's, not a
		// column — so there is no "queued" to ask for here.
		case core.TurnRunning, core.TurnDone, core.TurnStopped, core.TurnError:
			f.Status = core.TurnStatus(v)
		default:
			return f, errBadQuery("status must be running, done, stopped or error")
		}
	}
	if q.Has("conversation") {
		// "default" is the sentinel for the agent's default conversation, which
		// is stored as the empty string — the same spelling workflow IDs use.
		conv := q.Get("conversation")
		if conv == "default" {
			conv = ""
		}
		f.Conversation = &conv
	}
	return f, nil
}

type queryError struct{ msg string }

func (e queryError) Error() string { return e.msg }

func errBadQuery(msg string) error { return queryError{msg} }

// preview cuts s to at most n bytes on a rune boundary, reporting whether it had
// to. It differs from truncate() next door by saying so: a caller deciding
// whether to fetch the whole turn needs to know it is looking at a fragment.
func preview(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…", true
}
