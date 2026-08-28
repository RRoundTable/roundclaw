package adapter

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Agent CRUD. These change what exists while the process runs, which is the
// whole point of the registry: adding an agent used to mean editing YAML and
// restarting both binaries.
//
//	GET    /v1/agents          list
//	POST   /v1/agents          create
//	GET    /v1/agents/{agent}/definition
//	PUT    /v1/agents/{agent}/definition
//	DELETE /v1/agents/{agent}
//
// The definition routes are nested under /definition because GET /v1/agents/
// {agent} already means "what is this agent doing right now", and runtime state
// is asked for far more often than the definition.
func (h *HTTP) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents", h.listAgents)
	mux.HandleFunc("POST /v1/agents", h.createAgent)
	mux.HandleFunc("GET /v1/agents/{agent}/definition", h.getAgentDefinition)
	mux.HandleFunc("PUT /v1/agents/{agent}/definition", h.putAgentDefinition)
	mux.HandleFunc("GET /v1/agents/{agent}/persona", h.getAgentPersona)
	mux.HandleFunc("PUT /v1/agents/{agent}/persona", h.putAgentPersona)
	mux.HandleFunc("DELETE /v1/agents/{agent}", h.deleteAgent)
}

// The persona is an agent's CLAUDE.md — its instructions — which is a file in the
// workspace, not a registry column. It lives here (not in the definition) so it
// can be long-form and edited on its own. The gateway shares the workspace mount,
// so this reads and writes the same file the agent's next turn will load.
type personaBody struct {
	Persona string `json:"persona"`
}

func (h *HTTP) getAgentPersona(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("agent")
	if _, err := h.disp.Registry().Get(r.Context(), id); err != nil {
		writeAgentError(w, err)
		return
	}
	persona, err := h.disp.ReadPersona(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"agent_id": id, "persona": persona})
}

func (h *HTTP) putAgentPersona(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("agent")
	if _, err := h.disp.Registry().Get(r.Context(), id); err != nil {
		writeAgentError(w, err)
		return
	}
	var body personaBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	// Before the file moves, not after: a change that could never be judged is
	// refused rather than applied and then left standing forever.
	plan, ok := h.gateSelfChange(w, r, id)
	if !ok {
		return
	}
	if err := h.writePersona(id, body.Persona); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// A persona change never reaches registry.Update — it is a file, not a
	// column — so this is the one write that has to mint its own version. Without
	// it the most consequential edit an agent can receive would leave no trace.
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	if c.Note == "" {
		c.Note = personaVersionNote
	}
	version, err := h.disp.Registry().Snapshot(r.Context(), id, c)
	if err == nil {
		h.startSelfChangeGate(r, plan, id, version.Version)
	}
	if err != nil {
		// The persona is already written and the agent will use it. Report the
		// missing snapshot rather than a failure, so nobody re-PUTs a persona that
		// is in fact live.
		h.log.Error("persona written but not versioned", "agent", id, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"agent_id": id, "bytes": len(body.Persona), "status": "set",
			"warning": "persona saved, but no version was recorded: " + err.Error(),
		})
		return
	}

	h.log.Info("agent persona updated via API", "agent", id, "bytes", len(body.Persona), "version", version.Version)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": id, "bytes": len(body.Persona), "status": "set", "version": version.Version,
	})
}

// writePersona is the dispatcher's, so that a rollback, an approved proposal and
// a hand-written PUT all put the file in the same place with the same mode.
func (h *HTTP) writePersona(agentID, content string) error {
	return h.disp.WritePersona(agentID, content)
}

func (h *HTTP) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.disp.Registry().List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agents == nil {
		agents = []registry.Agent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (h *HTTP) createAgent(w http.ResponseWriter, r *http.Request) {
	agent, ok := decodeAgent(w, r)
	if !ok {
		return
	}
	// New agents are usable immediately unless the caller says otherwise.
	if !r.URL.Query().Has("disabled") {
		agent.Enabled = true
	}

	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	created, err := h.disp.Registry().Create(r.Context(), agent, c)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("agent created", "agent", created.ID, "channels", len(created.DiscordChannels))
	writeJSON(w, http.StatusCreated, created)
}

func (h *HTTP) getAgentDefinition(w http.ResponseWriter, r *http.Request) {
	agent, err := h.disp.Registry().Get(r.Context(), r.PathValue("agent"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (h *HTTP) putAgentDefinition(w http.ResponseWriter, r *http.Request) {
	agent, ok := decodeAgent(w, r)
	if !ok {
		return
	}
	// The path wins over the body, so a mismatched id cannot rename an agent
	// out from under its workspace and Claude session.
	agent.ID = r.PathValue("agent")

	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	plan, ok := h.gateSelfChange(w, r, agent.ID)
	if !ok {
		return
	}
	updated, err := h.disp.Registry().Update(r.Context(), agent, c)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if v, vErr := h.disp.Registry().LatestVersion(r.Context(), updated.ID); vErr == nil {
		h.startSelfChangeGate(r, plan, updated.ID, v.Version)
	}
	h.log.Info("agent updated", "agent", updated.ID, "enabled", updated.Enabled)
	writeJSON(w, http.StatusOK, updated)
}

func (h *HTTP) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("agent")

	// Stop every conversation before deleting, not after. The agent's workflows
	// outlive its definition, so anything still queued — in the default session
	// or any thread — would otherwise run on and fail one turn at a time with
	// "unknown agent": noisy, and it would deliver a string of errors to whoever
	// asked. Best-effort: a never-started agent has no workflow to stop, and that
	// must not block the delete.
	if err := h.disp.StopAll(r.Context(), id, "agent deleted"); err != nil {
		h.log.Info("could not stop agent before deleting it", "agent", id, "error", err)
	}

	if err := h.disp.Registry().Delete(r.Context(), id); err != nil {
		writeAgentError(w, err)
		return
	}
	// The workspace, database and Claude session are left alone, so recreating
	// the same ID resumes the conversation. Say so, because "deleted" usually
	// implies the data went too.
	h.log.Info("agent definition deleted; workspace retained", "agent", id)
	writeJSON(w, http.StatusOK, map[string]string{
		"deleted":   id,
		"retained":  "workspace, database and Claude session",
		"to_purge":  "remove the agent's directory under the workspace root",
		"to_resume": "create an agent with the same id",
	})
}

func decodeAgent(w http.ResponseWriter, r *http.Request) (registry.Agent, bool) {
	var agent registry.Agent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&agent); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return registry.Agent{}, false
	}
	return agent, true
}

func writeAgentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
