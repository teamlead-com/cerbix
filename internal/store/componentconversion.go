package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §15.0/§15.5: converting a status-page component from one source to another, and the
// composite lifecycle that follows once a service expresses what a monitor used to.
//
// A conversion is a customer-visible change of MEANING — the same page line stops reporting one
// thing and starts reporting another — so it is never inferred from an edit. Three properties
// carry that:
//
//   - PREVIEWED: the operator sees the line and the page summary as they WILL read, before and
//     after, and consents to that pair.
//   - REVERSIBLE: the replaced binding stays DORMANT and a manual status is preserved, so the
//     revert restores what was there instead of asking the operator to remember it.
//   - CAS-FENCED: consent is bound to the component's revision AND the page's component
//     generation, because the preview showed a page summary that a neighbour's edit can change.

// ErrComponentConversionStale is returned when the component or its page changed after the
// preview the operator consented to (§15.0, invariant 70). First committer wins; the caller
// re-previews. It is deliberately distinct from ErrConflict: the remedy is "look again", not
// "pick another name".
var ErrComponentConversionStale = errors.New("store: component conversion preview is stale")

// ErrComponentConversionTarget is returned when the requested target cannot back this
// component: it does not exist, it belongs to another tenant, or a monitor/service binding
// would violate the page's project scope.
var ErrComponentConversionTarget = errors.New("store: invalid conversion target for this component")

// ComponentConversionTarget names what the component should render from after the conversion.
// Exactly one binding is meaningful per source; the OTHER one is not cleared — it stays dormant.
type ComponentConversionTarget struct {
	Source    domain.ComponentSource
	ServiceID string // when Source == service
	MonitorID string // when Source == monitor
	// ManualStatus applies when Source == manual and the operator states a status now. Empty
	// keeps whatever manual status the component already carries — which is exactly what makes
	// a revert to manual restore the operator's last statement rather than blanking it.
	ManualStatus domain.ComponentStatus
}

// ComponentConversionPlan is the STRUCTURAL half of a preview: what the row would become, plus
// the two CAS tokens consent is bound to. The rendered half — this component's status and the
// page summary, before and after — is composed by the caller with the SAME resolver the public
// page uses, so a preview cannot disagree with the page it predicts.
type ComponentConversionPlan struct {
	// Current is the stored row.
	Current domain.Component
	// Proposed is the row as it WOULD be, in memory only. Its dormant bindings are already
	// filled in, so a caller rendering it sees exactly what the confirmation would produce.
	Proposed domain.Component
	// Revision and PageGeneration are the CAS tokens to hand back to Confirm.
	Revision       int64
	PageGeneration int64
	// NoOp is true when the target is what the component already renders from. Confirming a
	// no-op is allowed and changes nothing, so a double-clicked button cannot corrupt anything.
	NoOp bool
	// RevertsTo names the source this conversion could later be reverted to WITHOUT the
	// operator re-choosing a binding, or is empty when the replaced source keeps nothing.
	RevertsTo domain.ComponentSource
}

