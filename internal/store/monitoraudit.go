package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-026 §10 (D-0233) — a monitor write says who wrote it.
//
// FR-029's invariant 13 promised an acknowledged `cleanup.kind: none` "visible in the audit trail",
// and the discharge map could not cite a test for it: a monitor write left no `audit_logs` row, for any
// type. This file applies FR-026's rules to the three monitor writers the API reaches — the same
// validated actor, the same one-row-in-the-transaction, the same door split — and nothing new.

// monitorAuditHook is what a principal door hands the unexported writer: it runs inside the writer's
// transaction, immediately before the commit, so the row and the mutation share a fate (D2, D7).
// prevEnabled is meaningful for the update hook only; it lets the target name a pause or a resume.
type monitorAuditHook func(ctx context.Context, tx pgx.Tx, m domain.Monitor, prevEnabled bool) error

// MonitorAuditAction is D12's closed vocabulary: three words, as a type, beside the incident five.
type MonitorAuditAction string

const (
	MonitorAuditCreate MonitorAuditAction = "monitor.create"
	MonitorAuditUpdate MonitorAuditAction = "monitor.update"
	MonitorAuditDelete MonitorAuditAction = "monitor.delete"
)

// CreateMonitorByPrincipal is the only exported create door. The actor is in the signature, so a
// handler that forgets it does not compile — guard 1 is the compiler here.
func (s *Store) CreateMonitorByPrincipal(ctx context.Context, m domain.Monitor, actor AuditActor) (domain.Monitor, error) {
	if err := actor.valid(); err != nil {
		return domain.Monitor{}, err
	}
	return s.createMonitor(ctx, m, func(ctx context.Context, tx pgx.Tx, created domain.Monitor, _ bool) error {
		return insertProjectAudit(ctx, tx, created.ProjectID, actor, string(MonitorAuditCreate), MonitorCreateTarget(actor, created))
	})
}

// UpdateMonitorByPrincipal is the only exported update door.
func (s *Store) UpdateMonitorByPrincipal(ctx context.Context, m domain.Monitor, actor AuditActor) (domain.Monitor, error) {
	if err := actor.valid(); err != nil {
		return domain.Monitor{}, err
	}
	return s.updateMonitor(ctx, m, func(ctx context.Context, tx pgx.Tx, updated domain.Monitor, prevEnabled bool) error {
		return insertProjectAudit(ctx, tx, updated.ProjectID, actor, string(MonitorAuditUpdate), MonitorUpdateTarget(actor, updated, prevEnabled))
	})
}

// DeleteMonitorByPrincipal is the only exported delete door. It takes the monitor rather than its id
// because the target names slug, type and region, and after the DELETE there is no row to read them
// from; the handler already holds the row it authorized against.
func (s *Store) DeleteMonitorByPrincipal(ctx context.Context, m domain.Monitor, actor AuditActor) error {
	if err := actor.valid(); err != nil {
		return err
	}
	return s.deleteMonitor(ctx, m.ID, func(ctx context.Context, tx pgx.Tx, _ domain.Monitor, _ bool) error {
		return insertProjectAudit(ctx, tx, m.ProjectID, actor, string(MonitorAuditDelete), MonitorDeleteTarget(actor, m))
	})
}

// ── D13: the target names the document, never its contents ──────────────────────────────────────

func monitorAuditBody(m domain.Monitor) string {
	return fmt.Sprintf("monitor %s · %s · %s · region=%s", m.ID, m.Slug, m.Type, m.Region)
}

// monitorCleanupSuffix is FR-029 invariant 13's clause: an async canary whose workflow declares
// `cleanup.kind: none` with `acknowledged: true` says so in the trail. Any other document, and any
// canary whose document does not parse, contributes nothing — the target must never fail a write
// over a detail it could not read.
func monitorCleanupSuffix(m domain.Monitor) string {
	if m.Type != domain.MonitorAsyncCanary {
		return ""
	}
	w, err := domain.ParseCanaryConfig(m.Config)
	if err != nil {
		return ""
	}
	if w.Cleanup.Kind == domain.CanaryCleanupNone && w.Cleanup.Acknowledged {
		return " · cleanup=none acknowledged"
	}
	return ""
}

// MonitorCreateTarget, MonitorUpdateTarget and MonitorDeleteTarget are exported so the tests that pin
// D13's shapes read the builder the product uses rather than a copy that can drift.
func MonitorCreateTarget(actor AuditActor, m domain.Monitor) string {
	return incidentAuditTarget(actor, monitorAuditBody(m)+monitorCleanupSuffix(m))
}

func MonitorUpdateTarget(actor AuditActor, m domain.Monitor, prevEnabled bool) string {
	body := fmt.Sprintf("monitor %s · %s · updated", m.ID, m.Slug)
	if prevEnabled != m.Enabled {
		body += fmt.Sprintf(" · enabled %t→%t", prevEnabled, m.Enabled)
	}
	return incidentAuditTarget(actor, body+monitorCleanupSuffix(m))
}

func MonitorDeleteTarget(actor AuditActor, m domain.Monitor) string {
	return incidentAuditTarget(actor, monitorAuditBody(m)+" · deleted")
}
