package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-021 §15.0 — resolving what a status-page component says.
//
// This is ONE resolver, used by the public render AND by the conversion preview. The preview's
// whole purpose is to show the operator the page as it WILL read; a second implementation would
// eventually predict something the page does not do, which is the failure the preview exists to
// prevent.
//
// It also fixes three inherited defects the shipped renderer carried, each of which reported an
// unknown as health (§17):
//
//  1. a component with no binding and no manual status defaulted to `operational`;
//  2. a `pending` monitor — never confirmed either way — reported `operational`;
//  3. an EMPTY page summarized as `operational`, i.e. "all systems operational" with no systems.
//
// And one defect the discriminator introduces the fix for: the renderer branched on "is
// monitor_id populated", which after phase 4 would read a DORMANT binding — a component converted
// to a service would keep rendering its old monitor.
//
// COST: both halves are batched, so the whole page is a FIXED number of statements ([314] P1-3).
// Service components go through one page-scoped snapshot over (project, service) pairs — an
// org-level page spans projects, so a per-project batch was never one snapshot — and monitor
// components through three set-wise reads.

// publicComponentHardCeiling is the absolute fail-closed bound on an unauthenticated render
// (§15.0, invariant 71b). The per-page ceiling in `status_pages.component_ceiling` can only
// shrink and protects each page; this protects the PROCESS, and lives in code because it is not a
// property of any row. Above it the page refuses as a whole: a truncated subset posing as the
// complete page would be a lie of a worse kind than a 503.
const publicComponentHardCeiling = 500

// componentResolution is one component's resolved public state plus the operator-facing reason
// for anything absent.
type componentResolution struct {
	Status domain.ComponentStatus
	// Reason explains a non-measured status. It never reaches the public payload — a customer
	// gets the status, an operator gets the why.
	Reason string
	// Uptime90d is present only when the source can produce it AND the §11.2/§11.3 rules allow it
	// to be quoted; UptimeWithheld names the reason when it cannot.
	Uptime90d      *float64
	UptimeWithheld string
	Daily          []dayPoint
	// Unavailable marks a component the resolver could not EVALUATE because a read failed (not
	// because measurement is absent). Invariant 71a: this must not be published as the calm
	// statement `no_data`, so unlike `Reason` it DOES reach the public payload — it says only that
	// our own read failed, which is a fact about us and not about the customer's topology.
	Unavailable bool
	// Project is the project this component draws from, for the incident/maintenance sweep.
	Project string
}

