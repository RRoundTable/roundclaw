package adapter

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Skill management over the API.
//
// A skill names a host path (a SKILL.md directory), so writing one is bounded by
// who is asking, the same boundary tools draw (see mayWriteGrant). Granting a
// skill to an agent is done through the agent definition (its "skills" list),
// not here.
//
//	GET    /v1/skills              list registered skills
//	POST   /v1/skills             create or replace (id in the body)
//	GET    /v1/skills/{skill}      read
//	PUT    /v1/skills/{skill}      create or replace
//	DELETE /v1/skills/{skill}      delete
func (h *HTTP) registerSkillRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/skills", h.listSkills)
	mux.HandleFunc("POST /v1/skills", h.putSkill)
	mux.HandleFunc("GET /v1/skills/{skill}", h.getSkill)
	mux.HandleFunc("PUT /v1/skills/{skill}", h.putSkillByID)
	mux.HandleFunc("DELETE /v1/skills/{skill}", h.deleteSkill)
}

func (h *HTTP) listSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := h.disp.Registry().ListSkills(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if skills == nil {
		skills = []registry.Skill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

func (h *HTTP) putSkill(w http.ResponseWriter, r *http.Request) {
	sk, ok := decodeSkill(w, r, "")
	if !ok {
		return
	}
	h.saveSkill(w, r, sk)
}

func (h *HTTP) putSkillByID(w http.ResponseWriter, r *http.Request) {
	sk, ok := decodeSkill(w, r, r.PathValue("skill"))
	if !ok {
		return
	}
	h.saveSkill(w, r, sk)
}

func (h *HTTP) saveSkill(w http.ResponseWriter, r *http.Request, sk registry.Skill) {
	if !h.mayWriteGrant(w, r, "skill", sk.ID) {
		return
	}
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	caller := identityFrom(r)
	plan, ok := h.gateSelfChange(w, r, caller.AgentID)
	if !ok {
		return
	}
	saved, err := h.disp.Registry().PutSkill(r.Context(), sk, c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if caller.Established() {
		h.gateSelfGrantChange(r, plan, caller.AgentID)
	}
	h.log.Info("skill saved", "skill", saved.ID, "host_path", saved.HostPath)
	writeJSON(w, http.StatusOK, saved)
}

func (h *HTTP) getSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := h.disp.Registry().GetSkill(r.Context(), r.PathValue("skill"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such skill")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (h *HTTP) deleteSkill(w http.ResponseWriter, r *http.Request) {
	err := h.disp.Registry().DeleteSkill(r.Context(), r.PathValue("skill"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such skill")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("skill")})
}

func decodeSkill(w http.ResponseWriter, r *http.Request, idFromPath string) (registry.Skill, bool) {
	var sk registry.Skill
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&sk); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return registry.Skill{}, false
	}
	if idFromPath != "" {
		sk.ID = idFromPath
	}
	return sk, true
}
