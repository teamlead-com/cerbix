package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 — change intelligence's store (func-change-intelligence.md, iter-0165 changeset 2).
//
// This file owns the ONE writer of `service_changes` (RecordChangePhase — the per-identity
// advisory lock, the domain's order and text rules, the identical-replay rule, the decision
// link's validation — D2, D3, D4, D5, D11), the timeline's grouped keyset listing (D6), both
// directions of the incident link (D7: LinkPrecedingChanges at the `opened` delivery,
// ListIncidentChanges for the incident route), and retention by whole group keys (D9). The
// comparison (D8) lives in servicereport.go beside the series owner it extends.

// ChangeListLimitMax caps one timeline page (D6: `limit` 1..200).
const ChangeListLimitMax = 200

// ChangeRangeMax is the widest `[from, to)` the timeline answers (D6: 92 days, a quarter).
const ChangeRangeMax = 92 * 24 * time.Hour

// RecordChangeInput is one phase to record (D2, D3, D5). Every text field is expected to be
// CANONICAL already (the transport normalizes — domain.NormalizeChangeText); the store
// validates through the domain before any SQL and refuses a non-canonical value, so no writer
// reaches the table with un-validated text. MaxPast/MaxFuture are the handler's copies of the
// `change.max_past` / `change.max_future` bounds, checked here against the DATABASE clock read
// inside the write transaction (§5a).
type RecordChangeInput struct {
	ProjectID  string
	ServiceID  string
	Source     string
	ExternalID string
	Kind       domain.ChangeKind
	Phase      domain.ChangePhase
	OccurredAt time.Time
	Ref        string
	URL        string
	DecisionID *string
	// Actor is the request's principal — the immutable label plus the typed pair (D5). The
	// same triple the gate's mutations take.
	Actor     GateActor
	MaxPast   time.Duration
	MaxFuture time.Duration
}

// changeColumns is the one SELECT list every phase read shares.
const changeColumns = `
	c.id, c.project_id, c.service_id, c.source, c.external_id, c.kind, c.phase, c.ref, c.url,
	c.occurred_at, c.decision_id, c.actor_label, c.actor_user_id, c.via_token, c.recorded_at`

func scanChangeRow(row scannable) (domain.ChangePhaseRow, error) {
	var r domain.ChangePhaseRow
	if err := row.Scan(&r.ID, &r.ProjectID, &r.ServiceID, &r.Source, &r.ExternalID, &r.Kind, &r.Phase, &r.Ref, &r.URL,
		&r.OccurredAt, &r.DecisionID, &r.ActorLabel, &r.ActorUserID, &r.ViaToken, &r.RecordedAt); err != nil {
		return domain.ChangePhaseRow{}, err
	}
	r.OccurredAt, r.RecordedAt = r.OccurredAt.UTC(), r.RecordedAt.UTC()
	return r, nil
}

// changePhaseRank orders a group's phases for display and for the order check: `started`
// first, then the terminal; equal ranks fall back to occurred_at then id.
func changePhaseRank(p domain.ChangePhase) int {
	if p == domain.ChangePhaseStarted {
		return 0
	}
	return 1
}

func sortChangePhases(rows []domain.ChangePhaseRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		if ra, rb := changePhaseRank(a.Phase), changePhaseRank(b.Phase); ra != rb {
			return ra < rb
		}
		return a.ID < b.ID
	})
}

// newChangeID is the row id: a UUIDv7 whose 48-bit millisecond is the database instant the row
// is recorded at — the same builder the gate's decision ids use (§5 "v7 from the DB instant").
func newChangeID(recordedAt time.Time) (string, error) { return newGateDecisionID(recordedAt) }

// changeIdentityRowsTx reads every phase of one identity inside the caller's transaction (after
// the identity lock), ordered for the order check.
func changeIdentityRowsTx(ctx context.Context, q dbConn, projectID, serviceID, source, externalID string) ([]domain.ChangePhaseRow, error) {
	rows, err := q.Query(ctx, `
		SELECT`+changeColumns+`
		  FROM service_changes c
		 WHERE c.service_id = $1 AND c.project_id = $2 AND c.source = $3 AND c.external_id = $4`,
		serviceID, projectID, source, externalID)
	if err != nil {
		return nil, fmt.Errorf("store: read change identity: %w", err)
	}
	defer rows.Close()
	var out []domain.ChangePhaseRow
	for rows.Next() {
		r, err := scanChangeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan change row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate change identity: %w", err)
	}
	sortChangePhases(out)
	return out, nil
}

