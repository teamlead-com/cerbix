package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// CorrelateIncident is the phase-3 correlation attempt (FR-021 §14.3): ONE
// store transaction that computes the incident's impact links through the
// service graph and writes links + 🕸 notes atomically, or none of it.
//
//   - The attempt reads membership, graph and witnesses under the project's
//     service_membership → service_graph advisory locks (the §15.4 order), so
//     every fact it encodes existed together in ONE committed state — separate
//     READ COMMITTED reads could mix an already-deleted edge with a witness
//     that only committed after the deletion ([276] P1-1).
//   - The anchor's own services resolve via monitors[] membership (role
//     'context', never 'sli'). Correlation runs only at open-time events, so
//     the drift window is the delivery lag, accepted and stated.
//   - probable_root marks EVERY upstream service on a path with an open
//     incident; affected marks downstream ones; both directions back-fill the
//     counterpart incident so relation content never depends on opening order.
//   - Witnesses are BOUNDED per endpoint service: the oldest
//     domain.MaxCorrelationWitnessesPerService open anchored incidents by
//     (started_at, id) — a deterministic function of the committed state, so a
//     redelivery against unchanged state selects the identical set. Overflow
//     is returned for the caller to log and count — never silent ([276] P1-3,
//     [278] conditions, accepted [280]).
//   - Every involved incident row is locked FOR UPDATE in ascending id order
//     and its open state RECHECKED under the lock: a link or note landing in a
//     just-resolved incident would rewrite closed history.
//   - Link inserts are ON CONFLICT DO NOTHING; a 🕸 note is written only for
//     an incident that gained at least one NEW row, in this same transaction —
//     a redelivery inserts nothing and writes nothing.
//
// Returns the newly inserted links and the witness-overflow count. A missing,
// resolved or non-anchored incident returns (nil, 0, nil): skipped by design,
// delivered.
func (s *Store) CorrelateIncident(ctx context.Context, incidentID string) ([]domain.ServiceImpactLink, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("store: begin correlate: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The anchor row identifies the project; its identity fields are immutable,
	// so this single-row read may precede the snapshot locks.
	var projectID string
	var monitorID *string
	var status string
	err = tx.QueryRow(ctx,
		`SELECT project_id, monitor_id, status FROM incidents WHERE id = $1`,
		incidentID).Scan(&projectID, &monitorID, &status)
	if noRows(err) {
		return nil, 0, nil // deleted since enqueue — nothing to annotate
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: correlate read incident: %w", err)
	}
	if status == string(domain.IncidentResolved) || monitorID == nil {
		return nil, 0, nil // resolved before any attempt / not monitor-anchored: skipped by design
	}

	// ── the attempt snapshot: membership → graph locks, THEN every read ──────
	// Serializes against declaration writes and edge writes, so membership, the
	// graph and the witness set all come from one committed state (the §15.4
	// order; incident row locks come later, the same direction everywhere).
	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return nil, 0, err
	}
	if err := lockServiceGraph(ctx, tx, projectID); err != nil {
		return nil, 0, err
	}

	own, err := servicesOfMonitorTx(ctx, tx, *monitorID)
	if err != nil {
		return nil, 0, err
	}
	if len(own) == 0 {
		return nil, 0, nil // a monitor in no service's monitors[] writes nothing
	}

	g, err := loadProjectGraphTx(ctx, tx, projectID)
	if err != nil {
		return nil, 0, err
	}
	if len(g.parents) == 0 && len(g.children) == 0 {
		return nil, 0, nil // no edges in the project — no impact to link
	}

	// The anchor's REACHABLE endpoint services — ancestors and descendants of
	// its own services in the locked snapshot. Witnesses are read for those
	// endpoints ONLY and capped in SQL ([283]): reading the whole project's open
	// incidents would keep read work O(project incidents) per attempt and count
	// overflow for services this anchor cannot even link — false omission
	// telemetry. An attempt with no reachable endpoint reads no witnesses.
	upWalks := map[string]map[string][]string{}
	downWalks := map[string]map[string][]string{}
	endpointSet := map[string]bool{}
	for _, sOwn := range own {
		upWalks[sOwn] = g.walk(sOwn, true)
		downWalks[sOwn] = g.walk(sOwn, false)
		for e := range upWalks[sOwn] {
			endpointSet[e] = true
		}
		for e := range downWalks[sOwn] {
			endpointSet[e] = true
		}
	}
	if len(endpointSet) == 0 {
		return nil, 0, nil
	}
	endpoints := make([]string, 0, len(endpointSet))
	for e := range endpointSet {
		endpoints = append(endpoints, e)
	}
	sort.Strings(endpoints)
	witnesses, witnessOverflow, err := boundedWitnessesTx(ctx, tx, projectID, incidentID, endpoints)
	if err != nil {
		return nil, 0, err
	}

	type candidate struct {
		incident  string
		service   string
		role      string
		path      []string // root-first, endpoint-inclusive slugs
		witnesses []string // incidents whose openness this link cites (anchor implied)
	}
	better := func(a, b []string) bool { // canonical order: shortest, then lex
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return strings.Join(a, "\x00") < strings.Join(b, "\x00")
	}
	// Dedupe candidates by (incident, service, role), keeping the canonical path
	// and the union of witnesses.
	byKey := map[string]*candidate{}
	var keys []string
	add := func(c candidate) {
		k := c.incident + "|" + c.service + "|" + c.role
		if prev, ok := byKey[k]; ok {
			if better(c.path, prev.path) {
				prev.path = c.path
			}
			prev.witnesses = append(prev.witnesses, c.witnesses...)
			return
		}
		cc := c
		byKey[k] = &cc
		keys = append(keys, k)
	}

	for _, sOwn := range own {
		// upward: ancestors with an open incident → probable_root on the anchor,
		// AND the mirror back-fill: each witnessing upstream incident gains an
		// affected link naming the anchor's service. Without the mirror the
		// relation's CONTENT would depend on which incident opened first — the
		// child-first interleaving would leave the parent without its affected
		// row — and §14.3's symmetry is about content, not merely observation.
		for anc, path := range upWalks[sOwn] {
			for _, j := range witnesses[anc] {
				add(candidate{incident: incidentID, service: anc, role: domain.ImpactProbableRoot, path: path, witnesses: []string{j}})
				add(candidate{incident: j, service: sOwn, role: domain.ImpactAffected, path: path, witnesses: []string{incidentID}})
			}
		}
		// downward: open incidents on descendants → affected on the anchor, and
		// the probable_root back-fill on each such incident (same root-first array).
		for desc, path := range downWalks[sOwn] {
			for _, j := range witnesses[desc] {
				add(candidate{incident: incidentID, service: desc, role: domain.ImpactAffected, path: path, witnesses: []string{j}})
				add(candidate{incident: j, service: sOwn, role: domain.ImpactProbableRoot, path: path, witnesses: []string{j}})
			}
		}
	}
	if len(byKey) == 0 {
		return nil, witnessOverflow, nil
	}

	// ── lock phase: every involved incident, ascending id, then RECHECK open ──
	// The lock set is the anchor plus SELECTED witnesses only — the [278] bound
	// caps it by construction.
	involved := map[string]bool{incidentID: true}
	for _, c := range byKey {
		involved[c.incident] = true
		for _, w := range c.witnesses {
			involved[w] = true
		}
	}
	ids := make([]string, 0, len(involved))
	for id := range involved {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	stillOpen := map[string]string{} // id → current status, open rows only
	rows, err := tx.Query(ctx,
		`SELECT id, status FROM incidents WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("store: lock correlated incidents: %w", err)
	}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("store: scan locked incident: %w", err)
		}
		if st != string(domain.IncidentResolved) {
			stillOpen[id] = st
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate locked incidents: %w", err)
	}
	if _, ok := stillOpen[incidentID]; !ok {
		return nil, 0, nil // anchor resolved while we computed: the whole attempt is void
	}

	// ── insert phase: links, then notes for newly gained rows — atomically ────
	sort.Strings(keys)
	inserted := map[string][]domain.ServiceImpactLink{} // incident id → its NEW links
	var all []domain.ServiceImpactLink
	for _, k := range keys {
		c := byKey[k]
		if _, ok := stillOpen[c.incident]; !ok {
			continue // a resolved incident is never annotated
		}
		open := false
		for _, w := range c.witnesses {
			if _, ok := stillOpen[w]; ok {
				open = true
				break
			}
		}
		if !open {
			continue // the evidence resolved under us: no link without a live witness
		}
		var newRow bool
		err := tx.QueryRow(ctx, `
			INSERT INTO incident_service_impacts (incident_id, service_id, project_id, role, path)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT DO NOTHING
			RETURNING true`,
			c.incident, c.service, projectID, c.role, c.path).Scan(&newRow)
		if noRows(err) {
			continue // already linked by an earlier attempt — idempotent
		}
		if err != nil {
			return nil, 0, fmt.Errorf("store: insert impact link: %w", err)
		}
		link := domain.ServiceImpactLink{
			ServiceID: c.service,
			Slug:      g.slug[c.service],
			Name:      g.name[c.service],
			Role:      c.role,
			Path:      c.path,
		}
		inserted[c.incident] = append(inserted[c.incident], link)
		all = append(all, link)
	}

	for inc, links := range inserted {
		body := domain.RenderImpactNote(links)
		if body == "" {
			continue
		}
		// Test-only fault injection: proves the §14.3 "one transaction or nothing"
		// contract — a failed note insert rolls the LINK batch back too, and the
		// retry then produces exactly one batch ([276] P1-5a). nil in production.
		if s.correlateNoteFault != nil {
			if err := s.correlateNoteFault(); err != nil {
				return nil, 0, fmt.Errorf("store: insert impact note: %w", err)
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO incident_updates (incident_id, status, body, author) VALUES ($1, $2, $3, 'system')`,
			inc, stillOpen[inc], body); err != nil {
			return nil, 0, fmt.Errorf("store: insert impact note: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("store: commit correlate: %w", err)
	}
	return all, witnessOverflow, nil
}

// ListIncidentImpacts returns an incident's stored impact links, canonical
// order (role, then path length, then slug) — the authenticated DETAIL
// enrichment of §14.4; the incident list never carries these. Tenant-scoped at
// the owning data boundary ([276] P0-1): the project predicate is on the LINK
// rows themselves, so an incident id from another project yields nothing even
// if a caller's access check is buggy.
func (s *Store) ListIncidentImpacts(ctx context.Context, projectID, incidentID string) ([]domain.ServiceImpactLink, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.service_id, s.slug, s.name, i.role, i.path, i.computed_at
		  FROM incident_service_impacts i
		  JOIN services s ON s.id = i.service_id AND s.project_id = i.project_id
		 WHERE i.incident_id = $1 AND i.project_id = $2
		 ORDER BY i.role DESC, array_length(i.path, 1), s.slug`, incidentID, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list incident impacts: %w", err)
	}
	defer rows.Close()
	var out []domain.ServiceImpactLink
	for rows.Next() {
		var l domain.ServiceImpactLink
		if err := rows.Scan(&l.ServiceID, &l.Slug, &l.Name, &l.Role, &l.Path, &l.ComputedAt); err != nil {
			return nil, fmt.Errorf("store: scan incident impact: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ── graph loading and deterministic walking ─────────────────────────────────

type projectGraph struct {
	parents  map[string][]string // child → parents (depends_on)
	children map[string][]string // parent → children
	slug     map[string]string
	name     map[string]string
}

// boundedWitnessesTx selects, for each REACHABLE endpoint service, the oldest
// domain.MaxCorrelationWitnessesPerService open monitor-anchored incidents by
// ascending (started_at, id) — the deterministic [278] selection, scoped and
// CAPPED in SQL ([283]): the row transfer is at most endpoints × cap, never
// O(project open incidents), and the exact per-endpoint totals come from an
// aggregate so overflow counts ONLY omitted witnesses of reachable endpoints.
// The anchor is the attempt's subject, never a witness, never counted.
func boundedWitnessesTx(
	ctx context.Context, tx pgx.Tx, projectID, anchorID string, endpoints []string,
) (map[string][]string, int, error) {
	witnesses := map[string][]string{}
	rows, err := tx.Query(ctx, `
		SELECT e.sid, w.id
		  FROM unnest($3::uuid[]) AS e(sid)
		  JOIN LATERAL (
		      SELECT i.id
		        FROM incidents i
		        JOIN service_member_refs r
		          ON r.monitor_id = i.monitor_id AND r.role = 'context' AND r.service_id = e.sid
		       WHERE i.project_id = $1 AND i.status <> 'resolved'
		         AND i.monitor_id IS NOT NULL AND i.id <> $2
		       ORDER BY i.started_at, i.id
		       LIMIT $4
		  ) w ON true
		 ORDER BY e.sid, w.id`,
		projectID, anchorID, endpoints, domain.MaxCorrelationWitnessesPerService)
	if err != nil {
		return nil, 0, fmt.Errorf("store: bounded witnesses: %w", err)
	}
	for rows.Next() {
		var sid, inc string
		if err := rows.Scan(&sid, &inc); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("store: scan bounded witness: %w", err)
		}
		witnesses[sid] = append(witnesses[sid], inc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	// Exact overflow, aggregate-only (no row transfer): per reachable endpoint,
	// how many open witnesses exist beyond the cap. Exact count is normative —
	// the worker logs "dropped N" and the counter advances by N ([279]/[280]).
	overflow := 0
	rows, err = tx.Query(ctx, `
		SELECT r.service_id, count(*)
		  FROM incidents i
		  JOIN service_member_refs r
		    ON r.monitor_id = i.monitor_id AND r.role = 'context'
		 WHERE i.project_id = $1 AND i.status <> 'resolved'
		   AND i.monitor_id IS NOT NULL AND i.id <> $2
		   AND r.service_id = ANY($3::uuid[])
		 GROUP BY r.service_id`,
		projectID, anchorID, endpoints)
	if err != nil {
		return nil, 0, fmt.Errorf("store: witness counts: %w", err)
	}
	for rows.Next() {
		var sid string
		var total int
		if err := rows.Scan(&sid, &total); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("store: scan witness count: %w", err)
		}
		if extra := total - domain.MaxCorrelationWitnessesPerService; extra > 0 {
			overflow += extra
		}
	}
	rows.Close()
	return witnesses, overflow, rows.Err()
}

func servicesOfMonitorTx(ctx context.Context, tx pgx.Tx, monitorID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT service_id FROM service_member_refs WHERE monitor_id = $1 AND role = 'context' ORDER BY service_id`,
		monitorID)
	if err != nil {
		return nil, fmt.Errorf("store: services of monitor: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan service of monitor: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func loadProjectGraphTx(ctx context.Context, tx pgx.Tx, projectID string) (projectGraph, error) {
	g := projectGraph{
		parents: map[string][]string{}, children: map[string][]string{},
		slug: map[string]string{}, name: map[string]string{},
	}
	rows, err := tx.Query(ctx,
		`SELECT id, slug, name FROM services WHERE project_id = $1`, projectID)
	if err != nil {
		return g, fmt.Errorf("store: load project services: %w", err)
	}
	for rows.Next() {
		var id, slug, name string
		if err := rows.Scan(&id, &slug, &name); err != nil {
			rows.Close()
			return g, fmt.Errorf("store: scan project service: %w", err)
		}
		g.slug[id], g.name[id] = slug, name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return g, err
	}
	rows, err = tx.Query(ctx,
		`SELECT service_id, depends_on_id FROM service_dependencies WHERE project_id = $1`, projectID)
	if err != nil {
		return g, fmt.Errorf("store: load project graph: %w", err)
	}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return g, fmt.Errorf("store: scan project edge: %w", err)
		}
		g.parents[child] = append(g.parents[child], parent)
		g.children[parent] = append(g.children[parent], child)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return g, err
	}
	// Deterministic neighbour order (by slug) so the shortest/lex canonical path
	// never depends on map or row order.
	bySlug := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool { return g.slug[ids[i]] < g.slug[ids[j]] })
	}
	for _, ids := range g.parents {
		bySlug(ids)
	}
	for _, ids := range g.children {
		bySlug(ids)
	}
	return g, nil
}

