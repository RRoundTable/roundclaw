package adapter

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/roundtable/roundclaw/internal/registry"
)

// Who the server believes is calling.
//
// Until adr/003 authentication answered one question — may this credential be
// used at all — and authorship was whatever header the caller set. That is a
// changelog, not an audit log, and it cannot carry a gate: anything keyed on "an
// agent changed itself" is escaped by claiming to be a different agent.
//
// A credential now resolves to an identity, and the write surface it opens is
// bounded by subject as well as by path. Bounding by path alone was never enough
// for the routes self-improvement needs, because those are the routes that name
// host paths, and no shape check makes an arbitrary one safe.

// Scope is what a credential may reach.
type Scope int

const (
	// ScopeFull is an operator credential: every route, no subject bound. Its
	// authorship is still asserted rather than established, exactly as before.
	ScopeFull Scope = iota
	// ScopeDelegate is the shared credential an agent container has carried:
	// send requests, read status, manage schedules filed under an agent. It
	// names no agent, which is why it cannot be trusted to say who it is.
	ScopeDelegate
	// ScopeSelf is a per-agent credential. It reaches everything the delegate
	// surface does, plus the configuration of the one agent it names.
	ScopeSelf
)

// Identity is the resolved caller.
type Identity struct {
	Scope Scope
	// AgentID is set only for ScopeSelf, where the credential is one no other
	// agent holds. For every other scope it is empty, and authorship from those
	// callers stays asserted.
	AgentID string
}

// Established reports whether the server knows which agent this is, rather than
// having been told.
func (i Identity) Established() bool { return i.Scope == ScopeSelf && i.AgentID != "" }

type identityKey struct{}

func withIdentity(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
}

// identityFrom returns the caller resolved by the auth middleware.
//
// A request that never passed through it reports ScopeFull, which is what the
// webhook routes and any direct handler test already are: they sit outside
// bearer auth by design and are not subject-bounded.
func identityFrom(r *http.Request) Identity {
	id, ok := r.Context().Value(identityKey{}).(Identity)
	if !ok {
		return Identity{Scope: ScopeFull}
	}
	return id
}

// allowedFor reports whether an identity may reach a route.
//
// Shape only. Whether a self-scoped agent may write *this particular* tool is a
// question about grants, and grants are the handler's business — the same split
// the agent-scoped schedule routes already use, where the list admits the shape
// and the handler bounds the subject.
func allowedFor(id Identity, method, path string) bool {
	switch id.Scope {
	case ScopeFull:
		return true
	case ScopeDelegate:
		return delegateAllowed(method, path)
	case ScopeSelf:
		return delegateAllowed(method, path) || selfAllowed(id.AgentID, method, path)
	}
	return false
}

