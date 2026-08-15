package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// Applying the `services` map of a format-2 bundle (spec func-service-reliability §15.2).
//
// This runs INSIDE the transaction that is already applying the bundle's monitors: a service
// and the monitors it names have to become visible together, or a reconcile that crashed in
// between would leave a declaration pointing at rows nobody created.

var (
	// ErrServiceSlugOwnedByUI is returned when a bundle declares a slug a UI-owned service
	// already holds. Adoption never happens by name — and for services the slug's
	// project-uniqueness makes a second row physically impossible as well as forbidden, which
	// is a stronger consequence than the monitor case and worth its own error.
	ErrServiceSlugOwnedByUI = errors.New("store: service slug is owned by the UI")
	// ErrServiceSlugOwnedByOther is returned when another FILE provider owns the slug.
	ErrServiceSlugOwnedByOther = errors.New("store: service slug is owned by another provider")
	// ErrServiceMemberUnknown is returned when a declaration names a monitor slug that does
	// not resolve in this project.
	ErrServiceMemberUnknown = errors.New("store: service references an unknown monitor slug")
)

// ServiceApplyCounts summarizes what a bundle's services did.
type ServiceApplyCounts struct {
	Created  int
	Updated  int
	NoOp     int
	Orphaned int
	Restored int
}

