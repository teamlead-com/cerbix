package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.6a — the paging write surface, and the §16.4a lifecycle closes it triggers.
//
// Two writes live here, and they share one shape because they share one hazard: editing the
// declaration is also how a firing announcement can be ended by accident. An operator who turns
// ownership off, narrows `page_on`, disables a burn target or deletes a rule has destroyed the very
// rows an evaluator would need to notice the announcement is over — so the close is enqueued in the
// SAME transaction as the edit, through `closeServiceEpisodesTx`, from the episode's own snapshot.
// There is exactly one closing path in this package and neither function here adds a second.
//
// What each write refuses, and why:
//
//   - a FILE-MANAGED service refuses both (§16.6a: these fields are part of the desired state, so
//     the file owns them and the UI renders them read-only) — the refusal is inside the transaction
//     that holds the service row, so a concurrent apply cannot slip between the check and the write;
//   - a foreign `projectID` is ErrNotFound, never a different error, because "this is not yours" and
//     "this does not exist" must be indistinguishable across tenants;
//   - invalid input is rejected BEFORE anything is written, by the ONE domain validator the API and
//     the MaC apply also call — a second copy of the bounds here is how the two come to disagree.
//
// `alert_config_generation` and `sla_targets.alert_generation` are never touched by this file. The
// database owns them (migration 00082's triggers), because an API PATCH, a MaC apply and a direct
// UPDATE must ALL dis-arm delegation, and "every writer remembers to bump" is the assumption phase 4
// had to remove twice.

// AlertActor identifies who performed a paging-configuration change. It feeds the audit row written
// INSIDE the mutation transaction — the same convention `SecretActor` follows, for the same reason:
// an audit trail written afterwards is one that a crash can silently drop, and §16.6a requires EVERY
// paging-config change to carry its actor and its before/after, not only the moment ownership flips.
type AlertActor struct {
	// ActorUserID is a soft user FK; empty is stored as NULL (a machine actor).
	ActorUserID string
	// ViaToken marks a service-account API-token principal.
	ViaToken bool
}

