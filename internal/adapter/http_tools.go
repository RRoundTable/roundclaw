package adapter

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Tool management over the API.
//
// A tool names a host path and the env a local capability needs, so writing one
// is bounded by who is asking: an operator credential may write any tool, and a
// per-agent credential only the tools that agent holds, or one that does not
// exist yet (see mayWriteGrant). Granting a tool to an agent is done through the
// agent definition (its "tools" list) or the admin attach_tool action, not here.
//
//	GET    /v1/tools              list registered tools
//	POST   /v1/tools             create or replace (id in the body)
//	GET    /v1/tools/{tool}       read
//	PUT    /v1/tools/{tool}       create or replace
//	DELETE /v1/tools/{tool}       delete
func (h *HTTP) registerToolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tools", h.listTools)
	mux.HandleFunc("POST /v1/tools", h.putTool)
	mux.HandleFunc("GET /v1/tools/{tool}", h.getTool)
	mux.HandleFunc("PUT /v1/tools/{tool}", h.putToolByID)
	mux.HandleFunc("DELETE /v1/tools/{tool}", h.deleteTool)
}

func (h *HTTP) listTools(w http.ResponseWriter, r *http.Request) {
	tools, err := h.disp.Registry().ListTools(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tools == nil {
		tools = []registry.Tool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func (h *HTTP) putTool(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTool(w, r, "")
	if !ok {
		return
	}
	h.saveTool(w, r, t)
}

func (h *HTTP) putToolByID(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTool(w, r, r.PathValue("tool"))
	if !ok {
		return
	}
	h.saveTool(w, r, t)
}

func (h *HTTP) saveTool(w http.ResponseWriter, r *http.Request, t registry.Tool) {
	if !h.mayWriteGrant(w, r, "tool", t.ID) {
		return
	}
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	saved, err := h.disp.Registry().PutTool(r.Context(), t, c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.log.Info("tool saved", "tool", saved.ID, "host_path", saved.HostPath)
	writeJSON(w, http.StatusOK, saved)
}

func (h *HTTP) getTool(w http.ResponseWriter, r *http.Request) {
	t, err := h.disp.Registry().GetTool(r.Context(), r.PathValue("tool"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such tool")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *HTTP) deleteTool(w http.ResponseWriter, r *http.Request) {
	err := h.disp.Registry().DeleteTool(r.Context(), r.PathValue("tool"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such tool")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("tool")})
}

func decodeTool(w http.ResponseWriter, r *http.Request, idFromPath string) (registry.Tool, bool) {
	var t registry.Tool
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return registry.Tool{}, false
	}
	if idFromPath != "" {
		t.ID = idFromPath
	}
	return t, true
}
