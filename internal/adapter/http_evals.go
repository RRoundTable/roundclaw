package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Evals: measuring an agent, and comparing two measurements.
//
//	GET    /v1/evals                     list sets (?agent= to narrow)
//	POST   /v1/evals                     create or replace a set (id in the body)
//	GET    /v1/evals/{eval}              read a set
//	PUT    /v1/evals/{eval}              create or replace
//	DELETE /v1/evals/{eval}              delete a set (its runs are kept)
//	POST   /v1/evals/{eval}/run          start a run
//	GET    /v1/evals/runs                list runs (?eval=, ?agent=, ?limit=)
//	GET    /v1/evals/runs/{run}          one run with its per-case results
//	GET    /v1/evals/compare?base=&candidate=
//
// The compare route is the reason the others exist. It is arithmetic rather than
// a summary: it decides what counts as a regression so that the agent reading it
// explains the verdict instead of reaching one.
func (h *HTTP) registerEvalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/evals", h.listEvalSets)
	mux.HandleFunc("POST /v1/evals", h.putEvalSet)
	// Registered before the {eval} patterns so that "runs" and "compare" are not
	// read as the name of an eval set.
	mux.HandleFunc("GET /v1/evals/runs", h.listEvalRuns)
	mux.HandleFunc("GET /v1/evals/runs/{run}", h.getEvalRun)
	mux.HandleFunc("GET /v1/evals/compare", h.compareEvalRuns)
	mux.HandleFunc("GET /v1/evals/{eval}", h.getEvalSet)
	mux.HandleFunc("PUT /v1/evals/{eval}", h.putEvalSetByID)
	mux.HandleFunc("DELETE /v1/evals/{eval}", h.deleteEvalSet)
	mux.HandleFunc("POST /v1/evals/{eval}/run", h.runEval)
}

func (h *HTTP) listEvalSets(w http.ResponseWriter, r *http.Request) {
	sets, err := h.disp.Registry().ListEvalSets(r.Context(), r.URL.Query().Get("agent"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sets == nil {
		sets = []registry.EvalSet{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"eval_sets": sets})
}

func (h *HTTP) getEvalSet(w http.ResponseWriter, r *http.Request) {
	set, err := h.disp.Registry().GetEvalSet(r.Context(), r.PathValue("eval"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (h *HTTP) putEvalSet(w http.ResponseWriter, r *http.Request) { h.saveEvalSet(w, r, "") }

func (h *HTTP) putEvalSetByID(w http.ResponseWriter, r *http.Request) {
	h.saveEvalSet(w, r, r.PathValue("eval"))
}

func (h *HTTP) saveEvalSet(w http.ResponseWriter, r *http.Request, idFromPath string) {
	var set registry.EvalSet
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&set); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	if idFromPath != "" {
		set.ID = idFromPath
	}
	// Sets default to enabled; ?disabled parks one without deleting it.
	set.Enabled = !r.URL.Query().Has("disabled")

	saved, err := h.disp.Registry().PutEvalSet(r.Context(), set)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("eval set saved", "eval", saved.ID, "agent", saved.AgentID,
		"cases", len(saved.Cases), "full_grants", saved.FullGrants)
	writeJSON(w, http.StatusOK, saved)
}

func (h *HTTP) deleteEvalSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("eval")
	if err := h.disp.Registry().DeleteEvalSet(r.Context(), id); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"deleted":  id,
		"retained": "its runs, which are the record of how the agent scored",
	})
}

// runEvalBody starts a run.
type runEvalBody struct {
	// Version pins which agent version to evaluate. 0 evaluates whatever is live,
	// which the run then resolves to a concrete number — a run that cannot say
	// what it measured is worthless the moment the agent changes again.
	Version int `json:"version,omitempty"`
	// Notify names who to wake when the run ends. A run outlasts the turn that
	// started it by minutes, so the requester is told rather than made to wait —
	// the same return address a delegated request carries, and the same shape.
	Notify *notifyTarget `json:"notify,omitempty"`
}

func (h *HTTP) runEval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("eval")

	var body runEvalBody
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
			return
		}
	}

	run := registry.EvalRun{EvalSetID: id, Version: body.Version}
	if body.Notify != nil {
		if body.Notify.Agent == "" {
			writeError(w, http.StatusBadRequest, "notify.agent is required when notify is given")
			return
		}
		if _, err := h.disp.requireAgent(r.Context(), body.Notify.Agent); err != nil {
			h.writeLookupError(w, err)
			return
		}
		run.NotifyAgent, run.NotifyConversation = body.Notify.Agent, body.Notify.Conversation
	}

	started, err := h.disp.StartEvalRun(r.Context(), run)
	if errors.Is(err, ErrUnknownAgent) {
		writeError(w, http.StatusNotFound, "no such eval set")
		return
	}
	if err != nil {
		writeAgentError(w, err)
		return
	}

	h.log.Info("eval run started", "run", started.ID, "eval", id,
		"agent", started.AgentID, "version", started.Version, "cases", started.Total)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id":   started.ID,
		"eval_set": started.EvalSetID,
		"agent_id": started.AgentID,
		"version":  started.Version,
		"cases":    started.Total,
		"status":   started.Status,
		"note": "runs take minutes; poll GET /v1/evals/runs/" +
			strconv.FormatInt(started.ID, 10) + " or pass notify to be woken",
	})
}

func (h *HTTP) listEvalRuns(w http.ResponseWriter, r *http.Request) {
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
	runs, err := h.disp.Registry().ListEvalRuns(r.Context(), q.Get("eval"), q.Get("agent"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []registry.EvalRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "count": len(runs)})
}

func (h *HTTP) getEvalRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "run id must be an integer")
		return
	}
	run, err := h.disp.Registry().GetEvalRun(r.Context(), id)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	results, err := h.disp.Registry().EvalResults(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Outputs are whole transcripts; a run of twenty would bury the scores. They
	// are truncated unless asked for, the same bargain the turn history strikes.
	full := r.URL.Query().Get("full") == "true"
	if !full {
		for i := range results {
			results[i].Output, _ = preview(results[i].Output, historyPreview)
		}
	}
	if results == nil {
		results = []registry.EvalResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run": run, "results": results, "truncated": !full,
	})
}

func (h *HTTP) compareEvalRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	base, err := strconv.ParseInt(q.Get("base"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "base must be a run id")
		return
	}
	candidate, err := strconv.ParseInt(q.Get("candidate"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "candidate must be a run id")
		return
	}
	cmp, err := h.disp.Registry().CompareEvalRuns(r.Context(), base, candidate)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cmp)
}