// resolveComponents resolves a whole page at once.
//
// `withHistory` is false for the conversion preview: the preview needs statuses and the summary,
// not 90 days of strips per component, and asking for them would make an operator's click as
// expensive as a public render.
func (h *Handler) resolveComponents(
	ctx context.Context, comps []domain.Component, withHistory bool,
) (map[string]componentResolution, error) {
	out := make(map[string]componentResolution, len(comps))
	now := time.Now()
	win90, _ := sla.WindowByName("90d")

	// The two batches, collected from the ACTIVE source only: a dormant binding must not be read,
	// or a converted component would keep reporting what it stopped meaning.
	var refs []store.ServiceRef
	var monitorIDs []string
	for _, c := range comps {
		switch c.Source {
		case domain.ComponentSourceService:
			if c.ServiceID != "" {
				refs = append(refs, store.ServiceRef{ProjectID: c.SourceProject, ServiceID: c.ServiceID})
			}
		case domain.ComponentSourceMonitor:
			if c.MonitorID != "" {
				monitorIDs = append(monitorIDs, c.MonitorID)
			}
		}
	}
	services := map[string]store.ServicePageProjection{}
	if len(refs) > 0 {
		var err error
		if services, err = h.store.ServicePageProjections(ctx, refs, withHistory); err != nil {
			return nil, fmt.Errorf("service page projections: %w", err)
		}
	}
	monitors := map[string]store.MonitorPageProjection{}
	if len(monitorIDs) > 0 {
		var err error
		monitors, err = h.store.MonitorPageProjections(ctx, monitorIDs, now.Add(-win90.Duration), withHistory)
		if err != nil {
			return nil, fmt.Errorf("monitor page projections: %w", err)
		}
	}

	for _, c := range comps {
		res := componentResolution{Project: c.SourceProject}
		switch c.Source {
		case domain.ComponentSourceService:
			p, ok := services[c.ServiceID]
			if !ok {
				// Invariant 71a: an ACTIVE service the projection cannot find is a FAILED read,
				// not absent measurement. The FK is RESTRICT, so it cannot legitimately be
				// missing; publishing `no_data` here would present a bug as a calm fact.
				h.logger.Error("status page component references an unreadable service",
					slog.String("component", c.ID), slog.String("service", c.ServiceID),
					slog.String("project", c.SourceProject))
				if h.metrics != nil {
					h.metrics.RecordStatusPageUnreadableComponent()
				}
				res.Status, res.Reason, res.Unavailable = domain.CompNoData, "service_unreadable", true
				out[c.ID] = res
				continue
			}
			res.Status = publicServiceStatus(p)
			res.Reason, res.Uptime90d, res.UptimeWithheld = p.Reason, p.Uptime, p.UptimeWithheld
			if withHistory && p.SealedInWindow {
				for _, d := range p.Daily {
					// `Total` carries the DECIDABLE fraction of the day in basis points, so a
					// partial day is visibly partial instead of silently equal to a full one.
					res.Daily = append(res.Daily, dayPoint{
						Day:           d.Day.Format("2006-01-02"),
						UptimePercent: d.Uptime,
						Total:         int64(d.DecidableFraction * 10000),
					})
				}
			}
		case domain.ComponentSourceMonitor:
			p, ok := monitors[c.MonitorID]
			if !ok {
				// The shipped `SET NULL (monitor_id)` exception (§15.0): a deleted monitor turns
				// its component manual. Until the row is reconciled the honest render is "we do
				// not know", never "operational".
				res.Status, res.Reason = domain.CompNoData, "monitor_deleted"
				out[c.ID] = res
				continue
			}
			// `pending` maps to no_data — the corrected mapping (§17, D-0167).
			res.Status = domain.ComponentStatusFromMonitor(p.Status)
			if res.Status == domain.CompNoData {
				res.Reason = "monitor_never_confirmed"
			}
			res.Project = p.ProjectID
			if withHistory {
				res.Uptime90d = p.Uptime
				if p.Uptime == nil {
					res.UptimeWithheld = domain.ServiceReportReasonNothingMeasured
				}
				for _, d := range p.Daily {
					res.Daily = append(res.Daily, dayPoint{
						Day: d.Day.Format("2006-01-02"), UptimePercent: d.UptimePercent, Total: d.Total,
					})
				}
			}
		default: // manual
			if c.ManualStatus != "" {
				res.Status = c.ManualStatus
			} else {
				// The first inherited lie: a manual component whose operator has said nothing
				// used to render operational. It renders `no_data` now.
				res.Status, res.Reason = domain.CompNoData, "no_manual_status"
			}
		}
		out[c.ID] = res
	}
	return out, nil
}

// publicServiceStatus maps a service projection to the public vocabulary in the TOTAL precedence
// order of §15.0. It delegates the ordering to the store so there is ONE owner:
//
//  1. excluded (a declared maintenance window in force) → maintenance
//  2. down → major_outage   3. degraded → degraded   4. healthy → operational
//  5. anything else → no_data
func publicServiceStatus(p store.ServicePageProjection) domain.ComponentStatus {
	return store.PublicComponentStatus(store.ServiceStatusProjection{
		ServiceID: p.ServiceID, SLI: p.SLI, Excluded: p.Excluded,
		Reason: p.Reason, SealedThrough: p.SealedThrough, SealedInWindow: p.SealedInWindow,
	})
}

// summarize turns resolved components into the page summary, in the declared component order so
// the result does not depend on map iteration.
func summarize(comps []domain.Component, res map[string]componentResolution) domain.PageSummary {
	statuses := make([]domain.ComponentStatus, 0, len(comps))
	for _, c := range comps {
		statuses = append(statuses, res[c.ID].Status)
	}
	return domain.Summarize(statuses)
}
