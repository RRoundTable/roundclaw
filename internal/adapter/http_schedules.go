package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/roundtable/roundclaw/internal/registry"
)

// GET    /v1/schedules            list
// PUT    /v1/schedules/{schedule} create or replace
// GET    /v1/schedules/{schedule} read
// DELETE /v1/schedules/{schedule} delete
// POST   /v1/schedules/{schedule}/pause
// POST   /v1/schedules/{schedule}/resume
//
// The same schedules are also reachable under the agent that owns them:
//
// GET    /v1/agents/{agent}/schedules
// GET    /v1/agents/{agent}/schedules/{schedule}
// PUT    /v1/agents/{agent}/schedules/{schedule}
// DELETE /v1/agents/{agent}/schedules/{schedule}
// POST   /v1/agents/{agent}/schedules/{schedule}/pause
// POST   /v1/agents/{agent}/schedules/{schedule}/resume
//
// That route shape is what lets an agent manage its own recurring work with the
// restricted (delegate) token its container carries. Naming the agent in the
// path is a claim, not a proof — a shared token cannot say who is calling — so
// it is bounded the way notify is (see notifyTarget): the work runs on the named
// agent, its result can only be announced in a channel that agent is already
// bound to, and a schedule belonging to somebody else is neither readable nor
// overwritable. Nothing on this surface reaches secrets, tools or a definition.
func (h *HTTP) registerScheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/schedules", h.listSchedules)
	mux.HandleFunc("GET /v1/schedules/{schedule}", h.getSchedule)
	mux.HandleFunc("PUT /v1/schedules/{schedule}", h.putSchedule)
	mux.HandleFunc("DELETE /v1/schedules/{schedule}", h.deleteSchedule)
	mux.HandleFunc("POST /v1/schedules/{schedule}/pause", h.pauseSchedule)
	mux.HandleFunc("POST /v1/schedules/{schedule}/resume", h.resumeSchedule)

	mux.HandleFunc("GET /v1/agents/{agent}/schedules", h.listAgentSchedules)
	mux.HandleFunc("GET /v1/agents/{agent}/schedules/{schedule}", h.getAgentSchedule)
	mux.HandleFunc("PUT /v1/agents/{agent}/schedules/{schedule}", h.putAgentSchedule)
	mux.HandleFunc("DELETE /v1/agents/{agent}/schedules/{schedule}", h.deleteAgentSchedule)
	mux.HandleFunc("POST /v1/agents/{agent}/schedules/{schedule}/pause", h.pauseAgentSchedule)
	mux.HandleFunc("POST /v1/agents/{agent}/schedules/{schedule}/resume", h.resumeAgentSchedule)
}

