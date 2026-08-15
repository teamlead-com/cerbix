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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/fileprovider"
)

// applyBundleTimeout bounds the WHOLE bundle apply (Begin → plan → writes → Commit), on top of
// the per-statement statement_timeout/lock_timeout set inside the tx (spec §17): a bundle with
// thousands of statements cannot wedge the reconcile loop even if each statement stays under its
// own limit.
const applyBundleTimeout = 60 * time.Second

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
	NoChange     bool // nothing changed at all → NO DB write, generation NOT advanced (§7/§17)
}

// ApplyFileManagedBundle reconciles one desired project bundle into PostgreSQL in ONE
// transaction (spec §9): resolve tenant, read the provider-owned current set, plan via
// fileprovider, then apply create/update/dependency_update/noop/orphan/restore with the
// D-0142 config-write contract, dependency edges, provenance, monotonic generation, and the
// scheduler NOTIFY — all atomic, deterministic-ordered, and per-project fault-isolated.
// ApplyFileManagedBundle runs the reconcile on a POOL connection (used by tests and any
// non-leader caller). The leadership-fenced path is LeaderSession.ApplyFileManagedBundle.
// allowAbsence governs whether this apply may act on a UID's ABSENCE from the desired set
// (orphan-mark + grace-disable). It MUST be false whenever the surrounding directory scan is
// ambiguous — an unbound decode/scope error or a scan-level rejection — because absence then
// cannot be trusted as intent; the apply stays non-destructive (create/update/dependency/noop/
// restore only), keeping last-known-good for every absent UID (spec §9.1). The caller computes
// it PROVIDER-WIDE before applying any bundle.
func (s *Store) ApplyFileManagedBundle(ctx context.Context, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration, maxManaged int, allowAbsence bool) (ApplyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, applyBundleTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("store: begin apply bundle: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	res, err := s.applyBundleTx(ctx, tx, providerID, desired, sourcePath, orphanGrace, maxManaged, allowAbsence)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("store: commit apply bundle: %w", err)
	}
	return res, nil
}

// ApplyFileManagedBundle runs the reconcile on the LEADER's pinned connection, so a lost
// advisory lock (connection death) also aborts this transaction — a former leader can never
// commit after losing leadership (fencing, spec §17).
func (ls *LeaderSession) ApplyFileManagedBundle(ctx context.Context, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration, maxManaged int, allowAbsence bool) (ApplyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, applyBundleTimeout)
	defer cancel()
	tx, err := ls.conn.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("store: begin apply bundle (leader): %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	res, err := ls.store.applyBundleTx(ctx, tx, providerID, desired, sourcePath, orphanGrace, maxManaged, allowAbsence)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("store: commit apply bundle (leader fencing): %w", err)
	}
	return res, nil
}

