package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Proposals: changes written down, waiting on a person.
//
//	GET  /v1/proposals                    list (?status=pending, ?target=dev)
//	POST /v1/proposals                    file one
//	GET  /v1/proposals/{proposal}         read one
//	POST /v1/proposals/{proposal}/approve apply it, and record who said so
//	POST /v1/proposals/{proposal}/reject  close it without applying
//
// Approving is the only route here that changes the fleet, and it does so
// through the ordinary registry calls — so an approved change mints a version
// exactly like a hand edit, and can be rolled back the same way.
func (h *HTTP) registerProposalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/proposals", h.listProposals)
	mux.HandleFunc("POST /v1/proposals", h.createProposal)
	mux.HandleFunc("GET /v1/proposals/{proposal}", h.getProposal)
	mux.HandleFunc("POST /v1/proposals/{proposal}/approve", h.approveProposal)
	mux.HandleFunc("POST /v1/proposals/{proposal}/reject", h.rejectProposal)
}

func (h *HTTP) listProposals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		limit = n
	}
	proposals, err := h.disp.Registry().ListProposals(r.Context(), q.Get("status"), q.Get("target"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if proposals == nil {
		proposals = []registry.Proposal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": proposals, "count": len(proposals)})
}

func (h *HTTP) createProposal(w http.ResponseWriter, r *http.Request) {
	var p registry.Proposal
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	// The proposer names itself through the same header every write uses, so an
	// agent filing a proposal does not have to remember to sign it.
	if p.CreatedBy == "" {
		p.CreatedBy = r.Header.Get(headerAuthor)
	}

	filed, err := h.disp.Registry().CreateProposal(r.Context(), p)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("proposal filed", "proposal", filed.ID, "kind", filed.Kind,
		"target", filed.Target, "by", filed.CreatedBy)
	writeJSON(w, http.StatusCreated, filed)
}

func (h *HTTP) getProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := proposalID(w, r)
	if !ok {
		return
	}
	p, err := h.disp.Registry().GetProposal(r.Context(), id)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// decisionBody carries who decided and what they said about it.
type decisionBody struct {
	By   string `json:"by,omitempty"`
	Note string `json:"note,omitempty"`
}

func (h *HTTP) rejectProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := proposalID(w, r)
	if !ok {
		return
	}
	body := decodeDecision(r)
	decided, err := h.disp.RejectProposal(r.Context(), id, h.decider(r, body), body.Note)
	if errors.Is(err, registry.ErrConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("proposal rejected", "proposal", id, "by", decided.DecidedBy)
	writeJSON(w, http.StatusOK, decided)
}

// approveProposal applies the change and records the decision. The work itself
// is on the dispatcher, because the Discord approve button has to do exactly the
// same thing and two implementations would drift.
func (h *HTTP) approveProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := proposalID(w, r)
	if !ok {
		return
	}
	body := decodeDecision(r)
	by := h.decider(r, body)

	decided, version, err := h.disp.ApproveProposal(r.Context(), id, by, body.Note)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		h.log.Error("approved proposal could not be applied", "proposal", id, "error", err)
		writeError(w, http.StatusBadRequest, "approved, but applying it failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"proposal": decided,
		"version":  version,
		"undo":     UndoHint(decided, version),
	})
}

// decider is who to record as having made the decision. The body wins over the
// header, because a human approving through a tool states themselves explicitly
// while the header is whatever the calling process happens to be.
func (h *HTTP) decider(r *http.Request, body decisionBody) string {
	if body.By != "" {
		return body.By
	}
	if v := r.Header.Get(headerAuthor); v != "" {
		return v
	}
	return "api"
}

func decodeDecision(r *http.Request) decisionBody {
	var b decisionBody
	if r.ContentLength > 0 {
		_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&b)
	}
	return b
}

func proposalID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("proposal"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proposal id must be an integer")
		return 0, false
	}
	return id, true
}
