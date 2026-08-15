package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// SecretRefNotFoundError means a prepared monitor setting references no secret with
// that name in the monitor's project. The name is inventory metadata (never a value),
// safe to return as a bounded 400/bundle diagnostic.
type SecretRefNotFoundError struct {
	Setting string
	Name    string
}

// ErrSecretsFeatureDisabled prevents non-HTTP writers (notably MaC) from
// introducing inventory references while the operator-visible feature is off.
var ErrSecretsFeatureDisabled = errors.New("store: secret inventory is disabled")

func (e SecretRefNotFoundError) Error() string {
	return fmt.Sprintf("store: secret reference %q for setting %q was not found in the project", e.Name, e.Setting)
}

// ErrMonitorSlugImmutable is returned when an update submits a slug different from the
// stored one. The slug is the reference key a service declaration uses; a silently ignored
// change would leave the caller believing a rename happened.
var ErrMonitorSlugImmutable = errors.New("store: monitor slug is immutable")

// revisionFenceSetSQL is the D-0142 fence: bump the config generation and reset the
// freshness watermark, preserving it for push monitors (a push monitor has no scheduled
// out-of-order compare, so nulling it would fall back to created_at and fire a FALSE
// dead-man DOWN). UpdateMonitor and the secret-rotation fence MUST apply it identically —
// a review specifically required the rotation fence to match character for character — so
// it is one constant rather than two copies that can drift.
const revisionFenceSetSQL = `execution_revision = execution_revision + 1,
		        last_result_ts = CASE WHEN type = 'push' THEN last_result_ts ELSE NULL END`

// monitorSecretBindings resolves every prepared *_ref setting under FOR KEY SHARE.
// Callers MUST invoke it before locking/writing monitor rows: secret rows by id, then
// monitor rows is the fixed §4.3 lock order shared with rename/rotation. The query carries
// project_id in every predicate, so a same-named secret in another tenant is invisible.
func (s *Store) monitorSecretBindings(ctx context.Context, tx pgx.Tx, projectID string, m domain.Monitor) (map[string]string, error) {
	refs := map[string]string{}
	if !domain.CredentialedType(m.Type) {
		return refs, nil
	}
	for _, setting := range []string{"password_ref"} {
		if name := m.Config[setting]; name != "" {
			refs[setting] = name
		}
	}
	if len(refs) == 0 {
		return refs, nil
	}
	if !s.secretsEnabled {
		return nil, ErrSecretsFeatureDisabled
	}
	names := make([]string, 0, len(refs))
	for _, name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	rows, err := tx.Query(ctx,
		`SELECT id::text, name FROM project_secrets
		  WHERE project_id = $1 AND name = ANY($2::text[])
		  ORDER BY id FOR KEY SHARE`, projectID, names)
	if err != nil {
		return nil, fmt.Errorf("store: resolve monitor secret refs: %w", err)
	}
	defer rows.Close()
	idByName := make(map[string]string, len(names))
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: scan monitor secret ref: %w", err)
		}
		idByName[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate monitor secret refs: %w", err)
	}
	bindings := make(map[string]string, len(refs))
	for setting, name := range refs {
		id, ok := idByName[name]
		if !ok {
			return nil, SecretRefNotFoundError{Setting: setting, Name: name}
		}
		bindings[setting] = id
	}
	return bindings, nil
}

// planSecretBindings resolves the *_ref settings of MANY monitors in ONE id-ordered lock
// step, and is what an APPLY must use instead of calling monitorSecretBindings per entry.
//
// The fixed §4.3 order is "secret rows by id asc, then monitor rows". Resolving per entry
// satisfies that order within one entry and violates it across a bundle: the transaction
// takes S1, M1, S2, M2 …, so a concurrent rotate or rename — which takes ALL its secret
// rows in id order first — can hold S2 while waiting for M1, and deadlock. The outcome is
// fail-safe (both sides abort, the bundle keeps its last-known-good, `lock_timeout`
// bounds the wait), but a deadlock under normal operation is not a design, and a bundle
// large enough makes it likely rather than theoretical.
//
// It returns the UID that failed alongside the error so the caller can still attribute a
// missing reference to the entry that declared it.
func (s *Store) planSecretBindings(ctx context.Context, tx pgx.Tx, projectID string, byUID map[string]domain.Monitor) (map[string]map[string]string, string, error) {
	refsByUID := make(map[string]map[string]string, len(byUID))
	uids := make([]string, 0, len(byUID))
	names := map[string]bool{}
	for uid, m := range byUID {
		if !domain.CredentialedType(m.Type) {
			continue
		}
		refs := map[string]string{}
		for _, setting := range []string{"password_ref"} {
			if name := m.Config[setting]; name != "" {
				refs[setting] = name
				names[name] = true
			}
		}
		if len(refs) > 0 {
			refsByUID[uid] = refs
			uids = append(uids, uid)
		}
	}
	if len(refsByUID) == 0 {
		return map[string]map[string]string{}, "", nil
	}
	sort.Strings(uids) // deterministic attribution when several entries are affected
	if !s.secretsEnabled {
		return nil, uids[0], ErrSecretsFeatureDisabled
	}
	wanted := make([]string, 0, len(names))
	for name := range names {
		wanted = append(wanted, name)
	}
	sort.Strings(wanted)
	rows, err := tx.Query(ctx,
		`SELECT id::text, name FROM project_secrets
		  WHERE project_id = $1 AND name = ANY($2::text[])
		  ORDER BY id FOR KEY SHARE`, projectID, wanted)
	if err != nil {
		return nil, "", fmt.Errorf("store: resolve plan secret refs: %w", err)
	}
	defer rows.Close()
	idByName := make(map[string]string, len(wanted))
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, "", fmt.Errorf("store: scan plan secret ref: %w", err)
		}
		idByName[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: iterate plan secret refs: %w", err)
	}
	out := make(map[string]map[string]string, len(refsByUID))
	for _, uid := range uids {
		bindings := make(map[string]string, len(refsByUID[uid]))
		settings := make([]string, 0, len(refsByUID[uid]))
		for setting := range refsByUID[uid] {
			settings = append(settings, setting)
		}
		sort.Strings(settings)
		for _, setting := range settings {
			name := refsByUID[uid][setting]
			id, ok := idByName[name]
			if !ok {
				return nil, uid, SecretRefNotFoundError{Setting: setting, Name: name}
			}
			bindings[setting] = id
		}
		out[uid] = bindings
	}
	return out, "", nil
}

