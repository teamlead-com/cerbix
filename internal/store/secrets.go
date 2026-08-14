package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// Secret inventory (spec func-secret-inventory §4.1/§4.3/§4.5/§4.6/§6). Every method takes
// and predicates projectID — a missing project filter is a security defect per the repo's
// tenant invariant; there is NO global resolve-by-id path. Values are AEAD-encrypted at
// rest with AAD = CanonicalAAD(project_id, secret_id): stable identifiers only, so a
// rename is metadata-only (never re-encrypts) and a ciphertext row transplanted between
// tenant rows fails authentication at decrypt.
//
// Every mutation transaction starts by resolving the caller-supplied project id to its
// CANONICAL uuid text form (Postgres accepts many equivalent uuid spellings — uppercase,
// braces, urn:); the canonical string is what feeds the quota advisory lock and every AAD
// construction, so equivalent spellings can neither split the lock nor break decryption.

const (
	// maxProjectSecrets is the per-project inventory quota (spec §4.3), checked under a
	// per-project transactional advisory lock so two concurrent creates can't both read
	// free quota.
	maxProjectSecrets = 100
	// maxSecretValueBytes bounds a secret value (spec §5: value ≤ 4 KiB UTF-8 bytes).
	maxSecretValueBytes = 4096
)

// ErrSecretsUnavailable is returned when the secret inventory is used without a configured
// encryption key. Fail-closed by design (spec §4.1): the plaintext-tolerant legacy path is
// NEVER used for inventory values.
var ErrSecretsUnavailable = errors.New("store: secret inventory requires a configured encryption key")

// ErrSecretExists is returned when a create or rename collides with an existing secret
// name in the same project.
var ErrSecretExists = errors.New("store: a secret with this name already exists in the project")

// ErrSecretNameInvalid is returned when a secret name does not match the domain slug
// (domain.ValidSecretName).
var ErrSecretNameInvalid = errors.New("store: invalid secret name")

// ErrSecretValueInvalid is returned when a secret value is empty, is not valid UTF-8, or
// exceeds maxSecretValueBytes. Whitespace-only IS a valid value (spec §5 bounds bytes,
// not content).
var ErrSecretValueInvalid = errors.New("store: invalid secret value")

// ErrSecretQuota is returned when a create would exceed maxProjectSecrets for the project.
var ErrSecretQuota = errors.New("store: project secret quota exceeded")

// SecretInUseError is returned by DeleteProjectSecret while monitor_secret_refs rows still
// reference the secret (spec §4.6: 409 secret_in_use, no force-delete). Count is exact when
// produced by the under-lock guard, current/advisory when mapped from a commit-time FK
// violation.
type SecretInUseError struct{ Count int }

func (e SecretInUseError) Error() string {
	return fmt.Sprintf("store: secret is referenced by %d monitor setting(s)", e.Count)
}

// SecretRenamedInUseError is returned by UpdateProjectSecret when a rename is blocked by
// file-managed references (spec §4.6: 409 secret_renamed_in_use — the file source, not the
// DB, owns those names). Count is the number of file-managed references.
type SecretRenamedInUseError struct{ Count int }

func (e SecretRenamedInUseError) Error() string {
	return fmt.Sprintf("store: secret is referenced by %d file-managed monitor setting(s); rename in the file source", e.Count)
}

// SecretActor identifies who performs a secret mutation. It feeds the audit row written
// INSIDE the mutation transaction (spec §5: audit is part of the atomic change, never a
// best-effort follow-up). Fields mirror audit_logs: ActorUserID is a soft user FK (empty →
// NULL, e.g. a machine actor), ViaToken marks a service-account API-token principal.
type SecretActor struct {
	ActorUserID string
	ViaToken    bool
}

// ProjectSecret is the no-value DTO for inventory listings (spec §4.4.2: names/dates and
// used-by counts only — neither plaintext nor ciphertext is ever serialized outward).
type ProjectSecret struct {
	ID                string
	Name              string
	CreatedAt         time.Time
	RotatedAt         *time.Time
	UsedByTotal       int
	UsedByFileManaged int
}

