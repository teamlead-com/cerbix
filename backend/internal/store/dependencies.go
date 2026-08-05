package store

import (
	"context"
	"errors"
	"fmt"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// Dependency-graph errors surfaced to the API as 400s.
var (
	ErrDependencyCycle   = errors.New("store: dependency would create a cycle")
	ErrDependencyForeign = errors.New("store: dependency parents must be monitors of the same project")
)

// dependencyDepthCap bounds the recursive graph walks (validation and
// suppression). Chains deeper than this are rejected/ignored — real dependency
// trees are a handful of levels.
const dependencyDepthCap = 10

// ReplaceMonitorDependencies replaces a monitor's parent set atomically after
// validating the edges: every parent must be a different monitor of the same
// project, and none may (transitively) depend on the child — the graph stays a
// DAG. An empty/nil parents clears the set.
func (s *Store) ReplaceMonitorDependencies(ctx context.Context, monitorID, projectID string, parents []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin replace dependencies: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if len(parents) > 0 {
		// Same-project membership (and implicitly: existence).
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM monitors WHERE project_id = $1 AND id = ANY($2) AND id <> $3`,
			projectID, parents, monitorID).Scan(&n); err != nil {
			return fmt.Errorf("store: check dependency parents: %w", err)
		}
		if n != len(dedupe(parents)) {
			return ErrDependencyForeign
		}
		// Cycle check: adding child→parent edges is cyclic iff the child is
		// reachable walking UP from any new parent through existing edges.
		var cyclic bool
		if err := tx.QueryRow(ctx, `
			WITH RECURSIVE anc AS (
			    SELECT d.depends_on_id, 1 AS depth
			      FROM monitor_dependencies d
			     WHERE d.monitor_id = ANY($1)
			    UNION
			    SELECT d.depends_on_id, anc.depth + 1
			      FROM monitor_dependencies d
			      JOIN anc ON d.monitor_id = anc.depends_on_id
			     WHERE anc.depth < $3
			)
			SELECT EXISTS (SELECT 1 FROM anc WHERE depends_on_id = $2)`,
			parents, monitorID, dependencyDepthCap).Scan(&cyclic); err != nil {
			return fmt.Errorf("store: dependency cycle check: %w", err)
		}
		if cyclic {
			return ErrDependencyCycle
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM monitor_dependencies WHERE monitor_id = $1`, monitorID); err != nil {
		return fmt.Errorf("store: clear dependencies: %w", err)
	}
	for _, p := range dedupe(parents) {
		if _, err := tx.Exec(ctx,
			`INSERT INTO monitor_dependencies (monitor_id, depends_on_id) VALUES ($1, $2)`,
			monitorID, p); err != nil {
			return fmt.Errorf("store: insert dependency: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit replace dependencies: %w", err)
	}
	return nil
}

// DownAncestor names one down dependency ancestor of a monitor.
type DownAncestor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DownAncestors walks the dependency graph upward from a monitor and returns
// the (transitive) parents that are currently down or carry an open
// auto-incident — the suppression decision for the child's alerts. Empty when
// the monitor is the root cause of its own outage.
func (s *Store) DownAncestors(ctx context.Context, monitorID string) ([]DownAncestor, error) {
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE anc AS (
		    SELECT d.depends_on_id, 1 AS depth
		      FROM monitor_dependencies d
		     WHERE d.monitor_id = $1
		    UNION
		    SELECT d.depends_on_id, anc.depth + 1
		      FROM monitor_dependencies d
		      JOIN anc ON d.monitor_id = anc.depends_on_id
		     WHERE anc.depth < $2
		)
		SELECT DISTINCT m.id, m.name
		  FROM anc
		  JOIN monitors m ON m.id = anc.depends_on_id
		 WHERE m.status = 'down'
		    OR EXISTS (SELECT 1 FROM incidents i
		                WHERE i.monitor_id = m.id AND i.source = 'auto' AND i.status <> 'resolved')
		 ORDER BY m.name`,
		monitorID, dependencyDepthCap)
	if err != nil {
		return nil, fmt.Errorf("store: down ancestors: %w", err)
	}
	defer rows.Close()
	var out []DownAncestor
	for rows.Next() {
		var a DownAncestor
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, fmt.Errorf("store: scan down ancestor: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate down ancestors: %w", err)
	}
	return out, nil
}

// AppendSuppressionNote marks the monitor's open auto-incident timeline with a
// one-time system entry naming the suppressing ancestor. Idempotent per
// incident (marker-prefix check, like the RCA context note). No open incident —
// silently nothing to annotate.
func (s *Store) AppendSuppressionNote(ctx context.Context, monitorID, rootName string) (bool, error) {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO incident_updates (incident_id, status, body, author)
		SELECT i.id, i.status, $2, 'system'
		  FROM incidents i
		 WHERE i.monitor_id = $1 AND i.source = 'auto' AND i.status <> 'resolved'
		   AND NOT EXISTS (
		       SELECT 1 FROM incident_updates u
		        WHERE u.incident_id = i.id AND u.author = 'system' AND u.body LIKE $3
		   )
		 ORDER BY i.started_at DESC
		 LIMIT 1`,
		monitorID, domain.SuppressionMarker+" alerts muted: depends on "+rootName+", which is down.",
		domain.SuppressionMarker+"%")
	if err != nil {
		return false, fmt.Errorf("store: append suppression note: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

// dedupe returns the ids with duplicates removed, order preserved.
func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
