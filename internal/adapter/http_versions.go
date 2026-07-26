package adapter

import (
	"net/http"
	"strconv"
	"time"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Agent version history.
//
//	GET  /v1/agents/{agent}/versions            list, newest first
//	GET  /v1/agents/{agent}/versions/{version}  one snapshot (definition + persona)
//	POST /v1/agents/{agent}/versions/{version}/rollback
//
// Versions are minted by the registry on every definition write and by the
// persona endpoint on every persona write, so nothing here creates them — this
// is the read side, plus the one operation that reads history and writes it back.
//
// A rollback does not rewind the history; it applies an old snapshot as a *new*
// version. Rewinding would destroy the record of the change being undone, which
// is the thing you most want to look at afterwards.
func (h *HTTP) registerVersionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents/{agent}/versions", h.listVersions)
	mux.HandleFunc("GET /v1/agents/{agent}/versions/{version}", h.getVersion)
	mux.HandleFunc("POST /v1/agents/{agent}/versions/{version}/rollback", h.rollbackVersion)
}

// Change metadata travels in headers rather than the body, so that every write
// route can carry it without each one growing an envelope around its payload.
//
// Neither is authenticated — a bearer token says what may be done, not who is
// doing it — so they are a changelog, not an audit log. That is the honest
// reading: "curator, because eval run 12 regressed" is useful to whoever reads
// the history later, and nothing depends on it being true.
const (
	headerAuthor = "X-Roundclaw-Author"
	headerNote   = "X-Roundclaw-Note"
)

func changeFrom(r *http.Request) registry.Change {
	return registry.Change{
		Note:   r.Header.Get(headerNote),
		Author: r.Header.Get(headerAuthor),
	}
}

// versionSummary is a version without its payload — enough to see what changed
// and when, without pulling every persona the agent has ever had.
type versionSummary struct {
	Version   int    `json:"version"`
	Note      string `json:"note,omitempty"`
	Author    string `json:"author,omitempty"`
	CreatedAt string `json:"created_at"`
	// PersonaBytes and Description are the two things worth seeing in a list:
	// they are what a human recognises a version by.
	Description  string `json:"description,omitempty"`
	PersonaBytes int    `json:"persona_bytes"`
	Model        string `json:"model,omitempty"`
	Enabled      bool   `json:"enabled"`
}

func (h *HTTP) listVersions(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")

	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		limit = n
	}

	versions, err := h.disp.Registry().ListVersions(r.Context(), agentID, limit)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	// No 404 for an agent with no versions: history outlives the definition on
	// purpose, so "this agent was deleted, here is what it was" is a valid answer.
	out := make([]versionSummary, 0, len(versions))
	for _, v := range versions {
		out = append(out, versionSummary{
			Version:      v.Version,
			Note:         v.Note,
			Author:       v.Author,
			CreatedAt:    v.CreatedAt.UTC().Format(time.RFC3339),
			Description:  v.Definition.Description,
			PersonaBytes: len(v.Persona),
			Model:        v.Definition.Model,
			Enabled:      v.Definition.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": agentID, "versions": out, "count": len(out),
	})
}

func (h *HTTP) getVersion(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}
	v, err := h.disp.Registry().GetVersion(r.Context(), agentID, n)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *HTTP) rollbackVersion(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agent")
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}

	old, err := h.disp.Registry().GetVersion(r.Context(), agentID, n)
	if err != nil {
		writeAgentError(w, err)
		return
	}

	// The persona goes back first. The registry snapshots whatever the persona
	// source reads at write time, so writing the definition first would mint a
	// version pairing the restored definition with the persona still on disk —
	// a combination that never existed.
	if err := h.writePersona(agentID, old.Persona); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	c := changeFrom(r)
	if c.Note == "" {
		c.Note = "rolled back to v" + strconv.Itoa(n)
	}
	restored, err := h.disp.Registry().Update(r.Context(), old.Definition, c)
	if err != nil {
		writeAgentError(w, err)
		return
	}

	latest, err := h.disp.Registry().LatestVersion(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.log.Info("agent rolled back", "agent", agentID, "to", n, "new_version", latest.Version)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":    agentID,
		"rolled_back": n,
		"version":     latest.Version,
		"definition":  restored,
		"note": "history is append-only: the rollback is a new version, and the change it " +
			"undid is still on the record",
	})
}

// personaVersionNote explains a version minted by a persona write, when the
// caller supplied no note of its own.
const personaVersionNote = "persona edited"
