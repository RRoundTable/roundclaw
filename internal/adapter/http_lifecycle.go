package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/roundtable/roundclaw/internal/lifecycle"
	"github.com/roundtable/roundclaw/internal/registry"
	"github.com/roundtable/roundclaw/internal/routine"
)

// The routine lifecycle over HTTP (adr/013).
//
//	POST /v1/routine-candidates/{candidate}/draft   the steward drafts a routine from recurring work
//	GET  /v1/routine-candidates                     recurring work found, and what became of it
//	GET  /v1/routine-trials/{trial}                 a trial, its cases and its verdict
//	GET  /v1/routines/{routine}/trials              a routine's trials
func (h *HTTP) registerLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/routine-candidates/{candidate}/draft", h.draftCandidate)
	mux.HandleFunc("GET /v1/routine-candidates", h.listCandidates)
	mux.HandleFunc("GET /v1/routine-trials/{trial}", h.getTrial)
	mux.HandleFunc("GET /v1/routines/{routine}/trials", h.listRoutineTrials)
}

// submittedCase is one case a routine is tried on. A case drawn from recorded
// work names its turn; a case written by a submitter states the request and the
// outcome that would be accepted, for the examiner to write checks from.
type submittedCase struct {
	Name    string            `json:"name,omitempty"`
	Inputs  map[string]string `json:"inputs"`
	Agent   string            `json:"agent,omitempty"`
	TurnID  int64             `json:"turn_id,omitempty"`
	Request string            `json:"request,omitempty"`
	Outcome string            `json:"outcome,omitempty"`
}

type routineSubmission struct {
	Routine *routine.Source `json:"routine"`
	Cases   []submittedCase `json:"cases"`
}

// decodeSubmission accepts a bare routine, or a routine with the cases it is to
// be tried on.
func decodeSubmission(w http.ResponseWriter, r *http.Request) (routine.Source, []submittedCase, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid routine: "+err.Error())
		return routine.Source{}, nil, false
	}
	var sub routineSubmission
	if err := json.Unmarshal(raw, &sub); err == nil && sub.Routine != nil {
		return *sub.Routine, sub.Cases, true
	}
	var src routine.Source
	if err := json.Unmarshal(raw, &src); err != nil {
		writeError(w, http.StatusBadRequest, "invalid routine: "+err.Error())
		return routine.Source{}, nil, false
	}
	return src, nil, true
}

func (h *HTTP) compileOr400(w http.ResponseWriter, r *http.Request, src routine.Source) (routine.Plan, bool) {
	plan, err := h.disp.CompileRoutine(r.Context(), src)
	var cerr *routine.Error
	if errors.As(err, &cerr) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "routine refused", "diagnostics": cerr.Diagnostics})
		return plan, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return plan, false
	}
	return plan, true
}

func (h *HTTP) isSteward(id Identity) bool {
	return id.Scope == ScopeSelf && id.AgentID != "" && id.AgentID == h.disp.cfg.Routines.Steward
}

// putRoutine submits a routine or a new version of one. Nothing an agent
// submits serves before it passes a trial: a new routine goes to an admission
// trial on the cases it brings, a new version to a version trial on the
// routine's independent cases and its recent failures. A person may admit or
// promote directly — the control path — and the record says so (adr/009,
// adr/013).
func (h *HTTP) putRoutine(w http.ResponseWriter, r *http.Request) {
	src, cases, ok := decodeSubmission(w, r)
	if !ok {
		return
	}
	if pathID := r.PathValue("routine"); pathID != "" {
		if src.ID != "" && src.ID != pathID {
			writeError(w, http.StatusBadRequest, "the routine id in the body does not match the path")
			return
		}
		src.ID = pathID
	}
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}
	id := identityFrom(r)
	person := id.Scope == ScopeFull
	reg := h.disp.Registry()

	existing, err := reg.GetRoutine(r.Context(), src.ID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		owner := r.URL.Query().Get("owner")
		if id.Scope == ScopeSelf {
			owner = id.AgentID
		}
		plan, ok := h.compileOr400(w, r, src)
		if !ok {
			return
		}
		if person && len(cases) == 0 {
			h.putDirect(w, r, plan, owner, c)
			return
		}
		h.admissionTrial(w, r, plan, owner, c, cases, 0)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if id.Scope == ScopeSelf && existing.Owner != id.AgentID && !h.isSteward(id) {
		writeError(w, http.StatusForbidden, "only its owner, the steward or a person changes this routine; file a proposal")
		return
	}
	if existing.State == registry.RoutineTrial {
		writeError(w, http.StatusConflict, fmt.Sprintf("%s is on its admission trial; wait for the verdict", existing.ID))
		return
	}
	plan, ok := h.compileOr400(w, r, src)
	if !ok {
		return
	}
	if existing.State == registry.RoutineArchived || existing.State == registry.RoutineRejected {
		if person && len(cases) == 0 {
			h.putDirect(w, r, plan, existing.Owner, c)
			return
		}
		// Coming back is admission again: tried on its cases, with its history.
		h.admissionTrial(w, r, plan, existing.Owner, c, cases, 0)
		return
	}
	if person && !r.URL.Query().Has("trial") {
		h.putDirect(w, r, plan, existing.Owner, c)
		return
	}
	h.versionTrial(w, r, existing, plan, c)
}