// applyBundleTx is the whole reconcile body on a caller-owned transaction (no begin/commit).
// Both the pool and leader-fenced wrappers call it (spec §9): resolve tenant, read the
// provider-owned current set, plan, apply create/update/dependency_update/noop/orphan/restore
// with the D-0142 config-write contract, dependency edges, provenance, monotonic generation,
// and the scheduler NOTIFY — atomic, deterministic-ordered, per-project fault-isolated.
func (s *Store) applyBundleTx(ctx context.Context, tx pgx.Tx, providerID string, desired *fileprovider.DesiredProject, sourcePath string, orphanGrace time.Duration, maxManaged int, allowAbsence bool) (ApplyResult, error) {
	// Bound the whole transaction: a stuck statement or lock must not wedge the reconcile loop
	// (spec §17). SET LOCAL is tx-scoped.
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '20s'`); err != nil {
		return ApplyResult{}, fmt.Errorf("store: set statement_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		return ApplyResult{}, fmt.Errorf("store: set lock_timeout: %w", err)
	}

	// Serialize ALL applies for this provider so the max_managed_monitors quota check below is
	// authoritative — two concurrent applies (e.g. during a failover window) can't both read
	// free quota (spec §17; iter-0096 §3). Provider-scoped advisory xact lock (released on
	// commit/rollback), namespaced away from the dependency-graph lock.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('file_provider_quota'), hashtext($1))`, providerID); err != nil {
		return ApplyResult{}, fmt.Errorf("store: lock provider quota: %w", err)
	}

	// Resolve tenant in ONE query, constraining the project by its organization (spec §5).
	var orgID, projID string
	err := tx.QueryRow(ctx,
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

	// max_managed_monitors quota (spec §4/§17): only creates grow the provider's owned set.
	// Under the provider quota lock this count is authoritative. Reject the WHOLE bundle if it
	// would push the provider past its cap (LKG kept for this project). maxManaged<=0 = unbounded.
	if maxManaged > 0 {
		creates := 0
		for _, e := range plan.Entries {
			if e.Action == fileprovider.ActionCreate {
				creates++
			}
		}
		if creates > 0 {
			var owned int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM managed_monitors WHERE provider_id = $1`, providerID).Scan(&owned); err != nil {
				return ApplyResult{}, fmt.Errorf("store: count managed monitors: %w", err)
			}
			if owned+creates > maxManaged {
				return ApplyResult{}, &fileprovider.BundleError{
					Reason: fileprovider.ReasonQuotaExceeded,
					Msg:    fmt.Sprintf("provider would own %d monitors, over max_managed_monitors=%d", owned+creates, maxManaged),
				}
			}
		}
	}

	// Current bundle row: existence + generation + persisted diagnostics. The bundle row is
	// written on an axis INDEPENDENT of monitor state (§15): a stale rejected/error status is
	// cleared on a now-clean scan, a rename is picked up even for an empty/all-orphaned bundle,
	// and the first (even empty) bundle is recorded — all WITHOUT rewriting an unchanged row on
	// every resync.
	var (
		gen           int64
		curStatus     string
		curLastErr    string
		curBundlePath string
		bundleExisted bool
	)
	switch rerr := tx.QueryRow(ctx,
		`SELECT generation, status, last_error, source_path FROM file_provider_bundles
		  WHERE provider_id = $1 AND org_id = $2 AND project_id = $3`, providerID, orgID, projID).
		Scan(&gen, &curStatus, &curLastErr, &curBundlePath); {
	case noRows(rerr):
		bundleExisted = false
	case rerr != nil:
		return ApplyResult{}, fmt.Errorf("store: read bundle row: %w", rerr)
	default:
		bundleExisted = true
	}
	newGen := gen + 1

	// Track what ACTUALLY changed so a no-op writes nothing and the generation only advances on
	// a real state change (spec §7/§17):
	//   execChanged  → execution config changed → scheduler NOTIFY (create/update/re-enable/disable)
	//   stateChanged → any managed_monitors/monitor/dep mutation → bump generation + audit
	curHash := make(map[string]string, len(current))
	for _, c := range current {
		curHash[c.UID] = c.Hash
	}
	var execChanged, stateChanged bool

	// Every secret this apply touches is locked HERE, in one id-ordered step, before any
	// monitor row is written — the fixed §4.3 order. Resolving per entry inside the loop
	// below satisfied that order within an entry and violated it across the bundle
	// (S1→M1→S2→M2), which deadlocks against a rotate or rename that takes all its secret
	// rows first.
	needBindings := map[string]domain.Monitor{}
	for _, e := range plan.Entries {
		switch e.Action {
		case fileprovider.ActionCreate, fileprovider.ActionUpdate, fileprovider.ActionRestore:
			needBindings[e.UID] = desired.Monitors[e.UID].Monitor
		}
	}
	bindingsByUID, failedUID, berr := s.planSecretBindings(ctx, tx, projID, needBindings)
	if berr != nil {
		return ApplyResult{}, bindFileSecretRefError(berr, failedUID, desired)
	}

	// Phase 1 — monitors + provenance.
	for _, e := range plan.Entries {
		switch e.Action {
		case fileprovider.ActionCreate:
			m := desired.Monitors[e.UID].Monitor
			m.ProjectID = projID
			bindings := bindingsByUID[e.UID]
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
			if err := replaceMonitorSecretRefsTx(ctx, tx, created.ID, projID, bindings); err != nil {
				return ApplyResult{}, err
			}
			idByUID[e.UID] = created.ID
			if _, err := tx.Exec(ctx,
				`INSERT INTO managed_monitors (monitor_id, provider_id, org_id, project_id, source_uid, spec_hash, source_path, generation, applied_at)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				created.ID, providerID, orgID, projID, e.UID, desired.Monitors[e.UID].Hash, sourcePath, newGen, dbNow); err != nil {
				return ApplyResult{}, fmt.Errorf("store: insert provenance: %w", err)
			}
			execChanged, stateChanged = true, true

		case fileprovider.ActionUpdate:
			id := idByUID[e.UID]
			m := desired.Monitors[e.UID].Monitor
			m.ID, m.ProjectID = id, projID
			bindings := bindingsByUID[e.UID]
			if _, uerr := updateMonitorTx(ctx, tx, s, m); uerr != nil {
				return ApplyResult{}, uerr
			}
			if err := replaceMonitorSecretRefsTx(ctx, tx, id, projID, bindings); err != nil {
				return ApplyResult{}, err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET spec_hash=$2, source_path=$3, generation=$4, orphaned_at=NULL, applied_at=$5
				  WHERE monitor_id=$1`,
				id, desired.Monitors[e.UID].Hash, sourcePath, newGen, dbNow); err != nil {
				return ApplyResult{}, fmt.Errorf("store: update provenance: %w", err)
			}
			execChanged, stateChanged = true, true

		case fileprovider.ActionRestore:
			// Reintroducing a previously-orphaned UID (spec §10). Do a SEMANTIC write only when
			// actually needed — the monitor's ACTUAL enabled state differs from the DESIRED one
			// (e.g. it was grace-disabled but the file wants it enabled), or its config changed. A
			// restore whose desired state already matches (incl. a declarative `enabled: false`
			// that is already disabled) only CLEARS the orphan mark; it must NOT bump
			// execution_revision, reset the watermark, notify, or count as an execution change.
			id := idByUID[e.UID]
			if enabledByUID[e.UID] != desired.Monitors[e.UID].Monitor.Enabled || desired.Monitors[e.UID].Hash != curHash[e.UID] {
				m := desired.Monitors[e.UID].Monitor
				m.ID, m.ProjectID = id, projID
				bindings := bindingsByUID[e.UID]
				if _, uerr := updateMonitorTx(ctx, tx, s, m); uerr != nil {
					return ApplyResult{}, uerr
				}
				if err := replaceMonitorSecretRefsTx(ctx, tx, id, projID, bindings); err != nil {
					return ApplyResult{}, err
				}
				if _, err := tx.Exec(ctx,
					`UPDATE managed_monitors SET spec_hash=$2, source_path=$3, generation=$4, orphaned_at=NULL, applied_at=$5 WHERE monitor_id=$1`,
					id, desired.Monitors[e.UID].Hash, sourcePath, newGen, dbNow); err != nil {
					return ApplyResult{}, fmt.Errorf("store: restore provenance: %w", err)
				}
				execChanged = true
			} else {
				// Pure un-orphan: clear the mark + refresh provenance, no monitor write.
				if _, err := tx.Exec(ctx,
					`UPDATE managed_monitors SET source_path=$2, generation=$3, orphaned_at=NULL, applied_at=$4 WHERE monitor_id=$1`,
					id, sourcePath, newGen, dbNow); err != nil {
					return ApplyResult{}, fmt.Errorf("store: restore un-orphan: %w", err)
				}
			}
			stateChanged = true // clearing orphaned_at is a real state change

		case fileprovider.ActionDependencyUpdate:
			// The declarative dependency graph changed (affects delivery-time suppression, not
			// scheduling): refresh provenance + audit, but NO execution_revision bump / NOTIFY.
			if _, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET source_path=$2, generation=$3 WHERE monitor_id=$1`,
				idByUID[e.UID], sourcePath, newGen); err != nil {
				return ApplyResult{}, fmt.Errorf("store: dep provenance: %w", err)
			}
			stateChanged = true

		case fileprovider.ActionNoop:
			// Nothing semantic changed. Refresh ONLY the per-monitor provenance path if the file
			// was renamed — never bump the generation or touch the monitor (spec §7). The
			// `IS DISTINCT FROM` guard means a steady state writes NOTHING (no amplification, §17).
			if _, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET source_path=$2 WHERE monitor_id=$1 AND source_path IS DISTINCT FROM $2`,
				idByUID[e.UID], sourcePath); err != nil {
				return ApplyResult{}, fmt.Errorf("store: touch provenance path: %w", err)
			}

		case fileprovider.ActionOrphan:
			// A UID absent from the desired set. Act on absence ONLY when the scan is trusted
			// (allowAbsence): an ambiguous scan must never read a present-project UID as deleted
			// (spec §9.1). When trusted, record the FIRST orphaned_at; the grace timer below
			// governs the eventual disable, and history is never deleted (§10).
			if !allowAbsence {
				continue
			}
			tag, err := tx.Exec(ctx,
				`UPDATE managed_monitors SET orphaned_at=$2 WHERE monitor_id=$1 AND orphaned_at IS NULL`,
				idByUID[e.UID], dbNow)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("store: mark orphan: %w", err)
			}
			if tag.RowsAffected() > 0 {
				stateChanged = true // only the first absence writes; a repeat is a no-op
			}
		}
	}

	// Grace-elapsed disable: an already-orphaned, still-enabled, still-absent UID past its
	// grace window is disabled through the config-write path (§10). Deterministic UID order.
	// Suppressed entirely when absence is untrusted this scan (§9.1) — a former orphan must
	// not tick toward disable while the snapshot is ambiguous.
	if allowAbsence && orphanGrace >= 0 {
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
			execChanged, stateChanged = true, true
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

	// Bundle-row axis (independent of monitor state, §15): advance generation only on a real
	// state change; otherwise write the row iff it is missing, its recorded path drifted, or a
	// stale rejected/error status must be cleared after a now-clean scan. The first (even empty)
	// bundle records generation 1. A steady-state clean no-op — row already 'applied', no error,
	// path unchanged — writes NOTHING (no amplification, §7/§17).
	effGen := gen
	if stateChanged {
		effGen = newGen
	} else if !bundleExisted {
		effGen = 1
	}
	staleDiag := curStatus != "applied" || curLastErr != ""
	if stateChanged || !bundleExisted || curBundlePath != sourcePath || staleDiag {
		if _, err := tx.Exec(ctx,
			`INSERT INTO file_provider_bundles (provider_id, org_id, project_id, source_path, content_hash, generation, status, last_error, applied_at, attempted_at)
			 VALUES ($1,$2,$3,$4,$5,$6,'applied','',$7,$7)
			 ON CONFLICT (provider_id, org_id, project_id)
			 DO UPDATE SET source_path=EXCLUDED.source_path, content_hash=EXCLUDED.content_hash,
			               generation=EXCLUDED.generation, status='applied', last_error='',
			               applied_at=EXCLUDED.applied_at, attempted_at=EXCLUDED.attempted_at, updated_at=now()`,
			providerID, orgID, projID, sourcePath, bundleContentHash(desired), effGen, dbNow); err != nil {
			return ApplyResult{}, fmt.Errorf("store: upsert bundle: %w", err)
		}
	}

	// Audit on ANY persisted ownership/config state change (spec §9 step 10 / D-0148): create,
	// update, dependency change, restore, orphan-mark, un-orphan, and grace-disable are all
	// "changed applies". A pure no-op (nothing persisted) writes no audit. Wake the scheduler
	// ONLY when execution config changed (§12) — a dependency/ownership-only change affects
	// delivery-time suppression, not scheduling. All in THIS transaction.
	if stateChanged {
		c := plan.Counts()
		target := fmt.Sprintf("provider=%s project=%s gen=%d create=%d update=%d dep=%d restore=%d orphan=%d",
			providerID, desired.Project, effGen,
			c[fileprovider.ActionCreate], c[fileprovider.ActionUpdate], c[fileprovider.ActionDependencyUpdate],
			c[fileprovider.ActionRestore], c[fileprovider.ActionOrphan])
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
			 VALUES ($1, NULL, false, 'file_provider.apply', $2)`, orgID, target); err != nil {
			return ApplyResult{}, fmt.Errorf("store: audit file apply: %w", err)
		}
	}
	if execChanged {
		payload, _ := json.Marshal(struct {
			Provider   string `json:"provider"`
			Org        string `json:"org"`
			Project    string `json:"project"`
			Generation int64  `json:"generation"`
		}{providerID, orgID, projID, effGen})
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, FileConfigChannel, string(payload)); err != nil {
			return ApplyResult{}, fmt.Errorf("store: notify config change: %w", err)
		}
	}

	return ApplyResult{
		Organization: desired.Organization, Project: desired.Project,
		Generation: effGen, Counts: plan.Counts(),
		Changed: execChanged, NoChange: !stateChanged,
	}, nil
}