// validateRecordChangeInput is the store's D2 gate: every value about to be written goes
// through the domain's validators FIRST, so a non-canonical value never reaches SQL. The
// decision id is lower-cased for the byte-exact replay comparison; its existence is checked in
// the transaction.
func validateRecordChangeInput(in *RecordChangeInput) error {
	if err := domain.ValidateChangeSource(in.Source); err != nil {
		return err
	}
	if _, err := domain.ValidateChangeExternalID(in.ExternalID); err != nil {
		return err
	}
	if !domain.ValidChangeKind(in.Kind) {
		return domain.NewChangeError(domain.ChangeErrKindInvalid, "kind", "must be one of deploy|rollback|flag, got %q", in.Kind)
	}
	if !domain.ValidChangePhase(in.Phase) {
		return domain.NewChangeError(domain.ChangeErrPhaseInvalid, "phase", "must be one of started|succeeded|failed|cancelled, got %q", in.Phase)
	}
	if _, err := domain.ValidateChangeRef(in.Ref); err != nil {
		return err
	}
	if _, err := domain.ValidateChangeURL(in.URL); err != nil {
		return err
	}
	if in.OccurredAt.IsZero() {
		return domain.NewChangeError(domain.ChangeErrOccurredOutOfBounds, "occurred_at", "is required")
	}
	if in.DecisionID != nil {
		if _, ok := gateDecisionIDMillis(*in.DecisionID); !ok {
			return domain.NewChangeError(domain.ChangeErrDecisionUnknown, "decision_id", "%q is not a decision of this service", *in.DecisionID)
		}
		id := strings.ToLower(*in.DecisionID)
		in.DecisionID = &id
	}
	if in.Actor.Label == "" {
		return errors.New("store: recording a change requires an actor label")
	}
	if in.MaxPast <= 0 || in.MaxFuture < 0 {
		return errors.New("store: recording a change requires the max_past / max_future bounds")
	}
	return nil
}

