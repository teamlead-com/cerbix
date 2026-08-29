package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// FR-024 — the reliability gate's store (func-reliability-gate.md, iter-0163 changeset 2).
//
// Four files share this one: gatepolicy.go (the per-service policy: CAS on a DB-owned
// generation, the no-op rule, tombstone instead of DELETE, override closure — D11, D13a,
// D14), gateoverride.go (the override lifecycle and its read-time status — D9, D13a),
// gatedecision.go (the ONE transaction that reads every owner in one snapshot, applies the
// D4 algebra and writes the ledger row — D1, D6a, D7, D8a, D10) and gateledger.go (the
// project-scoped by-id read that prunes to one partition, and the live keyset listing — §5).
// This file holds what they share: the sentinel errors the API maps without string matching,
// the actor the handlers pass in so the audit row is written INSIDE the mutation transaction,
// canonical JSON, the UUIDv7 built from the database's clock, and SQLSTATE classification.

// Sentinel errors. Each maps to exactly one HTTP answer in the API layer.
var (
	// ErrGatePolicyNotConfigured: the service has no live policy (absent or tombstoned) — 404
	// `not_configured` on the policy routes.
	ErrGatePolicyNotConfigured = errors.New("store: gate policy is not configured")
	// ErrGateRevisionConflict: the caller's expected_revision / policy_revision is not the live
	// revision — 409 `revision_conflict`; nothing changed.
	ErrGateRevisionConflict = errors.New("store: gate policy revision conflict")
	// ErrGateOverrideActive: an unrevoked override already holds the service's one slot — 409
	// `override_active`.
	ErrGateOverrideActive = errors.New("store: an override is already active for this service")
	// ErrGateOverrideNotActive: a revoke of an override whose status is not `active` — 409
	// `override_not_active`, never a silent 204.
	ErrGateOverrideNotActive = errors.New("store: gate override is not active")
	// ErrGateOverrideNone: no active override — 404 `none_active` on the active-override read.
	ErrGateOverrideNone = errors.New("store: no active gate override")
	// ErrGateSnapshotConflict: the decision transaction hit a serialization failure twice — 503
	// `snapshot_conflict`, a transport error, never a decision (D6a).
	ErrGateSnapshotConflict = errors.New("store: gate decision snapshot conflict")
	// ErrGateLedgerUnwritable: no ledger partition holds evaluated_at — 503 `ledger_unwritable`;
	// a decision the ledger cannot hold is not a decision (D10).
	ErrGateLedgerUnwritable = errors.New("store: gate decision ledger has no partition for this instant")
	// ErrGateBudgetExceeded: the begin-through-commit budget ran out (the deadline wrapper
	// refused a statement, or the server killed one) — the `timeout` evaluation error kind.
	ErrGateBudgetExceeded = errors.New("store: gate decision budget exceeded")
	// ErrGateCursorInvalid: a listing cursor that does not decode — 400 `cursor_invalid`.
	ErrGateCursorInvalid = errors.New("store: gate listing cursor is invalid")
)

// GateValidationError is a refused override or listing input, naming the field the way
// domain.GatePolicyError names a policy field, so the API answers 400 with the field and the
// rule without re-deriving either.
type GateValidationError struct {
	Field string
	Msg   string
}

func (e *GateValidationError) Error() string { return e.Field + ": " + e.Msg }

func gateInvalid(field, format string, args ...any) error {
	return &GateValidationError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// GateActor is the request's principal as the handlers pass it into every gate mutation
// (D9, D-0188): the typed attribution the audit log already uses — a nullable user id and the
// via-token flag, via authz.Principal.AuditUserID() — AND the immutable server-derived label
// (authz.Principal.AuditLabel: `token:<name>` for an API token, the user for a person), which
// the override rows store because for a machine principal the typed half is `NULL + true`.
type GateActor struct {
	// ActorUserID is a soft user FK; empty is stored as NULL (a machine actor).
	ActorUserID string
	// ViaToken marks an API-token principal.
	ViaToken bool
	// Label is the principal's audit label; it must not be empty.
	Label string
}

func (a GateActor) userID() *string {
	if a.ActorUserID == "" {
		return nil
	}
	id := a.ActorUserID
	return &id
}

// insertGateAudit writes the audit row inside the caller's transaction, resolving the org from
// the project exactly as insertAlertAudit does. Policy and override mutations are audited;
// decisions are NOT (D10) — a busy pipeline would bury the audit log under its own heartbeat.
func insertGateAudit(ctx context.Context, tx pgx.Tx, projectID string, actor GateActor, action, target string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		SELECT p.org_id, $2, $3, $4, $5 FROM projects p WHERE p.id = $1`,
		projectID, actor.userID(), actor.ViaToken, action, target); err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	return nil
}

// lockServiceRowTx takes the service row FOR UPDATE — the serialization point every gate
// mutation shares (D9 names it for override creation; the policy CAS runs under it too) — and
// answers a foreign, unknown or malformed id with ErrNotFound, indistinguishably.
func lockServiceRowTx(ctx context.Context, tx pgx.Tx, projectID, serviceID string) error {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`, serviceID, projectID).Scan(&one)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock service: %w", err)
	}
	return nil
}