// putDirect records a routine or version that serves at once: a person's
// admission or promotion, the control path. The decision records it as made
// without a trial.
func (h *HTTP) putDirect(w http.ResponseWriter, r *http.Request, plan routine.Plan, owner string, c registry.Change) {
	res, err := h.disp.Registry().PutRoutine(r.Context(), plan, owner, c)
	h.writePut(w, res, err)
}

func (h *HTTP) writePut(w http.ResponseWriter, res registry.PutResult, err error) {
	if errors.Is(err, registry.ErrContractFrozen) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

// admissionTrial tries a routine an agent or a person submitted, on the cases
// it brought. A submitted case states its request and an outcome that would be
// accepted; the examiner writes its checks from those, never from the routine.
func (h *HTTP) admissionTrial(w http.ResponseWriter, r *http.Request, plan routine.Plan, owner string,
	c registry.Change, submitted []submittedCase, candidateID int64) {
	var cases []registry.TrialCase
	for i, sc := range submitted {
		name := sc.Name
		if name == "" {
			name = fmt.Sprintf("case-%d", i+1)
		}
		if strings.TrimSpace(sc.Request) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("case %q must state the request it stands for", name))
			return
		}
		cases = append(cases, registry.TrialCase{Name: name, Inputs: sc.Inputs,
			Source: registry.CaseSource{Request: sc.Request, Outcome: sc.Outcome}})
	}
	h.admit(w, r, plan, owner, c, cases, candidateID)
}