// RecordChangePhase records one phase of one change (D2–D5, D11), in ONE transaction:
//
//  1. the per-identity advisory lock — pg_advisory_xact_lock(hashtext(service/source/id)) —
//     is taken FIRST, so two pipelines reporting `succeeded` and `failed` for the same run at
//     the same instant cannot both pass the order check (D4); the lock lives as long as the
//     transaction and unrelated identities never queue behind each other;
//  2. the service must exist in the project (else ErrNotFound); `occurred_at` must lie within
//     [now − max_past, now + max_future] on the DATABASE clock read after the lock
//     (`occurred_at_out_of_bounds`);
//  3. the identity's rows are read. A row with the SAME phase decides between an identical
//     replay — kind, occurred_at (µs, UTC), ref, url, decision_id equal — which returns the
//     ORIGINAL row with replayed=true and writes nothing (a retry under another token keeps the
//     original actor and recorded_at), and a differing replay, which is `phase_exists` naming
//     the first differing field;
//  4. the group's kind must match (`kind_mismatch`); the domain's order rule applies
//     (`phase_order`); a terminal must not predate the group's `started`
//     (`occurred_at_before_start`);
//  5. a `decision_id`, when given, must be a ledger row of THIS service in THIS project
//     (`decision_unknown`); it is stored without a foreign key;
//  6. the row is inserted with an id built from the same database instant as recorded_at.
//
// Every text field is validated through the domain BEFORE any SQL (D2): a leading space or a
// U+200B is refused here, never repaired. Recording is not audited (D5).
func (s *Store) RecordChangePhase(ctx context.Context, in RecordChangeInput) (domain.ChangePhaseRow, bool, error) {
	if err := validateRecordChangeInput(&in); err != nil {
		return domain.ChangePhaseRow{}, false, err
	}
	occurred := in.OccurredAt.UTC().Truncate(time.Microsecond)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ChangePhaseRow{}, false, fmt.Errorf("store: begin record change: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// D4: one external identity, one writer at a time. hashtext is stable across sessions and
	// the key is the byte-exact identity (source is a slug; external_id is case-sensitive).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		in.ServiceID+"/"+in.Source+"/"+in.ExternalID); err != nil {
		return domain.ChangePhaseRow{}, false, fmt.Errorf("store: lock change identity: %w", err)
	}
	if err := serviceExistsOn(ctx, tx, in.ProjectID, in.ServiceID); err != nil {
		return domain.ChangePhaseRow{}, false, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return domain.ChangePhaseRow{}, false, err
	}
	if occurred.Before(now.Add(-in.MaxPast)) || occurred.After(now.Add(in.MaxFuture)) {
		return domain.ChangePhaseRow{}, false, domain.NewChangeError(domain.ChangeErrOccurredOutOfBounds, "occurred_at",
			"must be within %s behind and %s ahead of the server's current time %s, got %s",
			in.MaxPast, in.MaxFuture, now.Format(time.RFC3339), occurred.Format(time.RFC3339Nano))
	}

	existing, err := changeIdentityRowsTx(ctx, tx, in.ProjectID, in.ServiceID, in.Source, in.ExternalID)
	if err != nil {
		return domain.ChangePhaseRow{}, false, err
	}
	for _, r := range existing {
		if r.Phase != in.Phase {
			continue
		}
		if field := changeReplayDiffers(r, in, occurred); field != "" {
			return domain.ChangePhaseRow{}, false, domain.NewChangeError(domain.ChangeErrPhaseExists, field,
				"%s is already recorded with a different %s", in.Phase, field)
		}
		// Identical replay (D3): the original row, nothing written, nothing emitted.
		return r, true, nil
	}
	phases := make([]domain.ChangePhase, 0, len(existing))
	for _, r := range existing {
		phases = append(phases, r.Phase)
	}
	if len(existing) > 0 && existing[0].Kind != in.Kind {
		return domain.ChangePhaseRow{}, false, domain.NewChangeError(domain.ChangeErrKindMismatch, "kind",
			"this change is a %s; a %s phase cannot join it", existing[0].Kind, in.Kind)
	}
	if err := domain.ChangePhaseOrder(phases, in.Phase); err != nil {
		return domain.ChangePhaseRow{}, false, err
	}
	if domain.IsTerminalPhase(in.Phase) {
		for _, r := range existing {
			if r.Phase == domain.ChangePhaseStarted && occurred.Before(r.OccurredAt) {
				return domain.ChangePhaseRow{}, false, domain.NewChangeError(domain.ChangeErrOccurredBeforeStart, "occurred_at",
					"%s must not precede started at %s, got %s", in.Phase,
					r.OccurredAt.Format(time.RFC3339Nano), occurred.Format(time.RFC3339Nano))
			}
		}
	}
	if in.DecisionID != nil {
		ok, err := gateDecisionBelongsTx(ctx, tx, in.ProjectID, in.ServiceID, *in.DecisionID)
		if err != nil {
			return domain.ChangePhaseRow{}, false, err
		}
		if !ok {
			return domain.ChangePhaseRow{}, false, domain.NewChangeError(domain.ChangeErrDecisionUnknown, "decision_id",
				"%s is not a decision of this service", *in.DecisionID)
		}
	}

	id, err := newChangeID(now)
	if err != nil {
		return domain.ChangePhaseRow{}, false, err
	}
	row, err := scanChangeRow(tx.QueryRow(ctx, `
		INSERT INTO service_changes AS c
		    (id, project_id, service_id, source, external_id, kind, phase, ref, url,
		     occurred_at, decision_id, actor_label, actor_user_id, via_token, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING`+changeColumns,
		id, in.ProjectID, in.ServiceID, in.Source, in.ExternalID, string(in.Kind), string(in.Phase), in.Ref, in.URL,
		occurred, in.DecisionID, in.Actor.Label, in.Actor.userID(), in.Actor.ViaToken, now))
	if err != nil {
		if isUniqueViolation(err) {
			// Unreachable under the identity lock; kept as the honest answer if it ever is.
			return domain.ChangePhaseRow{}, false, domain.NewChangeError(domain.ChangeErrPhaseExists, "phase", "%s is already recorded", in.Phase)
		}
		return domain.ChangePhaseRow{}, false, fmt.Errorf("store: insert change phase: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ChangePhaseRow{}, false, fmt.Errorf("store: commit record change: %w", err)
	}
	return row, false, nil
}

// changeReplayDiffers compares the client-owned canonical fields of an existing row of the same
// phase with the input (D3 IDENTICAL), in the documented order, and names the first that
// differs — "" when the replay is identical. Server-derived fields take no part.
func changeReplayDiffers(r domain.ChangePhaseRow, in RecordChangeInput, occurred time.Time) string {
	switch {
	case r.Kind != in.Kind:
		return "kind"
	case !r.OccurredAt.Equal(occurred):
		return "occurred_at"
	case r.Ref != in.Ref:
		return "ref"
	case r.URL != in.URL:
		return "url"
	case (r.DecisionID == nil) != (in.DecisionID == nil) ||
		(r.DecisionID != nil && !strings.EqualFold(*r.DecisionID, *in.DecisionID)):
		return "decision_id"
	}
	return ""
}

// gateDecisionBelongsTx answers D11's question — is this id a ledger row of THIS service in
// THIS project — with the by-id read's own pruning (the id's day from its 48 bits), so the
// check touches one partition. A malformed id, a foreign project or service, an aged-out day
// and an unknown id all answer false.
func gateDecisionBelongsTx(ctx context.Context, q dbConn, projectID, serviceID, decisionID string) (bool, error) {
	from, to, ok := gateDecisionDay(decisionID)
	if !ok {
		return false, nil
	}
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM service_gate_decisions
		     WHERE evaluated_at >= $2 AND evaluated_at < $3 AND id = $1 AND project_id = $4 AND service_id = $5)`,
		decisionID, from, to, projectID, serviceID).Scan(&exists)
	if isInvalidTextRepresentation(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check change decision: %w", err)
	}
	return exists, nil
}

// ── The timeline (D6) ────────────────────────────────────────────────────────────────────

// ChangeCursor is the timeline's opaque keyset: the (latest_occurred_at µs, source, external_id)
// of the LAST RETURNED group; the next page is bound STRICTLY below it in the listing order
// `latest_occurred_at DESC, source, external_id`.
type ChangeCursor struct {
	LatestOccurredAt time.Time
	Source           string
	ExternalID       string
}

// Encode renders the cursor as URL-safe base64 of "<µs>:<source>:<external_id>". source is a
// slug and cannot contain ':', so the third field may.
func (c ChangeCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(
		strconv.FormatInt(c.LatestOccurredAt.UnixMicro(), 10) + ":" + c.Source + ":" + c.ExternalID))
}

// DecodeChangeCursor parses an encoded cursor strictly; anything that is not exactly the shape
// Encode produces — including a source that is not a slug or an external_id the domain refuses
// — is `cursor_invalid`.
func DecodeChangeCursor(s string) (ChangeCursor, error) {
	invalid := domain.NewChangeError(domain.ChangeErrCursorInvalid, "cursor", "does not decode")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ChangeCursor{}, invalid
	}
	us, rest, ok := strings.Cut(string(raw), ":")
	if !ok || us == "" {
		return ChangeCursor{}, invalid
	}
	source, externalID, ok := strings.Cut(rest, ":")
	if !ok {
		return ChangeCursor{}, invalid
	}
	micros, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return ChangeCursor{}, invalid
	}
	if domain.ValidateChangeSource(source) != nil {
		return ChangeCursor{}, invalid
	}
	if _, err := domain.ValidateChangeExternalID(externalID); err != nil {
		return ChangeCursor{}, invalid
	}
	return ChangeCursor{LatestOccurredAt: time.UnixMicro(micros).UTC(), Source: source, ExternalID: externalID}, nil
}

// ListChangeGroups is one page of a service's timeline over `[from, to)` (D6): change GROUPS —
// one per external identity — selected by `latest_occurred_at` (the max over the group's
// phases), ordered `latest_occurred_at DESC, source, external_id`, bound strictly below the
// cursor, `LIMIT limit + 1` GROUPS from the grouped subquery, ALL phases of a selected group
// nested (including a `started` that precedes `from`), the decision link read back by id
// (`aged_out` when the ledger row is gone — D11) and the incidents the group preceded (D7).
// `kinds` is a set (OR) and `source` one slug, both applied in the grouped subquery so they
// count BEFORE the limit. Groups, phases, decisions and links come from ONE snapshot. The
// handler owns the range/limit contract; the store refuses `to <= from`, a range over 92 days
// and a limit outside 1..200 defensively. A foreign or unknown service is ErrNotFound: a
// deleted service has no timeline (D10).
func (s *Store) ListChangeGroups(
	ctx context.Context, projectID, serviceID string, from, to time.Time,
	kinds []domain.ChangeKind, source *string, cursor *ChangeCursor, limit int,
) ([]domain.ChangeGroup, *ChangeCursor, error) {
	if !to.After(from) {
		return nil, nil, domain.NewChangeError(domain.ChangeErrRangeInvalid, "range", "to must be after from")
	}
	if to.Sub(from) > ChangeRangeMax {
		return nil, nil, domain.NewChangeError(domain.ChangeErrRangeTooWide, "range", "must span at most %d days, got %s",
			int(ChangeRangeMax.Hours()/24), to.Sub(from))
	}
	if limit <= 0 || limit > ChangeListLimitMax {
		return nil, nil, domain.NewChangeError(domain.ChangeErrLimitInvalid, "limit", "must be between 1 and %d, got %d", ChangeListLimitMax, limit)
	}
	kindSet := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if !domain.ValidChangeKind(k) {
			return nil, nil, domain.NewChangeError(domain.ChangeErrKindInvalid, "kind", "must be one of deploy|rollback|flag, got %q", k)
		}
		kindSet = append(kindSet, string(k))
	}
	if source != nil {
		if err := domain.ValidateChangeSource(*source); err != nil {
			return nil, nil, err
		}
	}

	tx, _, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	if err := serviceExistsOn(ctx, tx, projectID, serviceID); err != nil {
		return nil, nil, err
	}

	sql := `
		SELECT source, external_id, min(kind), max(occurred_at) AS latest
		  FROM service_changes
		 WHERE service_id = $1 AND project_id = $2`
	args := []any{serviceID, projectID}
	if source != nil {
		args = append(args, *source)
		sql += fmt.Sprintf(" AND source = $%d", len(args))
	}
	args = append(args, from, to)
	sql += fmt.Sprintf(`
		 GROUP BY source, external_id
		HAVING max(occurred_at) >= $%d AND max(occurred_at) < $%d`, len(args)-1, len(args))
	if len(kindSet) > 0 {
		args = append(args, kindSet)
		sql += fmt.Sprintf(" AND bool_or(kind = ANY($%d::text[]))", len(args))
	}
	if cursor != nil {
		args = append(args, cursor.LatestOccurredAt, cursor.Source, cursor.ExternalID)
		n := len(args)
		// Strictly below in (latest DESC, source ASC, external_id ASC): an older latest, or the
		// same latest and an identity after the cursor's.
		sql += fmt.Sprintf(" AND (max(occurred_at) < $%d OR (max(occurred_at) = $%d AND (source, external_id) > ($%d, $%d)))",
			n-2, n-2, n-1, n)
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(" ORDER BY latest DESC, source, external_id LIMIT $%d", len(args))

	type groupKey struct {
		source, externalID string
		kind               domain.ChangeKind
		latest             time.Time
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list change groups: %w", err)
	}
	var keys []groupKey
	for rows.Next() {
		var k groupKey
		if err := rows.Scan(&k.source, &k.externalID, &k.kind, &k.latest); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("store: scan change group: %w", err)
		}
		k.latest = k.latest.UTC()
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate change groups: %w", err)
	}
	var next *ChangeCursor
	if len(keys) > limit {
		keys = keys[:limit]
		last := keys[limit-1]
		next = &ChangeCursor{LatestOccurredAt: last.latest, Source: last.source, ExternalID: last.externalID}
	}
	groups := make([]domain.ChangeGroup, 0, len(keys))
	if len(keys) == 0 {
		return groups, nil, tx.Commit(ctx)
	}

	index := make(map[string]int, len(keys))
	sources := make([]string, 0, len(keys))
	externalIDs := make([]string, 0, len(keys))
	for i, k := range keys {
		index[k.source+"\x00"+k.externalID] = i
		sources = append(sources, k.source)
		externalIDs = append(externalIDs, k.externalID)
		groups = append(groups, domain.ChangeGroup{
			Source: k.source, ExternalID: k.externalID, Kind: k.kind, LatestOccurredAt: k.latest,
			Phases: []domain.ChangePhaseRow{}, Incidents: []domain.ChangeIncidentLink{},
		})
	}
	phaseRows, err := tx.Query(ctx, `
		SELECT`+changeColumns+`
		  FROM service_changes c
		  JOIN unnest($3::text[], $4::text[]) AS k(source, external_id)
		    ON k.source = c.source AND k.external_id = c.external_id
		 WHERE c.service_id = $1 AND c.project_id = $2`, serviceID, projectID, sources, externalIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list change phases: %w", err)
	}
	changeIDs := []string{}
	changeGroup := map[string]int{}
	for phaseRows.Next() {
		r, err := scanChangeRow(phaseRows)
		if err != nil {
			phaseRows.Close()
			return nil, nil, fmt.Errorf("store: scan change phase: %w", err)
		}
		i := index[r.Source+"\x00"+r.ExternalID]
		groups[i].Phases = append(groups[i].Phases, r)
		changeIDs = append(changeIDs, r.ID)
		changeGroup[r.ID] = i
	}
	phaseRows.Close()
	if err := phaseRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate change phases: %w", err)
	}
	for i := range groups {
		sortChangePhases(groups[i].Phases)
	}
	if err := attachChangeDecisionsTx(ctx, tx, projectID, groups); err != nil {
		return nil, nil, err
	}
	if err := attachChangeIncidentsTx(ctx, tx, projectID, changeIDs, changeGroup, groups); err != nil {
		return nil, nil, err
	}
	return groups, next, tx.Commit(ctx)
}

// attachChangeDecisionsTx resolves each group's decision link by id (D11): the newest phase
// that carries a decision_id names the group's decision; the ledger row's state/action are
// read back when it still exists, `aged_out` when it does not.
func attachChangeDecisionsTx(ctx context.Context, tx pgx.Tx, projectID string, groups []domain.ChangeGroup) error {
	want := map[string]bool{}
	for i := range groups {
		for j := len(groups[i].Phases) - 1; j >= 0; j-- {
			if id := groups[i].Phases[j].DecisionID; id != nil {
				groups[i].Decision = &domain.ChangeDecisionLink{ID: *id, AgedOut: true}
				want[*id] = true
				break
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, state, action, override_id FROM service_gate_decisions
		 WHERE id = ANY($1::uuid[]) AND project_id = $2`, ids, projectID)
	if err != nil {
		return fmt.Errorf("store: read change decisions: %w", err)
	}
	defer rows.Close()
	live := map[string]domain.ChangeDecisionLink{}
	for rows.Next() {
		var (
			id, state string
			action    *string
			override  *string
		)
		if err := rows.Scan(&id, &state, &action, &override); err != nil {
			return fmt.Errorf("store: scan change decision: %w", err)
		}
		st := domain.GateState(state)
		l := domain.ChangeDecisionLink{ID: id, State: &st, Overridden: override != nil}
		if action != nil {
			a := domain.GateAction(*action)
			l.Action = &a
		}
		live[id] = l
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate change decisions: %w", err)
	}
	for i := range groups {
		if d := groups[i].Decision; d != nil {
			if l, ok := live[d.ID]; ok {
				groups[i].Decision = &l
			}
		}
	}
	return nil
}