// dbNow reads the database's clock inside the transaction — statement_timestamp(), the instant
// THIS statement began, never now(): PostgreSQL's now() is frozen at BEGIN, so after a wait on the
// service row lock it describes the moment the caller queued, not the moment the row is written
// (review [19]: an override validated against BEGIN time after a two-minute wait committed already
// expired). Every gate comparison against "now" — an override's expiry, the seven-day bound, a
// status — uses THIS clock, read AFTER the lock, never the application's (D9).
func dbNow(ctx context.Context, q dbConn) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("store: read clock: %w", err)
	}
	return now.UTC(), nil
}

// canonicalJSONBytes encodes v as CANONICAL JSON — object keys sorted, no whitespace — by
// round-tripping the struct encoding through encoding/json's map form, which sorts keys.
// Equal facts give equal bytes, so the ledger's byte bounds are testable and a stored
// snapshot compares by text (§5a).
func canonicalJSONBytes(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

// newGateDecisionID builds the ledger id (§5 "Identity on a partitioned ledger"): a UUIDv7
// whose 48-bit timestamp is the MILLISECOND of evaluatedAt — the database clock the row is
// partitioned by, read in the decision transaction — followed by version 7, the RFC 4122
// variant and 74 random bits from crypto/rand. The database CHECKs the binding
// (gate_uuid_ms(id) = floor(epoch ms of evaluated_at)), so the writer cannot be trusted
// wrongly; this only has to be right.
func newGateDecisionID(evaluatedAt time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: gate decision id entropy: %w", err)
	}
	ms := evaluatedAt.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", fmt.Errorf("store: gate decision id: instant %s is outside the 48-bit millisecond range", evaluatedAt)
	}
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// gateDecisionIDMillis reads the 48-bit millisecond out of a decision id — the inverse of
// newGateDecisionID — strictly: a text that is not a canonical 36-character lowercase-or-
// uppercase hex UUID is refused. It is what lets the by-id read derive the row's UTC day from
// the id alone, with no application clock anywhere (§5).
func gateDecisionIDMillis(id string) (int64, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return 0, false
	}
	hexOnly := strings.ReplaceAll(id, "-", "")
	raw, err := hex.DecodeString(hexOnly)
	if err != nil || len(raw) != 16 {
		return 0, false
	}
	var ts [8]byte
	copy(ts[2:8], raw[0:6])
	return int64(binary.BigEndian.Uint64(ts[:])), true
}

// pgErrCode returns the SQLSTATE of err, "" when it is not a server error.
func pgErrCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// isSerializationFailure: SQLSTATE 40001 — the REPEATABLE READ snapshot lost to a concurrent
// write. The decision transaction retries the WHOLE transaction once on it (D6a).
func isSerializationFailure(err error) bool { return pgErrCode(err) == "40001" }

// isNoPartitionForRow: SQLSTATE 23514 raised by the partitioned INSERT because no attached
// partition holds the row — "no partition of relation ... found for row". Other 23514s are
// CHECK violations and keep their own meaning.
func isNoPartitionForRow(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		return false
	}
	return strings.Contains(pe.Message, "no partition of relation")
}

// isStatementTimeout: SQLSTATE 57014 (query_canceled) — the server bound the deadline wrapper
// set has fired.
func isStatementTimeout(err error) bool { return pgErrCode(err) == "57014" }