// ServiceAlertPolicy reads a service's stored paging declaration.
//
// It exists because a PATCH must merge onto what is actually stored: a partial edit whose merge BASE
// was invented is exactly how "leave ownership alone" turns into "disown". The read is tenant-scoped
// and answers a foreign, unknown or malformed id the same way the two writers do — one answer for
// all three, so existence never leaks across a tenant boundary — and it returns the CANONICAL form,
// because the value a caller echoes must be the value the database holds.
//
// Latches, leases and generations are absent from the return type, not filtered out of it: this is
// the declaration, and a read that could carry server-owned state is a read somebody will eventually
// send back as a write.
func (s *Store) ServiceAlertPolicy(
	ctx context.Context, projectID, serviceID string,
) (domain.ServiceAlertPolicy, error) {
	var p domain.ServiceAlertPolicy
	var pageOn []string
	err := s.pool.QueryRow(ctx, `
		SELECT owns_paging, page_on, page_on_unknown, confirm_evaluations
		  FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).
		Scan(&p.OwnsPaging, &pageOn, &p.PageOnUnknown, &p.ConfirmEvaluations)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.ServiceAlertPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: read alert policy: %w", err)
	}
	for _, state := range pageOn {
		p.PageOn = append(p.PageOn, domain.ServiceAlertState(state))
	}
	return p.Canonical(), nil
}

// ServiceAlertPolicyPatch is a PARTIAL declaration: a nil field means "leave this alone".
//
// It exists because the merge has to happen INSIDE the write transaction. An earlier revision read
// the policy, merged in Go and handed the store a full value, which loses a concurrent partial edit:
// two PATCHes that each mention one field both read the same row, and whichever commits second
// writes its stale copy of the field it never mentioned — silently restoring `owns_paging` after an
// explicit disown, or dropping a `confirm_evaluations` change, depending on the order. §16.6a's
// "omitted = unchanged" is a promise about the STORED value at commit time, not about the value the
// caller happened to read a moment earlier.
type ServiceAlertPolicyPatch struct {
	OwnsPaging         *bool
	PageOn             *[]domain.ServiceAlertState
	PageOnUnknown      *bool
	ConfirmEvaluations *int
}

// FullServiceAlertPolicyPatch is every field set, for callers that genuinely declare the whole
// policy (the MaC apply, and any test that means "make it exactly this").
func FullServiceAlertPolicyPatch(p domain.ServiceAlertPolicy) ServiceAlertPolicyPatch {
	states := append([]domain.ServiceAlertState(nil), p.PageOn...)
	return ServiceAlertPolicyPatch{
		OwnsPaging: &p.OwnsPaging, PageOn: &states,
		PageOnUnknown: &p.PageOnUnknown, ConfirmEvaluations: &p.ConfirmEvaluations,
	}
}

// Merged applies the patch to a declaration. Exported because the HTTP layer judges what it can
// judge without the stored row (the states themselves, the confirmation bound) before calling the
// store at all — using this same merge rather than a second one.
func (p ServiceAlertPolicyPatch) Merged(base domain.ServiceAlertPolicy) domain.ServiceAlertPolicy {
	if p.OwnsPaging != nil {
		base.OwnsPaging = *p.OwnsPaging
	}
	if p.PageOn != nil {
		base.PageOn = append([]domain.ServiceAlertState(nil), (*p.PageOn)...)
	}
	if p.PageOnUnknown != nil {
		base.PageOnUnknown = *p.PageOnUnknown
	}
	if p.ConfirmEvaluations != nil {
		base.ConfirmEvaluations = *p.ConfirmEvaluations
	}
	return base
}

// UpdateServiceAlertPolicy writes a service's paging declaration and ends any announcement the new
// declaration no longer covers, atomically.
//
// The closes are decided from the state BEFORE the update, because after it the policy no longer
// describes what was announced:
//
//	owns_paging true → false   every open episode of the service, both signals, `ownership_disabled`
//	page_on / page_on_unknown  the open HEALTH episode, when the state it announced is no longer
//	                           pageable, `policy_changed`
//	confirm_evaluations        NOTHING (§16.4a says so explicitly: confirmation governs the ONSET of
//	                           an announcement, not an announcement that is already open)
//	false → true ownership     NOTHING — there is nothing open to end
//
// It returns the canonical policy that was stored, so a caller can echo exactly what the database
// holds rather than what it was sent.
func (s *Store) UpdateServiceAlertPolicy(
	ctx context.Context, projectID, serviceID string, patch ServiceAlertPolicyPatch, actor AlertActor,
) (domain.ServiceAlertPolicy, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: begin alert policy write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The service row FIRST, tenant-scoped and locked: the ownership check, the before-state the
	// closes are decided from and the write itself must all see one version of this row, and a
	// concurrent file apply has to serialize behind it rather than race it.
	var before domain.ServiceAlertPolicy
	var pageOn []string
	var slug string
	var asOf time.Time
	err = tx.QueryRow(ctx, `
		SELECT owns_paging, page_on, page_on_unknown, confirm_evaluations, slug
		  FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`, serviceID, projectID).
		Scan(&before.OwnsPaging, &pageOn, &before.PageOnUnknown, &before.ConfirmEvaluations, &slug)
	if noRows(err) || isInvalidTextRepresentation(err) {
		// Wrong tenant, unknown id, or an id that is not even a uuid: one answer for all three, so
		// existence never leaks across a tenant boundary.
		return domain.ServiceAlertPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: lock service for alert policy: %w", err)
	}
	// The clock is read AFTER the lock is held, and it is `statement_timestamp()` rather than
	// `now()`. `now()` is transaction-START time: a writer that queued behind an evaluation would
	// stamp its close with an instant BEFORE the episode that evaluation had just opened, and an
	// announcement that ends before it begins is not a record anybody can read.
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: alert policy clock: %w", err)
	}
	for _, state := range pageOn {
		before.PageOn = append(before.PageOn, domain.ServiceAlertState(state))
	}
	// The stored value is canonicalized for the comparison as well, so a re-spelling of the same
	// declaration is not read as a policy change that closes a firing announcement.
	before = before.Canonical()

	if err := assertServiceNotFileManagedTx(ctx, tx, serviceID); err != nil {
		return domain.ServiceAlertPolicy{}, err
	}

	// The MERGE happens HERE, under the lock, onto the row this transaction just read — never onto
	// a value the caller read earlier. Then canonical, then validate, and all of it before any
	// write: the canonical form is what is stored and what the bounds are judged against, so an
	// input that only differs by order or repetition can never be accepted in one spelling and
	// refused in another.
	next := patch.Merged(before).Canonical()
	if err := next.Validate(); err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: %w", err)
	}

	// A declaration identical to the stored one is NOT a write. The trigger would not bump the
	// generation for it either, but an UPDATE still stamps `updated_at` and burns an MVCC row
	// version — and a UI that saves an unchanged form, or a client that PUTs on a timer, would then
	// show a service as freshly edited by nobody. Checked AFTER the lock, so the comparison is
	// against the row this transaction is about to write.
	diff := alertPolicyDiff(before, next)
	if diff == "" {
		return next, tx.Commit(ctx)
	}

	// ── The closes, from the BEFORE state, in this transaction ───────────────────────────────
	switch {
	case before.OwnsPaging && !next.OwnsPaging:
		// Disowning ends EVERYTHING this service has open, on both signals: the burn latches are
		// still armed and the live one is still firing, and after the commit nothing will evaluate
		// either. A close per episode, each from its own recipient snapshot.
		if _, err := closeServiceEpisodesTx(ctx, tx, asOf, serviceID, projectID, slug,
			episodeCloseFilter{}, domain.CloseOwnershipDisabled); err != nil {
			return domain.ServiceAlertPolicy{}, err
		}
	case pagingStatesChanged(before, next):
		// The health episode records the state it announced. If the new policy would not page that
		// state, the operator has withdrawn the statement — which is `policy_changed`, never a
		// recovery: nothing here is evidence about the service.
		//
		// Read under the same lock, so the state judged is the state that is open. The burn signal
		// is deliberately untouched: `page_on` is the LIVE policy and says nothing about a budget.
		var announced string
		err := tx.QueryRow(ctx, `
			SELECT state FROM service_alert_episodes
			 WHERE service_id = $1 AND signal = 'health' AND closed_at IS NULL
			 LIMIT 1`, serviceID).Scan(&announced)
		if err != nil && !noRows(err) {
			return domain.ServiceAlertPolicy{}, fmt.Errorf("store: read open health episode: %w", err)
		}
		if err == nil && !next.Pages(domain.ServiceAlertState(announced)) {
			if _, err := closeServiceEpisodesTx(ctx, tx, asOf, serviceID, projectID, slug,
				episodeCloseFilter{signal: domain.ServiceSignalHealth}, domain.ClosePolicyChanged); err != nil {
				return domain.ServiceAlertPolicy{}, err
			}
		}
	}

	// ── The write. Four columns, and never `alert_config_generation`: the trigger owns it. ───
	if _, err := tx.Exec(ctx, `
		UPDATE services
		   SET owns_paging = $3, page_on = $4, page_on_unknown = $5, confirm_evaluations = $6,
		       updated_at = now()
		 WHERE id = $1 AND project_id = $2`,
		serviceID, projectID, next.OwnsPaging, pageOnText(next.PageOn), next.PageOnUnknown,
		next.ConfirmEvaluations); err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: write alert policy: %w", err)
	}

	// ── The audit, in the SAME transaction, naming only what moved ───────────────────────────
	if err := insertAlertAudit(ctx, tx, projectID, actor, "service.alerting",
		"service="+serviceID+" "+diff); err != nil {
		return domain.ServiceAlertPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ServiceAlertPolicy{}, fmt.Errorf("store: commit alert policy: %w", err)
	}
	return next, nil
}

// SetServiceBurnAlerting writes one service burn target's alerting declaration — the enable switch
// and the rule set — and ends every announcement the new declaration no longer covers.
//
// `window` names the target (`sla_targets` is unique on (service_id, window_name)); a service with
// no target for that window is ErrNotFound, because the objective write path is what creates one and
// enabling burn alerting on an objective nobody declared would page against a number that does not
// exist.
//
// The closes, again from the state BEFORE the write:
//
//	burn_alert_enabled true → false   every open burn episode OF THAT TARGET, `burn_disabled`
//	a rule no longer declared         its open episode, `rule_removed`
//
// And the part that is not tidiness: the latch rows of rules that are no longer declared are
// DELETED. A stale latch keeps its target inside the arming query's "nothing this service owns may
// be blind" half; no evaluator writes it any more, so its lease expires and can never be renewed,
// and burn coverage for the whole service is then dis-armed permanently — the members page for
// themselves forever, which is safe but is not what the operator asked for. When the target is
// disabled outright every latch row of it goes, for the same reason.
func (s *Store) SetServiceBurnAlerting(
	ctx context.Context, projectID, serviceID, window string, enabled bool,
	rules []domain.BurnRule, actor AlertActor,
) error {
	// ONE validator (§16.6a / §16.4b): the bounds AND the canonical-key collision check, which is
	// what keeps a latch from having to answer for two rules. It is not re-implemented here.
	if err := domain.ValidateBurnRules(rules); err != nil {
		return fmt.Errorf("store: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin burn alerting write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The SERVICE row first, exactly as the policy write does: same tenant scoping, same lock, same
	// file-managed refusal. The target is reached THROUGH the service, so a foreign project can
	// never address a target by window name alone.
	var slug string
	var asOf time.Time
	err = tx.QueryRow(ctx,
		`SELECT slug FROM services WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		serviceID, projectID).Scan(&slug)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock service for burn alerting: %w", err)
	}
	if err := assertServiceNotFileManagedTx(ctx, tx, serviceID); err != nil {
		return err
	}

	var targetID string
	var beforeEnabled bool
	var beforeRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT id, burn_alert_enabled, burn_rules
		  FROM sla_targets WHERE service_id = $1 AND window_name = $2 FOR UPDATE`,
		serviceID, window).Scan(&targetID, &beforeEnabled, &beforeRaw)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock burn target: %w", err)
	}
	// The clock, AFTER both rows are held. See the note in UpdateServiceAlertPolicy: a writer that
	// waited behind an evaluation would otherwise close, with a transaction-start timestamp, an
	// episode that evaluation had opened later.
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&asOf); err != nil {
		return fmt.Errorf("store: burn alerting clock: %w", err)
	}
	// The old rules are needed for the audit's before-set and for "did the rules change?" — never
	// for deciding WHICH episodes and latches to end: those are addressed by the keys the rows
	// themselves carry, so a `burn_rules` payload that will not decode degrades the audit line and
	// nothing else.
	var beforeRules []domain.BurnRule
	// nil is the sentinel for "the stored payload would not decode" — rendered as `?` in the audit
	// line rather than as an empty set, which it is not. `burnRuleKeys` never returns nil.
	var beforeKeys []string
	if err := json.Unmarshal(beforeRaw, &beforeRules); err == nil {
		beforeKeys = burnRuleKeys(beforeRules)
	}
	keys := burnRuleKeys(rules)

	// `Firing` is zeroed on the way in. For a SERVICE target the latch is the normalized
	// `service_burn_alert_state` row (§16.4b) and the JSON's copy is read by nothing; storing a
	// caller's value there would leave a second, editable statement about what is currently firing.
	//
	// The array is stored in canonical KEY ORDER, and that is what makes the generation trigger
	// honest: it compares the stored JSON, so without a canonical order a pure reorder would bump
	// the generation, dis-arm the service and page its members for a change nobody made.
	stored := make([]domain.BurnRule, 0, len(rules))
	for _, r := range rules {
		r.Firing = false
		stored = append(stored, r)
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].Key() < stored[j].Key() })
	payload, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("store: marshal burn rules: %w", err)
	}

	// A declaration the row already holds is NOT a write: no closes to make, no latches to prune,
	// nothing to audit, and no `updated_at` stamp for an edit nobody made. Compared as the CANONICAL
	// bytes, after the lock, against what this transaction would otherwise store.
	if enabled == beforeEnabled && string(payload) == canonicalJSON(beforeRaw) {
		return tx.Commit(ctx)
	}

	// ── The closes, from the BEFORE state ────────────────────────────────────────────────────
	switch {
	case beforeEnabled && !enabled:
		// Disabling ends every announcement this target has open. `keepRuleKeys` stays nil: the
		// filter is off, which is what "all of them" means.
		if _, err := closeServiceEpisodesTx(ctx, tx, asOf, serviceID, projectID, slug,
			episodeCloseFilter{signal: domain.ServiceSignalBurn, targetID: targetID},
			domain.CloseBurnDisabled); err != nil {
			return err
		}
	case enabled:
		// Whatever the target still declares SURVIVES; anything else open under it is a rule that
		// no longer exists and its announcement ends. Run whenever the target is enabled rather
		// than only when the rules moved: with identical rules it matches nothing, and with an
		// episode orphaned by some earlier path it is the only thing that would ever close it.
		if _, err := closeServiceEpisodesTx(ctx, tx, asOf, serviceID, projectID, slug,
			episodeCloseFilter{signal: domain.ServiceSignalBurn, targetID: targetID, keepRuleKeys: keys},
			domain.CloseRuleRemoved); err != nil {
			return err
		}
	}

	// ── The latch rows of rules that no longer exist ─────────────────────────────────────────
	//
	// AFTER the close, which needs them to still be there: closing clears the level on the rows it
	// ends, and only then are the undeclared ones removed. A disabled target keeps none at all.
	var keep []string
	if enabled {
		keep = keys
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM service_burn_alert_state
		 WHERE service_id = $1 AND project_id = $2 AND sla_target_id = $3
		   AND ($4::text[] IS NULL OR NOT (rule_key = ANY($4::text[])))`,
		serviceID, projectID, targetID, keep); err != nil {
		return fmt.Errorf("store: prune burn latches: %w", err)
	}

	// ── The write ────────────────────────────────────────────────────────────────────────────
	if _, err := tx.Exec(ctx, `
		UPDATE sla_targets
		   SET burn_alert_enabled = $2, burn_rules = $3::jsonb, updated_at = now()
		 WHERE id = $1`, targetID, enabled, string(payload)); err != nil {
		return fmt.Errorf("store: write burn alerting: %w", err)
	}

	if diff := burnAlertingDiff(beforeEnabled, enabled, beforeKeys, keys); diff != "" {
		if err := insertAlertAudit(ctx, tx, projectID, actor, "service.burn_alerting",
			"service="+serviceID+" window="+window+" "+diff); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit burn alerting: %w", err)
	}
	return nil
}

