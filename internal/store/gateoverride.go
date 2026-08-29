package store

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The override lifecycle (func-reliability-gate D9, D13a; invariants 8, 9, 17, 21).
//
// One active override per service, enforced under the service lock: creation first closes an
// unrevoked row whose expiry has passed (`expired`, attribution NULL) — a partial unique index
// cannot consult now(), and without the closure one expired override would refuse every later
// one forever — then refuses a remaining unrevoked row with ErrGateOverrideActive, then
// inserts with the actor triple. Revocation is by the override's immutable id, never "the
// current one", and a revoke of anything but an `active` row is ErrGateOverrideNotActive.
// `status` is never stored: every read computes domain.GateOverrideStatusAt against the
// database's now() and the live policy revision read in the SAME statement.

// GateOverrideRecord is one override row with its read-time status.
type GateOverrideRecord struct {
	domain.GateOverride
	Status domain.GateOverrideStatus
}

// gateOverrideListLimit is the history window (D13a: "the newest 50").
const gateOverrideListLimit = 50

// gateOverrideColumns is the one SELECT list every override read shares. The two trailing
// expressions — the database clock and the service's LIVE revision — ride in the same
// statement so the status function is computed over one instant and one fact set.
const gateOverrideColumns = `
	o.id, o.service_id, o.project_id, o.policy_revision,
	o.actor_user_id, o.via_token, o.actor_label,
	o.reason, o.created_at, o.expires_at,
	o.revoked_at, o.revoked_reason,
	o.revoked_by_user_id, o.revoked_via_token, o.revoked_by_label,
	statement_timestamp(),
	(SELECT p.revision FROM service_gate_policies p WHERE p.service_id = o.service_id AND p.deleted_at IS NULL)`

func scanGateOverride(row scannable) (GateOverrideRecord, error) {
	var (
		rec    GateOverrideRecord
		reason *string
		now    time.Time
		live   *int64
	)
	o := &rec.GateOverride
	if err := row.Scan(&o.ID, &o.ServiceID, &o.ProjectID, &o.PolicyRevision,
		&o.ActorUserID, &o.ViaToken, &o.ActorLabel,
		&o.Reason, &o.CreatedAt, &o.ExpiresAt,
		&o.RevokedAt, &reason,
		&o.RevokedByUserID, &o.RevokedViaToken, &o.RevokedByLabel,
		&now, &live); err != nil {
		return GateOverrideRecord{}, err
	}
	if reason != nil {
		o.RevokedReason = domain.GateRevokedReason(*reason)
	}
	o.CreatedAt, o.ExpiresAt = o.CreatedAt.UTC(), o.ExpiresAt.UTC()
	if o.RevokedAt != nil {
		t := o.RevokedAt.UTC()
		o.RevokedAt = &t
	}
	rec.Status = domain.GateOverrideStatusAt(*o, now, live)
	return rec, nil
}

