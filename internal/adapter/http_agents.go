package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

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
	data, err := os.ReadFile(h.personaPath(id))
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]string{"agent_id": id, "persona": ""})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"agent_id": id, "persona": string(data)})
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
	if err := h.writePersona(id, body.Persona); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.log.Info("agent persona updated via API", "agent", id, "bytes", len(body.Persona))
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "bytes": len(body.Persona), "status": "set"})
}

func (h *HTTP) personaPath(agentID string) string {
	return filepath.Join(h.disp.Config().WorkDir(agentID), "CLAUDE.md")
}

func (h *HTTP) writePersona(agentID, content string) error {
	dir := h.disp.Config().WorkDir(agentID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(content), 0o640)
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

	created, err := h.disp.Registry().Create(r.Context(), agent)
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

	updated, err := h.disp.Registry().Update(r.Context(), agent)
	if err != nil {
		writeAgentError(w, err)
		return
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