// pagingStatesChanged reports whether the LIVE pageability declaration moved — `page_on` or
// `page_on_unknown`, and deliberately not `confirm_evaluations`.
//
// Both sides are canonical, so this is a set comparison in disguise. It exists as its own predicate
// because §16.4a's table turns on it: a confirmation edit must be unable to reach the close path at
// all, not merely unlikely to close anything.
func pagingStatesChanged(before, next domain.ServiceAlertPolicy) bool {
	if before.PageOnUnknown != next.PageOnUnknown || len(before.PageOn) != len(next.PageOn) {
		return true
	}
	for i := range before.PageOn {
		if before.PageOn[i] != next.PageOn[i] {
			return true
		}
	}
	return false
}

// pageOnText renders the canonical states for the `text[]` column.
func pageOnText(states []domain.ServiceAlertState) []string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, string(s))
	}
	return out
}

// alertPolicyDiff renders `field:before→after` for the fields that MOVED, and "" when none did.
// Only the changed fields, so an audit reader sees the decision rather than a restatement of the
// whole form; empty means the write was a no-op and gets no audit row, exactly as a no-op edge
// replace writes none.
func alertPolicyDiff(before, next domain.ServiceAlertPolicy) string {
	var parts []string
	if before.OwnsPaging != next.OwnsPaging {
		parts = append(parts, "owns_paging:"+strconv.FormatBool(before.OwnsPaging)+"→"+
			strconv.FormatBool(next.OwnsPaging))
	}
	if !sameStates(before.PageOn, next.PageOn) {
		parts = append(parts, "page_on:"+statesText(before.PageOn)+"→"+statesText(next.PageOn))
	}
	if before.PageOnUnknown != next.PageOnUnknown {
		parts = append(parts, "page_on_unknown:"+strconv.FormatBool(before.PageOnUnknown)+"→"+
			strconv.FormatBool(next.PageOnUnknown))
	}
	if before.ConfirmEvaluations != next.ConfirmEvaluations {
		parts = append(parts, "confirm_evaluations:"+strconv.Itoa(before.ConfirmEvaluations)+"→"+
			strconv.Itoa(next.ConfirmEvaluations))
	}
	return strings.Join(parts, " ")
}

