package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Service impact-graph errors (FR-021 §14.1–14.2). The API maps cycle/foreign/
// depth/limit to 400, stale to 409, pinned to 409.
var (
	ErrServiceGraphCycle   = errors.New("store: service dependency would create a cycle")
	ErrServiceGraphForeign = errors.New("store: service dependencies must be services of the same project")
	ErrServiceGraphDepth   = fmt.Errorf("store: service dependency chain would exceed the depth cap of %d", domain.ServiceGraphDepthCap)
	ErrServiceGraphLimit   = fmt.Errorf("store: a service takes at most %d direct dependencies", domain.MaxServiceDependencies)
	ErrServiceGraphStale   = errors.New("store: service dependencies changed concurrently (stale graph_generation)")
)

// ErrServicePinnedByFile is returned when deleting a service that an APPLIED
// file-owned service names in depends_on: a desired edge pins its target
// (§14.2). The message carries the provider and the referencing service so the
// 409 is actionable.
type ErrServicePinnedByFile struct {
	Provider string
	Service  string // slug of the referencing (dependent) service
}

func (e ErrServicePinnedByFile) Error() string {
	return fmt.Sprintf("store: service is pinned by %s: %s declares it in depends_on", e.Provider, e.Service)
}

// ServiceEdge names one neighbour in the impact graph.
type ServiceEdge struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// ServiceGraphView is the edge set of one service plus its concurrency token.
type ServiceGraphView struct {
	GraphGeneration int64         `json:"graph_generation"`
	DependsOn       []ServiceEdge `json:"depends_on"`
	DependedOnBy    []ServiceEdge `json:"depended_on_by"`
}

