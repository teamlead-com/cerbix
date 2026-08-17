package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/reliability"
)

// The status-page projection VOCABULARY (FR-021 §15.0). The projection itself is page-scoped and
// lives in `statuspageprojection.go`; what remains here is the shared shape and the ONE precedence
// rule every caller maps through. A status page is a customer-visible artifact, so every value is
// chosen so that the page can state exactly what was measured and nothing more:
//
//   - the SLI layer alone reaches the public page. The diagnostics layer names MONITORS, which
//     is internal topology, and the §14 impact links would publish the dependency graph.
//   - `Excluded` is the one bit `ServiceHealthNow` deliberately folds away (it maps both
//     "excluded by a maintenance window" and "genuinely unknown" to unknown). Rather than a
//     second evaluator — a second owner for the hardest-reviewed rule of phase 2 — the
//     projection calls the SAME `reliability.StateAt` and reads `Availability == AvailExcluded`,
//     which the outcome already carries.
//   - `SealedInWindow`, never "any fact exists": one ancient fact would otherwise claim a
//     history the strip cannot draw.

// ServiceStatusProjection is one service's public-projection input, at one snapshot instant.
type ServiceStatusProjection struct {
	ServiceID string
	// SLI is the customer-facing layer: healthy | degraded | down | unknown.
	SLI string
	// Excluded is true when a maintenance exclusion is in force AT the snapshot instant.
	Excluded bool
	// Reason carries why a non-measured SLI is not measured, for the operator-facing view only.
	Reason string
	// SealedThrough is the watermark: the 90-day public window ends HERE, never at "now".
	SealedThrough time.Time
	// SealedInWindow is whether a sealed fact exists INSIDE that window (drives the strip).
	SealedInWindow bool
}

// decodeEpochSnapshot turns a stored epoch snapshot into evaluator members. Shared by the
// projection and the neighbour-health batch so the two cannot decode it differently.
func decodeEpochSnapshot(snapshot, policyJSON []byte) ([]reliability.Member, domain.ServicePolicies, error) {
	var snap []domain.EpochMember
	var policies domain.ServicePolicies
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return nil, policies, fmt.Errorf("store: decode epoch snapshot: %w", err)
	}
	if err := json.Unmarshal(policyJSON, &policies); err != nil {
		return nil, policies, fmt.Errorf("store: decode epoch policies: %w", err)
	}
	members := make([]reliability.Member, 0, len(snap))
	for _, m := range snap {
		members = append(members, reliability.Member{
			MonitorID:  m.MonitorID,
			Type:       m.Semantics.Type,
			Region:     m.Semantics.Region,
			Enabled:    m.Semantics.Enabled,
			StaleAfter: m.StaleAfter,
			ArmedAt:    m.ArmedAt,
		})
	}
	return members, policies, nil
}

// ServiceDayPoint is one UTC day of a service's PUBLIC history.
type ServiceDayPoint struct {
	Day time.Time
	// Uptime is availability over the day's DECIDABLE time, in percent.
	Uptime float64
	// DecidableFraction is the §11.2 axis — the one that decides whether the number may be quoted
	// at all — not storage continuity. A partially decidable day is published WITH this fraction so
	// a reader can tell a quiet day from a half-measured one.
	DecidableFraction float64
}

// ── The public component status: the §15.0 precedence, in one place ──────────────────────

// PublicComponentStatus maps a service projection to the public vocabulary, in the TOTAL
// precedence order of §15.0. It lives beside the projection so the order cannot be re-invented
// per caller:
//
//  1. excluded (a declared maintenance window in force)  → maintenance
//  2. sli = down                                         → major_outage
//  3. sli = degraded                                     → degraded
//  4. sli = healthy                                      → operational
//  5. anything else (unknown, no SLI declared)            → no_data
//
// Two rules the order encodes deliberately:
//   - maintenance OUTRANKS absence: the operator declared that window, so it is the more
//     specific true statement even when nothing is sealed;
//   - `SealedInWindow` is NOT consulted here. It governs the history strip, never the status —
//     a live-healthy service before its first seal is operational, because its current state IS
//     measured even though its history is not.
func PublicComponentStatus(p ServiceStatusProjection) domain.ComponentStatus {
	switch {
	case p.Excluded:
		return domain.CompMaintenance
	case p.SLI == "down":
		return domain.CompMajorOutage
	case p.SLI == "degraded":
		return domain.CompDegraded
	case p.SLI == "healthy":
		return domain.CompOperational
	default:
		return domain.CompNoData
	}
}
