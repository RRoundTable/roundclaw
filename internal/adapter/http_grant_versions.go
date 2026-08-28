package adapter

import (
	"net/http"
	"strconv"
	"time"
)

// Version history for the two grantable things.
//
// The same three routes agent versions offer, pointed at tools and skills, which
// spec/003 gave a history for the first time. Change metadata rides the same
// headers (see http_versions.go) and carries the same weight: an author here is
// asserted by the caller, not established by the server.
//
// Listing does not 404 on a subject with no versions, matching listVersions: a
// tool's history outlives the tool on purpose, so "this was deleted, here is what
// it was" is a valid answer rather than a missing one.

func (h *HTTP) registerGrantVersionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tools/{tool}/versions", h.listToolVersions)
	mux.HandleFunc("GET /v1/tools/{tool}/versions/{version}", h.getToolVersion)
	mux.HandleFunc("POST /v1/tools/{tool}/versions/{version}/rollback", h.rollbackToolVersion)

	mux.HandleFunc("GET /v1/skills/{skill}/versions", h.listSkillVersions)
	mux.HandleFunc("GET /v1/skills/{skill}/versions/{version}", h.getSkillVersion)
	mux.HandleFunc("POST /v1/skills/{skill}/versions/{version}/rollback", h.rollbackSkillVersion)
}

// grantVersionSummary is a version without its definition: enough to see what
// changed and when without pulling every snapshot ever taken.
//
// Digest and DigestError are both present because they answer different
// questions. An empty digest with no error means the subject declared no
// identity — it has no version in the sense that matters, and says so. An empty
// digest with an error means it declared one that could not be read.
type grantVersionSummary struct {
	Version     int    `json:"version"`
	Digest      string `json:"digest,omitempty"`
	DigestError string `json:"digest_error,omitempty"`
	Note        string `json:"note,omitempty"`
	Author      string `json:"author,omitempty"`
	CreatedAt   string `json:"created_at"`
	Description string `json:"description,omitempty"`
}

func versionLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit must be an integer")
		return 0, false
	}
	return n, true
}

func versionNumber(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return 0, false
	}
	return n, true
}

func (h *HTTP) listToolVersions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("tool")
	limit, ok := versionLimit(w, r)
	if !ok {
		return
	}
	versions, err := h.disp.Registry().ListToolVersions(r.Context(), id, limit)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	out := make([]grantVersionSummary, 0, len(versions))
	for _, v := range versions {
		out = append(out, grantVersionSummary{
			Version:     v.Version,
			Digest:      v.Digest,
			DigestError: v.DigestErr,
			Note:        v.Note,
			Author:      v.Author,
			CreatedAt:   v.CreatedAt.UTC().Format(time.RFC3339),
			Description: v.Definition.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tool_id": id, "versions": out, "count": len(out),
	})
}

func (h *HTTP) getToolVersion(w http.ResponseWriter, r *http.Request) {
	n, ok := versionNumber(w, r)
	if !ok {
		return
	}
	v, err := h.disp.Registry().GetToolVersion(r.Context(), r.PathValue("tool"), n)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// rollbackToolVersion restores a tool and reports whether the content came back
// with the configuration.
//
// 200 either way: a rollback whose digest no longer matches did what was asked
// and is reporting a fact about the host, not failing. Turning that into an error
// would push callers to retry something no retry can fix.
func (h *HTTP) rollbackToolVersion(w http.ResponseWriter, r *http.Request) {
	n, ok := versionNumber(w, r)
	if !ok {
		return
	}
	res, err := h.disp.Registry().RollbackTool(r.Context(), r.PathValue("tool"), n, changeFrom(r))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *HTTP) listSkillVersions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("skill")
	limit, ok := versionLimit(w, r)
	if !ok {
		return
	}
	versions, err := h.disp.Registry().ListSkillVersions(r.Context(), id, limit)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	out := make([]grantVersionSummary, 0, len(versions))
	for _, v := range versions {
		out = append(out, grantVersionSummary{
			Version:     v.Version,
			Digest:      v.Digest,
			DigestError: v.DigestErr,
			Note:        v.Note,
			Author:      v.Author,
			CreatedAt:   v.CreatedAt.UTC().Format(time.RFC3339),
			Description: v.Definition.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"skill_id": id, "versions": out, "count": len(out),
	})
}

func (h *HTTP) getSkillVersion(w http.ResponseWriter, r *http.Request) {
	n, ok := versionNumber(w, r)
	if !ok {
		return
	}
	v, err := h.disp.Registry().GetSkillVersion(r.Context(), r.PathValue("skill"), n)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *HTTP) rollbackSkillVersion(w http.ResponseWriter, r *http.Request) {
	n, ok := versionNumber(w, r)
	if !ok {
		return
	}
	res, err := h.disp.Registry().RollbackSkill(r.Context(), r.PathValue("skill"), n, changeFrom(r))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