// applyBundleServicesTx reconciles the services this provider owns in one project.
//
// The caller must already hold the project's `service_membership` advisory lock: it is the
// outermost lock of §15.4, and this path both reads the affected set and writes declarations.
func (s *Store) applyBundleServicesTx(
	ctx context.Context, tx pgx.Tx, providerID, orgID, projID, sourcePath string,
	desired *fileprovider.DesiredProject, generation int64, dbNow time.Time,
) (ServiceApplyCounts, error) {
	var counts ServiceApplyCounts

	owned, err := readManagedServices(ctx, tx, providerID, orgID, projID)
	if err != nil {
		return counts, err
	}

	slugToID, err := monitorIDsBySlug(ctx, tx, projID)
	if err != nil {
		return counts, err
	}

	// Deterministic order: it is also the order this path takes service row locks, and a
	// fan-out that took them arbitrarily would deadlock against any other path holding two.
	slugs := make([]string, 0, len(desired.Services))
	for slug := range desired.Services {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	seen := map[string]bool{}
	for _, slug := range slugs {
		svc := desired.Services[slug]
		seen[slug] = true

		monitorIDs, err := resolveMemberSlugs(slug, svc.Monitors, slugToID)
		if err != nil {
			return counts, err
		}
		sliIDs, err := resolveMemberSlugs(slug, svc.SLI, slugToID)
		if err != nil {
			return counts, err
		}

		existing, err := lookupServiceBySlug(ctx, tx, projID, slug)
		if err != nil {
			return counts, err
		}

		switch {
		case existing == "":
			id, err := insertServiceTx(ctx, tx, projID, svc)
			if err != nil {
				return counts, err
			}
			if _, _, err := s.putServiceDeclarationTx(ctx, tx, projID, id,
				monitorIDs, sliIDs, svc.Policies, 0, DeclarationOptions{CreatedBy: "file:" + providerID}); err != nil {
				return counts, err
			}
			if err := upsertManagedService(ctx, tx, providerID, orgID, projID, id, slug, svc.Hash, sourcePath, generation); err != nil {
				return counts, err
			}
			counts.Created++

		case owned[slug].serviceID != existing:
			// The slug exists but this provider does not own it. Adoption never happens by
			// name — display equality is not identity — and for a service the project-unique
			// slug means there is no second row to create either.
			if owned[slug].serviceID == "" {
				var managedElsewhere bool
				if err := tx.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM managed_services WHERE service_id = $1)`, existing).Scan(&managedElsewhere); err != nil {
					return counts, fmt.Errorf("store: check service ownership: %w", err)
				}
				if managedElsewhere {
					return counts, fmt.Errorf("%w: %q", ErrServiceSlugOwnedByOther, slug)
				}
				return counts, fmt.Errorf("%w: %q", ErrServiceSlugOwnedByUI, slug)
			}
			return counts, fmt.Errorf("%w: %q", ErrServiceSlugOwnedByOther, slug)

		case owned[slug].specHash == svc.Hash && owned[slug].orphanedAt == nil:
			// The no-op rule: an unchanged canonical hash MUST NOT create a definition
			// revision. Moving a file may update provenance; it may not restate what
			// availability means.
			if err := touchManagedService(ctx, tx, existing, sourcePath, generation); err != nil {
				return counts, err
			}
			counts.NoOp++

		default:
			currentRevision, err := currentServiceRevision(ctx, tx, existing)
			if err != nil {
				return counts, err
			}
			// A service-only change must not bump any monitor's execution_revision — this
			// path writes no monitor row — and it must STILL create the matching epoch, which
			// putServiceDeclarationTx does unconditionally.
			if _, _, err := s.putServiceDeclarationTx(ctx, tx, projID, existing,
				monitorIDs, sliIDs, svc.Policies, currentRevision, DeclarationOptions{CreatedBy: "file:" + providerID}); err != nil {
				return counts, err
			}
			if err := updateServiceRowTx(ctx, tx, projID, existing, svc); err != nil {
				return counts, err
			}
			if err := upsertManagedService(ctx, tx, providerID, orgID, projID, existing, slug, svc.Hash, sourcePath, generation); err != nil {
				return counts, err
			}
			if owned[slug].orphanedAt != nil {
				counts.Restored++
			} else {
				counts.Updated++
			}
		}
	}

	// Absence from the bundle marks an owned service orphaned; it never deletes one. A
	// service carries facts, incidents and possibly a public projection, and silently
	// removing that because a file was edited is not a reconcile, it is data loss.
	//
	// A format-1 bundle is exempt: that format cannot express services at all, so its silence
	// about them is not a statement of intent. Treating it as one would mean downgrading a
	// file's `format:` line silently orphans every service it used to declare.
	if desired.Format < 2 {
		return counts, nil
	}
	for slug, row := range owned {
		if seen[slug] || row.orphanedAt != nil {
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE managed_services SET orphaned_at = $2 WHERE service_id = $1`, row.serviceID, dbNow); err != nil {
			return counts, fmt.Errorf("store: orphan service: %w", err)
		}
		counts.Orphaned++
	}
	return counts, nil
}

type managedServiceRow struct {
	serviceID  string
	specHash   string
	orphanedAt *time.Time
}

func readManagedServices(ctx context.Context, tx pgx.Tx, providerID, orgID, projID string) (map[string]managedServiceRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT source_uid, service_id, spec_hash, orphaned_at
		   FROM managed_services
		  WHERE provider_id = $1 AND org_id = $2 AND project_id = $3`, providerID, orgID, projID)
	if err != nil {
		return nil, fmt.Errorf("store: read managed services: %w", err)
	}
	defer rows.Close()
	out := map[string]managedServiceRow{}
	for rows.Next() {
		var uid string
		var r managedServiceRow
		if err := rows.Scan(&uid, &r.serviceID, &r.specHash, &r.orphanedAt); err != nil {
			return nil, fmt.Errorf("store: scan managed service: %w", err)
		}
		out[uid] = r
	}
	return out, rows.Err()
}

// monitorIDsBySlug maps every monitor slug in a project to its id. The bundle references
// slugs; only the database knows which row each one is.
func monitorIDsBySlug(ctx context.Context, tx pgx.Tx, projID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT slug, id FROM monitors WHERE project_id = $1`, projID)
	if err != nil {
		return nil, fmt.Errorf("store: read monitor slugs: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, id string
		if err := rows.Scan(&slug, &id); err != nil {
			return nil, fmt.Errorf("store: scan monitor slug: %w", err)
		}
		out[slug] = id
	}
	return out, rows.Err()
}

// resolveMemberSlugs turns declared slugs into ids, refusing one that does not exist.
//
// A service may legitimately name a monitor this bundle does not own — a file-managed
// service pointing at a UI-managed monitor is explicitly allowed — but it may not name one
// that is not there at all: that declaration could never be evaluated.
func resolveMemberSlugs(serviceSlug string, slugs []string, byslug map[string]string) ([]string, error) {
	out := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		id, ok := byslug[slug]
		if !ok {
			return nil, fmt.Errorf("%w: service %q names monitor %q", ErrServiceMemberUnknown, serviceSlug, slug)
		}
		out = append(out, id)
	}
	return out, nil
}