func (h *HTTP) admit(w http.ResponseWriter, r *http.Request, plan routine.Plan, owner string,
	c registry.Change, cases []registry.TrialCase, candidateID int64) {
	rules := h.disp.cfg.Routines.Lifecycle
	if len(cases) < rules.MinTrialCases {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"a routine is admitted only through a trial; submit at least %d cases to try it on", rules.MinTrialCases))
		return
	}
	reg := h.disp.Registry()
	res, err := reg.PutRoutineWith(r.Context(), plan, owner, c, registry.PutOptions{State: registry.RoutineTrial, Promote: false})
	if errors.Is(err, registry.ErrContractFrozen) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	version := res.Version
	if !res.Minted {
		version = res.Routine.PendingVersion
		if version == 0 {
			version = res.Routine.Version
		}
	}
	t, err := reg.CreateTrial(r.Context(), registry.Trial{
		Kind: registry.TrialAdmission, RoutineID: plan.Source.ID, Version: version, CandidateID: candidateID, Cases: cases,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if candidateID != 0 {
		if err := reg.UpdateCandidate(r.Context(), candidateID, registry.CandidateOnTrial, plan.Source.ID, "", true); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := h.disp.StartTrial(r.Context(), t.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"routine": plan.Source.ID, "version": version, "trial_id": t.ID, "state": registry.RoutineTrial})
}

// versionTrial records a new version on trial and starts comparing it with the
// current one on the routine's independent cases and its recent failures.
func (h *HTTP) versionTrial(w http.ResponseWriter, r *http.Request, rt registry.Routine, plan routine.Plan, c registry.Change) {
	reg := h.disp.Registry()
	if rt.PendingVersion != 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("v%d of %s is already on trial", rt.PendingVersion, rt.ID))
		return
	}
	res, err := reg.PutRoutineWith(r.Context(), plan, rt.Owner, c, registry.PutOptions{Promote: false})
	if errors.Is(err, registry.ErrContractFrozen) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !res.Minted {
		writeJSON(w, http.StatusOK, res)
		return
	}
	cases, err := h.versionTrialCases(r, rt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(cases) == 0 {
		rule := "no cases to try it against; a person can promote it"
		if err := reg.RejectRoutineVersion(r.Context(), rt.ID, res.Version, rule, nil); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeError(w, http.StatusConflict, fmt.Sprintf("v%d of %s was not tried: %s", res.Version, rt.ID, rule))
		return
	}
	t, err := reg.CreateTrial(r.Context(), registry.Trial{
		Kind: registry.TrialVersion, RoutineID: rt.ID, Version: res.Version, BaseVersion: rt.Version, Cases: cases,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.disp.StartTrial(r.Context(), t.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"routine": rt.ID, "version": res.Version, "trial_id": t.ID, "state": "on_trial"})
}

// versionTrialCases are the routine's independent cases, and its recent failed
// runs — marked targeted — checked by what their callers and the contract asked.
func (h *HTTP) versionTrialCases(r *http.Request, rt registry.Routine) ([]registry.TrialCase, error) {
	reg := h.disp.Registry()
	independent, err := reg.ListRoutineCases(r.Context(), rt.ID)
	if err != nil {
		return nil, err
	}
	var out []registry.TrialCase
	for _, c := range independent {
		out = append(out, registry.TrialCase{Name: c.Name, Inputs: c.Inputs, Checks: c.Checks, Source: c.Source})
	}
	runs, err := reg.ListRoutineRuns(r.Context(), registry.RunFilter{RoutineID: rt.ID, Limit: 100})
	if err != nil {
		return nil, err
	}
	max := h.disp.cfg.Routines.Lifecycle.MaxRecentFailures
	for _, run := range runs {
		if len(out) > 0 && countTargeted(out) >= max {
			break
		}
		if run.Version != rt.Version || strings.HasPrefix(run.RequestedBy, registry.TrialRequester) ||
			(run.Status != registry.RunFailed && (run.Expect.Empty() || routine.Held(run.ExpectVerdicts))) {
			continue
		}
		checks := rt.Contract.Done
		checks.MustContain = append(append([]string{}, checks.MustContain...), run.Expect.MustContain...)
		checks.MustNotContain = append(append([]string{}, checks.MustNotContain...), run.Expect.MustNotContain...)
		out = append(out, registry.TrialCase{
			Name: fmt.Sprintf("failed-run-%d", run.ID), Inputs: run.Inputs, Checks: checks, Targeted: true,
		})
	}
	return out, nil
}

func countTargeted(cs []registry.TrialCase) int {
	n := 0
	for _, c := range cs {
		if c.Targeted {
			n++
		}
	}
	return n
}

// draftCandidate is how the steward turns recurring work into a routine. Its
// cases are the recorded turns the work came from; the examiner writes their
// checks and the trial compares the routine with how the work was done. A draft
// whose contract matches a routine archived or rejected before brings that
// routine back through its trial, with its history, rather than a duplicate.
func (h *HTTP) draftCandidate(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	if id.Scope != ScopeFull && !h.isSteward(id) {
		writeError(w, http.StatusForbidden, "only the steward or a person drafts routines from recurring work")
		return
	}
	candID, err := strconv.ParseInt(r.PathValue("candidate"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "candidate must be a number")
		return
	}
	reg := h.disp.Registry()
	cand, err := reg.GetCandidate(r.Context(), candID)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	if cand.State != registry.CandidateWaiting {
		writeError(w, http.StatusConflict, fmt.Sprintf("candidate %d is %s", cand.ID, cand.State))
		return
	}
	src, submitted, ok := decodeSubmission(w, r)
	if !ok {
		return
	}
	c, ok := changeFrom(w, r)
	if !ok {
		return
	}

	evidence := map[string]lifecycle.Turn{}
	owners := map[string]int{}
	for _, raw := range cand.Evidence {
		var t lifecycle.Turn
		if err := json.Unmarshal(raw, &t); err != nil {
			writeError(w, http.StatusInternalServerError, "decode evidence: "+err.Error())
			return
		}
		evidence[fmt.Sprintf("%s/%d", t.Agent, t.TurnID)] = t
		owners[t.Agent]++
	}
	var cases []registry.TrialCase
	for i, sc := range submitted {
		t, found := evidence[fmt.Sprintf("%s/%d", sc.Agent, sc.TurnID)]
		if !found {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("case %d names turn %s/%d, which is not in the candidate's evidence", i, sc.Agent, sc.TurnID))
			return
		}
		name := sc.Name
		if name == "" {
			name = fmt.Sprintf("%s-turn-%d", t.Agent, t.TurnID)
		}
		cases = append(cases, registry.TrialCase{Name: name, Inputs: sc.Inputs, Source: registry.CaseSource{
			Agent: t.Agent, TurnID: t.TurnID, Request: t.Request, Outcome: t.Result, CostUSD: t.CostUSD,
		}})
	}

	plan, ok := h.compileOr400(w, r, src)
	if !ok {
		return
	}
	contractHash, err := routine.ContractHash(plan.Source.Contract)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if prior, found, err := reg.FindByContract(r.Context(), contractHash); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if found && prior != src.ID {
		src.ID = prior
		if plan, ok = h.compileOr400(w, r, src); !ok {
			return
		}
	}
	owner := ""
	for agent, n := range owners {
		if n > owners[owner] || (n == owners[owner] && agent < owner) {
			owner = agent
		}
	}
	h.admit(w, r, plan, owner, c, cases, cand.ID)
}

func (h *HTTP) listCandidates(w http.ResponseWriter, r *http.Request) {
	cs, err := h.disp.Registry().ListCandidates(r.Context(), intParam(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": cs})
}

func (h *HTTP) getTrial(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("trial"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "trial must be a number")
		return
	}
	t, err := h.disp.Registry().GetTrial(r.Context(), id)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *HTTP) listRoutineTrials(w http.ResponseWriter, r *http.Request) {
	ts, err := h.disp.Registry().ListTrials(r.Context(), r.PathValue("routine"), intParam(r, "limit", 20))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trials": ts})
}