// serviceExistsOn is the tenant check the override reads run before answering "none": a
// foreign, unknown or malformed service id is ErrNotFound on every gate route (D15).
func serviceExistsOn(ctx context.Context, q dbConn, projectID, serviceID string) error {
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1 AND project_id = $2)`, serviceID, projectID).Scan(&exists)
	if isInvalidTextRepresentation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: service scope: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// CreateGateOverride creates the service's one override (D9), in ONE transaction under the
// service row's lock: the policy must be live at exactly policyRevision (else
// ErrGateRevisionConflict); reason is 1..500 characters and expiresAt lies in
// (now, now + 7 d] on the DATABASE clock (else a GateValidationError naming the field); an
// unrevoked row whose expiry has passed is closed as `expired` first; a remaining unrevoked
// row is ErrGateOverrideActive; the row is inserted with the actor triple and the audit row
// `gate.override.create` in the same transaction.
func (s *Store) CreateGateOverride(
	ctx context.Context, projectID, serviceID string, policyRevision int64, reason string, expiresAt time.Time, actor GateActor,
) (string, error) {
	if actor.Label == "" {
		return "", errors.New("store: gate override requires an actor label")
	}
	if n := utf8.RuneCountInString(reason); n < domain.GateOverrideReasonMinLen || n > domain.GateOverrideReasonMaxLen {
		return "", gateInvalid("reason", "must be between %d and %d characters, got %d",
			domain.GateOverrideReasonMinLen, domain.GateOverrideReasonMaxLen, n)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin gate override: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceRowTx(ctx, tx, projectID, serviceID); err != nil {
		return "", err
	}
	live, err := liveGatePolicyRevisionOn(ctx, tx, serviceID)
	if err != nil {
		return "", err
	}
	if live == nil || *live != policyRevision {
		return "", ErrGateRevisionConflict
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return "", err
	}
	if !expiresAt.After(now) {
		return "", gateInvalid("expires_at", "must be after the server's current time %s, got %s",
			now.Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339))
	}
	if expiresAt.After(now.Add(domain.GateOverrideMaxDuration)) {
		return "", gateInvalid("expires_at", "must be at most %s ahead of the server's current time %s (the hard maximum), got %s",
			domain.GateOverrideMaxDuration, now.Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339))
	}

	// Release the slot an expired row holds (D9), then check it.
	if _, err := tx.Exec(ctx, `
		UPDATE service_gate_overrides
		   SET revoked_at = statement_timestamp(), revoked_reason = $2
		 WHERE service_id = $1 AND revoked_at IS NULL AND expires_at <= statement_timestamp()`,
		serviceID, string(domain.GateRevokedExpired)); err != nil {
		return "", fmt.Errorf("store: close expired gate override: %w", err)
	}
	var active bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM service_gate_overrides WHERE service_id = $1 AND revoked_at IS NULL)`,
		serviceID).Scan(&active); err != nil {
		return "", fmt.Errorf("store: gate override slot: %w", err)
	}
	if active {
		return "", ErrGateOverrideActive
	}

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO service_gate_overrides
		    (service_id, project_id, policy_revision, actor_user_id, via_token, actor_label, reason, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, statement_timestamp())
		RETURNING id`,
		serviceID, projectID, policyRevision, actor.userID(), actor.ViaToken, actor.Label, reason, expiresAt.UTC()).Scan(&id); err != nil {
		return "", fmt.Errorf("store: insert gate override: %w", err)
	}
	if err := insertGateAudit(ctx, tx, projectID, actor, "gate.override.create",
		"service="+serviceID+" override="+id+" policy_revision="+fmt.Sprint(policyRevision)+
			" expires_at="+expiresAt.UTC().Format(time.RFC3339)+" actor="+actor.Label); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit gate override: %w", err)
	}
	return id, nil
}

// RevokeGateOverride closes one override by its immutable id (D13a), in ONE transaction: the
// row must belong to the service and the tenant (else ErrNotFound); its status is computed
// against the live revision and the database's now(); anything but `active` is
// ErrGateOverrideNotActive — a stale screen learns it is stale; the row is closed as `manual`
// with the revoker's complete triple and audited as `gate.override.revoke`.
func (s *Store) RevokeGateOverride(ctx context.Context, projectID, serviceID, overrideID string, actor GateActor) error {
	if actor.Label == "" {
		return errors.New("store: gate override revoke requires an actor label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin gate override revoke: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockServiceRowTx(ctx, tx, projectID, serviceID); err != nil {
		return err
	}
	rec, err := scanGateOverride(tx.QueryRow(ctx,
		`SELECT`+gateOverrideColumns+`
		   FROM service_gate_overrides o
		  WHERE o.id = $1 AND o.service_id = $2 AND o.project_id = $3
		    FOR UPDATE OF o`, overrideID, serviceID, projectID))
	if noRows(err) || isInvalidTextRepresentation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read gate override: %w", err)
	}
	if rec.Status != domain.GateOverrideActive {
		return ErrGateOverrideNotActive
	}
	if _, err := tx.Exec(ctx, `
		UPDATE service_gate_overrides
		   SET revoked_at = statement_timestamp(), revoked_reason = $2,
		       revoked_by_user_id = $3, revoked_via_token = $4, revoked_by_label = $5
		 WHERE id = $1`,
		overrideID, string(domain.GateRevokedManual), actor.userID(), actor.ViaToken, actor.Label); err != nil {
		return fmt.Errorf("store: revoke gate override: %w", err)
	}
	if err := insertGateAudit(ctx, tx, projectID, actor, "gate.override.revoke",
		"service="+serviceID+" override="+overrideID+" revoker="+actor.Label); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit gate override revoke: %w", err)
	}
	return nil
}

// ActiveGateOverride is the service's ACTIVE override — status computed at read time — or
// ErrGateOverrideNone. A foreign or unknown service is ErrNotFound first.
func (s *Store) ActiveGateOverride(ctx context.Context, projectID, serviceID string) (GateOverrideRecord, error) {
	if err := serviceExistsOn(ctx, s.pool, projectID, serviceID); err != nil {
		return GateOverrideRecord{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT`+gateOverrideColumns+`
		   FROM service_gate_overrides o
		  WHERE o.service_id = $1 AND o.project_id = $2 AND o.revoked_at IS NULL
		  ORDER BY o.created_at DESC, o.id DESC`, serviceID, projectID)
	if err != nil {
		return GateOverrideRecord{}, fmt.Errorf("store: read active gate override: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scanGateOverride(rows)
		if err != nil {
			return GateOverrideRecord{}, fmt.Errorf("store: scan gate override: %w", err)
		}
		if rec.Status == domain.GateOverrideActive {
			return rec, nil
		}
	}
	if err := rows.Err(); err != nil {
		return GateOverrideRecord{}, err
	}
	return GateOverrideRecord{}, ErrGateOverrideNone
}