// attachChangeIncidentsTx reads the change side of the link table (D7) for every phase row on
// the page and nests each link under its group: the incident, when it opened, the role, the
// lag and the anchored phase id — the same rows the incident route reads.
func attachChangeIncidentsTx(ctx context.Context, tx pgx.Tx, projectID string, changeIDs []string, changeGroup map[string]int, groups []domain.ChangeGroup) error {
	if len(changeIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT ic.change_id, ic.incident_id, i.started_at, ic.role, ic.lag_seconds
		  FROM incident_changes ic
		  JOIN incidents i ON i.id = ic.incident_id AND i.project_id = ic.project_id
		 WHERE ic.change_id = ANY($1::uuid[]) AND ic.project_id = $2
		 ORDER BY i.started_at, ic.incident_id`, changeIDs, projectID)
	if err != nil {
		return fmt.Errorf("store: read change incidents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l domain.ChangeIncidentLink
		if err := rows.Scan(&l.ChangeID, &l.IncidentID, &l.OpenedAt, &l.Role, &l.LagSeconds); err != nil {
			return fmt.Errorf("store: scan change incident: %w", err)
		}
		l.OpenedAt = l.OpenedAt.UTC()
		if i, ok := changeGroup[l.ChangeID]; ok {
			groups[i].Incidents = append(groups[i].Incidents, l)
		}
	}
	return rows.Err()
}

// changeGroupTx reads one identity's phases inside the caller's snapshot as a ChangeGroup —
// the comparison's anchor (D8). ErrNotFound when the identity has no row.
func changeGroupTx(ctx context.Context, q dbConn, projectID, serviceID, source, externalID string) (domain.ChangeGroup, error) {
	rows, err := changeIdentityRowsTx(ctx, q, projectID, serviceID, source, externalID)
	if err != nil {
		return domain.ChangeGroup{}, err
	}
	if len(rows) == 0 {
		return domain.ChangeGroup{}, ErrNotFound
	}
	g := domain.ChangeGroup{Source: source, ExternalID: externalID, Kind: rows[0].Kind, Phases: rows, Incidents: []domain.ChangeIncidentLink{}}
	for _, r := range rows {
		if r.OccurredAt.After(g.LatestOccurredAt) {
			g.LatestOccurredAt = r.OccurredAt
		}
	}
	return g, nil
}

// ── The incident side of the link (D7) ───────────────────────────────────────────────────

// ListIncidentChanges is `GET /incidents/{id}/changes`: the changes that PRECEDED the incident,
// from the same `incident_changes` rows the timeline's `incidents[]` comes from (invariant 9),
// each with its anchored phase row, the copied instant and lag, and the group's CURRENT phases
// read live beside it. Scoped to the caller's project at the link rows (invariant 22); an
// incident that is not in the project is ErrNotFound. `own_service` links first, then
// `upstream`, nearest the open first within a role.
func (s *Store) ListIncidentChanges(ctx context.Context, projectID, incidentID string) ([]domain.IncidentChangeLink, error) {
	tx, _, err := s.beginReportSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; no-op after commit
	var owned bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incidents WHERE id = $1 AND project_id = $2)`, incidentID, projectID).Scan(&owned)
	if isInvalidTextRepresentation(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: incident changes scope: %w", err)
	}
	if !owned {
		return nil, ErrNotFound
	}
	rows, err := tx.Query(ctx, `
		SELECT`+changeColumns+`, ic.role, ic.occurred_at, ic.lag_seconds, ic.computed_at
		  FROM incident_changes ic
		  JOIN service_changes c ON c.id = ic.change_id AND c.project_id = ic.project_id
		 WHERE ic.incident_id = $1 AND ic.project_id = $2
		 ORDER BY ic.role, ic.lag_seconds, c.source, c.external_id`, incidentID, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list incident changes: %w", err)
	}
	out := []domain.IncidentChangeLink{}
	for rows.Next() {
		var l domain.IncidentChangeLink
		if err := rows.Scan(&l.Change.ID, &l.Change.ProjectID, &l.Change.ServiceID, &l.Change.Source, &l.Change.ExternalID,
			&l.Change.Kind, &l.Change.Phase, &l.Change.Ref, &l.Change.URL, &l.Change.OccurredAt, &l.Change.DecisionID,
			&l.Change.ActorLabel, &l.Change.ActorUserID, &l.Change.ViaToken, &l.Change.RecordedAt,
			&l.Role, &l.OccurredAt, &l.LagSeconds, &l.ComputedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan incident change: %w", err)
		}
		l.Change.OccurredAt, l.Change.RecordedAt = l.Change.OccurredAt.UTC(), l.Change.RecordedAt.UTC()
		l.OccurredAt, l.ComputedAt = l.OccurredAt.UTC(), l.ComputedAt.UTC()
		l.Phases = []domain.ChangePhaseRow{}
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate incident changes: %w", err)
	}
	if len(out) == 0 {
		return out, tx.Commit(ctx)
	}
	// The groups' live phases, one statement for every identity on the list.
	services := make([]string, 0, len(out))
	sources := make([]string, 0, len(out))
	externalIDs := make([]string, 0, len(out))
	index := map[string][]int{}
	for i, l := range out {
		services = append(services, l.Change.ServiceID)
		sources = append(sources, l.Change.Source)
		externalIDs = append(externalIDs, l.Change.ExternalID)
		k := l.Change.ServiceID + "\x00" + l.Change.Source + "\x00" + l.Change.ExternalID
		index[k] = append(index[k], i)
	}
	phaseRows, err := tx.Query(ctx, `
		SELECT DISTINCT`+changeColumns+`
		  FROM service_changes c
		  JOIN unnest($2::uuid[], $3::text[], $4::text[]) AS k(service_id, source, external_id)
		    ON k.service_id = c.service_id AND k.source = c.source AND k.external_id = c.external_id
		 WHERE c.project_id = $1`, projectID, services, sources, externalIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list incident change phases: %w", err)
	}
	defer phaseRows.Close()
	for phaseRows.Next() {
		r, err := scanChangeRow(phaseRows)
		if err != nil {
			return nil, fmt.Errorf("store: scan incident change phase: %w", err)
		}
		for _, i := range index[r.ServiceID+"\x00"+r.Source+"\x00"+r.ExternalID] {
			out[i].Phases = append(out[i].Phases, r)
		}
	}
	if err := phaseRows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate incident change phases: %w", err)
	}
	for i := range out {
		sortChangePhases(out[i].Phases)
	}
	return out, tx.Commit(ctx)
}