// replaceMonitorSecretRefsTx makes the normalized ref table exactly match the
// already-prepared config. The referenced secret rows remain key-share locked until commit.
func replaceMonitorSecretRefsTx(ctx context.Context, tx pgx.Tx, monitorID, projectID string, bindings map[string]string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM monitor_secret_refs WHERE monitor_id = $1 AND project_id = $2`, monitorID, projectID); err != nil {
		return fmt.Errorf("store: clear monitor secret refs: %w", err)
	}
	keys := make([]string, 0, len(bindings))
	for setting := range bindings {
		keys = append(keys, setting)
	}
	sort.Strings(keys)
	for _, setting := range keys {
		if _, err := tx.Exec(ctx,
			`INSERT INTO monitor_secret_refs (monitor_id, project_id, setting_key, secret_id)
			 VALUES ($1,$2,$3,$4)`, monitorID, projectID, setting, bindings[setting]); err != nil {
			return fmt.Errorf("store: insert monitor secret ref %q: %w", setting, err)
		}
	}
	return nil
}

// monitorColumns includes a correlated aggregate for depends_on; the bare `id`
// inside it resolves to the outer monitors row under any table alias.
const monitorColumns = "id, project_id, name, type, target, interval_seconds, timeout_seconds, retries, conditions, enabled, status, created_at, updated_at, push_token_enc, method, grace_seconds, config, auto_incident, failure_threshold, renotify_seconds, tags, region, escalation_policy_id, confirm_interval_seconds, consecutive_failures, execution_revision, state_sequence, last_probe_error_reason, last_probe_error_at, last_probe_error_job_id, " +
	"(SELECT COALESCE(array_agg(d.depends_on_id::text ORDER BY d.created_at), '{}') FROM monitor_dependencies d WHERE d.monitor_id = id) AS depends_on, slug"

// methodOrGet keeps the NOT NULL method column concrete; the prober ignores it
// for non-HTTP monitors.
func methodOrGet(m domain.Monitor) string {
	if m.Method == "" {
		return "GET"
	}
	return m.Method
}

// nullableID maps an empty id to SQL NULL (empty string is not a valid uuid) and a
// set id to itself, for nullable-fk columns like monitors.escalation_policy_id.
func nullableID(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

// scanMonitor scans monitorColumns into a Monitor. extra dests are appended to the Scan,
// letting a caller pull additional trailing columns (e.g. statement_timestamp()) selected
// alongside monitorColumns in the same query.
func (s *Store) scanMonitor(row pgx.Row, extra ...any) (domain.Monitor, error) {
	return s.scanMonitorMode(row, true, extra...)
}

// scanMonitorNoSecrets is the display/snapshot read boundary (§4.4.2). Secret config
// keys are omitted from the decoded schema without ever invoking Decrypt; references
// remain visible metadata. This is intentionally a separate scanner, not post-decrypt
// redaction, so APIs and scheduler snapshots cannot transiently hold credential plaintext.
func (s *Store) scanMonitorNoSecrets(row pgx.Row, extra ...any) (domain.Monitor, error) {
	return s.scanMonitorMode(row, false, extra...)
}

func (s *Store) scanMonitorMode(row pgx.Row, decryptConfigSecrets bool, extra ...any) (domain.Monitor, error) {
	var (
		m                       domain.Monitor
		typ                     string
		stat                    string
		pushToken               *string
		escPolicy               *string
		probeReason, probeJobID *string
	)
	var config []byte
	dests := []any{&m.ID, &m.ProjectID, &m.Name, &typ, &m.Target,
		&m.IntervalSeconds, &m.TimeoutSeconds, &m.Retries, &m.Conditions,
		&m.Enabled, &stat, &m.CreatedAt, &m.UpdatedAt, &pushToken, &m.Method, &m.GraceSeconds, &config, &m.AutoIncident, &m.FailureThreshold, &m.RenotifySeconds, &m.Tags, &m.Region, &escPolicy, &m.ConfirmIntervalSeconds, &m.ConsecutiveFailures, &m.ExecutionRevision, &m.StateSequence, &probeReason, &m.LastProbeErrorAt, &probeJobID, &m.DependsOn, &m.Slug}
	dests = append(dests, extra...)
	err := row.Scan(dests...)
	if err != nil {
		return domain.Monitor{}, err
	}
	m.Type = domain.MonitorType(typ)
	m.Status = domain.MonitorStatus(stat)
	if pushToken != nil {
		// push_token_enc holds the secret at rest (cipher-prefixed ciphertext when a key
		// is configured, otherwise plaintext). Decrypt is nil- and plaintext-tolerant.
		plain, derr := s.cipher.Decrypt(*pushToken)
		if derr != nil {
			return domain.Monitor{}, fmt.Errorf("store: decrypt push token: %w", derr)
		}
		m.PushToken = plain
	}
	if escPolicy != nil {
		m.EscalationPolicyID = *escPolicy
	}
	if probeReason != nil {
		m.LastProbeErrorReason = *probeReason
	}
	if probeJobID != nil {
		m.LastProbeErrorJobID = *probeJobID
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &m.Config); err != nil {
			return domain.Monitor{}, fmt.Errorf("store: decode monitor config: %w", err)
		}
		for k := range domain.SecretMonitorConfigKeys {
			if v, ok := m.Config[k]; ok {
				if !decryptConfigSecrets {
					delete(m.Config, k)
					continue
				}
				plain, err := s.cipher.Decrypt(v)
				if err != nil {
					return domain.Monitor{}, fmt.Errorf("store: decrypt monitor config %q: %w", k, err)
				}
				m.Config[k] = plain
			}
		}
	}
	return m, nil
}

// prepareCredentialUpdate validates a full safe-read config while allowing an omitted
// write-only inline password to mean "preserve the current ciphertext". The placeholder
// exists only to traverse the single domain validator; it is removed before persistence.
// A password_ref is never implicit and therefore never uses this path.
func prepareCredentialUpdate(typ domain.MonitorType, input map[string]string) (map[string]string, bool, error) {
	// One rule, owned by the domain. This used to validate through SurfaceAPI with a
	// placeholder password stuffed in and stripped out again — a workaround for a rule the
	// domain did not express, which meant the API and the store could (and did) disagree
	// about whether an omitted slot was legal.
	preserve := domain.CredentialUpdateOmitsSlot(typ, input)
	prepared, err := domain.PrepareCredentialSettings(typ, input, domain.SurfaceAPIUpdate)
	if err != nil {
		return nil, false, err
	}
	return prepared, preserve, nil
}

// storedCredentialTx reports which KIND of credential the stored row carries: a reference
// name, or an inline write-only value. A safe reader sees neither, so a partial update has
// to ask the row rather than the request.
func (s *Store) storedCredentialTx(ctx context.Context, tx pgx.Tx, m domain.Monitor) (ref string, inline bool, err error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT config FROM monitors WHERE id=$1 AND project_id=$2`, m.ID, m.ProjectID).Scan(&raw); err != nil {
		if noRows(err) {
			return "", false, ErrNotFound
		}
		return "", false, fmt.Errorf("store: read stored monitor credential: %w", err)
	}
	stored := map[string]string{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", false, fmt.Errorf("store: decode stored monitor config: %w", err)
	}
	return stored["password_ref"], stored["password"] != "", nil
}

// marshalConfigForUpdateTx preserves the exact old ciphertext for a write-only inline
// password omitted by a safe reader. It never decrypts that value. An explicit password
// or password_ref replaces the old credential normally.
func (s *Store) marshalConfigForUpdateTx(ctx context.Context, tx pgx.Tx, m domain.Monitor, preserveInline bool) ([]byte, error) {
	config, err := s.marshalConfig(m)
	if err != nil || !preserveInline {
		return config, err
	}
	var oldRaw []byte
	if err := tx.QueryRow(ctx, `SELECT config FROM monitors WHERE id=$1 AND project_id=$2`, m.ID, m.ProjectID).Scan(&oldRaw); err != nil {
		return nil, fmt.Errorf("store: read write-only monitor config: %w", err)
	}
	oldConfig := map[string]string{}
	if err := json.Unmarshal(oldRaw, &oldConfig); err != nil {
		return nil, fmt.Errorf("store: decode write-only monitor config: %w", err)
	}
	oldPassword, ok := oldConfig["password"]
	if !ok || oldPassword == "" {
		return nil, fmt.Errorf("store: credential settings require an existing write-only password to preserve")
	}
	encoded := map[string]string{}
	if err := json.Unmarshal(config, &encoded); err != nil {
		return nil, fmt.Errorf("store: decode prepared monitor config: %w", err)
	}
	encoded["password"] = oldPassword
	return json.Marshal(encoded)
}

