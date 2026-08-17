package api

import (
	"net/http"
	"sort"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-021 phase 5 (§16.6a): the HTTP write surface of alerting ownership.
//
// Two declarations live here and they are addressed SEPARATELY on purpose:
//
//	PATCH …/services/{id}/alerting                  who gets paged for a LIVE state
//	PUT   …/services/{id}/sla-target/burn-alerting  which budget burns page, per window
//
// They are not one endpoint because they are not one write. The store commits each in its own
// transaction, each enqueues a different family of §16.4a lifecycle closes (`ownership_disabled` /
// `policy_changed` against the health signal, `burn_disabled` / `rule_removed` against one target's
// burn signal), and each carries its own audit action. Folding them into the existing
// `PUT …/sla-target` body would have made an OBJECTIVE edit and a PAGING edit share a request that
// no layer can commit atomically — a half-applied 500 would leave the objective moved and the rules
// not, or the reverse. Keeping the objective write untouched also means the phase-2 contract of
// `{window, objective}` and nothing else survives verbatim.
//
// What NEVER appears in a request body here: `alert_config_generation`, `sla_targets.alert_generation`,
// and every latch/lease/state column (`live_firing`, `emitted_state`, `service_burn_alert_state`, a
// burn rule's `firing`). They are server-owned (§16.6a, §16.4b) and the refusal is STRUCTURAL rather
// than a denylist somebody has to remember to extend: the request types below simply do not declare
// those fields, and `decodeJSONBody` runs `DisallowUnknownFields`, so any of them is a 400. That is
// also why a burn rule is decoded through `burnRuleRequest` instead of `domain.BurnRule` — the
// latter HAS a `Firing` field, so `firing: true` would be a known field the store would silently
// zero, and §16.4b requires it to be refused rather than quietly ignored.

// serviceAlertingView is the DECLARATION, and only the declaration. Latch, lease, generation and
// episode state are absent by SCHEMA — this type has no field for them — so no future edit can leak
// one by forgetting to redact it.
type serviceAlertingView struct {
	OwnsPaging bool `json:"owns_paging"`
	// PageOn is never null: `[]` is a legal declaration meaning "page for no state" (§16.6a), and a
	// client that has to tell `null` from `[]` would be reading a JSON encoder's habit as policy.
	PageOn             []string `json:"page_on"`
	PageOnUnknown      bool     `json:"page_on_unknown"`
	ConfirmEvaluations int      `json:"confirm_evaluations"`
}

func newServiceAlertingView(p domain.ServiceAlertPolicy) serviceAlertingView {
	out := serviceAlertingView{
		OwnsPaging:         p.OwnsPaging,
		PageOnUnknown:      p.PageOnUnknown,
		ConfirmEvaluations: p.ConfirmEvaluations,
		PageOn:             make([]string, 0, len(p.PageOn)),
	}
	for _, s := range p.PageOn {
		out.PageOn = append(out.PageOn, string(s))
	}
	return out
}

// patchServiceAlertingRequest is a PATCH body: every field is a POINTER because "absent" and
// `false` are different statements. §16.6a's table says an omitted field is UNCHANGED, and a plain
// `bool` would read a body that never mentioned `owns_paging` as a request to DISOWN the service —
// closing every open announcement it has, on both signals, with `ownership_disabled`.
//
// `page_on` is `*[]string` for the same reason one level down: `[]` is an explicit declaration
// ("page for no state", which dis-arms LIVE coverage) and must be distinguishable from absence.
// JSON `null` decodes to a nil pointer and therefore reads as absent — the conservative of the two,
// since the alternative would let a null nobody meant retract a declaration.
type patchServiceAlertingRequest struct {
	OwnsPaging         *bool     `json:"owns_paging"`
	PageOn             *[]string `json:"page_on"`
	PageOnUnknown      *bool     `json:"page_on_unknown"`
	ConfirmEvaluations *int      `json:"confirm_evaluations"`
}

// alertActor carries the request's principal into the store, which writes the audit row INSIDE the
// mutation transaction. §16.6a requires EVERY paging-config change to be audited with its actor and
// its before/after — not only the moment ownership is switched on — and an audit written after the
// commit is one a crash can drop. Resolved exactly like `secretActor`: a synthetic API-token id maps
// to a NULL actor with `via_token` carrying the machine attribution, an OIDC client-credentials
// principal keeps its real JIT user uuid.
func (h *Handler) alertActor(r *http.Request) store.AlertActor {
	p, _ := h.principal(r)
	return store.AlertActor{ActorUserID: p.AuditUserID(), ViaToken: p.ViaToken}
}

// readServiceAlertPolicy resolves the stored declaration, or writes the response and reports false.
func (h *Handler) readServiceAlertPolicy(
	w http.ResponseWriter, r *http.Request, projectID, serviceID string,
) (domain.ServiceAlertPolicy, bool) {
	p, err := h.store.ServiceAlertPolicy(r.Context(), projectID, serviceID)
	if h.writeServiceError(w, err) {
		return domain.ServiceAlertPolicy{}, false
	}
	return p, true
}

// getServiceAlerting returns the paging declaration in force.
func (h *Handler) getServiceAlerting(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	p, ok := h.readServiceAlertPolicy(w, r, proj.ID, serviceID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newServiceAlertingView(p))
}

// patchServiceAlerting merges the fields the body actually carried onto the CURRENT declaration and
// writes the result.
//
// The order is deliberate. The body is decoded first (so a server-owned field is a 400 before
// anything is read), the current policy is read next (so a foreign or unknown service is a 404
// before anything is validated), the merge is validated by the ONE domain validator (so an invalid
// declaration is a 400 and the store is never called), and only then is the write attempted. Every
// refusal above therefore leaves the database untouched.
//
// The response is the policy the STORE returned, not the request as sent: `page_on` comes back
// sorted and deduplicated, because the canonical form is what was persisted and what a subsequent
// PATCH will merge onto.
func (h *Handler) patchServiceAlerting(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	var body patchServiceAlertingRequest
	if !decodeJSONBody(w, r, serviceMaxBody, &body) {
		return
	}
	next, ok := h.readServiceAlertPolicy(w, r, proj.ID, serviceID)
	if !ok {
		return
	}
	if body.OwnsPaging != nil {
		next.OwnsPaging = *body.OwnsPaging
	}
	if body.PageOn != nil {
		states := make([]domain.ServiceAlertState, 0, len(*body.PageOn))
		for _, s := range *body.PageOn {
			states = append(states, domain.ServiceAlertState(s))
		}
		next.PageOn = states
	}
	if body.PageOnUnknown != nil {
		next.PageOnUnknown = *body.PageOnUnknown
	}
	if body.ConfirmEvaluations != nil {
		next.ConfirmEvaluations = *body.ConfirmEvaluations
	}
	// The SAME validator the store and the MaC apply call — not a second copy of the bounds, which
	// is how the two come to disagree. Calling it here is what turns "confirm_evaluations: 0" or an
	// `unknown` in `page_on` into a 400 instead of a store error that would land as a 500.
	next = next.Canonical()
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, err := h.store.UpdateServiceAlertPolicy(r.Context(), proj.ID, serviceID, next, h.alertActor(r))
	if h.writeServiceError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, newServiceAlertingView(stored))
}