// isForeignKeyViolation reports whether err is a Postgres FK (23503) error — including one
// surfaced at Commit by a deferred constraint.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// isInvalidTextRepresentation reports whether err is a Postgres 22P02 error — the caller
// supplied a string that cannot even be parsed as a uuid. For lookups that means the row
// cannot exist.
func isInvalidTextRepresentation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func validateSecretName(name string) error {
	if !domain.ValidSecretName(name) {
		return fmt.Errorf("%w: %q must match %s", ErrSecretNameInvalid, name, domain.SecretNamePattern())
	}
	return nil
}

// validateSecretValue enforces the spec §5 value rules: non-empty, valid UTF-8, at most
// maxSecretValueBytes. Whitespace-only is deliberately accepted — a secret's content is
// the owner's business; only emptiness, encoding, and size are ours to police.
func validateSecretValue(value string) error {
	if len(value) == 0 {
		return fmt.Errorf("%w: empty value", ErrSecretValueInvalid)
	}
	if len(value) > maxSecretValueBytes {
		return fmt.Errorf("%w: value exceeds %d bytes", ErrSecretValueInvalid, maxSecretValueBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: value is not valid UTF-8", ErrSecretValueInvalid)
	}
	return nil
}

// singleRowQuerier is the QueryRow subset shared by *pgxpool.Pool and pgx.Tx.
type singleRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// canonicalProject resolves the caller-supplied project id to its CANONICAL uuid text
// form plus the owning org, or ErrNotFound. Run at the START of every secrets operation:
// the canonical string — never the caller's raw spelling — feeds the quota advisory lock
// and all AAD constructions, so two equivalent spellings of the same uuid always hash to
// the same lock and authenticate against the same ciphertext.
func canonicalProject(ctx context.Context, q singleRowQuerier, projectID string) (canonID, orgID string, err error) {
	err = q.QueryRow(ctx,
		`SELECT id::text, org_id::text FROM projects WHERE id = $1`, projectID).Scan(&canonID, &orgID)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("store: resolve project: %w", err)
	}
	return canonID, orgID, nil
}

// insertSecretAudit writes the audit row INSIDE the caller's mutation transaction, so the
// audit trail and the change commit or roll back together. Targets carry names and flags
// only — never values or ciphertext.
func insertSecretAudit(ctx context.Context, tx pgx.Tx, orgID string, actor SecretActor, action, target string) error {
	var actorID *string
	if actor.ActorUserID != "" {
		actorID = &actor.ActorUserID
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 VALUES ($1, $2, $3, $4, $5)`,
		orgID, actorID, actor.ViaToken, action, target); err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	return nil
}

// wipe zeroes a plaintext buffer. Defer it on every local plaintext copy made for
// encryption so the value does not linger in the heap longer than needed.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// CreateProjectSecret inserts a new inventory secret. Hard-fails without a cipher (spec
// §4.1 — never plaintext). One transaction: canonical project resolution, the per-project
// advisory quota lock, the quota count, the insert, and the audit row, so concurrent
// creates serialize on the quota check and the audit commits with the row. The row id is
// generated first (inside the tx) because the at-rest AAD binds (project_id, secret_id).
func (s *Store) CreateProjectSecret(ctx context.Context, actor SecretActor, projectID, name, value string) (ProjectSecret, error) {
	if s.cipher == nil {
		return ProjectSecret{}, ErrSecretsUnavailable
	}
	if err := validateSecretName(name); err != nil {
		return ProjectSecret{}, err
	}
	if err := validateSecretValue(value); err != nil {
		return ProjectSecret{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	projID, orgID, err := canonicalProject(ctx, tx, projectID)
	if err != nil {
		return ProjectSecret{}, err
	}
	// Per-project quota serialization (spec §4.3), namespaced away from other advisory
	// locks. hashtext runs over the CANONICAL id, so equivalent uuid spellings take the
	// same lock.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('project_secrets_quota'), hashtext($1))`, projID); err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: quota lock: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM project_secrets WHERE project_id = $1`, projID).Scan(&n); err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: quota count: %w", err)
	}
	if n >= maxProjectSecrets {
		return ProjectSecret{}, ErrSecretQuota
	}
	// The AAD needs the row id before the insert; the repo has no uuid dependency, so let
	// Postgres mint it.
	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: generate id: %w", err)
	}
	// One controlled plaintext copy, wiped on exit (no avoidable unwiped copies).
	buf := []byte(value)
	defer wipe(buf)
	encVal, err := s.cipher.EncryptBytes(buf, secret.CanonicalAAD(projID, id))
	if err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: encrypt: %w", err)
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO project_secrets (id, project_id, name, value_encrypted)
		 VALUES ($1, $2, $3, $4) RETURNING created_at`,
		id, projID, name, encVal).Scan(&createdAt)
	if isUniqueViolation(err) {
		return ProjectSecret{}, ErrSecretExists
	}
	if isForeignKeyViolation(err) {
		// The project vanished between resolution and insert.
		return ProjectSecret{}, ErrNotFound
	}
	if err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: insert: %w", err)
	}
	if err := insertSecretAudit(ctx, tx, orgID, actor, "secret.create", name); err != nil {
		return ProjectSecret{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectSecret{}, fmt.Errorf("store: create secret: commit: %w", err)
	}
	return ProjectSecret{ID: id, Name: name, CreatedAt: createdAt}, nil
}