// marshalConfig encrypts secret config values (when a cipher is set) and encodes
// the config map to JSON for storage ('{}' when empty).
func (s *Store) marshalConfig(m domain.Monitor) ([]byte, error) {
	if len(m.Config) == 0 {
		return []byte("{}"), nil
	}
	enc := make(map[string]string, len(m.Config))
	for k, v := range m.Config {
		if domain.SecretMonitorConfigKeys[k] {
			ev, err := s.cipher.Encrypt(v)
			if err != nil {
				return nil, fmt.Errorf("store: encrypt monitor config %q: %w", k, err)
			}
			enc[k] = ev
		} else {
			enc[k] = v
		}
	}
	b, err := json.Marshal(enc)
	if err != nil {
		return nil, fmt.Errorf("store: encode monitor config: %w", err)
	}
	return b, nil
}

// MonitorStatuses returns the current status of each given monitor id (missing
// ids are simply absent from the map) — used by composite evaluation.
func (s *Store) MonitorStatuses(ctx context.Context, ids []string) (map[string]domain.MonitorStatus, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, status FROM monitors WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: monitor statuses: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.MonitorStatus, len(ids))
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, fmt.Errorf("store: scan monitor status: %w", err)
		}
		out[id] = domain.MonitorStatus(st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate monitor statuses: %w", err)
	}
	return out, nil
}

