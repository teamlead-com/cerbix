package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Ledger reads (func-reliability-gate §5 "Identity on a partitioned ledger" and "The listing
// contract"; invariants 10, 12).
//
// Both reads are PROJECT-scoped, never service-nested: a decision outlives its service, and
// the moment the evidence is wanted is often the moment the service is gone. The by-id read
// derives the row's UTC day from the id's own 48-bit millisecond and asks for that day only,
// so PostgreSQL prunes to one child — or to none when the day was detached or never created
// — with no application clock and no date prefilter anywhere. The listing is a LIVE keyset
// over `(evaluated_at DESC, id DESC)`, one statement per page, `LIMIT + 1` to learn whether a
// next page exists, the cursor encoded from the last RETURNED row.

// GateListLimitMax caps one listing page (§5: "capped at 200").
const GateListLimitMax = 200

// gateDecisionColumns is the by-id read's SELECT list: every top-level column plus the
// canonical evidence, from which the D7 response is reconstructed as it was.
const gateDecisionColumns = `
	id, service_id, service_slug, service_name, state, action, reasons, evidence,
	policy_revision, window_name, override_id, evaluated_at, sealed_through`

func scanGateDecision(row scannable) (domain.GateDecision, error) {
	var (
		dec               domain.GateDecision
		action            *string
		reasons, evidence []byte
	)
	if err := row.Scan(&dec.DecisionID, &dec.ServiceID, &dec.ServiceSlug, &dec.ServiceName, &dec.State, &action,
		&reasons, &evidence, &dec.PolicyRevision, &dec.Window, &dec.OverrideID, &dec.EvaluatedAt, &dec.SealedThrough); err != nil {
		return domain.GateDecision{}, err
	}
	dec.SchemaVersion = domain.GateDecisionSchemaV1
	dec.EvaluatedAt = dec.EvaluatedAt.UTC()
	if dec.SealedThrough != nil {
		t := dec.SealedThrough.UTC()
		dec.SealedThrough = &t
	}
	if action != nil {
		a := domain.GateAction(*action)
		dec.Action = &a
	}
	if err := json.Unmarshal(reasons, &dec.Reasons); err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: decode gate reasons: %w", err)
	}
	if dec.Reasons == nil {
		dec.Reasons = []domain.GateReasonEntry{}
	}
	if err := json.Unmarshal(evidence, &dec.GateDecisionEvidence); err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: decode gate evidence: %w", err)
	}
	return dec, nil
}

// gateDecisionDay is the UTC day the id names — [day, day + 1) — derived from the id alone.
func gateDecisionDay(decisionID string) (from, to time.Time, ok bool) {
	ms, ok := gateDecisionIDMillis(decisionID)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	t := time.UnixMilli(ms).UTC()
	from = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return from, from.Add(24 * time.Hour), true
}

// gateDecisionByIDSQL is the pruning read: the day bounds first, so the planner keeps exactly
// the child that holds that day (or none), then the id and the tenant.
const gateDecisionByIDSQL = `
	SELECT` + gateDecisionColumns + `
	  FROM service_gate_decisions
	 WHERE evaluated_at >= $2 AND evaluated_at < $3 AND id = $1 AND project_id = $4`

// GetGateDecision reads one decision by id within the project (§5). A malformed id, a foreign
// project, a detached or never-created day and an unknown id all answer ErrNotFound.
func (s *Store) GetGateDecision(ctx context.Context, projectID, decisionID string) (domain.GateDecision, error) {
	from, to, ok := gateDecisionDay(decisionID)
	if !ok {
		return domain.GateDecision{}, ErrNotFound
	}
	dec, err := scanGateDecision(s.pool.QueryRow(ctx, gateDecisionByIDSQL, decisionID, from, to, projectID))
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.GateDecision{}, ErrNotFound
	}
	if err != nil {
		return domain.GateDecision{}, fmt.Errorf("store: read gate decision: %w", err)
	}
	return dec, nil
}

// GateCursor is the listing's opaque keyset: the (evaluated_at µs, id) of the last RETURNED
// item; the next page is bound STRICTLY below it by row comparison.
type GateCursor struct {
	EvaluatedAt time.Time
	ID          string
}

