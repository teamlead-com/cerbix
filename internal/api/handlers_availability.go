package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
)

const (
	defaultAvailabilityDays = 90
	maxAvailabilityDays     = 365
)

// availabilitySince parses the ?days= window and returns the UTC start-of-day
// cutoff to pass to the store.
func availabilitySince(r *http.Request) time.Time {
	days := defaultAvailabilityDays
	if q := r.URL.Query().Get("days"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			days = n
		}
	}
	if days > maxAvailabilityDays {
		days = maxAvailabilityDays
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -(days - 1))
}

// monitorAvailability returns a monitor's per-day availability for the window.
func (h *Handler) monitorAvailability(w http.ResponseWriter, r *http.Request) {
	mon, ok := h.monitorAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	days, err := h.store.MonitorDailyAvailability(r.Context(), mon.ID, availabilitySince(r))
	if err != nil {
		h.serverError(w, "monitor_availability", err)
		return
	}
	writeJSON(w, http.StatusOK, days)
}

// projectAvailability returns a project's per-day availability for the window.
func (h *Handler) projectAvailability(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	days, err := h.store.ProjectDailyAvailability(r.Context(), proj.ID, availabilitySince(r))
	if err != nil {
		h.serverError(w, "project_availability", err)
		return
	}
	writeJSON(w, http.StatusOK, days)
}