// burnRuleRequest is a burn rule as a CLIENT may state it: the four DECLARED fields and nothing
// else. `firing` is deliberately missing — see the file header — so a body carrying it is a 400
// from `DisallowUnknownFields` rather than a value the store silently zeroes.
type burnRuleRequest struct {
	LongWindowSeconds  int     `json:"long_window_seconds"`
	ShortWindowSeconds int     `json:"short_window_seconds"`
	Threshold          float64 `json:"threshold"`
	Severity           string  `json:"severity"`
}

// setServiceBurnAlertingRequest is a full REPLACE of one target's burn declaration: `burn_rules` is
// the complete set, and an omitted list declares none. It is a PUT rather than a PATCH because that
// is what the store's write is — the rules that are no longer declared have their episodes closed
// (`rule_removed`) and their latch rows deleted, and a partial body could not say which those are.
type setServiceBurnAlertingRequest struct {
	Window           string            `json:"window"`
	BurnAlertEnabled bool              `json:"burn_alert_enabled"`
	BurnRules        []burnRuleRequest `json:"burn_rules"`
}

type serviceBurnAlertingView struct {
	Window           string            `json:"window"`
	BurnAlertEnabled bool              `json:"burn_alert_enabled"`
	BurnRules        []domain.BurnRule `json:"burn_rules"`
}

// setServiceBurnAlerting declares which error-budget burns page for one service SLA target.
//
// The window names the target; a service with no objective for that window is a 404 from the store,
// because the objective write is what creates one and enabling burn alerting on a number nobody
// declared would page against a threshold that does not exist. That is why this is a sibling of
// `PUT …/sla-target` rather than a field of it: the objective is the prerequisite, and the two stay
// independently addressable.
func (h *Handler) setServiceBurnAlerting(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	serviceID, ok := serviceIDParam(w, r)
	if !ok {
		return
	}
	var body setServiceBurnAlertingRequest
	if !decodeJSONBody(w, r, serviceMaxBody, &body) {
		return
	}
	if body.Window == "" {
		body.Window = "30d"
	}
	if _, valid := sla.WindowByName(body.Window); !valid {
		writeError(w, http.StatusBadRequest, "unknown window")
		return
	}
	rules := make([]domain.BurnRule, 0, len(body.BurnRules))
	for _, rr := range body.BurnRules {
		rules = append(rules, domain.BurnRule{
			LongWindowSeconds:  rr.LongWindowSeconds,
			ShortWindowSeconds: rr.ShortWindowSeconds,
			Threshold:          rr.Threshold,
			Severity:           rr.Severity,
		})
	}
	// One validator again (§16.6a / §16.4b): the per-rule bounds AND the canonical-key collision
	// check, which is what keeps one latch from having to answer for two rules. Run before the
	// store call so a bad rule set is a 400 that writes nothing.
	if err := domain.ValidateBurnRules(rules); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.SetServiceBurnAlerting(r.Context(), proj.ID, serviceID, body.Window,
		body.BurnAlertEnabled, rules, h.alertActor(r)); h.writeServiceError(w, err) {
		return
	}
	// The echo is the CANONICAL stored form: the store persists the array in canonical key order
	// (which is what makes the generation trigger's JSON comparison honest) with `firing` zeroed,
	// so the response is sorted the same way rather than in the order the client happened to type.
	sort.Slice(rules, func(i, j int) bool { return rules[i].Key() < rules[j].Key() })
	writeJSON(w, http.StatusOK, serviceBurnAlertingView{
		Window: body.Window, BurnAlertEnabled: body.BurnAlertEnabled, BurnRules: rules,
	})
}
