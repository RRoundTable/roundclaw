package adapter

import (
	"encoding/json"
	"net/http"

	"github.com/roundtable/roundclaw/internal/registry"
)

// GET    /v1/schedules            list
// PUT    /v1/schedules/{schedule} create or replace
// GET    /v1/schedules/{schedule} read
// DELETE /v1/schedules/{schedule} delete
// POST   /v1/schedules/{schedule}/pause
// POST   /v1/schedules/{schedule}/resume
func (h *HTTP) registerScheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/schedules", h.listSchedules)
	mux.HandleFunc("GET /v1/schedules/{schedule}", h.getSchedule)
	mux.HandleFunc("PUT /v1/schedules/{schedule}", h.putSchedule)
	mux.HandleFunc("DELETE /v1/schedules/{schedule}", h.deleteSchedule)
	mux.HandleFunc("POST /v1/schedules/{schedule}/pause", h.pauseSchedule)
	mux.HandleFunc("POST /v1/schedules/{schedule}/resume", h.resumeSchedule)
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

func (h *HTTP) putSchedule(w http.ResponseWriter, r *http.Request) {
	// Enabled is read separately as a pointer: a plain bool cannot tell an
	// omitted field from an explicit false, so a caller updating only a prompt
	// would silently pause the schedule.
	var body struct {
		registry.Schedule
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
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