// PreviewComponentConversion validates a target and returns the plan. It takes no locks and
// writes nothing: the fence is the CAS pair, not a stored preview row, so an abandoned preview
// leaves no state to expire and two operators previewing at once block nobody.
func (s *Store) PreviewComponentConversion(
	ctx context.Context, orgID, componentID string, target ComponentConversionTarget,
) (ComponentConversionPlan, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ComponentConversionPlan{}, fmt.Errorf("store: begin preview conversion: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	plan, err := buildConversionPlanTx(ctx, tx, orgID, componentID, target)
	if err != nil {
		return ComponentConversionPlan{}, err
	}
	return plan, tx.Commit(ctx)
}

// ConfirmComponentConversion applies a previewed conversion under both CAS tokens.
//
// The page row is locked FIRST and the component second — the same order CreateComponent takes,
// so a conversion and a create cannot deadlock. Re-validation happens INSIDE the lock: an
// operator's consent is checked against the state it will actually be applied to, never against
// the state the preview read.
func (s *Store) ConfirmComponentConversion(
	ctx context.Context, orgID, componentID string, target ComponentConversionTarget,
	revision, pageGeneration int64, actor GraphActor,
) (domain.Component, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: begin confirm conversion: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var pageID string
	if err := tx.QueryRow(ctx,
		`SELECT status_page_id FROM components WHERE id = $1 AND org_id = $2`,
		componentID, orgID).Scan(&pageID); noRows(err) {
		return domain.Component{}, ErrNotFound
	} else if err != nil {
		return domain.Component{}, fmt.Errorf("store: locate component page: %w", err)
	}
	var gen int64
	if err := tx.QueryRow(ctx,
		`SELECT component_generation FROM status_pages WHERE id = $1 FOR UPDATE`, pageID).Scan(&gen); err != nil {
		return domain.Component{}, fmt.Errorf("store: lock status page: %w", err)
	}
	if gen != pageGeneration {
		return domain.Component{}, fmt.Errorf("%w: page generation %d, expected %d",
			ErrComponentConversionStale, gen, pageGeneration)
	}
	plan, err := buildConversionPlanTx(ctx, tx, orgID, componentID, target)
	if err != nil {
		return domain.Component{}, err
	}
	if plan.Revision != revision {
		return domain.Component{}, fmt.Errorf("%w: component revision %d, expected %d",
			ErrComponentConversionStale, plan.Revision, revision)
	}
	if plan.NoOp {
		// Nothing to write, so nothing to audit: a repeated confirmation is not an act.
		return plan.Current, tx.Commit(ctx)
	}

	p := plan.Proposed
	row := tx.QueryRow(ctx, `
		UPDATE components
		   SET source = $2, source_project = $3, monitor_id = $4, service_id = $5,
		       manual_status = $6, updated_at = now()
		 WHERE id = $1 RETURNING `+componentColumns,
		componentID, string(p.Source), nullableID(p.SourceProject),
		nullableID(p.MonitorID), nullableID(p.ServiceID), p.ManualStatus)
	updated, err := scanComponent(row)
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: apply component conversion: %w", err)
	}
	// Audited in the SAME transaction, naming BOTH ends: "what it stopped saying" is the half a
	// customer would ask about, and the row is the only durable record of it.
	target4 := "component=" + componentID + " page=" + pageID +
		" from=" + string(plan.Current.Source) + " to=" + string(updated.Source) +
		" actor=" + actor.Label
	var actorUserID *string
	if actor.UserID != "" {
		actorUserID = &actor.UserID
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_logs (org_id, actor_user_id, via_token, action, target)
		 VALUES ($1, $2, $3, 'statuspage.component.converted', $4)`,
		orgID, actorUserID, actor.ViaToken, target4); err != nil {
		return domain.Component{}, fmt.Errorf("store: audit component conversion: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Component{}, fmt.Errorf("store: commit component conversion: %w", err)
	}
	return updated, nil
}

// buildConversionPlanTx is the ONE validator, shared by preview and confirm so the two cannot
// judge a target differently — the failure mode where a preview promises what the confirmation
// then refuses.
func buildConversionPlanTx(
	ctx context.Context, tx pgx.Tx, orgID, componentID string, target ComponentConversionTarget,
) (ComponentConversionPlan, error) {
	if !domain.ValidComponentSource(target.Source) {
		return ComponentConversionPlan{}, fmt.Errorf("%w: unknown source %q",
			ErrComponentConversionTarget, target.Source)
	}
	if target.ManualStatus != "" {
		if !target.ManualStatus.Valid() || !target.ManualStatus.Measured() {
			// `no_data` is computed, never typed: an operator saying "no data" is saying nothing,
			// and the render already says it when nothing is measured.
			return ComponentConversionPlan{}, fmt.Errorf("%w: manual status %q cannot be stated",
				ErrComponentConversionTarget, target.ManualStatus)
		}
	}
	row := tx.QueryRow(ctx,
		`SELECT `+componentColumns+` FROM components WHERE id = $1 AND org_id = $2`, componentID, orgID)
	current, err := scanComponent(row)
	if noRows(err) {
		return ComponentConversionPlan{}, ErrNotFound
	}
	if err != nil {
		return ComponentConversionPlan{}, fmt.Errorf("store: read component: %w", err)
	}
	var pageProject *string
	var gen int64
	if err := tx.QueryRow(ctx,
		`SELECT project_id, component_generation FROM status_pages WHERE id = $1`,
		current.StatusPageID).Scan(&pageProject, &gen); err != nil {
		return ComponentConversionPlan{}, fmt.Errorf("store: read component page: %w", err)
	}

	proposed := current
	proposed.Source = target.Source
	switch target.Source {
	case domain.ComponentSourceService:
		id := target.ServiceID
		if id == "" {
			id = current.ServiceID // a revert names no id: the dormant binding IS the target
		}
		proj, err := bindingProjectTx(ctx, tx, `services`, id, orgID)
		if err != nil {
			return ComponentConversionPlan{}, err
		}
		if err := assertBindingInPageScope(pageProject, proj); err != nil {
			return ComponentConversionPlan{}, err
		}
		proposed.ServiceID, proposed.SourceProject = id, proj
	case domain.ComponentSourceMonitor:
		id := target.MonitorID
		if id == "" {
			id = current.MonitorID
		}
		proj, err := bindingProjectTx(ctx, tx, `monitors`, id, orgID)
		if err != nil {
			return ComponentConversionPlan{}, err
		}
		if err := assertBindingInPageScope(pageProject, proj); err != nil {
			return ComponentConversionPlan{}, err
		}
		proposed.MonitorID, proposed.SourceProject = id, proj
	case domain.ComponentSourceManual:
		if target.ManualStatus != "" {
			proposed.ManualStatus = target.ManualStatus
		}
		// Both bindings stay exactly as they are, dormant. `source_project` stays too: it is
		// the project of the BINDINGS, and dropping it would make the revert re-resolve a
		// tenant it already proved.
	}

	// [314] P1-4 — the dormant bindings are part of the row, so they are part of the validation.
	// Validating only the NEW binding let an org-level page convert monitor(project A) → service
	// (project B): the preview came back clean, `source_project` became B, the monitor stayed
	// dormant at A, and the composite FK then failed at CONFIRM as a raw 500. A conversion that
	// cannot be applied must be refused where the operator is still looking at it.
	if err := assertRetainedBindingsSameProjectTx(ctx, tx, orgID, proposed); err != nil {
		return ComponentConversionPlan{}, err
	}

	plan := ComponentConversionPlan{
		Current: current, Proposed: proposed, Revision: current.Revision, PageGeneration: gen,
	}
	plan.NoOp = current.Source == proposed.Source &&
		current.ServiceID == proposed.ServiceID &&
		current.MonitorID == proposed.MonitorID &&
		current.ManualStatus == proposed.ManualStatus
	// What a later revert could restore without re-choosing: the source being replaced, but only
	// when it keeps something to be restored FROM.
	switch current.Source {
	case domain.ComponentSourceService:
		if proposed.Source != domain.ComponentSourceService && current.ServiceID != "" {
			plan.RevertsTo = domain.ComponentSourceService
		}
	case domain.ComponentSourceMonitor:
		if proposed.Source != domain.ComponentSourceMonitor && current.MonitorID != "" {
			plan.RevertsTo = domain.ComponentSourceMonitor
		}
	case domain.ComponentSourceManual:
		// Even an EMPTY manual source is a reversible state ([314] P1-4): "the operator has said
		// nothing", which publishes no_data, is a statement the revert restores exactly. Requiring
		// a stored status here made the most common starting point look irreversible.
		if proposed.Source != domain.ComponentSourceManual {
			plan.RevertsTo = domain.ComponentSourceManual
		}
	}
	return plan, nil
}

// bindingProjectTx resolves a binding's project WITHIN the org, so a crafted id from another
// tenant is ErrComponentConversionTarget rather than a successful cross-tenant bind. The table
// name is a compile-time literal from the two call sites above, never caller input.
func bindingProjectTx(ctx context.Context, tx pgx.Tx, table, id, orgID string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: %s binding requires an id", ErrComponentConversionTarget, table)
	}
	var proj string
	q := `SELECT t.project_id FROM ` + table + ` t JOIN projects p ON p.id = t.project_id
	       WHERE t.id = $1 AND p.org_id = $2`
	err := tx.QueryRow(ctx, q, id, orgID).Scan(&proj)
	if noRows(err) {
		return "", fmt.Errorf("%w: %s %s is not in this organization", ErrComponentConversionTarget, table, id)
	}
	if err != nil {
		return "", fmt.Errorf("store: resolve conversion binding: %w", err)
	}
	return proj, nil
}

// assertBindingInPageScope refuses in Go what the deferred trigger would refuse at COMMIT, so
// the operator gets a named reason instead of a constraint message.
func assertBindingInPageScope(pageProject *string, bindingProject string) error {
	if pageProject != nil && *pageProject != bindingProject {
		return fmt.Errorf("%w: the page is scoped to one project and this binding belongs to another",
			ErrComponentConversionTarget)
	}
	return nil
}

// UpdateComponent writes the operator-editable fields and advances BOTH counters. The source and
// its bindings are deliberately NOT editable here: changing what a page line means goes through
// the previewed conversion above, and an update path that could do it silently would be the way
// around that gate.
func (s *Store) UpdateComponent(ctx context.Context, orgID string, c domain.Component) (domain.Component, error) {
	if err := c.Validate(); err != nil {
		return domain.Component{}, fmt.Errorf("store: invalid component: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: begin update component: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// PAGE FIRST, explicitly ([318] P0-2). Writing the component and letting the AFTER trigger
	// take the page afterwards is component→page, which cycles against the confirm path's
	// page→component and deadlocks for real (`40P01`). The earlier "page-first" claim was false:
	// blocking while another session holds the page does not prove this path had not already
	// locked the component tuple.
	if err := lockComponentPageTx(ctx, tx, c.ID, orgID); err != nil {
		return domain.Component{}, err
	}

	row := tx.QueryRow(ctx, `
		UPDATE components
		   SET name = $3, description = $4, group_name = $5, position = $6,
		       -- A DORMANT manual status is preserved: the edit form of a service-backed
		       -- component does not show it, so accepting the empty value it submits would
		       -- silently destroy what a revert to manual is supposed to restore.
		       manual_status = CASE WHEN source = 'manual' THEN $7 ELSE manual_status END,
		       updated_at = now()
		 WHERE id = $1 AND org_id = $2 RETURNING `+componentColumns,
		c.ID, orgID, c.Name, c.Description, c.GroupName, c.Position, c.ManualStatus)
	updated, err := scanComponent(row)
	if noRows(err) {
		return domain.Component{}, ErrNotFound
	}
	if err != nil {
		return domain.Component{}, fmt.Errorf("store: update component: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Component{}, fmt.Errorf("store: commit update component: %w", err)
	}
	return updated, nil
}

// assertRetainedBindingsSameProjectTx refuses a proposed row whose DORMANT binding belongs to a
// different project than the active one. Keeping a dormant binding is the whole point of the
// design, so the question is never "is one retained" — it is whether both can live under the ONE
// `source_project` column the row has.
//
// The schema would refuse a mismatch anyway; that is what the composite FKs are for. But it would
// refuse it as an opaque constraint error at CONFIRM time, after the operator consented to a
// preview that promised the change would work ([314] P1-4).
func assertRetainedBindingsSameProjectTx(
	ctx context.Context, tx pgx.Tx, orgID string, proposed domain.Component,
) error {
	if proposed.SourceProject == "" {
		if proposed.MonitorID != "" || proposed.ServiceID != "" {
			return fmt.Errorf("%w: a binding is retained with no project", ErrComponentConversionTarget)
		}
		return nil
	}
	// Check the binding that is NOT the active one: the active one's project is what
	// `source_project` was just set from.
	var kind, table, id string
	switch proposed.Source {
	case domain.ComponentSourceService:
		kind, table, id = "monitor", "monitors", proposed.MonitorID
	case domain.ComponentSourceMonitor:
		kind, table, id = "service", "services", proposed.ServiceID
	default:
		// A manual source keeps both, and both were validated when they became active. The one
		// case worth re-checking is a pair that disagrees, which the two lookups below cover.
		if err := assertDormantProject(ctx, tx, orgID, "monitor", "monitors", proposed.MonitorID, proposed.SourceProject); err != nil {
			return err
		}
		return assertDormantProject(ctx, tx, orgID, "service", "services", proposed.ServiceID, proposed.SourceProject)
	}
	return assertDormantProject(ctx, tx, orgID, kind, table, id, proposed.SourceProject)
}

// assertDormantProject verifies one retained id sits in the row's project. An empty id is nothing
// to check; a missing row is a target error rather than a 500, because the operator can act on it.
func assertDormantProject(ctx context.Context, tx pgx.Tx, orgID, kind, table, id, project string) error {
	if id == "" {
		return nil
	}
	got, err := bindingProjectTx(ctx, tx, table, id, orgID)
	if err != nil {
		return err
	}
	if got != project {
		return fmt.Errorf("%w: the %s binding kept for a revert belongs to another project — "+
			"remove it before converting, since a component records ONE project for all its bindings",
			ErrComponentConversionTarget, kind)
	}
	return nil
}

// lockComponentPageTx takes the component's PAGE row before the component itself is touched.
//
// Every structural mutation goes through here so there is ONE order — page, then component — and
// therefore no cycle. The lookup is a plain read: the component row is not locked by it, which is
// the point; locking it first is precisely the order this function exists to prevent.
func lockComponentPageTx(ctx context.Context, tx pgx.Tx, componentID, orgID string) error {
	var pageID string
	err := tx.QueryRow(ctx,
		`SELECT status_page_id FROM components WHERE id = $1 AND org_id = $2`,
		componentID, orgID).Scan(&pageID)
	if noRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: locate component page: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM status_pages WHERE id = $1 FOR UPDATE`, pageID); err != nil {
		return fmt.Errorf("store: lock component page: %w", err)
	}
	return nil
}