// CreateMonitor inserts a monitor. The caller validates via domain first.
func (s *Store) CreateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) {
	if domain.CredentialedType(m.Type) {
		prepared, err := domain.PrepareCredentialSettings(m.Type, m.Config, domain.SurfaceAPI)
		if err != nil {
			return domain.Monitor{}, err
		}
		m.Config = prepared
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: begin create monitor: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	bindings, err := s.monitorSecretBindings(ctx, tx, m.ProjectID, m)
	if err != nil {
		return domain.Monitor{}, err
	}
	created, err := insertMonitorTx(ctx, tx, s, m)
	if err != nil {
		return domain.Monitor{}, err
	}
	if err := replaceMonitorSecretRefsTx(ctx, tx, created.ID, m.ProjectID, bindings); err != nil {
		return domain.Monitor{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Monitor{}, fmt.Errorf("store: commit create monitor: %w", err)
	}
	return created, nil
}

// GetMonitorByPushToken returns the push monitor with the given token, or ErrNotFound.
// Lookup is by the blind index (hash of the presented token), so the plaintext never
// needs to be stored or compared. It also returns statement_timestamp() from the SAME
// query — the trusted ingress received_at the push handler passes to RecordPushResult, so
// ordering never degrades into processed_at under later queue/handler delay (spec §4).
func (s *Store) GetMonitorByPushToken(ctx context.Context, token string) (domain.Monitor, time.Time, error) {
	var receivedAt time.Time
	row := s.pool.QueryRow(ctx,
		`SELECT `+monitorColumns+`, statement_timestamp() FROM monitors WHERE push_token_hash = $1`,
		HashToken(token))
	m, err := s.scanMonitor(row, &receivedAt)
	if noRows(err) {
		return domain.Monitor{}, time.Time{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, time.Time{}, fmt.Errorf("store: get monitor by push token: %w", err)
	}
	return m, receivedAt, nil
}

// RecordPushResult applies a push (dead-man's-switch) heartbeat via its own trusted
// entrypoint (spec func-result-protocol §4): revision-exempt, ordered by the server
// receivedAt captured at ingress. In one transaction it locks the monitor, RE-CHECKS
// type='push' AND enabled (a ping accepted just before a disable must not mutate a
// now-disabled monitor), inserts the heartbeat (ts = receivedAt, observed_at = client ts
// or NULL), advances last_result_ts = receivedAt (so the dead-man CAS sees fresh pings),
// and applies the status change. A bad client clock never rejects a push — the heartbeat
// IS the liveness signal. observedAt is the optional raw client timestamp (zero = absent).
func (s *Store) RecordPushResult(ctx context.Context, monitorID string, up bool, msg string, receivedAt time.Time, observedAt time.Time) (ResultOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: begin push result: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var typ string
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT type, enabled FROM monitors WHERE id = $1 FOR UPDATE`, monitorID).Scan(&typ, &enabled)
	if noRows(err) {
		return ResultOutcome{}, ErrNotFound
	}
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: push result lock: %w", err)
	}
	// Current-state re-check: dropped if it is no longer an enabled push monitor.
	if domain.MonitorType(typ) != domain.MonitorPush || !enabled {
		return s.commitOutcome(ctx, tx, ResultOutcome{})
	}

	var obs *time.Time
	if !observedAt.IsZero() {
		obs = &observedAt
	}
	pushInsert, err := tx.Exec(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, msg, observed_at)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (monitor_id, ts) DO NOTHING`,
		monitorID, receivedAt, up, msg, obs)
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: push result insert: %w", err)
	}
	// Service reliability marks the bucket only for a row that was ACTUALLY inserted. Two
	// pings at the same instant are one observation, and treating the second as arriving
	// data would let a duplicate be filed as evidence the seal excluded.
	if pushInsert.RowsAffected() > 0 {
		if err := s.noteHeartbeatForServices(ctx, tx, monitorID, receivedAt); err != nil {
			return ResultOutcome{}, err
		}
	}
	prev, cur, suppressed, err := recordCheckStatusTx(ctx, tx, monitorID, up)
	if err != nil {
		return ResultOutcome{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE monitors
		    SET last_result_ts = $2,
		        last_probe_error_reason = CASE WHEN last_probe_error_at <= $2 THEN NULL ELSE last_probe_error_reason END,
		        last_probe_error_at = CASE WHEN last_probe_error_at <= $2 THEN NULL ELSE last_probe_error_at END,
		        last_probe_error_job_id = CASE WHEN last_probe_error_at <= $2 THEN NULL ELSE last_probe_error_job_id END
		  WHERE id = $1`, monitorID, receivedAt); err != nil {
		return ResultOutcome{}, fmt.Errorf("store: advance last_result_ts: %w", err)
	}
	return s.commitOutcome(ctx, tx, ResultOutcome{
		Applied: true, Inserted: true, Prev: prev, Cur: cur, Suppressed: suppressed,
	})
}

// UpdateMonitor is the USER-managed (API/UI) config-write path. It rejects a monitor owned
// by a file provider with ErrManagedByFile — the ownership check runs inside the same
// transaction as the write (under the monitor's row lock), so a concurrent file apply that
// claims ownership cannot slip past a stale handler-level check (spec §8/§9.2). Type and
// push_token are immutable. ErrNotFound if the monitor is gone. Caller validates via domain.
func (s *Store) UpdateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error) {
	preserveInline := false
	if domain.CredentialedType(m.Type) {
		prepared, preserve, err := prepareCredentialUpdate(m.Type, m.Config)
		if err != nil {
			return domain.Monitor{}, err
		}
		m.Config = prepared
		preserveInline = preserve
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: begin update monitor: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	// A partial update that omitted the credential slot keeps whatever the row already has,
	// and that may be a REFERENCE rather than an inline value. The reference has to be
	// restored HERE — before the bindings are resolved — or the stored config would keep a
	// ref the normalized `monitor_secret_refs` table no longer has, which is the divergence
	// §4.3 exists to prevent. Reading the old config takes no row lock, so the fixed order
	// (secret rows, then the monitor row) is unaffected.
	if preserveInline {
		storedRef, storedInline, rerr := s.storedCredentialTx(ctx, tx, m)
		if rerr != nil {
			return domain.Monitor{}, rerr
		}
		switch {
		case storedRef != "":
			m.Config["password_ref"] = storedRef
			preserveInline = false // the credential is a reference; nothing inline to carry
		case storedInline:
			// keep preserveInline: marshalConfigForUpdateTx carries the ciphertext forward
		default:
			return domain.Monitor{}, fmt.Errorf("store: credential settings require an existing credential to preserve")
		}
	}
	// Fixed §4.3 order: referenced secret rows first, monitor row second.
	bindings, err := s.monitorSecretBindings(ctx, tx, m.ProjectID, m)
	if err != nil {
		return domain.Monitor{}, err
	}
	var exists int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM monitors WHERE id = $1 AND project_id = $2 FOR UPDATE`, m.ID, m.ProjectID).Scan(&exists); noRows(err) {
		return domain.Monitor{}, ErrNotFound
	} else if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: lock monitor: %w", err)
	}
	if err := assertNotFileManagedTx(ctx, tx, m.ID); err != nil {
		return domain.Monitor{}, err
	}
	// The slug is IMMUTABLE. A caller that submits a different one gets told rather than
	// silently ignored: it is the key a service declaration names this monitor by, and
	// letting it move would turn a rename into a guarded declaration mutation across every
	// referencing service.
	if m.Slug != "" {
		var stored string
		if err := tx.QueryRow(ctx, `SELECT slug FROM monitors WHERE id = $1`, m.ID).Scan(&stored); err != nil {
			return domain.Monitor{}, fmt.Errorf("store: read monitor slug: %w", err)
		}
		if m.Slug != stored {
			return domain.Monitor{}, fmt.Errorf("%w: %q is stored, %q was submitted", ErrMonitorSlugImmutable, stored, m.Slug)
		}
	}
	updated, err := updateMonitorTxPrepared(ctx, tx, s, m, preserveInline)
	if err != nil {
		return domain.Monitor{}, err
	}
	if err := replaceMonitorSecretRefsTx(ctx, tx, m.ID, m.ProjectID, bindings); err != nil {
		return domain.Monitor{}, err
	}
	// The epoch bump lives inside updateMonitorTxPrepared, so every caller of the shared
	// write contract gets it — not just this one.
	if err := tx.Commit(ctx); err != nil {
		return domain.Monitor{}, fmt.Errorf("store: commit update monitor: %w", err)
	}
	return updated, nil
}

// updateMonitorTx is the shared config-write contract (D-0142): it bumps execution_revision,
// applies the freshness-watermark / push re-arm rules, and returns the updated row. Both the
// user path (UpdateMonitor, after its ownership guard) and the file-apply path (which owns
// the row and must NOT be blocked by the guard) call it inside their own transaction.
func updateMonitorTx(ctx context.Context, tx pgx.Tx, s *Store, m domain.Monitor) (domain.Monitor, error) {
	preserveInline := false
	if domain.CredentialedType(m.Type) {
		prepared, preserve, err := prepareCredentialUpdate(m.Type, m.Config)
		if err != nil {
			return domain.Monitor{}, err
		}
		m.Config = prepared
		preserveInline = preserve
	}
	return updateMonitorTxPrepared(ctx, tx, s, m, preserveInline)
}

func updateMonitorTxPrepared(ctx context.Context, tx pgx.Tx, s *Store, m domain.Monitor, preserveInline bool) (domain.Monitor, error) {
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
	config, err := s.marshalConfigForUpdateTx(ctx, tx, m, preserveInline)
	if err != nil {
		return domain.Monitor{}, err
	}
	row := tx.QueryRow(ctx,
		`UPDATE monitors
		    SET name = $2, target = $3, interval_seconds = $4, timeout_seconds = $5,
		        retries = $6, conditions = $7, enabled = $8, method = $9, grace_seconds = $10, config = $11, auto_incident = $12, failure_threshold = $13, renotify_seconds = $14, tags = $15, region = $16, escalation_policy_id = $17, confirm_interval_seconds = $18, updated_at = now(),
		        -- Config generation + freshness watermark, shared verbatim with the rotation
		        -- fence (see revisionFenceSetSQL): bump on ANY UpdateMonitor, because a missed
		        -- bump reopens the stale-config vulnerability while an extra one costs a single
		        -- re-probe. Status/counter/reencrypt writes use different statements and do not
		        -- touch either column.
		        `+revisionFenceSetSQL+`,
		        -- Re-arm: a disabled→enabled transition (RHS sees the pre-update row) starts a new
		        -- liveness epoch. For push the dead-man window restarts from the enable moment via
		        -- push_armed_at; the pre-disable ping is not proof of liveness after re-enable, and
		        -- we never fabricate a last_result_ts. All types reset live state to pending + clear
		        -- the confirmation counter and bump state_sequence WITHOUT enqueuing an outbox event,
		        -- so an undelivered pre-disable DOWN is dropped as superseded at delivery (#2).
		        push_armed_at = CASE WHEN type = 'push' AND NOT enabled AND $8 THEN statement_timestamp() ELSE push_armed_at END,
		        status = CASE WHEN NOT enabled AND $8 THEN 'pending' ELSE status END,
		        consecutive_failures = CASE WHEN NOT enabled AND $8 THEN 0 ELSE consecutive_failures END,
		        state_sequence = CASE WHEN NOT enabled AND $8 THEN state_sequence + 1 ELSE state_sequence END
		  WHERE id = $1 RETURNING `+monitorColumns,
		m.ID, m.Name, m.Target, m.IntervalSeconds, m.TimeoutSeconds, m.Retries, conditions, m.Enabled, methodOrGet(m), m.GraceSeconds, config, m.AutoIncident, m.FailureThreshold, m.RenotifySeconds, tags, region, nullableID(m.EscalationPolicyID), m.ConfirmIntervalSeconds)
	updated, err := s.scanMonitorNoSecrets(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: update monitor: %w", err)
	}
	// Evaluation epochs for every service whose SLI declares this monitor, in THIS
	// transaction. The epoch and the write it describes have to become visible together:
	// snapshotting later would leave an interval in which concurrent ingest is attributed to
	// an epoch that does not yet describe the monitor it measured. Services that read the
	// same inputs as before get nothing — the snapshot-hash no-op rule lives here, and only
	// here (FR-021 §6.2).
	//
	// It belongs on the SHARED contract, not on the API handler above it. Sitting one level
	// up, it covered UpdateMonitor alone, so every file-provider apply — target, condition,
	// region and enabled changes included — advanced execution semantics with no epoch at
	// all, and the declared linearization point held for exactly one of its callers.
	if err := s.BumpEpochsForMonitor(ctx, tx, updated.ProjectID, updated.ID); err != nil {
		return domain.Monitor{}, err
	}
	return updated, nil
}

// GetMonitor returns a monitor by id.
func (s *Store) GetMonitor(ctx context.Context, id string) (domain.Monitor, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE id = $1`, id)
	m, err := s.scanMonitorNoSecrets(row)
	if noRows(err) {
		return domain.Monitor{}, ErrNotFound
	}
	if err != nil {
		return domain.Monitor{}, fmt.Errorf("store: get monitor: %w", err)
	}
	return m, nil
}

// ListMonitorsByProject returns all monitors in a project.
func (s *Store) ListMonitorsByProject(ctx context.Context, projectID string) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list monitors by project: %w", err)
	}
	return s.collectMonitorsNoSecrets(rows)
}

// MonitorRegions returns the region of each given monitor id (missing/deleted ids are
// absent). Used to verify a pull agent only posts results for its own region.
func (s *Store) MonitorRegions(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, region FROM monitors WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: monitor regions: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, region string
		if err := rows.Scan(&id, &region); err != nil {
			return nil, fmt.Errorf("store: scan monitor region: %w", err)
		}
		out[id] = region
	}
	return out, rows.Err()
}

// ListEnabledMonitors returns all enabled monitors (for the scheduler).
func (s *Store) ListEnabledMonitors(ctx context.Context) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled monitors: %w", err)
	}
	return s.collectMonitors(rows)
}

// ListEnabledMonitorSnapshots is the ciphertext/plaintext-free scheduler surface used
// when credential envelopes are enforced. Legacy mode retains ListEnabledMonitors until
// the rollout flips, so existing inline monitors keep their current transport semantics.
func (s *Store) ListEnabledMonitorSnapshots(ctx context.Context) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+monitorColumns+` FROM monitors WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled monitor snapshots: %w", err)
	}
	return s.collectMonitorsNoSecrets(rows)
}

