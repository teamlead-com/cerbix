package store

import (
	"context"
	"fmt"
)

// The project set of a status page, written ONCE and asked in both directions.
//
// Three surfaces used to answer this question and all three answered it differently: the public
// render collected only the components' `source_project` and never seeded the page's OWN project;
// the feed seeded the own project and then walked `components → monitors`, so a service-backed
// component brought nothing; and the subscriber fan-out was that same monitor JOIN, so a page made
// entirely of Service components emailed nobody about its own incidents. A reader saw the incident
// on the page and got no mail about it, which is worse than either behaviour on its own.
//
// The authoritative axis (D-0021/D-0055, made explicit by D-0180) is `status_pages.project_id` when
// the page is project-scoped, UNION every non-NULL `components.source_project`. NOT filtered by
// `source`: a conversion deliberately keeps the DORMANT binding's `source_project`, and the renderer
// already resolves such a component to that project, so filtering here would silently narrow what
// the page shows compared to what it emails about.
const statusPageProjectsSQL = `
	  SELECT sp.project_id FROM status_pages sp
	   WHERE sp.id = $1 AND sp.project_id IS NOT NULL
	   UNION
	  SELECT c.source_project FROM components c
	   WHERE c.status_page_id = $1 AND c.source_project IS NOT NULL`

// statusPageReportsProjectSQL is the SAME axis as a predicate over one page row, for the INVERSE
// direction. It is here, beside the forward query, and not written out again in `subscribers.go`:
// the two are one rule and the whole point of D-0180 is that they cannot drift. A `WHERE id IN
// (forward query)` would express it in one string, but it would make the subscriber fan-out a
// correlated scan of every page for every project; this keeps the text shared and the plan sane.
//
// `$1` is the PROJECT and `sp` is the page in the caller's FROM.
const statusPageReportsProjectSQL = `(
		    sp.project_id = $1
		    OR EXISTS (SELECT 1 FROM components c
		                WHERE c.status_page_id = sp.id AND c.source_project = $1)
		)`

// StatusPageProjectIDs is the FORWARD direction: which projects' incidents this page reports.
// Sorted, so two identical pages issue identical SQL downstream.
func (s *Store) StatusPageProjectIDs(ctx context.Context, pageID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, statusPageProjectsSQL+` ORDER BY 1`, pageID)
	if err != nil {
		return nil, fmt.Errorf("store: status page projects: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan status page project: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate status page projects: %w", err)
	}
	return out, nil
}
