package api

import (
	"errors"
	"net/http"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// The read side of FR-041a: the resolver behind the SPA's /i/:ref route (D21), an incident's phone
// activity and a channel's phone deliveries (§4.5, §5.5, R13). No response carries a number: a
// delivery has its mask and nothing else (P3).

// channelPhoneDeliveriesLimit is how many of a channel's last deliveries are returned (§4.5).
const channelPhoneDeliveriesLimit = 50

// getIncidentSubroute serves the GET routes below /api/v1/incidents/ that have two segments:
// `by-ref/{ref}` and `{incidentID}/{updates|postmortem|phone-activity}`. See Router for why one
// pattern carries them. A GET of any other two-segment path is answered 404 here — including a
// POST-only subpath such as `{incidentID}/acknowledge`, which therefore answers 404 rather than
// the mux's 405 (TestIncidentSubrouteMethodAnswers).
func (h *Handler) getIncidentSubroute(w http.ResponseWriter, r *http.Request) {
	first, second := r.PathValue("first"), r.PathValue("second")
	if first == "by-ref" {
		r.SetPathValue("ref", second)
		h.getIncidentByRef(w, r)
		return
	}
	var serve http.HandlerFunc
	switch second {
	case "updates":
		serve = h.listIncidentUpdates
	case "postmortem":
		serve = h.getPostmortem
	case "phone-activity":
		serve = h.getIncidentPhoneActivity
	default:
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("incidentID", first)
	serve(w, r)
}

// getIncidentByRef resolves a short reference to its incident, project and organization under
// getIncident's two steps: a malformed, unknown or invisible reference is the same 404, and only an
// incident the principal can see but not read answers 403.
func (h *Handler) getIncidentByRef(w http.ResponseWriter, r *http.Request) {
	inc, err := h.store.IncidentByRef(r.Context(), r.PathValue("ref"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "incident_by_ref", err)
		return
	}
	proj, ok := h.projectAccess(w, r, inc.ProjectID, authz.ActionProjectRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		IncidentID string `json:"incident_id"`
		ProjectID  string `json:"project_id"`
		OrgID      string `json:"org_id"`
	}{inc.ID, proj.ID, proj.OrgID})
}

// phoneDeliveriesBody is the body of both delivery reads. `actions` joins it with part (b) (R13).
type phoneDeliveriesBody struct {
	Deliveries []domain.PhoneDeliveryView `json:"deliveries"`
}

// getIncidentPhoneActivity lists an incident's phone deliveries (project:read).
func (h *Handler) getIncidentPhoneActivity(w http.ResponseWriter, r *http.Request) {
	inc, ok := h.incidentAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	deliveries, err := h.store.ListIncidentPhoneDeliveries(r.Context(), inc.ID)
	if err != nil {
		h.serverError(w, "list_incident_phone_deliveries", err)
		return
	}
	writeJSON(w, http.StatusOK, phoneDeliveriesBody{Deliveries: nonNilDeliveries(deliveries)})
}

// listChannelPhoneDeliveries lists a channel's last phone deliveries (project:read).
func (h *Handler) listChannelPhoneDeliveries(w http.ResponseWriter, r *http.Request) {
	ch, err := h.store.GetNotificationChannel(r.Context(), r.PathValue("channelID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		h.serverError(w, "get_channel", err)
		return
	}
	if _, ok := h.projectAccess(w, r, ch.ProjectID, authz.ActionProjectRead); !ok {
		return
	}
	deliveries, err := h.store.ListChannelPhoneDeliveries(r.Context(), ch.ID, channelPhoneDeliveriesLimit)
	if err != nil {
		h.serverError(w, "list_channel_phone_deliveries", err)
		return
	}
	writeJSON(w, http.StatusOK, phoneDeliveriesBody{Deliveries: nonNilDeliveries(deliveries)})
}

// nonNilDeliveries makes an empty answer `[]`, never `null`.
func nonNilDeliveries(d []domain.PhoneDeliveryView) []domain.PhoneDeliveryView {
	if d == nil {
		return []domain.PhoneDeliveryView{}
	}
	return d
}