// StalePushMonitors returns enabled push monitors whose dead-man's switch has
// tripped: no liveness within their interval+grace. One query with an
// index-backed watermark replaces a per-monitor latest-heartbeat lookup.
func (s *Store) StalePushMonitors(ctx context.Context) ([]domain.Monitor, error) {
	// No `status <> 'down'` filter: a down push monitor STAYS in the stale set so the
	// dead-man re-samples DOWN each idle tick (sample-based SLA reflects a sustained
	// outage). The scheduler throttles re-fires via nextRun; RecordDeadmanResult only
	// transitions on the first (its recordCheckStatusTx prev!=cur guard). Freshness is
	// max(push_armed_at, last_result_ts) — REAL observations plus the re-arm epoch (a
	// dead-man DOWN heartbeat advances neither) — with created_at as the floor for a
	// never-armed, never-reported monitor. Identical to the dead-man CAS cutoff, so the
	// monitor's own synthetic samples don't make it look fresh, and a real ping (or a
	// re-enable stamping push_armed_at) removes it.
	rows, err := s.pool.Query(ctx, `
		SELECT `+monitorColumns+` FROM monitors m
		 WHERE m.type = 'push' AND m.enabled
		   AND COALESCE(GREATEST(m.push_armed_at, m.last_result_ts), m.created_at)
		       < now() - make_interval(secs => m.interval_seconds + m.grace_seconds)`)
	if err != nil {
		return nil, fmt.Errorf("store: stale push monitors: %w", err)
	}
	return s.collectMonitors(rows)
}

// SetMonitorStatus updates a monitor's last-known status and returns the previous
// status, so callers can detect up/down transitions. ErrNotFound if the monitor
// is gone. Test support: production writes status through RecordCheckStatus
// (ingest); integration tests use this to arrange transition scenarios.
func (s *Store) SetMonitorStatus(ctx context.Context, id string, status domain.MonitorStatus) (domain.MonitorStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin set monitor status: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var prev domain.MonitorStatus
	err = tx.QueryRow(ctx,
		`WITH prev AS (SELECT id, status AS old FROM monitors WHERE id = $1)
		 UPDATE monitors m SET status = $2, updated_at = now()
		 FROM prev WHERE m.id = prev.id
		 RETURNING prev.old`,
		id, string(status)).Scan(&prev)
	if noRows(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: set monitor status: %w", err)
	}
	// On a real transition, enqueue the raw fact in the same transaction; the
	// outbox worker applies the notification policy. No dual-write.
	if prev != status {
		seq, err := bumpStateSequenceTx(ctx, tx, id)
		if err != nil {
			return "", err
		}
		payload, err := json.Marshal(domain.MonitorTransition{MonitorID: id, Prev: prev, Cur: status, Seq: seq})
		if err != nil {
			return "", fmt.Errorf("store: marshal transition: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicMonitorTransition, payload); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit set monitor status: %w", err)
	}
	return prev, nil
}

// RecordCheckStatus applies one check result to a monitor's status atomically,
// with alert confirmations and maintenance suppression:
//   - a down flip happens only after failure_threshold consecutive failures
//     (the live consecutive_failures counter); recovery (up) is immediate and
//     resets the counter;
//   - when the flip occurs inside an active maintenance window it is suppressed:
//     the status still changes (accuracy + SLA) but no transition notification is
//     enqueued and suppressed=true tells the caller not to open an incident.
//
// It returns the previous and new status plus whether the change was suppressed.
func (s *Store) RecordCheckStatus(ctx context.Context, monitorID string, up bool) (prev, cur domain.MonitorStatus, suppressed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("store: begin record status: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	prev, cur, suppressed, err = recordCheckStatusTx(ctx, tx, monitorID, up)
	if err != nil {
		return "", "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", false, fmt.Errorf("store: commit record status: %w", err)
	}
	return prev, cur, suppressed, nil
}

// recordCheckStatusTx applies one check result to a monitor's live status inside an
// existing transaction (see RecordCheckStatus for the confirmation/suppression
// semantics). It does NOT commit — the caller owns the transaction, which lets the
// heartbeat insert, the status flip and the transition-outbox event all land in one
// atomic unit (RecordResult).
func recordCheckStatusTx(ctx context.Context, tx pgx.Tx, monitorID string, up bool) (prev, cur domain.MonitorStatus, suppressed bool, err error) {
	var prevS, curS string
	var inMaint bool
	var fails, threshold, confirmSec int
	err = tx.QueryRow(ctx,
		`WITH cur AS (
		   SELECT id, project_id, status AS old_status, consecutive_failures, failure_threshold
		   FROM monitors WHERE id = $1 FOR UPDATE
		 ),
		 maint AS (
		   SELECT EXISTS(
		     SELECT 1 FROM maintenance_windows mw, cur
		     WHERE mw.starts_at <= now() AND mw.ends_at >= now()
		       AND (mw.monitor_id = cur.id OR (mw.monitor_id IS NULL AND mw.project_id = cur.project_id))
		   ) AS in_maint
		 )
		 UPDATE monitors m SET
		   consecutive_failures = CASE WHEN $2 THEN 0 ELSE cur.consecutive_failures + 1 END,
		   status = CASE
		     WHEN $2 THEN 'up'
		     WHEN cur.consecutive_failures + 1 >= cur.failure_threshold THEN 'down'
		     ELSE m.status
		   END,
		   -- last_notified_at drives re-notify: stamped on a fresh down (including a
		   -- down that starts during maintenance, so the renotify job re-enqueues it
		   -- and it delivers once the window ends — the initial notify is muted at
		   -- DELIVERY, not dropped here), cleared on recovery, else left for the
		   -- reminder job to bump.
		   last_notified_at = CASE
		     WHEN $2 THEN NULL
		     WHEN cur.consecutive_failures + 1 >= cur.failure_threshold AND cur.old_status <> 'down' THEN now()
		     ELSE m.last_notified_at
		   END,
		   updated_at = now()
		 FROM cur, maint
		 WHERE m.id = cur.id
		 RETURNING cur.old_status, m.status, maint.in_maint, m.consecutive_failures, m.failure_threshold, m.confirm_interval_seconds`,
		monitorID, up).Scan(&prevS, &curS, &inMaint, &fails, &threshold, &confirmSec)
	if noRows(err) {
		return "", "", false, ErrNotFound
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: record check status: %w", err)
	}
	prev, cur = domain.MonitorStatus(prevS), domain.MonitorStatus(curS)
	// Confirmation phase entered/continuing (a failure counted, no verdict yet):
	// wake the scheduler leader so the next probe comes at the confirm interval
	// instead of the main one. Same-transaction NOTIFY — delivered on commit; a
	// missed signal is harmless (the snapshot refresh path catches up).
	if !up && cur != domain.StatusDown && fails > 0 && fails < threshold && confirmSec > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, ConfirmChannel, monitorID); err != nil {
			return "", "", false, fmt.Errorf("store: notify confirm phase: %w", err)
		}
	}
	// Enqueue the transition on every real flip, INCLUDING one during maintenance:
	// the fact and its event are always recorded, and the outbox worker mutes the
	// down-notify DELIVERY while the window is active. Suppressing here (as before)
	// meant a monitor that went down mid-window and stayed down was never alerted
	// even after the window closed. (Incident opening is still gated on !inMaint in
	// ingest via the returned suppressed flag — maintenance shouldn't spawn incidents.)
	if prev != cur {
		seq, err := bumpStateSequenceTx(ctx, tx, monitorID)
		if err != nil {
			return "", "", false, err
		}
		payload, err := json.Marshal(domain.MonitorTransition{MonitorID: monitorID, Prev: prev, Cur: cur, Seq: seq})
		if err != nil {
			return "", "", false, fmt.Errorf("store: marshal transition: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicMonitorTransition, payload); err != nil {
			return "", "", false, err
		}
	}
	return prev, cur, inMaint, nil
}

// bumpStateSequenceTx increments the monitor's monotonic transition counter and
// returns the new value, inside the caller's transaction. Callers invoke it only
// on a real status flip (the row is already row-locked in the same tx), so the
// counter advances exactly once per applied transition and the returned value is
// stamped into the transition outbox event for the delivery-time staleness check.
func bumpStateSequenceTx(ctx context.Context, tx pgx.Tx, monitorID string) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx,
		`UPDATE monitors SET state_sequence = state_sequence + 1 WHERE id = $1 RETURNING state_sequence`,
		monitorID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: bump state sequence: %w", err)
	}
	return seq, nil
}

