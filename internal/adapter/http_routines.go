package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/roundtable/roundclaw/internal/core"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
)

// Routines: the library agents hand known work to (SPEC-004).
//
//	GET  /v1/routines                               library, with each routine's record
//	POST /v1/routines                               submit a routine (compiled at admission)
//	GET  /v1/routines/{routine}                     contract, versions, figures, decisions
//	PUT  /v1/routines/{routine}                     a new version (the contract is frozen)
//	GET  /v1/routines/{routine}/versions/{version}  one version
//	POST /v1/routines/{routine}/versions/{version}/rollback
//	POST /v1/routines/{routine}/archive | restore
//	POST /v1/routines/{routine}/runs                hand work off
//	GET  /v1/routines/{routine}/runs                its runs, newest first
//	GET  /v1/routine-runs/{run}                     a run and its step trace
//	POST /v1/routine-runs/{run}/stop                stop it; never inferred
func (h *HTTP) registerRoutineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/routines", h.listRoutines)
	mux.HandleFunc("POST /v1/routines", h.putRoutine)
	mux.HandleFunc("GET /v1/routines/{routine}", h.getRoutine)
	mux.HandleFunc("PUT /v1/routines/{routine}", h.putRoutine)
	mux.HandleFunc("GET /v1/routines/{routine}/versions/{version}", h.getRoutineVersion)
	mux.HandleFunc("POST /v1/routines/{routine}/versions/{version}/rollback", h.rollbackRoutine)
	mux.HandleFunc("POST /v1/routines/{routine}/archive", h.archiveRoutine)
	mux.HandleFunc("POST /v1/routines/{routine}/restore", h.restoreRoutine)
	mux.HandleFunc("POST /v1/routines/{routine}/runs", h.runRoutine)
	mux.HandleFunc("GET /v1/routines/{routine}/runs", h.listRoutineRuns)
	mux.HandleFunc("GET /v1/routine-runs/{run}", h.getRoutineRun)
	mux.HandleFunc("POST /v1/routine-runs/{run}/stop", h.stopRoutineRun)
	h.registerLifecycleRoutes(mux)
}

// libraryEntry is what an agent sees when it chooses: what the routine is for,
// what it takes, what it may do, whether it can run, and its record by
// standard. Every figure is counted from recorded runs.
type libraryEntry struct {
	ID       string                  `json:"id"`
	Intent   string                  `json:"intent"`
	Inputs   []string                `json:"inputs,omitempty"`
	Effects  routine.Effect          `json:"effects"`
	Owner    string                  `json:"owner,omitempty"`
	State    string                  `json:"state"`
	Version  int                     `json:"version"`
	Steps    int                     `json:"steps"`
	Runnable bool                    `json:"runnable"`
	Why      string                  `json:"why_not_runnable,omitempty"`
	Current  registry.RoutineFigures `json:"current"`
}

func (h *HTTP) libraryEntry(r *http.Request, rt registry.Routine) (libraryEntry, error) {
	cur, _, err := h.disp.Registry().RoutineMetrics(r.Context(), rt.ID, rt.Version)
	if err != nil {
		return libraryEntry{}, err
	}
	e := libraryEntry{
		ID: rt.ID, Intent: rt.Contract.Intent, Inputs: rt.Contract.Inputs, Effects: rt.Contract.Effects,
		Owner: rt.Owner, State: rt.State, Version: rt.Version, Steps: len(rt.Plan.Source.Steps),
		Runnable: true, Current: cur,
	}
	if e.Effects == "" {
		e.Effects = routine.EffectNone
	}
	// Never told a capability works when it does not (invariant 10): a routine
	// whose agents are gone is shown as unable to run, and why.
	for _, st := range rt.Plan.Source.Steps {
		if st.Kind != routine.KindAgent {
			continue
		}
		a, err := h.disp.Registry().Get(r.Context(), st.Agent)
		if err != nil || !a.Enabled {
			e.Runnable, e.Why = false, "agent "+st.Agent+" is missing or disabled"
			break
		}
	}
	if rt.State != registry.RoutineActive {
		e.Runnable, e.Why = false, "archived"
	}
	return e, nil
}

func (h *HTTP) listRoutines(w http.ResponseWriter, r *http.Request) {
	routines, err := h.disp.Registry().ListRoutines(r.Context(), !r.URL.Query().Has("all"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]libraryEntry, 0, len(routines))
	for _, rt := range routines {
		e, err := h.libraryEntry(r, rt)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"routines": out})
}

