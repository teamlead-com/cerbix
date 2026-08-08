package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// ErrBundleTenantNotFound means the bundle's organization/project slug pair does not resolve
// to an existing tenant. The file provider never creates tenants (spec §5); the caller keeps
// last-known-good and records the rejection.
var ErrBundleTenantNotFound = errors.New("store: bundle organization/project not found")

// FileConfigChannel is the scheduler wake-up channel for a committed execution-config change
// from a file apply (spec §12). Payload is bounded generation/tenant identity, never YAML.
const FileConfigChannel = "monitor_config_changed"

// ApplyResult is a bounded summary of one bundle apply for metrics/logs (no per-UID labels).
type ApplyResult struct {
	Organization string
	Project      string
	Generation   int64
	Counts       map[fileprovider.Action]int
	Changed      bool // execution config changed → scheduler NOTIFY emitted
}

// ApplyFileManagedBundle reconciles one desired project bundle into PostgreSQL in ONE
// transaction (spec §9): resolve tenant, read the provider-owned current set, plan via
// fileprovider, then apply create/update/dependency_update/noop/orphan/restore with the
// D-0142 config-write contract, dependency edges, provenance, monotonic generation, and the
// scheduler NOTIFY — all atomic, deterministic-ordered, and per-project fault-isolated.
func (s *Store) ApplyFileManagedBundle(ctx context.Context, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration) (ApplyResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("store: begin apply bundle: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Resolve tenant in ONE query, constraining the project by its organization (spec §5).
	var orgID, projID string
	err = tx.QueryRow(ctx,
		`SELECT o.id, p.id FROM projects p JOIN organizations o ON o.id = p.org_id
		  WHERE o.slug = $1 AND p.slug = $2`, desired.Organization, desired.Project).Scan(&orgID, &projID)
	if noRows(err) {
		return ApplyResult{}, ErrBundleTenantNotFound
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("store: resolve bundle tenant: %w", err)
	}

	// Serialize dependency-graph mutations for this project (same lock the API path uses).
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('monitor_dependencies'), hashtext($1))`, projID); err != nil {
		return ApplyResult{}, fmt.Errorf("store: lock dependency graph: %w", err)
	}

	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&dbNow); err != nil {
		return ApplyResult{}, fmt.Errorf("store: apply clock: %w", err)
	}

	current, idByUID, orphanedAt, enabledByUID, err := s.readManagedSet(ctx, tx, providerID, orgID, projID)
	if err != nil {
		return ApplyResult{}, err
	}

	plan, err := fileprovider.Plan(desired, current)
	if err != nil {
		return ApplyResult{}, err // typed *fileprovider.BundleError (e.g. type_change) — bundle rejected, LKG kept
	}

	// Current bundle generation → next.
	var gen int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(generation, 0) FROM file_provider_bundles
		  WHERE provider_id = $1 AND org_id = $2 AND project_id = $3`, providerID, orgID, projID).Scan(&gen); err != nil && !noRows(err) {
		return ApplyResult{}, fmt.Errorf("store: read bundle generation: %w", err)
	}
	newGen := gen + 1

	changed := false
	// Phase 1 — monitors + provenance.
	for _, e := range plan.Entries {
		switch e.Action {
		case fileprovider.ActionCreate:
			m := desired.Monitors[e.UID].Monitor
			m.ProjectID = projID
			if m.Type == domain.MonitorPush {
				tok, terr := generatePushTokenStore()
				if terr != nil {
					return ApplyResult{}, terr
				}
				m.PushToken = tok
			}
			created, cerr := insertMonitorTx(ctx, tx, s, m)
			if cerr != nil {
				return ApplyResult{}, cerr
			}
			idByUID[e.UID] = created.ID
			if _, err := tx.Exec(ctx,
				`INSERT INTO managed_monitors (monitor_id, provider_id, org_id, project_id, source_uid, spec_hash, source_path, generation, applied_at)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				created.ID, providerID, orgID, projID, e.UID, desired.Monitors[e.UID].Hash, sourcePath, newGen, dbNow); err != nil {
				return ApplyResult{}, fmt.Errorf("store: insert provenance: %w", err)
			}
			changed = true

		case fileprovider.ActionUpdate, fileprovider.ActionRestore:
			id := idByUID[e.UID]
			m := desired.Monitors[e.UID].Monitor
			m.ID, m.ProjectID = id, projID
			if _, uerr := updateMonitorTx(ctx, tx, s, m); uerr != nil {
				return ApplyResult{}, uerr
			}
			if _, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET spec_hash=$2, source_path=$3, generation=$4, orphaned_at=NULL, applied_at=$5
				  WHERE monitor_id=$1`,
				id, desired.Monitors[e.UID].Hash, sourcePath, newGen, dbNow); err != nil {
				return ApplyResult{}, fmt.Errorf("store: update provenance: %w", err)
			}
			changed = true

		case fileprovider.ActionDependencyUpdate, fileprovider.ActionNoop:
			// Provenance-only: a no-op (comment/rename) or a dependency-only change must NOT
			// write the monitor row or bump execution_revision (spec §7). Advancing generation
			// and refreshing the relative path is provenance, not a semantic write.
			if _, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET source_path=$2, generation=$3 WHERE monitor_id=$1`,
				idByUID[e.UID], sourcePath, newGen); err != nil {
				return ApplyResult{}, fmt.Errorf("store: touch provenance: %w", err)
			}

		case fileprovider.ActionOrphan:
			// First valid absence: record orphaned_at. Do NOT disable yet — the grace timer
			// (handled below) governs the disable, and history is never deleted (spec §10).
			if _, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET orphaned_at=$2 WHERE monitor_id=$1 AND orphaned_at IS NULL`,
				idByUID[e.UID], dbNow); err != nil {
				return ApplyResult{}, fmt.Errorf("store: mark orphan: %w", err)
			}
		}
	}

	// Grace-elapsed disable: an already-orphaned, still-enabled, still-absent UID past its
	// grace window is disabled through the config-write path (§10). Deterministic UID order.
	if orphanGrace >= 0 {
		var graced []string
		for _, c := range current {
			if !c.Orphaned {
				continue
			}
			if _, present := desired.Monitors[c.UID]; present {
				continue
			}
			if !enabledByUID[c.UID] {
				continue
			}
			if dbNow.Sub(orphanedAt[c.UID]) >= orphanGrace {
				graced = append(graced, c.UID)
			}
		}
		sort.Strings(graced)
		for _, uid := range graced {
			m, gerr := getMonitorTx(ctx, tx, s, idByUID[uid])
			if gerr != nil {
				return ApplyResult{}, gerr
			}
			m.Enabled = false
			if _, uerr := updateMonitorTx(ctx, tx, s, m); uerr != nil {
				return ApplyResult{}, uerr
			}
			changed = true
		}
	}

	// Phase 2 — dependency edges for created/restored/dep-changed monitors (deterministic).
	for _, e := range plan.Entries {
		reassert := e.Action == fileprovider.ActionCreate || e.Action == fileprovider.ActionRestore || e.DependencyChange
		if !reassert {
			continue
		}
		id := idByUID[e.UID]
		parents := make([]string, 0, len(desired.Monitors[e.UID].DependsOn))
		for _, depUID := range desired.Monitors[e.UID].DependsOn {
			pid, ok := idByUID[depUID]
			if !ok {
				return ApplyResult{}, fmt.Errorf("store: dependency %q unresolved for %q", depUID, e.UID)
			}
			parents = append(parents, pid)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM monitor_dependencies WHERE monitor_id=$1`, id); err != nil {
			return ApplyResult{}, fmt.Errorf("store: clear file deps: %w", err)
		}
		for _, pid := range parents {
			if _, err := tx.Exec(ctx,
				`INSERT INTO monitor_dependencies (monitor_id, depends_on_id) VALUES ($1,$2)`, id, pid); err != nil {
				return ApplyResult{}, fmt.Errorf("store: insert file dep: %w", err)
			}
		}
	}

	// Bundle generation/provenance + status, upserted in the same tx.
	if _, err := tx.Exec(ctx,
		`INSERT INTO file_provider_bundles (provider_id, org_id, project_id, source_path, content_hash, generation, status, last_error, applied_at, attempted_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'applied','',$7,$7)
		 ON CONFLICT (provider_id, org_id, project_id)
		 DO UPDATE SET source_path=EXCLUDED.source_path, content_hash=EXCLUDED.content_hash,
		               generation=EXCLUDED.generation, status='applied', last_error='',
		               applied_at=EXCLUDED.applied_at, attempted_at=EXCLUDED.attempted_at, updated_at=now()`,
		providerID, orgID, projID, sourcePath, bundleContentHash(desired), newGen, dbNow); err != nil {
		return ApplyResult{}, fmt.Errorf("store: upsert bundle: %w", err)
	}

	// Scheduler wake-up ONLY when execution config changed (spec §12). Bounded payload.
	if changed {
		payload, _ := json.Marshal(struct {
			Provider   string `json:"provider"`
			Org        string `json:"org"`
			Project    string `json:"project"`
			Generation int64  `json:"generation"`
		}{providerID, orgID, projID, newGen})
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, FileConfigChannel, string(payload)); err != nil {
			return ApplyResult{}, fmt.Errorf("store: notify config change: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("store: commit apply bundle: %w", err)
	}
	return ApplyResult{Organization: desired.Organization, Project: desired.Project, Generation: newGen, Counts: plan.Counts(), Changed: changed}, nil
}

// TenantRef is an (organization, project) slug pair a provider owns bundles for.
type TenantRef struct {
	Organization string
	Project      string
}

// FileProviderProjects lists the distinct tenants a provider currently owns a bundle for
// (by slug). The reconcile loop uses it to detect a whole-project disappearance: an owned
// project absent from the current valid snapshot is a candidate for orphaning (unless the
// scan suspended orphaning). Ordered for deterministic processing.
func (s *Store) FileProviderProjects(ctx context.Context, providerID string) ([]TenantRef, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT o.slug, p.slug
		   FROM file_provider_bundles b
		   JOIN organizations o ON o.id = b.org_id
		   JOIN projects p ON p.id = b.project_id
		  WHERE b.provider_id = $1
		  ORDER BY o.slug, p.slug`, providerID)
	if err != nil {
		return nil, fmt.Errorf("store: file provider projects: %w", err)
	}
	defer rows.Close()
	var out []TenantRef
	for rows.Next() {
		var t TenantRef
		if err := rows.Scan(&t.Organization, &t.Project); err != nil {
			return nil, fmt.Errorf("store: scan provider project: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// readManagedSet reads the provider's owned monitors for one tenant into fileprovider
// current-state values (plus id/orphan/enabled lookups the apply needs), inside the tx.
func (s *Store) readManagedSet(ctx context.Context, tx pgx.Tx, providerID, orgID, projID string) (
	current []fileprovider.CurrentMonitor, idByUID map[string]string, orphanedAt map[string]time.Time, enabledByUID map[string]bool, err error,
) {
	rows, err := tx.Query(ctx,
		`SELECT mm.source_uid, m.type, mm.spec_hash, m.id::text, mm.orphaned_at, m.enabled,
		        COALESCE(array_remove(array_agg(dep_mm.source_uid), NULL), '{}')
		   FROM managed_monitors mm
		   JOIN monitors m ON m.id = mm.monitor_id
		   LEFT JOIN monitor_dependencies d ON d.monitor_id = mm.monitor_id
		   LEFT JOIN managed_monitors dep_mm ON dep_mm.monitor_id = d.depends_on_id
		        AND dep_mm.provider_id = mm.provider_id AND dep_mm.org_id = mm.org_id AND dep_mm.project_id = mm.project_id
		  WHERE mm.provider_id=$1 AND mm.org_id=$2 AND mm.project_id=$3
		  GROUP BY mm.source_uid, m.type, mm.spec_hash, m.id, mm.orphaned_at, m.enabled`,
		providerID, orgID, projID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: read managed set: %w", err)
	}
	defer rows.Close()
	idByUID = map[string]string{}
	orphanedAt = map[string]time.Time{}
	enabledByUID = map[string]bool{}
	for rows.Next() {
		var (
			uid, typ, hash, id string
			orph               *time.Time
			enabled            bool
			deps               []string
		)
		if err := rows.Scan(&uid, &typ, &hash, &id, &orph, &enabled, &deps); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("store: scan managed set: %w", err)
		}
		cm := fileprovider.CurrentMonitor{UID: uid, Type: typ, Hash: hash, DependsOn: sortUniq(deps), Orphaned: orph != nil}
		current = append(current, cm)
		idByUID[uid] = id
		enabledByUID[uid] = enabled
		if orph != nil {
			orphanedAt[uid] = *orph
		}
	}
	return current, idByUID, orphanedAt, enabledByUID, rows.Err()
}

// insertMonitorTx inserts a monitor inside an existing transaction (mirrors CreateMonitor's
// column contract) for the file-apply path, which must not open its own connection.
func insertMonitorTx(ctx context.Context, tx pgx.Tx, s *Store, m domain.Monitor) (domain.Monitor, error) {
	conditions := m.Conditions
	if conditions == nil {
		conditions = []string{}
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	region := m.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	var pushHash, pushEnc *string
	if m.PushToken != "" {
		h := HashToken(m.PushToken)
		enc, eerr := s.cipher.Encrypt(m.PushToken)
		if eerr != nil {
			return domain.Monitor{}, fmt.Errorf("store: encrypt push token: %w", eerr)
		}
		pushHash, pushEnc = &h, &enc
	}
	config, err := s.marshalConfig(m)
	if err != nil {
		return domain.Monitor{}, err
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO monitors (project_id, name, type, target, interval_seconds, timeout_seconds, retries, conditions, enabled, push_token_hash, push_token_enc, method, grace_seconds, config, auto_incident, failure_threshold, renotify_seconds, tags, region, escalation_policy_id, confirm_interval_seconds)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) RETURNING `+monitorColumns,
		m.ProjectID, m.Name, string(m.Type), m.Target, m.IntervalSeconds, m.TimeoutSeconds, m.Retries, conditions, m.Enabled, pushHash, pushEnc, methodOrGet(m), m.GraceSeconds, config, m.AutoIncident, m.FailureThreshold, m.RenotifySeconds, tags, region, nullableID(m.EscalationPolicyID), m.ConfirmIntervalSeconds)
	created, err := s.scanMonitor(row)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: insert monitor: %w", err)
	}
	return created, nil
}

// getMonitorTx reads one monitor by id inside a transaction.
func getMonitorTx(ctx context.Context, tx pgx.Tx, s *Store, id string) (domain.Monitor, error) {
	row := tx.QueryRow(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE id = $1`, id)
	m, err := s.scanMonitor(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: get monitor (tx): %w", err)
	}
	return m, nil
}

// bundleContentHash is a stable digest of all monitor canonical hashes for the bundle's
// last-known-good content_hash (order-independent).
func bundleContentHash(dp *fileprovider.DesiredProject) string {
	hashes := make([]string, 0, len(dp.Monitors))
	for uid, dm := range dp.Monitors {
		hashes = append(hashes, uid+":"+dm.Hash)
	}
	sort.Strings(hashes)
	sum := HashToken("")
	for _, h := range hashes {
		sum = HashToken(sum + h)
	}
	return sum
}

// generatePushTokenStore mints a push bearer token server-side, fail-closed (never issues a
// predictable secret). Mirrors the API's generator so file-created push monitors are secure.
func generatePushTokenStore() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: crypto/rand unavailable for push token: %w", err)
	}
	return "cbxp_" + hex.EncodeToString(b), nil
}

// sortUniq sorts + dedupes a string slice (drops empties), matching fileprovider set
// normalization so current/desired dependency sets compare equal.
func sortUniq(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