// burnAlertingDiff renders the burn declaration's before/after: the switch, and the RULE KEYS —
// the canonical identity a latch and an episode are stored under, which is what makes "which rule
// did this operator remove?" answerable from the audit log alone. A `burn_rules` payload that would
// not decode reads as `?`, rather than as an empty set it is not.
func burnAlertingDiff(beforeEnabled, enabled bool, beforeKeys, keys []string) string {
	var parts []string
	if beforeEnabled != enabled {
		parts = append(parts, "enabled:"+strconv.FormatBool(beforeEnabled)+"→"+
			strconv.FormatBool(enabled))
	}
	if beforeKeys == nil {
		parts = append(parts, "rules:?→"+keysText(keys))
	} else if !sameKeys(beforeKeys, keys) {
		parts = append(parts, "rules:"+keysText(beforeKeys)+"→"+keysText(keys))
	}
	return strings.Join(parts, " ")
}

func sameStates(a, b []domain.ServiceAlertState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameKeys compares two rule-key sets as SETS: the order rules were authored in is not a
// declaration, and an audit line saying the rules changed when only their order did would train a
// reader to ignore the one that matters.
func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func statesText(states []domain.ServiceAlertState) string {
	return "{" + strings.Join(pageOnText(states), ",") + "}"
}

func keysText(keys []string) string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return "{" + strings.Join(sorted, ",") + "}"
}

// insertAlertAudit writes the audit row inside the caller's transaction, resolving the org from the
// project the same way every other service-scoped audit in this package does.
func insertAlertAudit(
	ctx context.Context, tx pgx.Tx, projectID string, actor AlertActor, action, target string,
) error {
	var actorID *string
	if actor.ActorUserID != "" {
		actorID = &actor.ActorUserID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		SELECT p.org_id, $2, $3, $4, $5 FROM projects p WHERE p.id = $1`,
		projectID, actorID, actor.ViaToken, action, target); err != nil {
		return fmt.Errorf("store: audit %s: %w", action, err)
	}
	return nil
}

// canonicalJSON re-encodes a stored `burn_rules` payload the way the write path encodes it, so the
// no-op comparison is about the DECLARATION and not about whitespace or key order a previous writer
// happened to produce. An undecodable payload compares equal to nothing, which correctly makes the
// write proceed and replace it.
func canonicalJSON(raw []byte) string {
	var rules []domain.BurnRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return ""
	}
	for i := range rules {
		rules[i].Firing = false
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Key() < rules[j].Key() })
	out, err := json.Marshal(rules)
	if err != nil {
		return ""
	}
	return string(out)
}