// selfAllowed is the surface a per-agent credential adds on top of the delegate
// one: its own definition, its own instructions, its own history, and the tools
// and skills it might hold.
//
// The agent routes are bounded here, by comparing the id in the path with the id
// in the credential — a self token for one agent is simply not a credential for
// another. The tool and skill routes cannot be bounded that way because a tool
// belongs to no agent, so they are admitted by shape and refused in the handler
// unless the caller actually holds the tool (see requireHoldsTool).
func selfAllowed(agentID, method, path string) bool {
	if agentID == "" {
		return false
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 2 || segs[0] != "v1" {
		return false
	}

	switch segs[1] {
	case "agents":
		// Its own agent, never another's. This is the subject bound.
		if len(segs) < 3 || segs[2] != agentID {
			return false
		}
		switch method {
		case http.MethodGet:
			// /v1/agents/{self}/definition | persona | versions | versions/{v}
			return len(segs) >= 4 && (segs[3] == "definition" || segs[3] == "persona" || segs[3] == "versions")
		case http.MethodPut:
			// /v1/agents/{self}/definition | persona
			return len(segs) == 4 && (segs[3] == "definition" || segs[3] == "persona")
		case http.MethodPost:
			// /v1/agents/{self}/versions/{v}/rollback — undoing its own change
			return len(segs) == 6 && segs[3] == "versions" && segs[5] == "rollback"
		}
		return false

	case "tools", "skills":
		// Shape only; the handler decides whether this agent holds it. Deleting
		// a registry row is not here: removing a shared tool for everybody is
		// not what "remove it from myself" means, and the grant lives on the
		// agent's own definition, which it can already write.
		switch method {
		case http.MethodGet:
			return true
		case http.MethodPost, http.MethodPut:
			return true
		}
		return false
	}
	return false
}

// mayWriteGrant reports whether the caller may write this tool or skill, and
// writes the refusal if not.
//
// Full and delegate scopes are bounded by allowedFor alone — a delegate never
// reaches here, and an operator is not subject-bounded. A per-agent credential
// is: it may change something it holds, and something that does not exist yet.
//
// "Holds it" rather than "owns it" because a tool is owned by nobody. The spec
// says as much — changing a shared tool changes it for every agent holding it —
// and lists the consequence as a known limit rather than pretending otherwise.
// What this refuses is the case with no defence at all: changing a tool the
// caller does not use, on behalf of the agents that do.
func (h *HTTP) mayWriteGrant(w http.ResponseWriter, r *http.Request, kind, id string) bool {
	caller := identityFrom(r)
	if caller.Scope != ScopeSelf {
		return true
	}
	agent, err := h.disp.Registry().Get(r.Context(), caller.AgentID)
	if err != nil {
		writeAgentError(w, err)
		return false
	}

	held := agent.Tools
	if kind == "skill" {
		held = agent.Skills
	}
	for _, g := range held {
		if g == id {
			return true
		}
	}

	// Something it does not hold is writable only if it does not exist: an agent
	// may register a new capability for itself, but not reach into one already in
	// use by somebody else.
	var exists bool
	if kind == "skill" {
		_, err = h.disp.Registry().GetSkill(r.Context(), id)
	} else {
		_, err = h.disp.Registry().GetTool(r.Context(), id)
	}
	exists = err == nil
	if !exists {
		return true
	}

	writeError(w, http.StatusForbidden,
		"this token may change only the "+kind+"s "+caller.AgentID+" holds; it does not hold "+id)
	return false
}

// gateSelfChange is the half of the measurement gate that runs at write time.
//
// spec/003 says a self-made change does not stick until it has been measured.
// The measuring is asynchronous — a run takes minutes and this write does not —
// so what happens here is the two things that must be true before the change is
// allowed to apply at all: something defines "better" for this agent, and there
// is a completed run of the version being replaced to compare against.
//
// Checked before the write rather than after, so a change that could never be
// judged is refused instead of applied and then left standing forever.
func (h *HTTP) gateSelfChange(w http.ResponseWriter, r *http.Request, agentID string) (registry.GatePlan, bool) {
	if !identityFrom(r).Established() {
		// A person's change is not gated. The gate exists because an agent
		// judging its own output is an agent that drifts; an operator editing an
		// agent is somebody taking responsibility, and making them wait on an
		// eval run would put a delay in front of fixing a wedged agent.
		return registry.GatePlan{}, true
	}
	current, err := h.disp.Registry().LatestVersion(r.Context(), agentID)
	if err != nil {
		writeAgentError(w, err)
		return registry.GatePlan{}, false
	}
	plan, err := h.disp.Registry().PlanGate(r.Context(), agentID, current.Version)
	if err != nil {
		if errors.Is(err, registry.ErrUngateable) {
			writeError(w, http.StatusPreconditionFailed, err.Error())
			return registry.GatePlan{}, false
		}
		writeAgentError(w, err)
		return registry.GatePlan{}, false
	}
	return plan, true
}

// gateSelfGrantChange gates a self-made change to a tool or skill.
//
// A grant write mints no agent version by itself, so there would be nothing for
// a gate to judge — except that an agent version records the tool and skill
// versions it holds. Snapshotting the agent after the grant moved mints a
// version whose grants differ, and that version is what goes on trial. The
// agent's configuration did change; it changed on the far side of a pointer.
//
// Without this a self-made change to a tool's host_path would be the one
// unmeasured way an agent can alter what it is, which is the hole the gate
// exists to close.
func (h *HTTP) gateSelfGrantChange(r *http.Request, plan registry.GatePlan, agentID string) {
	if plan.EvalSetID == "" {
		return
	}
	v, err := h.disp.Registry().Snapshot(r.Context(), agentID,
		registry.Change{Author: agentAuthorPrefix + agentID, Note: "a grant it holds changed"})
	if err != nil {
		h.log.Error("a self-made grant change was applied but could not be versioned",
			"agent", agentID, "error", err)
		return
	}
	h.startSelfChangeGate(r, plan, agentID, v.Version)
}

// startSelfChangeGate puts the version this write just minted on trial.
//
// A failure to start the run is logged and not returned: the change is already
// applied, and reporting a 500 over a written change would invite a retry that
// mints a second identical version. What it costs is that this one version goes
// unjudged, which the run's absence makes visible rather than hiding.
func (h *HTTP) startSelfChangeGate(r *http.Request, plan registry.GatePlan, agentID string, version int) {
	if plan.EvalSetID == "" || version == 0 {
		return
	}
	run, err := h.disp.StartEvalRun(r.Context(), registry.EvalRun{
		EvalSetID:    plan.EvalSetID,
		AgentID:      agentID,
		Version:      version,
		GatesVersion: version,
		BaselineRun:  plan.Baseline,
	})
	if err != nil {
		h.log.Error("a self-made change was applied but its measurement could not be started",
			"agent", agentID, "version", version, "error", err)
		return
	}
	h.log.Info("a self-made change is on trial",
		"agent", agentID, "version", version, "run", run.ID, "baseline", plan.Baseline)
}
