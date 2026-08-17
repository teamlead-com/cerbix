package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §15.5, invariant 74 — converting a composite monitor into a Service.
//
// A composite already answers "is this thing up", so the conversion is mostly a change of
// vocabulary: its children become the service's SLI members and its aggregation mode becomes the
// service's aggregation policy. What makes it delicate is that the two vocabularies are not the
// same size. A composite is binary (up / down) and flat over its children; a service aggregates
// WITHIN a region and then ACROSS regions, and distinguishes degraded and unknown from down.
//
// The translation is therefore exact where it can be and REFUSES where it cannot, because a
// conversion that quietly changes what "down" means for a page a customer reads is worse than no
// conversion tool at all.

// ErrCompositeNotComposite is returned when the target monitor is not a composite.
var ErrCompositeNotComposite = errors.New("store: monitor is not a composite")

// ErrCompositeQuorumNotTranslatable is returned when a quorum composite's children span more
// than one region. A flat "M of N children voted down" is not expressible as the service model's
// per-region quorum followed by a region rollup, and inventing thresholds that merely look
// similar would silently redefine availability. The operator declares the policy explicitly
// instead — the service can still be built by hand, and §15.5 already says retire is available
// for any composite, converted or not.
var ErrCompositeQuorumNotTranslatable = errors.New(
	"store: a quorum composite with children in more than one region cannot be translated automatically")

// ErrCompositeChildMissing is returned when a declared child of the composite no longer exists.
// Converting on the survivors would silently change what the aggregation MEANS — a 3-child `all`
// becomes a 2-child `all`, and a quorum's threshold moves relative to a different N. The operator
// either fixes the composite or states a different intent.
var ErrCompositeChildMissing = errors.New("store: a declared child of this composite no longer exists")

// ErrCompositeSLIRequired is returned when the caller did not state which children become the
// service's reliability inputs. §15.5 is explicit: `requires: explicit confirmation of sli[] —
// never silently "all children"`. Defaulting here would make that consent impossible at every
// caller, which is the same as not having it.
var ErrCompositeSLIRequired = errors.New("store: converting a composite requires an explicit sli selection")

// ErrCompositeSLINotAChild is returned when the stated SLI names something that is not a live
// child of this composite.
var ErrCompositeSLINotAChild = errors.New("store: every sli member must be a live child of the composite")

// ErrServiceSlugTaken is returned when the derived service slug already exists. It names the
// existing slug rather than silently creating a suffixed twin — a second "checkout-2" service is
// exactly the kind of near-duplicate nobody notices until two pages disagree.
var ErrServiceSlugTaken = errors.New("store: a service with this slug already exists")

// CompositeConversion is the result of converting a composite.
type CompositeConversion struct {
	Service  domain.Service
	Revision domain.DefinitionRevision
	Epoch    domain.EvaluationEpoch
	// Monitor is the composite AFTER the link was written.
	Monitor domain.Monitor
	// AlreadyConverted is true when the composite was already linked to a service and this call
	// returned that service unchanged.
	AlreadyConverted bool
}