// GetGateOverride reads one override, whatever its status, with the full actor and revoker
// triples. Unknown, foreign or malformed → ErrNotFound.
func (s *Store) GetGateOverride(ctx context.Context, projectID, serviceID, overrideID string) (GateOverrideRecord, error) {
	rec, err := scanGateOverride(s.pool.QueryRow(ctx,
		`SELECT`+gateOverrideColumns+`
		   FROM service_gate_overrides o
		  WHERE o.id = $1 AND o.service_id = $2 AND o.project_id = $3`, overrideID, serviceID, projectID))
	if noRows(err) || isInvalidTextRepresentation(err) {
		return GateOverrideRecord{}, ErrNotFound
	}
	if err != nil {
		return GateOverrideRecord{}, fmt.Errorf("store: read gate override: %w", err)
	}
	return rec, nil
}

// ListGateOverrides is the history: the newest 50 by (created_at DESC, id DESC) over the
// matching index, each with its read-time status. A foreign or unknown service is
// ErrNotFound; a service with no history is an empty, non-nil list.
func (s *Store) ListGateOverrides(ctx context.Context, projectID, serviceID string) ([]GateOverrideRecord, error) {
	if err := serviceExistsOn(ctx, s.pool, projectID, serviceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT`+gateOverrideColumns+`
		   FROM service_gate_overrides o
		  WHERE o.service_id = $1 AND o.project_id = $2
		  ORDER BY o.created_at DESC, o.id DESC
		  LIMIT $3`, serviceID, projectID, gateOverrideListLimit)
	if err != nil {
		return nil, fmt.Errorf("store: list gate overrides: %w", err)
	}
	defer rows.Close()
	out := []GateOverrideRecord{}
	for rows.Next() {
		rec, err := scanGateOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan gate override: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// activeGateOverrideTx is the decision path's read of the override that applies at the
// snapshot instant (D9): an unrevoked row whose status, computed against the live revision
// passed in and `at` (= evaluated_at), is `active`. It runs inside the decision transaction so
// the override and every other fact come from one snapshot (D6a).
func activeGateOverrideTx(ctx context.Context, tx pgx.Tx, serviceID string, at time.Time, liveRevision *int64) (*domain.GateOverride, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, service_id, project_id, policy_revision, actor_user_id, via_token, actor_label,
		       reason, created_at, expires_at
		  FROM service_gate_overrides
		 WHERE service_id = $1 AND revoked_at IS NULL
		 ORDER BY created_at DESC, id DESC`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("store: read gate override for decision: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var o domain.GateOverride
		if err := rows.Scan(&o.ID, &o.ServiceID, &o.ProjectID, &o.PolicyRevision, &o.ActorUserID, &o.ViaToken, &o.ActorLabel,
			&o.Reason, &o.CreatedAt, &o.ExpiresAt); err != nil {
			return nil, fmt.Errorf("store: scan gate override for decision: %w", err)
		}
		o.CreatedAt, o.ExpiresAt = o.CreatedAt.UTC(), o.ExpiresAt.UTC()
		if domain.GateOverrideStatusAt(o, at, liveRevision) == domain.GateOverrideActive {
			return &o, nil
		}
	}
	return nil, rows.Err()
}
