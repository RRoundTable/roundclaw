package adapter

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Secret management over the API.
//
// A secret carries a value, unlike an agent definition, so these routes never
// return one: PUT and DELETE change it, GET lists names and timestamps only.
// The value travels in exactly one direction — in on a PUT, into a container at
// turn time — and is encrypted at rest in between.
//
//	GET    /v1/secrets                       list global secret names
//	PUT    /v1/secrets/{name}                set a global secret
//	DELETE /v1/secrets/{name}                remove a global secret
//	GET    /v1/agents/{agent}/secrets        list an agent's secret names
//	PUT    /v1/agents/{agent}/secrets/{name} set an agent secret
//	DELETE /v1/agents/{agent}/secrets/{name} remove an agent secret
//
// Global secrets reach every agent; a per-agent secret of the same name
// overrides the global one for that agent.
func (h *HTTP) registerSecretRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/secrets", h.listGlobalSecrets)
	mux.HandleFunc("PUT /v1/secrets/{name}", h.putGlobalSecret)
	mux.HandleFunc("DELETE /v1/secrets/{name}", h.deleteGlobalSecret)
	mux.HandleFunc("GET /v1/agents/{agent}/secrets", h.listAgentSecrets)
	mux.HandleFunc("PUT /v1/agents/{agent}/secrets/{name}", h.putAgentSecret)
	mux.HandleFunc("DELETE /v1/agents/{agent}/secrets/{name}", h.deleteAgentSecret)
}

type secretBody struct {
	Value string `json:"value"`
}

func (h *HTTP) listGlobalSecrets(w http.ResponseWriter, r *http.Request) {
	h.listSecrets(w, r, "")
}

func (h *HTTP) putGlobalSecret(w http.ResponseWriter, r *http.Request) {
	h.putSecret(w, r, "", r.PathValue("name"))
}

func (h *HTTP) deleteGlobalSecret(w http.ResponseWriter, r *http.Request) {
	h.deleteSecret(w, r, "", r.PathValue("name"))
}

func (h *HTTP) listAgentSecrets(w http.ResponseWriter, r *http.Request) {
	h.listSecrets(w, r, r.PathValue("agent"))
}

func (h *HTTP) putAgentSecret(w http.ResponseWriter, r *http.Request) {
	h.putSecret(w, r, r.PathValue("agent"), r.PathValue("name"))
}

func (h *HTTP) deleteAgentSecret(w http.ResponseWriter, r *http.Request) {
	h.deleteSecret(w, r, r.PathValue("agent"), r.PathValue("name"))
}

func (h *HTTP) listSecrets(w http.ResponseWriter, r *http.Request, scope string) {
	metas, err := h.disp.Registry().ListSecrets(r.Context(), scope)
	if err != nil {
		writeSecretError(w, err)
		return
	}
	if metas == nil {
		metas = []registry.SecretMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": metas})
}

func (h *HTTP) putSecret(w http.ResponseWriter, r *http.Request, scope, name string) {
	var body secretBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	if body.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	if err := h.disp.Registry().PutSecret(r.Context(), scope, name, body.Value); err != nil {
		writeSecretError(w, err)
		return
	}
	// Log the name and scope, never the value.
	h.log.Info("secret set", "scope", scopeLabel(scope), "name", name)
	writeJSON(w, http.StatusOK, map[string]string{"scope": scope, "name": name, "status": "set"})
}

func (h *HTTP) deleteSecret(w http.ResponseWriter, r *http.Request, scope, name string) {
	if err := h.disp.Registry().DeleteSecret(r.Context(), scope, name); err != nil {
		writeSecretError(w, err)
		return
	}
	h.log.Info("secret deleted", "scope", scopeLabel(scope), "name", name)
	writeJSON(w, http.StatusOK, map[string]string{"scope": scope, "name": name, "status": "deleted"})
}

func scopeLabel(scope string) string {
	if scope == "" {
		return "(global)"
	}
	return scope
}

func writeSecretError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrSecretsDisabled):
		// The value cannot be encrypted without a master key, so refuse rather
		// than store plaintext.
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