// ConvertCompositeToService creates a service that expresses what the composite expresses, links
// the two, and audits the act — in ONE serialized transaction.
//
// The composite row is taken FOR UPDATE FIRST, so two simultaneous confirms cannot both pass the
// "not yet converted" check and create two services with last-writer-wins on the link column.
// Re-confirming an already-converted composite is an idempotent no-op that returns the existing
// service, keyed on the link column itself rather than on a separate marker that could disagree
// with it.
//
// The composite keeps running throughout: this call does not disable, retire or hide it. Retiring
// is the operator's separate, explicit act (§15.5).
// `sli` is the operator's explicit selection of which live children become the service's
// reliability inputs. It is a REQUIRED argument with no default: the children are always the
// operational context, but what availability MEANS is a declaration, and §15.5 requires it to be
// confirmed rather than inferred.
func (s *Store) ConvertCompositeToService(
	ctx context.Context, projectID, monitorID string, sli []string, actor GraphActor,
) (CompositeConversion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompositeConversion{}, fmt.Errorf("store: begin composite conversion: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// §15.4 lock order: the membership advisory lock is OUTERMOST, before any row. This call
	// writes a declaration, so it has to serialize with every other membership write in the
	// project — including the maintenance-preview staleness check that compares member sets.
	if err := lockServiceMembership(ctx, tx, projectID); err != nil {
		return CompositeConversion{}, err
	}
	row := tx.QueryRow(ctx,
		`SELECT `+monitorColumns+` FROM monitors WHERE id = $1 AND project_id = $2 FOR UPDATE`,
		monitorID, projectID)
	composite, err := s.scanMonitorNoSecrets(row)
	if noRows(err) {
		return CompositeConversion{}, ErrNotFound
	}
	if err != nil {
		return CompositeConversion{}, fmt.Errorf("store: lock composite: %w", err)
	}
	if composite.Type != domain.MonitorComposite {
		return CompositeConversion{}, fmt.Errorf("%w: %s is a %s monitor",
			ErrCompositeNotComposite, composite.Name, composite.Type)
	}
	// Idempotent on the LINK, not on a separate marker: the link is the fact, so a re-confirm
	// cannot disagree with it.
	if composite.SupersededByServiceID != "" {
		svc, err := s.getServiceTx(ctx, tx, projectID, composite.SupersededByServiceID)
		if err != nil {
			return CompositeConversion{}, err
		}
		return CompositeConversion{Service: svc, Monitor: composite, AlreadyConverted: true},
			tx.Commit(ctx)
	}
	// A file-owned composite's declaration belongs to its provider (§15.1): converting it here
	// would create a UI-owned service the next reconcile knows nothing about.
	if err := assertNotFileManagedTx(ctx, tx, monitorID); err != nil {
		return CompositeConversion{}, err
	}

	children := composite.ChildIDs()
	if len(children) == 0 {
		return CompositeConversion{}, fmt.Errorf("%w: the composite declares no children",
			ErrCompositeNotComposite)
	}
	live, regions, err := liveChildrenTx(ctx, tx, projectID, children)
	if err != nil {
		return CompositeConversion{}, err
	}
	// PARTIAL loss is refused, not silently converted ([314] P1-5). A composite can outlive a
	// deleted child, and converting on the survivors would move the aggregation's meaning without
	// anybody stating it: `all` over 2 is not `all` over 3, and a quorum threshold M is defined
	// against a specific N. Naming the missing ids is what makes this fixable.
	if len(live) != len(children) {
		return CompositeConversion{}, fmt.Errorf("%w: %s (declared %d, live %d)",
			ErrCompositeChildMissing, strings.Join(missingIDs(children, live), ", "),
			len(children), len(live))
	}

	// The SLI is the operator's statement, validated against the live children.
	chosen := dedupe(sli)
	if len(chosen) == 0 {
		return CompositeConversion{}, fmt.Errorf("%w: choose from %d live children",
			ErrCompositeSLIRequired, len(live))
	}
	liveSet := make(map[string]bool, len(live))
	for _, id := range live {
		liveSet[id] = true
	}
	for _, id := range chosen {
		if !liveSet[id] {
			return CompositeConversion{}, fmt.Errorf("%w: %s", ErrCompositeSLINotAChild, id)
		}
	}
	// The POLICY is derived from the SLI, not from the child count: the aggregation describes what
	// the reliability inputs mean, so a partial selection with a total-count threshold would state
	// a rule about members it does not measure.
	policies, err := compositePolicies(composite, len(chosen), regions)
	if err != nil {
		return CompositeConversion{}, err
	}

	slug := deriveServiceSlug(composite.Slug, composite.Name)
	var existing string
	err = tx.QueryRow(ctx,
		`SELECT slug FROM services WHERE project_id = $1 AND slug = $2`, projectID, slug).Scan(&existing)
	switch {
	case err == nil:
		return CompositeConversion{}, fmt.Errorf("%w: %q", ErrServiceSlugTaken, existing)
	case !noRows(err):
		return CompositeConversion{}, fmt.Errorf("store: check service slug: %w", err)
	}
	// The SHARED creator, not a second INSERT: it owns the project cap, the routing-owner
	// checks in §15.4 order, and the COALESCE that keeps nullable owner ids scannable. A private
	// insert here would be the copy that drifts.
	svc, err := createServiceCoreTx(ctx, s, tx, domain.Service{
		ProjectID: projectID, Slug: slug, Name: composite.Name,
		Description: "Converted from the composite monitor " + composite.Name + ".",
	})
	if err != nil {
		return CompositeConversion{}, err
	}

	// Every live child is the operational CONTEXT — that is what the composite watched — while the
	// SLI is only what the operator selected. The two lists are declared separately for exactly
	// this reason: keeping a diagnostic visible must not change the number.
	rev, epoch, err := s.putServiceDeclarationTx(ctx, tx, projectID, svc.ID, live, chosen, policies, 0,
		DeclarationOptions{CreatedBy: actor.Label})
	if err != nil {
		return CompositeConversion{}, err
	}
	linkRow := tx.QueryRow(ctx,
		`UPDATE monitors SET superseded_by_service_id = $3, updated_at = now()
		  WHERE id = $1 AND project_id = $2 RETURNING `+monitorColumns,
		monitorID, projectID, svc.ID)
	linked, err := s.scanMonitorNoSecrets(linkRow)
	if err != nil {
		return CompositeConversion{}, fmt.Errorf("store: link composite to service: %w", err)
	}
	if err := auditProjectActTx(ctx, tx, projectID, "monitor.converted_to_service",
		fmt.Sprintf("monitor=%s service=%s context=%d sli=%d actor=%s",
			monitorID, svc.ID, len(live), len(chosen), actor.Label), actor); err != nil {
		return CompositeConversion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompositeConversion{}, fmt.Errorf("store: commit composite conversion: %w", err)
	}
	return CompositeConversion{Service: svc, Revision: rev, Epoch: epoch, Monitor: linked}, nil
}

// liveChildrenTx returns the children that still exist in the project, id-ascending and locked
// FOR KEY SHARE, plus their region distribution. Ascending order is the project-wide monitor lock
// order — the same one putServiceDeclarationTx takes, so the two cannot deadlock.
func liveChildrenTx(ctx context.Context, tx pgx.Tx, projectID string, children []string) ([]string, map[string]int, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, region FROM monitors WHERE id = ANY($1) AND project_id = $2 ORDER BY id FOR KEY SHARE`,
		children, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: resolve composite children: %w", err)
	}
	defer rows.Close()
	var live []string
	regions := map[string]int{}
	for rows.Next() {
		var id, region string
		if err := rows.Scan(&id, &region); err != nil {
			return nil, nil, fmt.Errorf("store: scan composite child: %w", err)
		}
		live = append(live, id)
		regions[region]++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return live, regions, nil
}

// compositePolicies translates a composite's aggregation into service policies.
//
//	all    → every member good              → AggAll  + all regions good
//	any    → at least one member good        → AggAny  + any region good
//	quorum → down when >= M members not good → AggQuorum with degraded_min = n - M + 1
//
// The quorum arithmetic is the whole reason this function exists: the composite states the
// threshold as a DOWN-vote count and the service states it as a minimum GOOD count, so a
// transcription would invert the meaning. It is only exact while every child sits in ONE region,
// which is why the multi-region case refuses instead of approximating.
func compositePolicies(composite domain.Monitor, members int, regions map[string]int) (domain.ServicePolicies, error) {
	var p domain.ServicePolicies
	switch composite.CompositeMode() {
	case "any":
		p.Aggregation.Mode = domain.AggAny
		p.Region.Mode = domain.RegionAny
	case "quorum":
		if len(regions) > 1 {
			return p, fmt.Errorf("%w: children span %d regions", ErrCompositeQuorumNotTranslatable, len(regions))
		}
		m := composite.CompositeQuorum()
		if m < 1 || m > members {
			return p, fmt.Errorf("%w: quorum %d is outside 1..%d",
				ErrCompositeQuorumNotTranslatable, m, members)
		}
		p.Aggregation.Mode = domain.AggQuorum
		p.Aggregation.DegradedMin = members - m + 1
		// HealthyMin == DegradedMin: the EXACT binary mapping ([316]). A composite has two states,
		// so `HealthyMin = members` would have added a degraded band the composite never had —
		// reporting more than it did, on a page a customer reads, without anybody asking for it.
		// Widening the vocabulary is an owner decision with a preview that shows the delta, not a
		// side effect of a conversion.
		p.Aggregation.HealthyMin = p.Aggregation.DegradedMin
		p.Region.Mode = domain.RegionAll
	default: // "all"
		p.Aggregation.Mode = domain.AggAll
		p.Region.Mode = domain.RegionAll
	}
	return p, nil
}

// deriveServiceSlug reuses the composite's slug, falling back to a slugified name when the
// monitor has none. It never suffixes to avoid a collision — the caller returns a 409 naming the
// existing slug instead.
func deriveServiceSlug(monitorSlug, name string) string {
	if s := strings.TrimSpace(monitorSlug); s != "" {
		return s
	}
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// getServiceTx reads a service inside an open transaction.
func (s *Store) getServiceTx(ctx context.Context, tx pgx.Tx, projectID, id string) (domain.Service, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, project_id, slug, name, description,
		       COALESCE(escalation_policy_id::text,''), COALESCE(oncall_schedule_id::text,''),
		       created_at, updated_at
		  FROM services WHERE id = $1 AND project_id = $2`, id, projectID)
	return scanService(row)
}

// ListMonitorsSupersededBy returns the composites a service supersedes, slug-ordered — the OTHER
// end of the one stored link (§15.5). There is no second column to keep in step: both ends read
// `monitors.superseded_by_service_id`.
func (s *Store) ListMonitorsSupersededBy(ctx context.Context, projectID, serviceID string) ([]domain.Monitor, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+monitorColumns+` FROM monitors
		  WHERE project_id = $1 AND superseded_by_service_id = $2 ORDER BY slug, name`,
		projectID, serviceID)
	if err != nil {
		return nil, fmt.Errorf("store: list superseded monitors: %w", err)
	}
	defer rows.Close()
	out := []domain.Monitor{}
	for rows.Next() {
		m, err := s.scanMonitorNoSecrets(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan superseded monitor: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// missingIDs returns the declared children that no longer exist, in declaration order.
func missingIDs(declared, live []string) []string {
	have := make(map[string]bool, len(live))
	for _, id := range live {
		have[id] = true
	}
	var out []string
	for _, id := range declared {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}