// Result-ingest outcome reasons (spec func-result-protocol §4/§5). Low-cardinality; the
// caller maps them to the result_* metric families.
const (
	ReasonMissingTimestamp = "missing_timestamp"
	ReasonFutureTimestamp  = "future_timestamp"
	ReasonOutsideRetention = "outside_retention"
	ReasonOutOfOrder       = "out_of_order"
	ReasonDuplicate        = "duplicate"
	ReasonStaleRevision    = "stale_revision"
	ReasonMissingRevision  = "missing_revision"
)

// ResultOutcome is what a scheduled/push result did. Applied = live status/counters/outbox
// ran (a fresh result); Inserted = a heartbeat row was written (SLA). Reason is "" when
// Applied, else the outcome the caller reports as a metric.
type ResultOutcome struct {
	Applied    bool
	Inserted   bool
	Prev, Cur  domain.MonitorStatus
	Suppressed bool
	Reason     string
	// MissingRevisionObserved is set when a scheduled result carried no revision and was
	// accepted under observe mode (the migration counter watched before switching to
	// enforce). Orthogonal to Reason (the result is still applied/inserted normally).
	MissingRevisionObserved bool
}

// ProbeErrorOutcome reports whether an executor diagnostic was recorded. It is separate
// from ResultOutcome so callers cannot accidentally interpret diagnostics as live checks.
type ProbeErrorOutcome struct {
	Recorded bool
	Reason   string
}

// RecordProbeError stores a revision-fenced executor diagnostic without touching the
// heartbeat/SLA/status/counter/incident/outbox paths. The UPDATE is the CAS/linearization
// point; a stale revision is rejected exactly like a stale normal result.
func (s *Store) RecordProbeError(ctx context.Context, monitorID string, revision int64, probeErr domain.ProbeError) (ProbeErrorOutcome, error) {
	if monitorID == "" || revision < 1 || !domain.ValidProbeErrorReason(probeErr.Reason) {
		return ProbeErrorOutcome{}, errors.New("store: invalid probe_error result")
	}
	ct, err := s.pool.Exec(ctx,
		`UPDATE monitors
		    SET last_probe_error_reason=$3,
		        last_probe_error_at=statement_timestamp(),
		        last_probe_error_job_id=NULLIF($4,'')
		  WHERE id=$1 AND execution_revision=$2 AND enabled`,
		monitorID, revision, probeErr.Reason, probeErr.JobID)
	if err != nil {
		return ProbeErrorOutcome{}, fmt.Errorf("store: record probe error: %w", err)
	}
	if ct.RowsAffected() == 1 {
		return ProbeErrorOutcome{Recorded: true}, nil
	}
	var currentRevision int64
	if err := s.pool.QueryRow(ctx, `SELECT execution_revision FROM monitors WHERE id=$1`, monitorID).Scan(&currentRevision); noRows(err) {
		return ProbeErrorOutcome{}, ErrNotFound
	} else if err != nil {
		return ProbeErrorOutcome{}, fmt.Errorf("store: classify rejected probe error: %w", err)
	}
	if currentRevision != revision {
		return ProbeErrorOutcome{Reason: ReasonStaleRevision}, nil
	}
	return ProbeErrorOutcome{Reason: MaterializeSkippedCurrentState}, nil
}

// withMissing tags the outcome as an observe-mode missing-revision acceptance.
func (o ResultOutcome) withMissing(b bool) ResultOutcome { o.MissingRevisionObserved = b; return o }

