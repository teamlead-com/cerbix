package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// escapeLike escapes the LIKE wildcards so user input is matched literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Search returns projects, monitors and incidents matching q (case-insensitive
// substring), up to `limit` of each. It does NOT apply tenant scoping — the API
// layer filters the hits by the caller's visibility before returning them.
func (s *Store) Search(ctx context.Context, q string, limit int) ([]domain.SearchHit, error) {
	if limit <= 0 {
		limit = 8
	}
	like := "%" + escapeLike(q) + "%"

	queries := []string{
		`SELECT 'project'::text, p.id, p.org_id, p.id, p.name, p.slug
		   FROM projects p
		  WHERE p.name ILIKE $1 ESCAPE '\' OR p.slug ILIKE $1 ESCAPE '\'
		  ORDER BY p.name LIMIT $2`,
		`SELECT 'monitor'::text, m.id, p.org_id, m.project_id, m.name, m.type
		   FROM monitors m JOIN projects p ON p.id = m.project_id
		  WHERE m.name ILIKE $1 ESCAPE '\'
		  ORDER BY m.name LIMIT $2`,
		`SELECT 'incident'::text, i.id, p.org_id, i.project_id, i.title, i.status
		   FROM incidents i JOIN projects p ON p.id = i.project_id
		  WHERE i.title ILIKE $1 ESCAPE '\'
		  ORDER BY i.started_at DESC LIMIT $2`,
	}

	var hits []domain.SearchHit
	for _, sql := range queries {
		rows, err := s.pool.Query(ctx, sql, like, limit)
		if err != nil {
			return nil, fmt.Errorf("store: search: %w", err)
		}
		batch, err := scanHits(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, batch...)
	}
	return hits, nil
}

func scanHits(rows pgx.Rows) ([]domain.SearchHit, error) {
	defer rows.Close()
	var out []domain.SearchHit
	for rows.Next() {
		var h domain.SearchHit
		if err := rows.Scan(&h.Type, &h.ID, &h.OrgID, &h.ProjectID, &h.Label, &h.Sub); err != nil {
			return nil, fmt.Errorf("store: scan search hit: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate search hits: %w", err)
	}
	return out, nil
}