func (h *HTTP) listSchedules(w http.ResponseWriter, r *http.Request) {
	views, err := h.disp.ListSchedules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if views == nil {
		views = []ScheduleView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": views})
}

func (h *HTTP) getSchedule(w http.ResponseWriter, r *http.Request) {
	view, err := h.disp.GetSchedule(r.Context(), r.PathValue("schedule"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// scheduleBody is a schedule definition as it arrives on the wire.
//
// Enabled is read separately as a pointer: a plain bool cannot tell an omitted
// field from an explicit false, so a caller updating only a prompt would
// silently pause the schedule.
type scheduleBody struct {
	registry.Schedule
	Enabled *bool `json:"enabled"`
}

func decodeScheduleBody(w http.ResponseWriter, r *http.Request) (scheduleBody, bool) {
	var body scheduleBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return scheduleBody{}, false
	}
	return body, true
}

func (h *HTTP) putSchedule(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeScheduleBody(w, r)
	if !ok {
		return
	}
	s := body.Schedule
	// The path wins, so a mismatched body cannot create a second schedule under
	// a name the caller did not ask for.
	s.ID = r.PathValue("schedule")

	switch {
	case body.Enabled != nil:
		s.Enabled = *body.Enabled
	default:
		// Keep an existing schedule's state; a new one starts running, because
		// creating a schedule that silently does nothing is a trap.
		existing, err := h.disp.GetSchedule(r.Context(), s.ID)
		s.Enabled = err != nil || existing.Enabled
	}

	view, err := h.disp.PutSchedule(r.Context(), s)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("schedule saved", "schedule", view.ID, "agent", view.AgentID,
		"cron", view.Cron, "tz", view.Timezone)
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTP) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("schedule")
	if err := h.disp.DeleteSchedule(r.Context(), id); err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("schedule deleted", "schedule", id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (h *HTTP) pauseSchedule(w http.ResponseWriter, r *http.Request)  { h.setPaused(w, r, true) }
func (h *HTTP) resumeSchedule(w http.ResponseWriter, r *http.Request) { h.setPaused(w, r, false) }

func (h *HTTP) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	view, err := h.disp.SetSchedulePaused(r.Context(), r.PathValue("schedule"), paused, "via api")
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ---- the same schedules, scoped to the agent that owns them ----------------

func (h *HTTP) listAgentSchedules(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	// An agent that does not exist has no schedules, which on the wire reads
	// exactly like an agent that has none yet. Say which it is.
	if _, err := h.disp.Registry().Get(r.Context(), agent); err != nil {
		writeAgentError(w, err)
		return
	}
	all, err := h.disp.ListSchedules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mine := []ScheduleView{}
	for _, v := range all {
		if v.AgentID == agent {
			mine = append(mine, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": mine})
}

func (h *HTTP) getAgentSchedule(w http.ResponseWriter, r *http.Request) {
	view, err := h.ownedSchedule(r.Context(), r.PathValue("agent"), r.PathValue("schedule"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTP) putAgentSchedule(w http.ResponseWriter, r *http.Request) {
	agent, id := r.PathValue("agent"), r.PathValue("schedule")
	body, ok := decodeScheduleBody(w, r)
	if !ok {
		return
	}
	s := body.Schedule
	// The path wins over the body for both fields. Otherwise a request could
	// create a schedule under a name nobody asked for, or hand recurring work to
	// an agent it only claims to be.
	s.ID = id
	s.AgentID = agent

	existing, err := h.disp.GetSchedule(r.Context(), id)
	if err == nil && existing.AgentID != agent {
		// Schedule ids are unique across the fleet, so this is a real collision,
		// not a permissions detail. Replacing it would move somebody else's
		// recurring work onto this agent.
		writeError(w, http.StatusConflict, "schedule "+id+" belongs to another agent")
		return
	}
	switch {
	case body.Enabled != nil:
		s.Enabled = *body.Enabled
	default:
		s.Enabled = err != nil || existing.Enabled
	}

	if err := h.assertOwnChannel(r.Context(), agent, s.ChannelID); err != nil {
		writeAgentError(w, err)
		return
	}

	view, err := h.disp.PutSchedule(r.Context(), s)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("schedule saved", "schedule", view.ID, "agent", view.AgentID,
		"cron", view.Cron, "tz", view.Timezone, "scope", "agent")
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTP) deleteAgentSchedule(w http.ResponseWriter, r *http.Request) {
	agent, id := r.PathValue("agent"), r.PathValue("schedule")
	if _, err := h.ownedSchedule(r.Context(), agent, id); err != nil {
		writeAgentError(w, err)
		return
	}
	if err := h.disp.DeleteSchedule(r.Context(), id); err != nil {
		writeAgentError(w, err)
		return
	}
	h.log.Info("schedule deleted", "schedule", id, "agent", agent)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (h *HTTP) pauseAgentSchedule(w http.ResponseWriter, r *http.Request) {
	h.setAgentPaused(w, r, true)
}

func (h *HTTP) resumeAgentSchedule(w http.ResponseWriter, r *http.Request) {
	h.setAgentPaused(w, r, false)
}

func (h *HTTP) setAgentPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	agent, id := r.PathValue("agent"), r.PathValue("schedule")
	if _, err := h.ownedSchedule(r.Context(), agent, id); err != nil {
		writeAgentError(w, err)
		return
	}
	view, err := h.disp.SetSchedulePaused(r.Context(), id, paused, "via api ("+agent+")")
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ownedSchedule reads a schedule and hides one that belongs to another agent.
//
// Not-found rather than forbidden: from this agent's side of the fence the
// schedule does not exist, and a distinct "it exists but is not yours" would map
// the fleet's schedule names for anyone holding the shared delegate token.
func (h *HTTP) ownedSchedule(ctx context.Context, agent, id string) (ScheduleView, error) {
	view, err := h.disp.GetSchedule(ctx, id)
	if err != nil {
		return ScheduleView{}, err
	}
	if view.AgentID != agent {
		return ScheduleView{}, fmt.Errorf("%w: no schedule %s for agent %s", registry.ErrNotFound, id, agent)
	}
	return view, nil
}

// assertOwnChannel refuses a delivery channel the agent is not bound to.
//
// A schedule names where its result is announced. Without this, naming a channel
// id would be enough to post into any channel the bot can see, on a timer — and
// the caller's identity on this surface is a claim, not a proof. Empty is
// allowed: that is a job whose output is a side effect, recorded and not
// announced.
func (h *HTTP) assertOwnChannel(ctx context.Context, agentID, channelID string) error {
	if channelID == "" {
		return nil
	}
	agent, err := h.disp.Registry().Get(ctx, agentID)
	if err != nil {
		return err
	}
	if slices.Contains(agent.DiscordChannels, channelID) {
		return nil
	}
	if len(agent.DiscordChannels) == 0 {
		return fmt.Errorf("agent %s is bound to no channel, so its schedule cannot report into %s",
			agentID, channelID)
	}
	return fmt.Errorf("channel %s is not bound to agent %s; its channels are %s",
		channelID, agentID, strings.Join(agent.DiscordChannels, ", "))
}