func (h *HTTP) getRoutine(w http.ResponseWriter, r *http.Request) {
	reg := h.disp.Registry()
	rt, err := reg.GetRoutine(r.Context(), r.PathValue("routine"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	versions, err := reg.ListRoutineVersions(r.Context(), rt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type versionSummary struct {
		Version   int       `json:"version"`
		PlanHash  string    `json:"plan_hash"`
		Author    string    `json:"author,omitempty"`
		Note      string    `json:"note,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	}
	summaries := make([]versionSummary, 0, len(versions))
	for _, v := range versions {
		summaries = append(summaries, versionSummary{v.Version, v.PlanHash, v.Author, v.Note, v.CreatedAt})
	}
	cur, life, err := reg.RoutineMetrics(r.Context(), rt.ID, rt.Version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	decisions, err := reg.ListRoutineDecisions(r.Context(), rt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"routine": rt, "versions": summaries,
		"figures":   map[string]any{"current": cur, "lifetime": life},
		"decisions": decisions,
	})
}

func (h *HTTP) getRoutineVersion(w http.ResponseWriter, r *http.Request) {
	n, ok := versionNumber(w, r)
	if !ok {
		return
	}
	v, err := h.disp.Registry().GetRoutineVersion(r.Context(), r.PathValue("routine"), n)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// mayChangeRoutine is the owner bound for self credentials.
func (h *HTTP) mayChangeRoutine(w http.ResponseWriter, r *http.Request) (registry.Routine, bool) {
	rt, err := h.disp.Registry().GetRoutine(r.Context(), r.PathValue("routine"))
	if err != nil {
		writeRegistryError(w, err)
		return rt, false
	}
	if id := identityFrom(r); id.Scope == ScopeSelf && rt.Owner != id.AgentID {
		writeError(w, http.StatusForbidden, "only its owner or a person changes this routine")
		return rt, false
	}
	return rt, true
}

func (h *HTTP) rollbackRoutine(w http.ResponseWriter, r *http.Request) {
	n, ok := versionNumber(w, r)
	if !ok {
		return
	}
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	if _, ok := h.mayChangeRoutine(w, r); !ok {
		return
	}
	res, err := h.disp.Registry().RollbackRoutine(r.Context(), r.PathValue("routine"), n, c)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *HTTP) archiveRoutine(w http.ResponseWriter, r *http.Request) {
	h.setRoutineState(w, r, registry.RoutineArchived)
}

func (h *HTTP) restoreRoutine(w http.ResponseWriter, r *http.Request) {
	h.setRoutineState(w, r, registry.RoutineActive)
}

// setRoutineState archives or restores at once. It is the control path, and it
// never waits on a measurement.
func (h *HTTP) setRoutineState(w http.ResponseWriter, r *http.Request, state string) {
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	rt, ok := h.mayChangeRoutine(w, r)
	if !ok {
		return
	}
	rule := "archived explicitly"
	if state == registry.RoutineActive {
		rule = "restored explicitly"
	}
	if c.Note != "" {
		rule += ": " + c.Note
	}
	if err := h.disp.Registry().SetRoutineState(r.Context(), rt.ID, state, rule, nil, c.Author); err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"routine": rt.ID, "state": state})
}

type routineRunBody struct {
	Inputs    map[string]string `json:"inputs"`
	Expect    routine.Condition `json:"expect"`
	Notify    *notifyTarget     `json:"notify,omitempty"`
	InsteadOf int64             `json:"instead_of,omitempty"`
}

// runRoutine hands work to a routine and answers at once; the result arrives
// later wherever the request said.
func (h *HTTP) runRoutine(w http.ResponseWriter, r *http.Request) {
	var body routineRunBody
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid run request: "+err.Error())
		return
	}
	id := identityFrom(r)
	req := RoutineRunRequest{
		RoutineID: r.PathValue("routine"), Inputs: body.Inputs, Origin: core.HTTPPollOrigin(),
		Caller: id.AgentID, Full: id.Scope == ScopeFull, Expect: body.Expect, InsteadOf: body.InsteadOf,
		RequestedBy: id.AgentID,
	}
	if body.Notify != nil {
		origin, err := h.resolveNotify(r.Context(), "", "", *body.Notify)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Origin = origin
		if req.RequestedBy == "" {
			// Claimed, not proved, for a shared delegate token — the same bound
			// delegation already lives with.
			req.RequestedBy = body.Notify.Agent
		}
	}
	run, err := h.disp.StartRoutineRun(r.Context(), req)
	switch {
	case errors.Is(err, ErrRoutineInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrRoutineArchived):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrRoutineAuthority):
		writeError(w, http.StatusForbidden, err.Error())
	case err != nil:
		writeRegistryError(w, err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.ID, "routine": run.RoutineID, "version": run.Version, "status": run.Status})
	}
}

func (h *HTTP) listRoutineRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runs, err := h.disp.Registry().ListRoutineRuns(r.Context(), registry.RunFilter{
		RoutineID: r.PathValue("routine"), RequestedBy: q.Get("requested_by"),
		Status: q.Get("status"), Limit: intParam(r, "limit", 20),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []registry.RoutineRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// compactOutput is how much of an output a trace shows unless ?full is asked:
// inspection must not flood whoever is reading it.
const compactOutput = 400

// getRoutineRun reads a run and its step trace straight from the registry. It
// never touches Temporal or a model, so it answers while the run is stuck.
func (h *HTTP) getRoutineRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "run must be a number")
		return
	}
	reg := h.disp.Registry()
	run, err := reg.GetRoutineRun(r.Context(), runID)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	steps, err := reg.ListStepRuns(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !r.URL.Query().Has("full") {
		run.Output = shorten(run.Output, compactOutput)
		for i := range steps {
			steps[i].Output = shorten(steps[i].Output, compactOutput)
		}
	}
	if steps == nil {
		steps = []registry.StepRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "steps": steps})
}

func (h *HTTP) stopRoutineRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "run must be a number")
		return
	}
	if err := h.disp.StopRoutineRun(r.Context(), runID); err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "status": "stopping"})
}

func shorten(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func writeRegistryError(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