// ── The correlation write (D7) ───────────────────────────────────────────────────────────

// ChangeCorrelationLink is one link LinkPrecedingChanges wrote.
type ChangeCorrelationLink struct {
	ChangeID   string
	ServiceID  string
	Source     string
	ExternalID string
	Kind       domain.ChangeKind
	Role       string
	OccurredAt time.Time
	LagSeconds int64
}

// ChangeCorrelation is what one correlation attempt did: the links it INSERTED and whether it
// appended the note. A redelivery that finds the note present reports neither.
type ChangeCorrelation struct {
	Links     []ChangeCorrelationLink
	NoteAdded bool
	// Skipped is set when the incident is gone, is not a service incident, or already carries
	// the `🚀 Changes:` note — nothing was written and nothing needed to be.
	Skipped bool
}

// LinkPrecedingChanges is D7's write at a service auto-incident's `opened` delivery, in ONE
// transaction: the incident row is locked; if the `🚀 Changes:` note is already present the
// attempt writes nothing (a retried delivery); otherwise every change GROUP whose latest phase
// KNOWN NOW lies in `[opened_at − window, opened_at]` on the incident's own service
// (`own_service`) and on every service its `incident_service_impacts` rows mark
// `probable_root` (`upstream`) gets one `incident_changes` row anchoring that latest phase —
// its occurred_at and the lag copied, never updated — and the note naming at most `noteMax` of
// them (the rest counted) is appended through the marker guard. Links and note commit
// together or not at all. A change recorded after the open is not back-linked: the window is
// fixed at open. Errors are the CALLER's to absorb (fail-open at the worker, invariant 8).
func (s *Store) LinkPrecedingChanges(ctx context.Context, incidentID string, window time.Duration, noteMax int) (ChangeCorrelation, error) {
	if window <= 0 {
		return ChangeCorrelation{}, errors.New("store: change correlation requires a positive window")
	}
	if noteMax < 1 {
		return ChangeCorrelation{}, domain.NewChangeError(domain.ChangeErrNoteMaxInvalid, "correlation_note_max", "must be at least 1, got %d", noteMax)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: begin change correlation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var (
		projectID, status string
		serviceID         *string
		openedAt          time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT project_id, service_id, started_at, status FROM incidents WHERE id = $1 FOR UPDATE`,
		incidentID).Scan(&projectID, &serviceID, &openedAt, &status)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return ChangeCorrelation{Skipped: true}, nil
	}
	if err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: lock incident for change correlation: %w", err)
	}
	if serviceID == nil {
		return ChangeCorrelation{Skipped: true}, nil
	}
	openedAt = openedAt.UTC()
	var noted bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM incident_updates u
		                WHERE u.incident_id = $1 AND u.author = 'system' AND u.body LIKE $2)`,
		incidentID, domain.ChangesMarker+"%").Scan(&noted); err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: check changes note: %w", err)
	}
	if noted {
		return ChangeCorrelation{Skipped: true}, nil
	}

	// The candidate services and their roles: the own service, then the probable roots.
	roleOf := map[string]string{*serviceID: domain.ChangeLinkRoleOwnService}
	upstream, err := tx.Query(ctx, `
		SELECT service_id FROM incident_service_impacts
		 WHERE incident_id = $1 AND project_id = $2 AND role = 'probable_root'`, incidentID, projectID)
	if err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: read probable roots: %w", err)
	}
	for upstream.Next() {
		var sid string
		if err := upstream.Scan(&sid); err != nil {
			upstream.Close()
			return ChangeCorrelation{}, fmt.Errorf("store: scan probable root: %w", err)
		}
		if _, own := roleOf[sid]; !own {
			roleOf[sid] = domain.ChangeLinkRoleUpstream
		}
	}
	upstream.Close()
	if err := upstream.Err(); err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: iterate probable roots: %w", err)
	}
	services := make([]string, 0, len(roleOf))
	for sid := range roleOf {
		services = append(services, sid)
	}
	sort.Strings(services)

	// The anchors: per group in the window, the latest phase row known now (id DESC breaks a
	// same-instant tie deterministically).
	anchors, err := tx.Query(ctx, `
		WITH g AS (
		    SELECT service_id, source, external_id, max(occurred_at) AS latest
		      FROM service_changes
		     WHERE project_id = $1 AND service_id = ANY($2::uuid[])
		     GROUP BY service_id, source, external_id
		    HAVING max(occurred_at) >= $3 AND max(occurred_at) <= $4)
		SELECT DISTINCT ON (c.service_id, c.source, c.external_id)
		       c.id, c.service_id, c.source, c.external_id, c.kind, c.ref, c.occurred_at
		  FROM g
		  JOIN service_changes c
		    ON c.service_id = g.service_id AND c.source = g.source AND c.external_id = g.external_id
		   AND c.occurred_at = g.latest
		 ORDER BY c.service_id, c.source, c.external_id, c.id DESC`,
		projectID, services, openedAt.Add(-window), openedAt)
	if err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: read preceding changes: %w", err)
	}
	var links []ChangeCorrelationLink
	entries := map[string]domain.ChangeNoteEntry{}
	for anchors.Next() {
		var l ChangeCorrelationLink
		var ref string
		if err := anchors.Scan(&l.ChangeID, &l.ServiceID, &l.Source, &l.ExternalID, &l.Kind, &ref, &l.OccurredAt); err != nil {
			anchors.Close()
			return ChangeCorrelation{}, fmt.Errorf("store: scan preceding change: %w", err)
		}
		l.OccurredAt = l.OccurredAt.UTC()
		l.Role = roleOf[l.ServiceID]
		l.LagSeconds = int64(openedAt.Sub(l.OccurredAt) / time.Second)
		links = append(links, l)
		entries[l.ChangeID] = domain.ChangeNoteEntry{Kind: l.Kind, Ref: ref, Source: l.Source, LagSeconds: l.LagSeconds}
	}
	anchors.Close()
	if err := anchors.Err(); err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: iterate preceding changes: %w", err)
	}
	if len(links) == 0 {
		return ChangeCorrelation{}, nil
	}
	// Nearest the open first; the identity is the deterministic tiebreaker.
	sort.Slice(links, func(i, j int) bool {
		a, b := links[i], links[j]
		if a.LagSeconds != b.LagSeconds {
			return a.LagSeconds < b.LagSeconds
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.ExternalID < b.ExternalID
	})
	for _, l := range links {
		if _, err := tx.Exec(ctx, `
			INSERT INTO incident_changes (incident_id, change_id, project_id, role, occurred_at, lag_seconds)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			incidentID, l.ChangeID, projectID, l.Role, l.OccurredAt, l.LagSeconds); err != nil {
			return ChangeCorrelation{}, fmt.Errorf("store: insert incident change link: %w", err)
		}
	}
	// Test-only fault injection between the links and the note: proves "one transaction or
	// nothing" — a failure here leaves neither. nil in production.
	if s.changeNoteFault != nil {
		if err := s.changeNoteFault(); err != nil {
			return ChangeCorrelation{}, fmt.Errorf("store: insert changes note: %w", err)
		}
	}
	noteEntries := make([]domain.ChangeNoteEntry, 0, len(links))
	for _, l := range links {
		noteEntries = append(noteEntries, entries[l.ChangeID])
	}
	body := domain.RenderChangesNote(noteEntries, len(links), noteMax)
	ct, err := tx.Exec(ctx, `
		INSERT INTO incident_updates (incident_id, status, body, author)
		SELECT $1, $2, $3, 'system'
		 WHERE NOT EXISTS (
		       SELECT 1 FROM incident_updates u
		        WHERE u.incident_id = $1 AND u.author = 'system' AND u.body LIKE $4)`,
		incidentID, status, body, domain.ChangesMarker+"%")
	if err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: insert changes note: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ChangeCorrelation{}, fmt.Errorf("store: commit change correlation: %w", err)
	}
	return ChangeCorrelation{Links: links, NoteAdded: ct.RowsAffected() == 1}, nil
}

// ── Retention (D9) ───────────────────────────────────────────────────────────────────────

// PurgeChangeGroups removes at most `groupsPerBatch` change GROUPS whose latest phase is older
// than cutoff (D9): the group KEYS are selected in a deterministic order
// (latest_occurred_at, service_id, source, external_id) and EVERY phase row of those keys is
// deleted in the same statement, so a group is never split and a young terminal keeps an old
// `started`. `incident_changes` rows follow by cascade. Returns the groups and rows removed;
// the caller repeats until a batch selects fewer than the bound.
func (s *Store) PurgeChangeGroups(ctx context.Context, cutoff time.Time, groupsPerBatch int) (groups, rows int, err error) {
	if groupsPerBatch < 1 {
		return 0, 0, domain.NewChangeError(domain.ChangeErrRetentionBatchInvalid, "retention_groups_per_batch", "must be at least 1, got %d", groupsPerBatch)
	}
	var g, r int64
	err = s.pool.QueryRow(ctx, `
		WITH candidates AS (
		    SELECT DISTINCT service_id, source, external_id
		      FROM service_changes
		     WHERE occurred_at < $1),
		victims AS (
		    SELECT c.service_id, c.source, c.external_id
		      FROM service_changes c
		      JOIN candidates k
		        ON k.service_id = c.service_id AND k.source = c.source AND k.external_id = c.external_id
		     GROUP BY c.service_id, c.source, c.external_id
		    HAVING max(c.occurred_at) < $1
		     ORDER BY max(c.occurred_at), c.service_id, c.source, c.external_id
		     LIMIT $2),
		del AS (
		    DELETE FROM service_changes c
		     USING victims v
		     WHERE c.service_id = v.service_id AND c.source = v.source AND c.external_id = v.external_id
		    RETURNING 1)
		SELECT (SELECT count(*) FROM victims), (SELECT count(*) FROM del)`,
		cutoff.UTC(), groupsPerBatch).Scan(&g, &r)
	if err != nil {
		return 0, 0, fmt.Errorf("store: purge change groups: %w", err)
	}
	return int(g), int(r), nil
}