// UpdateProjectSecret renames and/or rotates a secret in ONE transaction (spec §4.6). The
// secret row is locked FOR UPDATE first (fixed §4.3 lock order: secret row, then monitor
// rows by id asc). Rotate re-encrypts under the SAME AAD — (project_id, secret_id) is
// stable — sets rotated_at, and runs the §4.5 rotation fence: every referencing monitor
// (disabled included) gets the same D-0142 execution_revision bump + freshness-watermark
// reset UpdateMonitor applies, so in-flight old-value jobs are rejected at ingest and the
// next dispatch reads the new value + new revision atomically. Rename is blocked by
// file-managed references; UI-managed references are atomically re-pointed via a dedicated
// metadata write (identity/value unchanged → no execution_revision / updated_at bump).
// The audit row commits in the same tx; the monitor_config_changed NOTIFY is sent in-tx
// and only when monitor rows actually changed. Returns what happened plus the re-point
// count.
func (s *Store) UpdateProjectSecret(ctx context.Context, actor SecretActor, projectID, name string, newName, newValue *string) (renamed, rotated bool, repointed int, err error) {
	if s.cipher == nil {
		return false, false, 0, ErrSecretsUnavailable
	}
	if newName == nil && newValue == nil {
		return false, false, 0, errors.New("store: update secret: nothing to change")
	}
	if newValue != nil {
		if err := validateSecretValue(*newValue); err != nil {
			return false, false, 0, err
		}
	}
	if newName != nil {
		if err := validateSecretName(*newName); err != nil {
			return false, false, 0, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, 0, fmt.Errorf("store: update secret: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	projID, orgID, err := canonicalProject(ctx, tx, projectID)
	if err != nil {
		return false, false, 0, err
	}
	var id, curName string
	err = tx.QueryRow(ctx,
		`SELECT id::text, name FROM project_secrets WHERE project_id = $1 AND name = $2 FOR UPDATE`,
		projID, name).Scan(&id, &curName)
	if noRows(err) {
		return false, false, 0, ErrNotFound
	}
	if err != nil {
		return false, false, 0, fmt.Errorf("store: update secret: lock: %w", err)
	}

	fenced := 0
	if newValue != nil {
		// One controlled plaintext copy, wiped on exit.
		buf := []byte(*newValue)
		defer wipe(buf)
		encVal, eerr := s.cipher.EncryptBytes(buf, secret.CanonicalAAD(projID, id))
		if eerr != nil {
			return false, false, 0, fmt.Errorf("store: update secret: encrypt: %w", eerr)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE project_secrets SET value_encrypted = $1, rotated_at = now()
			  WHERE id = $2 AND project_id = $3`,
			encVal, id, projID); err != nil {
			return false, false, 0, fmt.Errorf("store: update secret: rotate: %w", err)
		}
		fenced, err = fenceSecretMonitors(ctx, tx, projID, id)
		if err != nil {
			return false, false, 0, err
		}
		rotated = true
	}

	if newName != nil && *newName != curName {
		// Rename guard: any file-managed reference blocks the rename — the file source owns
		// those names (spec §4.6).
		var fileManaged int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM monitor_secret_refs r
			   JOIN managed_monitors mm ON mm.monitor_id = r.monitor_id
			  WHERE r.secret_id = $1 AND r.project_id = $2`,
			id, projID).Scan(&fileManaged); err != nil {
			return false, false, 0, fmt.Errorf("store: update secret: rename guard: %w", err)
		}
		if fileManaged > 0 {
			return false, false, 0, SecretRenamedInUseError{Count: fileManaged}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE project_secrets SET name = $1 WHERE id = $2 AND project_id = $3`,
			*newName, id, projID); err != nil {
			if isUniqueViolation(err) {
				return false, false, 0, ErrSecretExists
			}
			return false, false, 0, fmt.Errorf("store: update secret: rename: %w", err)
		}
		repointed, err = repointSecretRefs(ctx, tx, projID, id, curName, *newName)
		if err != nil {
			return false, false, 0, err
		}
		renamed = true
	}

	// Audit and NOTIFY fire ONLY when rows actually changed (spec §4.5): a no-op call
	// (rename to the current name, no value) commits nothing and records nothing.
	if renamed || rotated {
		target := curName
		if renamed {
			target = curName + " → " + *newName
		}
		if err := insertSecretAudit(ctx, tx, orgID, actor, "secret.update",
			fmt.Sprintf("%s · renamed=%t rotated=%t repointed=%d", target, renamed, rotated, repointed)); err != nil {
			return false, false, 0, err
		}
	}
	if fenced > 0 {
		// Wake the scheduler in-tx, exactly like a file-provider apply: the NOTIFY is
		// delivered on COMMIT, so listeners only ever observe the fenced state.
		payload, _ := json.Marshal(struct {
			Reason  string `json:"reason"`
			Project string `json:"project"`
			Secret  string `json:"secret"`
			Fenced  int    `json:"fenced"`
		}{"secret_rotation", projID, id, fenced})
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, FileConfigChannel, string(payload)); err != nil {
			return false, false, 0, fmt.Errorf("store: update secret: notify config change: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, false, 0, fmt.Errorf("store: update secret: commit: %w", err)
	}
	return renamed, rotated, repointed, nil
}

// fenceSecretMonitors is the §4.5 rotation fence, inside the caller's rotate transaction.
// It locks every monitor row referencing the secret in the fixed §4.3 order (monitor id
// asc, after the secret row the caller already holds FOR UPDATE), then applies the SAME
// D-0142 stale-config semantics updateMonitorTx uses: execution_revision + 1 (fail-safe —
// a missed bump would let an in-flight old-credential job's result land as current), and
// the freshness-watermark reset (scheduled monitors NULL last_result_ts so the first
// result of the new generation is not rejected against the old one; push monitors
// preserve it — it is their real-ping liveness watermark). Disabled monitors are included:
// they keep their config current for the moment they are re-enabled. Returns how many
// monitor rows were fenced.
func fenceSecretMonitors(ctx context.Context, tx pgx.Tx, projectID, secretID string) (int, error) {
	rows, err := tx.Query(ctx,
		`SELECT m.id::text FROM monitors m
		  WHERE m.project_id = $2
		    AND m.id IN (SELECT r.monitor_id FROM monitor_secret_refs r
		                  WHERE r.secret_id = $1 AND r.project_id = $2)
		  ORDER BY m.id
		    FOR UPDATE`,
		secretID, projectID)
	if err != nil {
		return 0, fmt.Errorf("store: rotation fence: lock monitors: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: rotation fence: scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: rotation fence: iterate: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ct, err := tx.Exec(ctx,
		`UPDATE monitors
		    SET execution_revision = execution_revision + 1,
		        last_result_ts = CASE WHEN type = 'push' THEN last_result_ts ELSE NULL END
		  WHERE id = ANY($1::uuid[]) AND project_id = $2`,
		ids, projectID)
	if err != nil {
		return 0, fmt.Errorf("store: rotation fence: bump revisions: %w", err)
	}
	return int(ct.RowsAffected()), nil
}

// repointSecretRefs rewrites the `<setting_key>` ref NAME in each referencing monitor's
// config from oldName to newName, inside the caller's rename transaction. This is a
// dedicated metadata write, NOT UpdateMonitor: the monitor's identity and effective
// credential value are unchanged, so execution_revision and updated_at are deliberately
// untouched (spec §4.6). Monitor rows are locked in the fixed §4.3 order (monitor_id asc,
// after the secret row the caller already holds FOR UPDATE). A ref row whose monitor
// config does NOT carry the old name is a broken invariant — §4.3 guarantees the persisted
// `*_ref` name and the ref row cannot diverge — so the whole rename fails and rolls back
// rather than papering over corrupt state.
func repointSecretRefs(ctx context.Context, tx pgx.Tx, projectID, secretID, oldName, newName string) (int, error) {
	rows, err := tx.Query(ctx,
		`SELECT r.monitor_id::text, r.setting_key FROM monitor_secret_refs r
		  WHERE r.secret_id = $1 AND r.project_id = $2
		  ORDER BY r.monitor_id, r.setting_key`,
		secretID, projectID)
	if err != nil {
		return 0, fmt.Errorf("store: repoint secret refs: list: %w", err)
	}
	type ref struct{ monitorID, settingKey string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.monitorID, &r.settingKey); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: repoint secret refs: scan: %w", err)
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: repoint secret refs: iterate: %w", err)
	}

	repointed := 0
	for _, r := range refs {
		var raw []byte
		if err := tx.QueryRow(ctx,
			`SELECT config FROM monitors WHERE id = $1 AND project_id = $2 FOR UPDATE`,
			r.monitorID, projectID).Scan(&raw); err != nil {
			return 0, fmt.Errorf("store: repoint secret refs: lock monitor %s: %w", r.monitorID, err)
		}
		cfg := map[string]string{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return 0, fmt.Errorf("store: repoint secret refs: decode config of %s: %w", r.monitorID, err)
			}
		}
		if cfg[r.settingKey] != oldName {
			// Broken §4.3 invariant: the ref row says this monitor references the secret,
			// but the persisted config disagrees. Fail the rename — a silent skip would
			// leave a ref row pointing at a name the config never carried.
			return 0, fmt.Errorf("store: repoint secret refs: monitor %s config %q does not reference %q (ref/config invariant broken)",
				r.monitorID, r.settingKey, oldName)
		}
		cfg[r.settingKey] = newName
		b, err := json.Marshal(cfg)
		if err != nil {
			return 0, fmt.Errorf("store: repoint secret refs: encode config of %s: %w", r.monitorID, err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE monitors SET config = $1 WHERE id = $2 AND project_id = $3`,
			b, r.monitorID, projectID); err != nil {
			return 0, fmt.Errorf("store: repoint secret refs: update %s: %w", r.monitorID, err)
		}
		repointed++
	}
	return repointed, nil
}

// DeleteProjectSecret deletes a secret unless it is still referenced (spec §4.6/§6). One
// transaction: canonical project resolution, FOR UPDATE lock, exact ref count UNDER the
// lock (returned in SecretInUseError without attempting the delete), the delete, and the
// audit row. The deferred FK on monitor_secret_refs remains the commit-time guard for any
// path that bypasses the count; a commit-time 23503 is mapped to the same typed error with
// a current (advisory) count.
func (s *Store) DeleteProjectSecret(ctx context.Context, actor SecretActor, projectID, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: delete secret: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	projID, orgID, err := canonicalProject(ctx, tx, projectID)
	if err != nil {
		return err
	}
	var id string
	err = tx.QueryRow(ctx,
		`SELECT id::text FROM project_secrets WHERE project_id = $1 AND name = $2 FOR UPDATE`,
		projID, name).Scan(&id)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: delete secret: lock: %w", err)
	}
	var refs int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM monitor_secret_refs WHERE secret_id = $1 AND project_id = $2`,
		id, projID).Scan(&refs); err != nil {
		return fmt.Errorf("store: delete secret: ref count: %w", err)
	}
	if refs > 0 {
		return SecretInUseError{Count: refs}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_secrets WHERE id = $1 AND project_id = $2`, id, projID); err != nil {
		return fmt.Errorf("store: delete secret: %w", err)
	}
	if err := insertSecretAudit(ctx, tx, orgID, actor, "secret.delete", name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		if isForeignKeyViolation(err) {
			// The deferred FK fired at COMMIT. Report a current count from a fresh,
			// tenant-scoped query — advisory, not exact (spec §6).
			var n int
			_ = s.pool.QueryRow(ctx,
				`SELECT count(*) FROM monitor_secret_refs WHERE secret_id = $1 AND project_id = $2`,
				id, projID).Scan(&n)
			return SecretInUseError{Count: n}
		}
		return fmt.Errorf("store: delete secret: commit: %w", err)
	}
	return nil
}

// ListProjectSecrets returns the project's inventory: names, dates, and used-by counts
// (total and file-managed). It NEVER selects value_encrypted — the listing is a no-decrypt
// read by schema (spec §4.4.2). ErrNotFound when the project itself does not exist.
func (s *Store) ListProjectSecrets(ctx context.Context, projectID string) ([]ProjectSecret, error) {
	projID, _, err := canonicalProject(ctx, s.pool, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT ps.id::text, ps.name, ps.created_at, ps.rotated_at,
		        (SELECT count(*) FROM monitor_secret_refs r
		          WHERE r.secret_id = ps.id AND r.project_id = ps.project_id) AS used_total,
		        (SELECT count(*) FROM monitor_secret_refs r
		           JOIN managed_monitors mm ON mm.monitor_id = r.monitor_id
		          WHERE r.secret_id = ps.id AND r.project_id = ps.project_id) AS used_file_managed
		   FROM project_secrets ps
		  WHERE ps.project_id = $1
		  ORDER BY ps.name`,
		projID)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	defer rows.Close()
	var out []ProjectSecret
	for rows.Next() {
		var p ProjectSecret
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.RotatedAt, &p.UsedByTotal, &p.UsedByFileManaged); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate secrets: %w", err)
	}
	return out, nil
}

// resolveProjectSecret returns the secret's id and decrypted value, tenant-predicated by
// projectID (canonicalized first — the AAD binds the canonical form). This is a decrypt
// path, unexported by design: its ONLY intended callers are in-package — the authoritative
// materializer (spec §4.4.3, which will own a joined batch read) and rotation/reencrypt
// internals — never a display/list path.
func (s *Store) resolveProjectSecret(ctx context.Context, projectID, name string) (string, []byte, error) {
	if s.cipher == nil {
		return "", nil, ErrSecretsUnavailable
	}
	projID, _, err := canonicalProject(ctx, s.pool, projectID)
	if err != nil {
		return "", nil, err
	}
	var id, enc string
	err = s.pool.QueryRow(ctx,
		`SELECT id::text, value_encrypted FROM project_secrets WHERE project_id = $1 AND name = $2`,
		projID, name).Scan(&id, &enc)
	if noRows(err) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("store: resolve secret: %w", err)
	}
	pt, err := s.cipher.DecryptBytes(enc, secret.CanonicalAAD(projID, id))
	if err != nil {
		return "", nil, fmt.Errorf("store: resolve secret %q: %w", name, err)
	}
	return id, pt, nil
}
