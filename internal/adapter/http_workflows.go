package adapter

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Workflow management and manual runs.
//
//	GET    /v1/workflows                    list
//	POST   /v1/workflows                    create or replace (id in the body)
//	GET    /v1/workflows/{workflow}         read
//	PUT    /v1/workflows/{workflow}         create or replace
//	DELETE /v1/workflows/{workflow}         delete
//	POST   /v1/workflows/{workflow}/run     start one run now
func (h *HTTP) registerWorkflowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workflows", h.listWorkflows)
	mux.HandleFunc("POST /v1/workflows", h.putWorkflow)
	mux.HandleFunc("GET /v1/workflows/{workflow}", h.getWorkflowDef)
	mux.HandleFunc("PUT /v1/workflows/{workflow}", h.putWorkflowByID)
	mux.HandleFunc("DELETE /v1/workflows/{workflow}", h.deleteWorkflow)
	mux.HandleFunc("POST /v1/workflows/{workflow}/run", h.runWorkflow)
}

func (h *HTTP) listWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := h.disp.Registry().ListWorkflows(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wfs == nil {
		wfs = []registry.Workflow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": wfs})
}

func (h *HTTP) putWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := decodeWorkflow(w, r, "")
	if !ok {
		return
	}
	h.saveWorkflow(w, r, wf)
}

func (h *HTTP) putWorkflowByID(w http.ResponseWriter, r *http.Request) {
	wf, ok := decodeWorkflow(w, r, r.PathValue("workflow"))
	if !ok {
		return
	}
	h.saveWorkflow(w, r, wf)
}

func (h *HTTP) saveWorkflow(w http.ResponseWriter, r *http.Request, wf registry.Workflow) {
	// Workflows default to enabled; ?disabled creates or leaves one paused.
	wf.Enabled = !r.URL.Query().Has("disabled")
	saved, err := h.disp.Registry().PutWorkflow(r.Context(), wf)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.log.Info("workflow saved", "workflow", saved.ID, "steps", len(saved.Steps))
	writeJSON(w, http.StatusOK, saved)
}

func (h *HTTP) getWorkflowDef(w http.ResponseWriter, r *http.Request) {
	wf, err := h.disp.Registry().GetWorkflow(r.Context(), r.PathValue("workflow"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such workflow")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (h *HTTP) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	err := h.disp.Registry().DeleteWorkflow(r.Context(), r.PathValue("workflow"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such workflow")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("workflow")})
}

func (h *HTTP) runWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("workflow")
	execID, err := h.disp.RunWorkflow(r.Context(), id)
	if errors.Is(err, ErrUnknownAgent) {
		writeError(w, http.StatusNotFound, "no such workflow")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.log.Info("workflow run started", "workflow", id, "execution", execID)
	writeJSON(w, http.StatusAccepted, map[string]string{"workflow": id, "execution_id": execID, "status": "started"})
}

func decodeWorkflow(w http.ResponseWriter, r *http.Request, idFromPath string) (registry.Workflow, bool) {
	var wf registry.Workflow
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&wf); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return registry.Workflow{}, false
	}
	if idFromPath != "" {
		wf.ID = idFromPath
	}
	return wf, true
}
