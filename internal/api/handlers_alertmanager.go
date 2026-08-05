package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// alertmanagerPayload is the subset of Prometheus Alertmanager's webhook payload
// (schema version 4) that the receiver consumes.
type alertmanagerPayload struct {
	Alerts []alertmanagerAlert `json:"alerts"`
}

type alertmanagerAlert struct {
	Status      string            `json:"status"` // "firing" | "resolved"
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Fingerprint string            `json:"fingerprint"`
}

// alertmanagerWebhook ingests an Alertmanager webhook and opens/closes incidents.
// It is authed like any project write (a service-account bearer token in
// Alertmanager's http_config): a firing alert opens an incident correlated by its
// fingerprint (idempotent — a second firing reuses the open one), and a resolved
// alert closes the incident that fingerprint opened. Unknown fingerprints on a
// resolve are ignored. The response reports how many incidents were opened/resolved.
func (h *Handler) alertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	p, _ := h.principal(r)

	var payload alertmanagerPayload
	if !decodeJSON(w, r, &payload) {
		return
	}

	var opened, resolved, ignored int
	for _, a := range payload.Alerts {
		key := a.Fingerprint
		if key == "" {
			key = a.Labels["alertname"] // fall back to alertname when no fingerprint is sent
		}
		if key == "" {
			ignored++
			continue
		}
		switch strings.ToLower(a.Status) {
		case "resolved":
			did, err := h.resolveExternalIncident(r, proj.ID, key, p.UserID)
			if err != nil {
				h.serverError(w, "alertmanager_resolve", err)
				return
			}
			if did {
				resolved++
			} else {
				ignored++
			}
		case "firing", "":
			did, err := h.openExternalIncident(r, proj.ID, key, a, p.UserID)
			if err != nil {
				h.serverError(w, "alertmanager_open", err)
				return
			}
			if did {
				opened++
			} else {
				ignored++ // already open — idempotent no-op
			}
		default:
			ignored++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"opened": opened, "resolved": resolved, "ignored": ignored})
}

// openExternalIncident opens an incident for a firing alert unless one is already
// open for the same fingerprint. Returns whether it opened a new incident.
func (h *Handler) openExternalIncident(r *http.Request, projectID, key string, a alertmanagerAlert, author string) (bool, error) {
	if _, err := h.store.FindOpenIncidentByExternalKey(r.Context(), projectID, key); err == nil {
		return false, nil // already open — idempotent
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	inc := domain.Incident{
		ProjectID:   projectID,
		Title:       alertTitle(a),
		Status:      domain.IncidentInvestigating,
		Impact:      alertImpact(a.Labels["severity"]),
		Source:      domain.SourceAPI,
		ExternalKey: key,
	}
	if _, err := h.store.CreateIncident(r.Context(), inc, alertBody(a), author); err != nil {
		return false, err
	}
	if h.metrics != nil {
		h.metrics.RecordIncidentOpened()
	}
	return true, nil
}

// resolveExternalIncident closes the open incident for a fingerprint, if any.
func (h *Handler) resolveExternalIncident(r *http.Request, projectID, key, author string) (bool, error) {
	inc, err := h.store.FindOpenIncidentByExternalKey(r.Context(), projectID, key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil // nothing open for this fingerprint — ignore
	}
	if err != nil {
		return false, err
	}
	upd := domain.IncidentUpdate{
		IncidentID: inc.ID,
		Status:     domain.IncidentResolved,
		Body:       "Resolved by Alertmanager.",
		Author:     author,
	}
	if _, err := h.store.AddIncidentUpdate(r.Context(), upd); err != nil {
		return false, err
	}
	return true, nil
}

// alertTitle prefers the annotation summary, then alertname, then a generic label.
func alertTitle(a alertmanagerAlert) string {
	if s := strings.TrimSpace(a.Annotations["summary"]); s != "" {
		return s
	}
	if n := strings.TrimSpace(a.Labels["alertname"]); n != "" {
		return n
	}
	return "External alert"
}

// alertBody composes the opening update body from the alert's description/labels.
func alertBody(a alertmanagerAlert) string {
	if d := strings.TrimSpace(a.Annotations["description"]); d != "" {
		return d
	}
	if n := a.Labels["alertname"]; n != "" {
		return fmt.Sprintf("Alertmanager alert %q is firing.", n)
	}
	return "Alertmanager alert is firing."
}

// alertImpact maps an Alertmanager severity label onto an incident impact.
func alertImpact(severity string) domain.IncidentImpact {
	switch strings.ToLower(severity) {
	case "critical", "page":
		return domain.ImpactCritical
	case "warning", "major":
		return domain.ImpactMajor
	default:
		return domain.ImpactMinor
	}
}