// RecordScheduledResult records a worker/agent (scheduled) probe result following the
// ordered pipeline in spec func-result-protocol §4, in ONE transaction:
//
//	missing ts → SELECT … FOR UPDATE (lock + DB clock + watermark) → revision gate
//	(enforce|observe) → timestamp bounds (BEFORE insert) → INSERT ON CONFLICT →
//	0 rows = duplicate → watermark compare (out-of-order = SLA-only | fresh = apply).
//
// This ordering is what simultaneously gives gate-before-insert, redelivery idempotency,
// and the guarantee that a future- or out-of-window row is never persisted. now is the DB
// statement_timestamp() (statement-scope, current even after the FOR UPDATE wait).
func (s *Store) RecordScheduledResult(ctx context.Context, hb domain.Heartbeat) (ResultOutcome, error) {
	// Step 1 — a scheduled result MUST carry a timestamp; a zero wire ts is fail-closed.
	if hb.Ts.IsZero() {
		return ResultOutcome{Reason: ReasonMissingTimestamp}, nil
	}
	ts := hb.Ts

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: begin record result: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Step 2 — lock the monitor (existence + serialise vs config writes), read the DB clock,
	// the freshness watermark and the current config generation in one statement.
	var lastTs *time.Time
	var dbNow time.Time
	var curRev int64
	var enabled bool
	err = tx.QueryRow(ctx,
		`SELECT last_result_ts, statement_timestamp(), execution_revision, enabled FROM monitors WHERE id = $1 FOR UPDATE`,
		hb.MonitorID).Scan(&lastTs, &dbNow, &curRev, &enabled)
	if noRows(err) {
		return ResultOutcome{}, ErrNotFound
	}
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: record result lock: %w", err)
	}
	// A disable committed before this authoritative ingest lock invalidates the
	// in-flight probe. It must not add an SLA row or mutate liveness.
	if !enabled {
		return s.commitOutcome(ctx, tx, ResultOutcome{Reason: MaterializeSkippedCurrentState})
	}

	// Step 3 — revision gate (BEFORE any insert): a result produced under a stale config
	// must not mutate the new one, and (in enforce) a result with no revision is a bug/old
	// binary. Present-but-mismatched → reject ALWAYS; missing → reject in enforce, tolerated
	// + counted in observe (rolling-upgrade window). A reject inserts nothing.
	missingObserved := false
	if hb.ExecutionRevision == 0 {
		if s.resultRevisionMode != "observe" {
			return s.commitOutcome(ctx, tx, ResultOutcome{Reason: ReasonMissingRevision})
		}
		missingObserved = true
	} else if hb.ExecutionRevision != curRev {
		return s.commitOutcome(ctx, tx, ResultOutcome{Reason: ReasonStaleRevision})
	}

	// Step 4 — timestamp bounds, BEFORE the insert so a bad row never lands.
	skew := s.resultSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	if ts.After(dbNow.Add(skew)) {
		return s.commitOutcome(ctx, tx, ResultOutcome{Reason: ReasonFutureTimestamp}.withMissing(missingObserved))
	}
	if s.resultRetention > 0 && ts.Before(dbNow.Add(-s.resultRetention)) {
		return s.commitOutcome(ctx, tx, ResultOutcome{Reason: ReasonOutsideRetention}.withMissing(missingObserved))
	}

	// Step 5 — insert; observed_at == ts for a scheduled result.
	ct, err := tx.Exec(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, latency_ms, code, msg, observed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$2) ON CONFLICT (monitor_id, ts) DO NOTHING`,
		hb.MonitorID, ts, hb.Up, hb.LatencyMS, hb.Code, hb.Msg)
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: record result insert: %w", err)
	}
	// Step 6 — duplicate re-delivery: the fact is already recorded, apply nothing.
	if ct.RowsAffected() == 0 {
		return s.commitOutcome(ctx, tx, ResultOutcome{Reason: ReasonDuplicate}.withMissing(missingObserved))
	}
	// Step 6a — service reliability: mark the bucket this heartbeat belongs to. Reached only
	// past the duplicate short-circuit above, so a redelivery never marks anything.
	if err := s.noteHeartbeatForServices(ctx, tx, hb.MonitorID, ts); err != nil {
		return ResultOutcome{}, err
	}

	// Step 7 — watermark compare.
	if lastTs != nil && !ts.After(*lastTs) {
		// Out-of-order: heartbeat kept for SLA, live state untouched.
		return s.commitOutcome(ctx, tx, ResultOutcome{Inserted: true, Reason: ReasonOutOfOrder}.withMissing(missingObserved))
	}
	prev, cur, suppressed, err := recordCheckStatusTx(ctx, tx, hb.MonitorID, hb.Up)
	if err != nil {
		return ResultOutcome{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE monitors
		    SET last_result_ts = $2,
		        last_probe_error_reason = CASE WHEN last_probe_error_at <= $3 THEN NULL ELSE last_probe_error_reason END,
		        last_probe_error_at = CASE WHEN last_probe_error_at <= $3 THEN NULL ELSE last_probe_error_at END,
		        last_probe_error_job_id = CASE WHEN last_probe_error_at <= $3 THEN NULL ELSE last_probe_error_job_id END
		  WHERE id = $1`, hb.MonitorID, ts, dbNow); err != nil {
		return ResultOutcome{}, fmt.Errorf("store: advance last_result_ts: %w", err)
	}
	return s.commitOutcome(ctx, tx, ResultOutcome{
		Applied: true, Inserted: true, Prev: prev, Cur: cur, Suppressed: suppressed,
	}.withMissing(missingObserved))
}

// RecordDeadmanResult applies a push-timeout DOWN from the scheduler leader directly (no
// synthetic heartbeat through the dispatcher — origin stays a trusted server-side call).
// In ONE transaction it re-checks staleness ATOMICALLY under the monitor's lock — still an
// enabled push monitor, same config generation, and no result since the scheduler's
// evaluation cutoff — so a real ping (or a config change / disable) that landed since the
// staleness snapshot causes the synthetic DOWN to be dropped, closing the dead-man race.
// On pass it inserts a DOWN heartbeat (SLA sample; sample-based SLA would otherwise stay
// 100% on stale UP samples) and applies the status transition through the SAME
// recordCheckStatusTx as a real result (confirmation/maintenance/transition-outbox
// preserved). It does NOT advance last_result_ts — only a real observation does — so the
// scheduler re-samples DOWN each idle tick (throttled by nextRun) until a real ping
// advances the watermark. cutoff is now - (interval + grace), computed by the scheduler.
func (s *Store) RecordDeadmanResult(ctx context.Context, monitorID string, revision int64, cutoff time.Time) (ResultOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: begin deadman: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var typ, status string
	var enabled bool
	var curRev int64
	var lastOrCreated time.Time
	err = tx.QueryRow(ctx,
		`SELECT type, enabled, status, execution_revision, COALESCE(GREATEST(push_armed_at, last_result_ts), created_at)
		   FROM monitors WHERE id = $1 FOR UPDATE`,
		monitorID).Scan(&typ, &enabled, &status, &curRev, &lastOrCreated)
	if noRows(err) {
		return ResultOutcome{}, ErrNotFound
	}
	if err != nil {
		return ResultOutcome{}, fmt.Errorf("store: deadman lock: %w", err)
	}
	// Atomic staleness re-check. Any failure drops the synthetic DOWN.
	if domain.MonitorType(typ) != domain.MonitorPush || !enabled || curRev != revision || !lastOrCreated.Before(cutoff) {
		return s.commitOutcome(ctx, tx, ResultOutcome{})
	}

	// DOWN heartbeat for SLA continuity (statement_timestamp() → distinct ts per tick). Does
	// NOT touch last_result_ts (that watermark tracks real observations, driving this CAS).
	var deadmanTs *time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO heartbeats (monitor_id, ts, up, msg, observed_at)
		 VALUES ($1, statement_timestamp(), false, $2, NULL) ON CONFLICT (monitor_id, ts) DO NOTHING
		 RETURNING ts`,
		monitorID, "push timeout: no heartbeat within interval").Scan(&deadmanTs)
	if err != nil && !noRows(err) {
		return ResultOutcome{}, fmt.Errorf("store: deadman insert: %w", err)
	}
	// noRows means ON CONFLICT swallowed it — a synthetic DOWN already exists for this
	// instant. Rare, but gated for the same reason as every other path: only a row that was
	// really inserted is arriving data.
	if deadmanTs != nil {
		if err := s.noteHeartbeatForServices(ctx, tx, monitorID, *deadmanTs); err != nil {
			return ResultOutcome{}, err
		}
	}
	prev, cur, suppressed, err := recordCheckStatusTx(ctx, tx, monitorID, false)
	if err != nil {
		return ResultOutcome{}, err
	}
	return s.commitOutcome(ctx, tx, ResultOutcome{
		Applied: true, Inserted: true, Prev: prev, Cur: cur, Suppressed: suppressed,
	})
}

// commitOutcome commits tx and returns the outcome (or a wrapped commit error).
func (s *Store) commitOutcome(ctx context.Context, tx pgx.Tx, o ResultOutcome) (ResultOutcome, error) {
	if err := tx.Commit(ctx); err != nil {
		return ResultOutcome{}, fmt.Errorf("store: commit result: %w", err)
	}
	return o, nil
}

// EnqueueRenotifyReminders re-sends the down alert for monitors that have stayed
// down longer than their renotify_seconds since the last notification. It claims
// the due monitors, enqueues one reminder transition each, and bumps
// last_notified_at so the next reminder waits another interval — all in one
// transaction. Returns the number of reminders enqueued. Leader-only.
func (s *Store) EnqueueRenotifyReminders(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin renotify: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx,
		`SELECT id, state_sequence FROM monitors
		  WHERE status = 'down' AND renotify_seconds > 0 AND last_notified_at IS NOT NULL
		    AND escalation_policy_id IS NULL
		    AND last_notified_at <= now() - make_interval(secs => renotify_seconds)
		  FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("store: select renotify due: %w", err)
	}
	type reminderDue struct {
		id  string
		seq int64
	}
	var due []reminderDue
	for rows.Next() {
		var d reminderDue
		if err := rows.Scan(&d.id, &d.seq); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan renotify id: %w", err)
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate renotify due: %w", err)
	}
	for _, d := range due {
		// Carry the current state_sequence (no bump — a reminder isn't a transition).
		// If the monitor recovers before this reminder is delivered, its sequence has
		// advanced past d.seq and the delivery worker drops the reminder as stale.
		payload, err := json.Marshal(domain.MonitorTransition{MonitorID: d.id, Prev: domain.StatusDown, Cur: domain.StatusDown, Reminder: true, Seq: d.seq})
		if err != nil {
			return 0, fmt.Errorf("store: marshal reminder: %w", err)
		}
		if err := enqueueOutboxTx(ctx, tx, domain.TopicMonitorTransition, payload); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE monitors SET last_notified_at = now() WHERE id = $1`, d.id); err != nil {
			return 0, fmt.Errorf("store: bump last_notified_at: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit renotify: %w", err)
	}
	return len(due), nil
}

// DeleteMonitor removes a monitor (and its heartbeats via cascade).
// DeleteMonitor is a USER-managed (API/UI) op: it rejects a file-owned monitor with
// ErrManagedByFile (checked under the row lock, atomic against a concurrent file apply —
// spec §8). The file provider never physically deletes; it orphans/disables instead (§10).
func (s *Store) DeleteMonitor(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete monitor: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The monitor's project has to be known BEFORE the membership lock, because the lock is
	// per project — and the lock has to come before the monitor row, per §15.4.
	var projectID string
	if err := tx.QueryRow(ctx, `SELECT project_id::text FROM monitors WHERE id = $1`, id).Scan(&projectID); err != nil {
		if noRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: read monitor project: %w", err)
	}
	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return err
	}

	var exists int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM monitors WHERE id = $1 FOR UPDATE`, id).Scan(&exists); noRows(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("store: lock monitor: %w", err)
	}
	if err := assertNotFileManagedTx(ctx, tx, id); err != nil {
		return err
	}

	// Deleting a monitor that a service still declares has to REMOVE ITS REACH, in this
	// transaction, as a declaration in its own right (§15.1).
	//
	// The deferred FK on service_member_refs otherwise fired at COMMIT and rejected the
	// delete outright — including the ordinary UI-monitor-in-a-UI-service case, which is not
	// a conflict at all, just an operator removing a check. Nor may the reference simply be
	// dropped: what a service measures is a DECLARED thing, and changing it silently would
	// move the meaning of that service's availability with nobody having said so.
	if err := s.retireMonitorFromServicesTx(ctx, tx, projectID, id); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM monitors WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete monitor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete monitor: %w", err)
	}
	return nil
}