// Encode renders the cursor as URL-safe base64 of "<evaluated_at µs>:<id>".
func (c GateCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.EvaluatedAt.UnixMicro(), 10) + ":" + c.ID))
}

// DecodeGateCursor parses an encoded cursor strictly; anything that is not exactly the shape
// Encode produces is ErrGateCursorInvalid.
func DecodeGateCursor(s string) (GateCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return GateCursor{}, ErrGateCursorInvalid
	}
	us, id, ok := strings.Cut(string(raw), ":")
	if !ok || us == "" || id == "" {
		return GateCursor{}, ErrGateCursorInvalid
	}
	micros, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return GateCursor{}, ErrGateCursorInvalid
	}
	if _, ok := gateDecisionIDMillis(id); !ok {
		return GateCursor{}, ErrGateCursorInvalid
	}
	return GateCursor{EvaluatedAt: time.UnixMicro(micros).UTC(), ID: strings.ToLower(id)}, nil
}

// gateSummaryColumns is the listing's SELECT list — the summary's fields, nothing from the
// evidence.
const gateSummaryColumns = `
	id, service_id, service_slug, service_name, state, action, reasons, policy_revision, override_id, evaluated_at`

// ListGateDecisions is one page of the project's ledger over [from, to) (§5), newest first,
// optionally for one service, bound strictly below the cursor. It issues ONE statement with
// LIMIT limit + 1, returns the first `limit` items and a cursor from the last of THOSE — never
// from the probe row — or a nil cursor on the last page. The handler owns the range and limit
// contract; the store refuses `to <= from` and a limit outside 1..200 defensively. A foreign
// service_id is an empty page, not ErrNotFound: the ledger outlives services.
func (s *Store) ListGateDecisions(
	ctx context.Context, projectID string, from, to time.Time, serviceID *string, cursor *GateCursor, limit int,
) ([]domain.GateDecisionSummary, *GateCursor, error) {
	if !to.After(from) {
		return nil, nil, gateInvalid("range", "to must be after from")
	}
	if limit <= 0 || limit > GateListLimitMax {
		return nil, nil, gateInvalid("limit", "must be between 1 and %d, got %d", GateListLimitMax, limit)
	}
	sql := `SELECT` + gateSummaryColumns + `
	  FROM service_gate_decisions
	 WHERE project_id = $1 AND evaluated_at >= $2 AND evaluated_at < $3`
	args := []any{projectID, from, to}
	if serviceID != nil {
		args = append(args, *serviceID)
		sql += fmt.Sprintf(" AND service_id = $%d", len(args))
	}
	if cursor != nil {
		args = append(args, cursor.EvaluatedAt, cursor.ID)
		sql += fmt.Sprintf(" AND (evaluated_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(" ORDER BY evaluated_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if isInvalidTextRepresentation(err) {
		return []domain.GateDecisionSummary{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list gate decisions: %w", err)
	}
	defer rows.Close()
	items := make([]domain.GateDecisionSummary, 0, limit+1)
	for rows.Next() {
		var (
			it      domain.GateDecisionSummary
			action  *string
			reasons []byte
		)
		if err := rows.Scan(&it.DecisionID, &it.ServiceID, &it.ServiceSlug, &it.ServiceName, &it.State, &action,
			&reasons, &it.PolicyRevision, &it.OverrideID, &it.EvaluatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: scan gate decision summary: %w", err)
		}
		it.SchemaVersion = domain.GateDecisionSchemaV1
		it.EvaluatedAt = it.EvaluatedAt.UTC()
		if action != nil {
			a := domain.GateAction(*action)
			it.Action = &a
		}
		if err := json.Unmarshal(reasons, &it.Reasons); err != nil {
			return nil, nil, fmt.Errorf("store: decode gate reasons: %w", err)
		}
		if it.Reasons == nil {
			it.Reasons = []domain.GateReasonEntry{}
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		if isInvalidTextRepresentation(err) {
			return []domain.GateDecisionSummary{}, nil, nil
		}
		return nil, nil, fmt.Errorf("store: iterate gate decisions: %w", err)
	}
	if len(items) <= limit {
		return items, nil, nil
	}
	items = items[:limit]
	last := items[limit-1]
	return items, &GateCursor{EvaluatedAt: last.EvaluatedAt, ID: last.DecisionID}, nil
}