func lookupServiceBySlug(ctx context.Context, tx pgx.Tx, projID, slug string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM services WHERE project_id = $1 AND slug = $2 FOR UPDATE`, projID, slug).Scan(&id)
	if noRows(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: look up service by slug: %w", err)
	}
	return id, nil
}

func insertServiceTx(ctx context.Context, tx pgx.Tx, projID string, svc fileprovider.DesiredService) (string, error) {
	escalation, oncall, err := resolveServiceOwnerTx(ctx, tx, projID, svc)
	if err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO services (project_id, slug, name, description, escalation_policy_id, oncall_schedule_id)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		projID, svc.Slug, svc.Name, svc.Description, escalation, oncall).Scan(&id); err != nil {
		return "", fmt.Errorf("store: insert file-managed service: %w", err)
	}
	return id, nil
}

func updateServiceRowTx(ctx context.Context, tx pgx.Tx, projID, id string, svc fileprovider.DesiredService) error {
	escalation, oncall, err := resolveServiceOwnerTx(ctx, tx, projID, svc)
	if err != nil {
		return err
	}
	// The slug is the identity this row was found by and is never rewritten here.
	if _, err := tx.Exec(ctx,
		`UPDATE services
		    SET name = $2, description = $3,
		        escalation_policy_id = $4, oncall_schedule_id = $5, updated_at = now()
		  WHERE id = $1`,
		id, svc.Name, svc.Description, escalation, oncall); err != nil {
		return fmt.Errorf("store: update file-managed service: %w", err)
	}
	return nil
}

// ErrServiceOwnerUnknown is returned when a bundle names an escalation policy or on-call
// schedule this project does not have.
var ErrServiceOwnerUnknown = errors.New("store: service owner references an unknown routing target")

// resolveServiceOwnerTx turns the bundle's owner NAMES into ids in this project.
//
// The owner was parsed, validated and folded into the canonical hash — and then dropped on
// the floor: only name and description were persisted. So a bundle declaring who is
// responsible applied "successfully", changed nothing about routing, and its hash asserted
// the declaration was in force. An unresolvable name is refused rather than nulled, because
// silently having no owner is exactly the outcome the declaration was written to prevent.
func resolveServiceOwnerTx(
	ctx context.Context, tx pgx.Tx, projID string, svc fileprovider.DesiredService,
) (escalation, oncall *string, err error) {
	if name := svc.EscalationPolicy; name != "" {
		var id string
		err := tx.QueryRow(ctx,
			`SELECT id::text FROM escalation_policies WHERE project_id = $1 AND name = $2`,
			projID, name).Scan(&id)
		if noRows(err) {
			return nil, nil, fmt.Errorf("%w: escalation policy %q", ErrServiceOwnerUnknown, name)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("store: resolve escalation policy: %w", err)
		}
		escalation = &id
	}
	if name := svc.OncallSchedule; name != "" {
		var id string
		err := tx.QueryRow(ctx,
			`SELECT id::text FROM oncall_schedules WHERE project_id = $1 AND name = $2`,
			projID, name).Scan(&id)
		if noRows(err) {
			return nil, nil, fmt.Errorf("%w: on-call schedule %q", ErrServiceOwnerUnknown, name)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("store: resolve on-call schedule: %w", err)
		}
		oncall = &id
	}
	return escalation, oncall, nil
}

func currentServiceRevision(ctx context.Context, tx pgx.Tx, serviceID string) (int64, error) {
	var rev int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM service_definition_revisions
		  WHERE service_id = $1 AND state = 'effective'`, serviceID).Scan(&rev); err != nil {
		return 0, fmt.Errorf("store: current service revision: %w", err)
	}
	return rev, nil
}

func upsertManagedService(ctx context.Context, tx pgx.Tx, providerID, orgID, projID, serviceID, sourceUID, hash, sourcePath string, generation int64) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO managed_services
		   (service_id, provider_id, org_id, project_id, source_uid, spec_hash, source_path, generation, applied_at, orphaned_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now(), NULL)
		 ON CONFLICT (service_id) DO UPDATE
		    SET provider_id = EXCLUDED.provider_id, source_uid = EXCLUDED.source_uid,
		        spec_hash = EXCLUDED.spec_hash, source_path = EXCLUDED.source_path,
		        generation = EXCLUDED.generation, applied_at = now(), orphaned_at = NULL`,
		serviceID, providerID, orgID, projID, sourceUID, hash, sourcePath, generation); err != nil {
		return fmt.Errorf("store: upsert managed service: %w", err)
	}
	return nil
}

// touchManagedService records provenance for a no-op apply. Moving a file may update where
// it came from; it may not restate what availability means, so nothing else is written.
func touchManagedService(ctx context.Context, tx pgx.Tx, serviceID, sourcePath string, generation int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE managed_services SET source_path = $2, generation = $3, applied_at = now() WHERE service_id = $1`,
		serviceID, sourcePath, generation); err != nil {
		return fmt.Errorf("store: touch managed service: %w", err)
	}
	return nil
}