// bindFileSecretRefError converts a store resolution miss into the file provider's
// bounded, tenant-bindable rejection. Other infrastructure failures retain their cause.
func bindFileSecretRefError(err error, uid string, desired *fileprovider.DesiredProject) error {
	if errors.Is(err, ErrSecretsFeatureDisabled) {
		return &fileprovider.BundleError{
			Reason:  fileprovider.ReasonFeatureDisabled,
			UID:     uid,
			Msg:     "secret inventory references are disabled",
			Org:     desired.Organization,
			Project: desired.Project,
		}
	}
	var missing SecretRefNotFoundError
	if !errors.As(err, &missing) {
		return err
	}
	return &fileprovider.BundleError{
		Reason:  fileprovider.ReasonSecretRefNotFound,
		UID:     uid,
		Msg:     fmt.Sprintf("secret reference %q for setting %q does not exist in the project", missing.Name, missing.Setting),
		Org:     desired.Organization,
		Project: desired.Project,
	}
}

// TenantRef is an (organization, project) slug pair a provider owns bundles for, plus the
// tenant's last-known-good source path. SourcePath is the path the bundle was last applied
// from; the reconcile loop uses it to key path-driven orphan protection (§9.1): an owned
// project whose prior path is present-but-rejected this scan is frozen, not orphaned.
type TenantRef struct {
	Organization string
	Project      string
	SourcePath   string
}