// lockServiceGraph serializes every edge-mutating path of a project — UI
// replace-set, create-with-edges, the format-2 edge track, service delete —
// so the cycle/depth checks run against committed state (§14.1). In the §15.4
// order it sits immediately after service_membership; no path takes them in
// the other direction.
func lockServiceGraph(ctx context.Context, tx pgx.Tx, projectID string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('service_graph'), hashtext($1))`, projectID); err != nil {
		return fmt.Errorf("store: lock service graph: %w", err)
	}
	return nil
}

// GetServiceDependencies returns the service's upstream and downstream edges
// with the graph_generation token the UI must echo back on a replace-set.
// ONE SQL statement — one MVCC snapshot — so the returned token always names
// the returned set; two pool queries could straddle a concurrent replace and
// hand back new edges under the old token, earning the next legitimate PUT a
// spurious 409 ([276] P2-1).
func (s *Store) GetServiceDependencies(ctx context.Context, projectID, serviceID string) (ServiceGraphView, error) {
	var v ServiceGraphView
	rows, err := s.pool.Query(ctx, `
		SELECT s.graph_generation, e.dir, e.id, e.slug, e.name
		  FROM services s
		  LEFT JOIN LATERAL (
		      SELECT 'up' AS dir, p.id, p.slug, p.name
		        FROM service_dependencies d JOIN services p ON p.id = d.depends_on_id
		       WHERE d.service_id = s.id
		      UNION ALL
		      SELECT 'down', c.id, c.slug, c.name
		        FROM service_dependencies d JOIN services c ON c.id = d.service_id
		       WHERE d.depends_on_id = s.id
		  ) e ON true
		 WHERE s.id = $1 AND s.project_id = $2
		 ORDER BY e.dir, e.slug`, serviceID, projectID)
	if err != nil {
		return v, fmt.Errorf("store: get service dependencies: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var dir, id, slug, name *string
		if err := rows.Scan(&v.GraphGeneration, &dir, &id, &slug, &name); err != nil {
			return v, fmt.Errorf("store: scan service dependency: %w", err)
		}
		if dir == nil {
			continue // the LEFT JOIN row of an edgeless service: generation only
		}
		e := ServiceEdge{ID: *id, Slug: *slug, Name: *name}
		if *dir == "up" {
			v.DependsOn = append(v.DependsOn, e)
		} else {
			v.DependedOnBy = append(v.DependedOnBy, e)
		}
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("store: iterate service dependencies: %w", err)
	}
	if !found {
		return v, ErrNotFound
	}
	return v, nil
}

// GraphActor is the typed audit attribution for an edge write ([276] P1-4):
// the canonical actor_user_id / via_token columns, plus a human-readable label
// (an email, a token name, or the provider id on the format-2 edge track) that
// lands in the audit target and in created_by on the surviving edge rows.
type GraphActor struct {
	UserID   string // authz.Principal.AuditUserID(); empty for machine actors → NULL
	ViaToken bool
	Label    string
}

// ReplaceServiceDependencies atomically replaces a service's upstream edge set
// under the per-project service_graph lock. expectedGeneration is the edge
// set's own CAS (§14.2) — edges are outside definition revisions, so the
// declaration token cannot protect them; a stale value is ErrServiceGraphStale
// (409) and first-committer-wins. A no-op replace (identical set) bumps
// nothing and audits nothing. Every non-no-op delta writes ONE audit row —
// typed actor attribution in the CANONICAL columns plus added/removed slugs in
// the same transaction: created_by on surviving rows cannot testify about a
// removal. One validator, one mutator for the UI route and the format-2 edge
// track alike (§14.2).
func (s *Store) ReplaceServiceDependencies(
	ctx context.Context, projectID, serviceID string, parents []string,
	expectedGeneration int64, actor GraphActor,
) (ServiceGraphView, error) {
	parents = dedupe(parents)
	if len(parents) > domain.MaxServiceDependencies {
		return ServiceGraphView{}, ErrServiceGraphLimit
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: begin replace service dependencies: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceGraph(ctx, tx, projectID); err != nil {
		return ServiceGraphView{}, err
	}
	var gen int64
	err = tx.QueryRow(ctx,
		`SELECT graph_generation FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		serviceID, projectID).Scan(&gen)
	if noRows(err) {
		return ServiceGraphView{}, ErrNotFound
	}
	if err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: lock service for graph write: %w", err)
	}
	if gen != expectedGeneration {
		return ServiceGraphView{}, ErrServiceGraphStale
	}

	// Current set, for the no-op decision and the audit delta.
	current := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT depends_on_id FROM service_dependencies WHERE service_id = $1`, serviceID)
	if err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: read current dependencies: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ServiceGraphView{}, fmt.Errorf("store: scan current dependency: %w", err)
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: iterate current dependencies: %w", err)
	}
	next := map[string]bool{}
	for _, p := range parents {
		next[p] = true
	}
	noop := len(current) == len(next)
	if noop {
		for id := range next {
			if !current[id] {
				noop = false
				break
			}
		}
	}
	if noop {
		// Identical set: no generation bump, no audit, nothing to validate.
		if err := tx.Commit(ctx); err != nil {
			return ServiceGraphView{}, fmt.Errorf("store: commit no-op graph write: %w", err)
		}
		return s.GetServiceDependencies(ctx, projectID, serviceID)
	}

	if len(parents) > 0 {
		// Same-project membership (and implicitly: existence) — schema enforces it
		// again at insert via the shared-project composite FKs; validating here turns
		// a constraint violation into the typed 400.
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM services WHERE project_id = $1 AND id = ANY($2) AND id <> $3`,
			projectID, parents, serviceID).Scan(&n); err != nil {
			return ServiceGraphView{}, fmt.Errorf("store: check dependency services: %w", err)
		}
		if n != len(parents) {
			return ServiceGraphView{}, ErrServiceGraphForeign
		}
		// Cycle: the child reachable walking UP from any new parent. The capped walk
		// is EXHAUSTIVE, not truncated: the depth invariant below holds at every
		// write, so any pre-existing chain terminates within the cap.
		var cyclic bool
		if err := tx.QueryRow(ctx, `
			WITH RECURSIVE anc AS (
			    SELECT d.depends_on_id, 1 AS depth
			      FROM service_dependencies d
			     WHERE d.service_id = ANY($1)
			    UNION
			    SELECT d.depends_on_id, anc.depth + 1
			      FROM service_dependencies d
			      JOIN anc ON d.service_id = anc.depends_on_id
			     WHERE anc.depth < $3
			)
			SELECT EXISTS (SELECT 1 FROM anc WHERE depends_on_id = $2)`,
			parents, serviceID, domain.ServiceGraphDepthCap).Scan(&cyclic); err != nil {
			return ServiceGraphView{}, fmt.Errorf("store: service cycle check: %w", err)
		}
		if cyclic {
			return ServiceGraphView{}, ErrServiceGraphCycle
		}
		// Depth: the longest chain THROUGH this write must stay within the cap —
		// longest ancestor chain above any new parent, plus the new edge, plus the
		// longest descendant chain below the child (§14.1: 9 and 10 valid, 11 is the
		// rejected write). Both walks bound at cap+1 only to DETECT overflow.
		var maxUp, maxDown int
		if err := tx.QueryRow(ctx, `
			WITH RECURSIVE up AS (
			    SELECT unnest($1::uuid[]) AS node, 0 AS depth
			    UNION ALL
			    SELECT d.depends_on_id, up.depth + 1
			      FROM service_dependencies d
			      JOIN up ON d.service_id = up.node
			     WHERE up.depth <= $2
			)
			SELECT COALESCE(max(depth), 0) FROM up`,
			parents, domain.ServiceGraphDepthCap).Scan(&maxUp); err != nil {
			return ServiceGraphView{}, fmt.Errorf("store: service depth check (up): %w", err)
		}
		if err := tx.QueryRow(ctx, `
			WITH RECURSIVE down AS (
			    SELECT $1::uuid AS node, 0 AS depth
			    UNION ALL
			    SELECT d.service_id, down.depth + 1
			      FROM service_dependencies d
			      JOIN down ON d.depends_on_id = down.node
			     WHERE down.depth <= $2
			)
			SELECT COALESCE(max(depth), 0) FROM down`,
			serviceID, domain.ServiceGraphDepthCap).Scan(&maxDown); err != nil {
			return ServiceGraphView{}, fmt.Errorf("store: service depth check (down): %w", err)
		}
		if maxUp+1+maxDown > domain.ServiceGraphDepthCap {
			return ServiceGraphView{}, ErrServiceGraphDepth
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM service_dependencies WHERE service_id = $1`, serviceID); err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: clear service dependencies: %w", err)
	}
	for _, p := range parents {
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_dependencies (service_id, depends_on_id, project_id, created_by)
			SELECT $1, $2, project_id, $3 FROM services WHERE id = $1`,
			serviceID, p, actor.Label); err != nil {
			return ServiceGraphView{}, fmt.Errorf("store: insert service dependency: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE services SET graph_generation = graph_generation + 1, updated_at = now() WHERE id = $1`,
		serviceID); err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: bump graph generation: %w", err)
	}

	// The delta audit, in this same transaction (§14.2). Slugs, sorted, bounded by
	// the edge cap on both sides.
	added, removed := diffIDs(current, next)
	slugs, err := slugsFor(ctx, tx, append(append([]string{}, added...), removed...))
	if err != nil {
		return ServiceGraphView{}, err
	}
	target := "service=" + serviceID + " actor=" + actor.Label +
		" added=[" + strings.Join(mapSlugs(added, slugs), ",") + "]" +
		" removed=[" + strings.Join(mapSlugs(removed, slugs), ",") + "]"
	var actorUserID *string
	if actor.UserID != "" {
		actorUserID = &actor.UserID
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 SELECT p.org_id, $2, $3, 'service.dependencies.replaced', $4
		   FROM projects p WHERE p.id = $1`,
		projectID, actorUserID, actor.ViaToken, target); err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: audit service dependencies: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ServiceGraphView{}, fmt.Errorf("store: commit replace service dependencies: %w", err)
	}
	return s.GetServiceDependencies(ctx, projectID, serviceID)
}

// assertServiceNotPinnedByFileEdgesTx blocks deleting a service that an applied
// file-owned service names in depends_on (§14.2): the desired edge pins its
// target, and the 409 names the provider and the referencing service so the
// operator knows which bundle to edit. UI-owned edges never pin. An ORPHANED
// managed service still pins ([276] P1-2): MaC deliberately preserves an
// absent-from-bundle service as file-owned last-known-good, so its desired
// edges are exactly the state the pin exists to keep restorable — excluding
// orphans would let a target delete break LKG literally.
func assertServiceNotPinnedByFileEdgesTx(ctx context.Context, tx pgx.Tx, serviceID string) error {
	var provider, slug string
	err := tx.QueryRow(ctx, `
		SELECT ms.provider_id, c.slug
		  FROM service_dependencies d
		  JOIN managed_services ms ON ms.service_id = d.service_id
		  JOIN services c ON c.id = d.service_id
		 WHERE d.depends_on_id = $1
		 ORDER BY c.slug
		 LIMIT 1`, serviceID).Scan(&provider, &slug)
	if noRows(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: check file-pinned edges: %w", err)
	}
	return ErrServicePinnedByFile{Provider: provider, Service: slug}
}

func diffIDs(current, next map[string]bool) (added, removed []string) {
	for id := range next {
		if !current[id] {
			added = append(added, id)
		}
	}
	for id := range current {
		if !next[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func slugsFor(ctx context.Context, tx pgx.Tx, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, slug FROM services WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: resolve slugs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return nil, fmt.Errorf("store: scan slug: %w", err)
		}
		out[id] = slug
	}
	return out, rows.Err()
}

func mapSlugs(ids []string, slugs map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s, ok := slugs[id]; ok {
			out = append(out, s)
		} else {
			// A removed edge whose service was deleted concurrently: the id is the
			// only durable name left.
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