// retireMonitorFromServicesTx writes a SYSTEM definition revision for every service that
// declares the monitor, with that monitor removed from both lists.
//
// It is a revision rather than a quiet DELETE of the refs because the two lists are the
// declaration: a service whose SLI shrank has had the meaning of its availability changed,
// and that belongs on the record with an author, an effective boundary and an epoch — the
// same treatment a human making the same change would get. The author says `system:` so a
// reader can tell it was a consequence rather than an intent.
func (s *Store) retireMonitorFromServicesTx(ctx context.Context, tx pgx.Tx, projectID, monitorID string) error {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT service_id::text FROM service_member_refs
		  WHERE project_id = $1 AND monitor_id = $2 ORDER BY 1`,
		projectID, monitorID)
	if err != nil {
		return fmt.Errorf("store: find services declaring monitor: %w", err)
	}
	serviceIDs, err := collectIDs(rows)
	if err != nil {
		return err
	}

	for _, serviceID := range serviceIDs {
		monitors, sli, policies, revision, err := currentDeclarationTx(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if _, _, err := s.putServiceDeclarationTx(ctx, tx, projectID, serviceID,
			without(monitors, monitorID), without(sli, monitorID), policies, revision,
			DeclarationOptions{CreatedBy: "system:monitor-deleted"}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
			 SELECT p.org_id, NULL, false, 'service.member_retired', $2
			   FROM projects p WHERE p.id = $1`,
			projectID, fmt.Sprintf("service=%s monitor=%s", serviceID, monitorID)); err != nil {
			return fmt.Errorf("store: audit member retirement: %w", err)
		}
	}
	return nil
}

func without(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// currentDeclarationTx reads the revision in force, so a system rewrite changes exactly one
// thing and carries everything else forward untouched.
func currentDeclarationTx(ctx context.Context, tx pgx.Tx, serviceID string) (
	monitors, sli []string, policies domain.ServicePolicies, revision int64, err error,
) {
	var revisionID string
	var policyJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT id, revision, policies FROM service_definition_revisions
		  WHERE service_id = $1 AND state = 'effective'
		  ORDER BY effective_at DESC, revision DESC LIMIT 1`, serviceID).
		Scan(&revisionID, &revision, &policyJSON)
	if noRows(err) {
		return nil, nil, domain.ServicePolicies{}, 0, nil
	}
	if err != nil {
		return nil, nil, domain.ServicePolicies{}, 0, fmt.Errorf("store: read current declaration: %w", err)
	}
	if err := json.Unmarshal(policyJSON, &policies); err != nil {
		return nil, nil, domain.ServicePolicies{}, 0, fmt.Errorf("store: decode policies: %w", err)
	}
	monitors, sli, err = revisionMembers(ctx, tx, revisionID)
	return monitors, sli, policies, revision, err
}

func (s *Store) collectMonitors(rows pgx.Rows) ([]domain.Monitor, error) {
	defer rows.Close()
	var out []domain.Monitor
	for rows.Next() {
		m, err := s.scanMonitor(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan monitor: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate monitors: %w", err)
	}
	return out, nil
}

func (s *Store) collectMonitorsNoSecrets(rows pgx.Rows) ([]domain.Monitor, error) {
	defer rows.Close()
	var out []domain.Monitor
	for rows.Next() {
		m, err := s.scanMonitorNoSecrets(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan monitor: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate monitors: %w", err)
	}
	return out, nil
}

// ListRegions returns the distinct worker-pool regions in use across all monitors,
// always including the default (core), sorted. Powers the region picker in the UI.
func (s *Store) ListRegions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT region FROM monitors WHERE region <> '' ORDER BY region`)
	if err != nil {
		return nil, fmt.Errorf("store: list regions: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{domain.DefaultRegion: true}
	out := []string{domain.DefaultRegion}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("store: scan region: %w", err)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate regions: %w", err)
	}
	return out, nil
}