// FileProviderProjects lists the distinct tenants a provider currently owns a bundle for
// (by slug). The reconcile loop uses it to detect a whole-project disappearance: an owned
// project absent from the current valid snapshot is a candidate for orphaning (unless the
// scan suspended orphaning). Ordered for deterministic processing.
func (s *Store) FileProviderProjects(ctx context.Context, providerID string) ([]TenantRef, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT o.slug, p.slug, b.source_path
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
		if err := rows.Scan(&t.Organization, &t.Project, &t.SourcePath); err != nil {
			return nil, fmt.Errorf("store: scan provider project: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// rowExecer is the subset of pgxpool.Pool / *pgxpool.Conn used by recordBundleAttempt, so the
// same body runs on the pool (non-leader/tests) or on the leader's fenced connection.
type rowExecer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// recordBundleAttempt persists a REJECTED/ERRORED apply attempt on the bundle's diagnostics row
// (spec §15): status + a bounded last_error + attempted_at, WITHOUT advancing the applied
// generation or last-known-good. Slugs are resolved tenant-safely; an unresolvable tenant is a
// no-op. lastErr is truncated and must already be bounded/secret-free (a reason code).
func recordBundleAttempt(ctx context.Context, q rowExecer, providerID, orgSlug, projSlug, sourcePath, status, lastErr string) error {
	var orgID, projID string
	err := q.QueryRow(ctx,
		`SELECT o.id, p.id FROM projects p JOIN organizations o ON o.id = p.org_id
		  WHERE o.slug = $1 AND p.slug = $2`, orgSlug, projSlug).Scan(&orgID, &projID)
	if noRows(err) {
		return nil // unbound — nothing to pin
	}
	if err != nil {
		return fmt.Errorf("store: resolve bundle tenant (attempt): %w", err)
	}
	if len(lastErr) > 500 {
		lastErr = lastErr[:500]
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO file_provider_bundles (provider_id, org_id, project_id, source_path, status, last_error, attempted_at)
		 VALUES ($1,$2,$3,$4,$5,$6,now())
		 ON CONFLICT (provider_id, org_id, project_id)
		 DO UPDATE SET status = EXCLUDED.status, last_error = EXCLUDED.last_error,
		               attempted_at = now(), source_path = EXCLUDED.source_path, updated_at = now()`,
		providerID, orgID, projID, sourcePath, status, lastErr); err != nil {
		return fmt.Errorf("store: record bundle attempt: %w", err)
	}
	return nil
}

// RecordBundleAttempt persists a rejection on a POOL connection (tests / non-leader callers).
func (s *Store) RecordBundleAttempt(ctx context.Context, providerID, orgSlug, projSlug, sourcePath, status, lastErr string) error {
	return recordBundleAttempt(ctx, s.pool, providerID, orgSlug, projSlug, sourcePath, status, lastErr)
}

// recordBundleAttemptByPath marks an EXISTING bundle row (matched by provider + relative source
// path) rejected/errored, WITHOUT inserting. It exists for an invalid file at a PREVIOUSLY KNOWN
// path — a known last-known-good bundle whose YAML we cannot decode, so we cannot re-derive its
// tenant (spec §13/§9.1). A brand-new unknown invalid file matches no row and stays process-local.
func recordBundleAttemptByPath(ctx context.Context, q rowExecer, providerID, sourcePath, status, lastErr string) error {
	if sourcePath == "" {
		return nil
	}
	if len(lastErr) > 500 {
		lastErr = lastErr[:500]
	}
	if _, err := q.Exec(ctx,
		`UPDATE file_provider_bundles SET status=$3, last_error=$4, attempted_at=now(), updated_at=now()
		  WHERE provider_id=$1 AND source_path=$2`, providerID, sourcePath, status, lastErr); err != nil {
		return fmt.Errorf("store: record bundle attempt by path: %w", err)
	}
	return nil
}

// RecordBundleAttemptByPath (leader-fenced) marks a known-path bundle rejected without insert.
func (ls *LeaderSession) RecordBundleAttemptByPath(ctx context.Context, providerID, sourcePath, status, lastErr string) error {
	return recordBundleAttemptByPath(ctx, ls.conn, providerID, sourcePath, status, lastErr)
}

// RecordBundleAttempt persists a rejection on the LEADER's fenced connection, so a former leader
// that has lost the advisory lock (connection death) cannot overwrite a new leader's applied
// diagnostics with a stale error (§17). Runs on the same pinned conn the apply tx used.
func (ls *LeaderSession) RecordBundleAttempt(ctx context.Context, providerID, orgSlug, projSlug, sourcePath, status, lastErr string) error {
	return recordBundleAttempt(ctx, ls.conn, providerID, orgSlug, projSlug, sourcePath, status, lastErr)
}

// FileProviderDiagnostic is one bundle's tenant-safe status for the diagnostics API
// (spec §15). Path is the RELATIVE source path; no absolute filesystem path is exposed.
type FileProviderDiagnostic struct {
	Provider     string     `json:"provider"`
	Organization string     `json:"organization"`
	Project      string     `json:"project"`
	SourcePath   string     `json:"path"`
	Generation   int64      `json:"generation"`
	Status       string     `json:"status"`
	LastError    string     `json:"last_error,omitempty"`
	AppliedAt    *time.Time `json:"applied_at,omitempty"`
}

// FileProviderDiagnostics returns file-provider bundle status. An empty orgID returns every
// bundle (global-admin); a non-empty orgID restricts to that organization (org-admin) — the
// filter is applied in SQL so a caller can never see another tenant's bundle/error/path.
func (s *Store) FileProviderDiagnostics(ctx context.Context, orgID string) ([]FileProviderDiagnostic, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT b.provider_id, o.slug, p.slug, b.source_path, b.generation, b.status, b.last_error, b.applied_at
		   FROM file_provider_bundles b
		   JOIN organizations o ON o.id = b.org_id
		   JOIN projects p ON p.id = b.project_id
		  WHERE ($1 = '' OR b.org_id = $1::uuid)
		  ORDER BY b.provider_id, o.slug, p.slug`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: file provider diagnostics: %w", err)
	}
	defer rows.Close()
	var out []FileProviderDiagnostic
	for rows.Next() {
		var d FileProviderDiagnostic
		if err := rows.Scan(&d.Provider, &d.Organization, &d.Project, &d.SourcePath, &d.Generation, &d.Status, &d.LastError, &d.AppliedAt); err != nil {
			return nil, fmt.Errorf("store: scan diagnostic: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FileProviderCounts returns a provider's managed and orphaned monitor counts across all its
// tenants — the `managed_monitors`/`orphaned_monitors` gauges (spec §16).
func (s *Store) FileProviderCounts(ctx context.Context, providerID string) (managed, orphaned int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE orphaned_at IS NOT NULL)
		   FROM managed_monitors WHERE provider_id = $1`, providerID).Scan(&managed, &orphaned)
	if err != nil {
		return 0, 0, fmt.Errorf("store: file provider counts: %w", err)
	}
	return managed, orphaned, nil
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
	slug, err := resolveMonitorSlugTx(ctx, tx, m)
	if err != nil {
		return domain.Monitor{}, err
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds, retries, conditions, enabled, push_token_hash, push_token_enc, method, grace_seconds, config, auto_incident, failure_threshold, renotify_seconds, tags, region, escalation_policy_id, confirm_interval_seconds)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22) RETURNING `+monitorColumns,
		m.ProjectID, m.Name, slug, string(m.Type), m.Target, m.IntervalSeconds, m.TimeoutSeconds, m.Retries, conditions, m.Enabled, pushHash, pushEnc, methodOrGet(m), m.GraceSeconds, config, m.AutoIncident, m.FailureThreshold, m.RenotifySeconds, tags, region, nullableID(m.EscalationPolicyID), m.ConfirmIntervalSeconds)
	created, err := s.scanMonitorNoSecrets(row)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: insert monitor: %w", err)
	}
	return created, nil
}

// getMonitorTx reads one monitor by id inside a transaction.
func getMonitorTx(ctx context.Context, tx pgx.Tx, s *Store, id string) (domain.Monitor, error) {
	row := tx.QueryRow(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE id = $1`, id)
	m, err := s.scanMonitorNoSecrets(row)
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