// walk BFS-walks from start (up through parents, or down through children) and
// returns every reached node with its CANONICAL path — root-first slug array,
// endpoint-inclusive; shortest wins, equal lengths tie-break lexicographically
// (§14.3). Levels are bounded by the depth cap, which the write-side depth
// invariant makes exhaustive rather than truncating.
func (g projectGraph) walk(start string, up bool) map[string][]string {
	edges := g.children
	if up {
		edges = g.parents
	}
	best := map[string][]string{} // node → canonical path
	frontier := map[string][]string{start: {g.slug[start]}}
	for depth := 0; depth < domain.ServiceGraphDepthCap && len(frontier) > 0; depth++ {
		next := map[string][]string{}
		// Deterministic frontier order.
		nodes := make([]string, 0, len(frontier))
		for n := range frontier {
			nodes = append(nodes, n)
		}
		sort.Slice(nodes, func(i, j int) bool { return g.slug[nodes[i]] < g.slug[nodes[j]] })
		for _, n := range nodes {
			base := frontier[n]
			for _, nb := range edges[n] {
				if nb == start {
					continue
				}
				if _, done := best[nb]; done {
					continue // finalized at a shorter depth
				}
				var cand []string
				if up {
					cand = append([]string{g.slug[nb]}, base...) // upstream endpoint first
				} else {
					cand = append(append([]string{}, base...), g.slug[nb])
				}
				if prev, ok := next[nb]; !ok || strings.Join(cand, "\x00") < strings.Join(prev, "\x00") {
					next[nb] = cand
				}
			}
		}
		for n, p := range next {
			best[n] = p
		}
		frontier = next
	}
	return best
}
