package fileprovider

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §16.6a — the `alerting:` declaration in bundle format 2.
//
// The service's fields are indented four spaces and `format2Bundle` ends with the last of
// them, so appending a block is exactly declaring one more field on `checkout`.
func alertingYAML(block string) string { return format2Bundle + block }

// specAlerting is the spec's example, verbatim.
const specAlerting = `    alerting:
      owns_paging: true
      page_on: [down]
      page_on_unknown: false
      confirm_evaluations: 2
`

func alertingOf(t *testing.T, y string) *domain.ServiceAlertPolicy {
	t.Helper()
	return decodeOK(t, y).Services["checkout"].Alerting
}

func hashOf(t *testing.T, y string) string {
	t.Helper()
	return decodeOK(t, y).Services["checkout"].Hash
}

func TestServiceAlertingDeclarationParses(t *testing.T) {
	p := alertingOf(t, alertingYAML(specAlerting))
	if p == nil {
		t.Fatal("the declared `alerting:` block parsed to nil")
	}
	if !p.OwnsPaging || p.PageOnUnknown || p.ConfirmEvaluations != 2 {
		t.Errorf("policy = %+v", *p)
	}
	if len(p.PageOn) != 1 || p.PageOn[0] != domain.ServiceAlertDown {
		t.Errorf("page_on = %v, want [down]", p.PageOn)
	}
	// The parse hands back what the ONE validator accepts, canonical — the same shape the API
	// path stores, so the two cannot drift into different answers for the same declaration.
	if err := p.Validate(); err != nil {
		t.Fatalf("the decoded policy fails the shared validator: %v", err)
	}
}

// A field omitted INSIDE a present block takes the documented §16.6a default. That is a
// statement; the block's absence is not (see TestAbsentAlertingBlockDeclaresNothing).
func TestAlertingFieldDefaultsInsideAPresentBlock(t *testing.T) {
	p := alertingOf(t, alertingYAML("    alerting:\n      owns_paging: true\n"))
	if p == nil {
		t.Fatal("a present block parsed to nil")
	}
	if !p.OwnsPaging || p.PageOnUnknown || p.ConfirmEvaluations != 2 {
		t.Errorf("policy = %+v, want the quiet defaults with ownership on", *p)
	}
	if len(p.PageOn) != 1 || p.PageOn[0] != domain.ServiceAlertDown {
		t.Errorf("page_on = %v, want the default [down]", p.PageOn)
	}
	// An empty block is still a declaration: every default, stated.
	if empty := alertingOf(t, alertingYAML("    alerting: {}\n")); empty == nil {
		t.Error("`alerting: {}` parsed to nil; an empty block is a declaration, not silence")
	}
}

// An unknown key inside `alerting:` is refused exactly like every other unknown key — there is
// no field for it to land in — and the rejection names the FILE it came from, which is the only
// way an operator finds a typo in a directory of bundles.
func TestUnknownAlertingKeyIsRejected(t *testing.T) {
	y := alertingYAML(specAlerting + "      page_on_maybe: true\n")
	be := decodeErr(t, y)
	if be.Reason != ReasonUnknownField {
		t.Fatalf("reason = %s, want unknown_field", be.Reason)
	}

	res := GroupBundles([]Candidate{{Path: "/p/checkout.yaml", RelPath: "checkout.yaml", Data: []byte(y)}}, projectScope())
	if len(res.Errors) != 1 {
		t.Fatalf("%d scan errors, want 1", len(res.Errors))
	}
	if res.Errors[0].RelPath != "checkout.yaml" {
		t.Errorf("error names %q, want the offending file", res.Errors[0].RelPath)
	}
	if res.Errors[0].Err.Reason != ReasonUnknownField {
		t.Errorf("reason = %s, want unknown_field", res.Errors[0].Err.Reason)
	}
	if len(res.Valid) != 0 {
		t.Error("the bundle applied despite an unknown alerting key")
	}
}

// A server-owned field has no place in the declaration at all, so naming one is the same
// rejection: the bundle cannot restate a generation the database owns.
func TestServerOwnedAlertingFieldIsRejected(t *testing.T) {
	be := decodeErr(t, alertingYAML(specAlerting+"      alert_config_generation: 7\n"))
	if be.Reason != ReasonUnknownField {
		t.Errorf("reason = %s, want unknown_field", be.Reason)
	}
}

// The bounds are the domain's, checked at PARSE time — so a bad bundle is refused with its file
// and keeps its last-known-good, rather than being discovered half-applied.
func TestInvalidAlertingIsRefusedAtParseTime(t *testing.T) {
	for _, tc := range []struct{ what, block string }{
		{"confirm_evaluations below the bound", "    alerting:\n      confirm_evaluations: 0\n"},
		{"confirm_evaluations above the bound", "    alerting:\n      confirm_evaluations: 11"},
		{"confirm_evaluations negative", "    alerting:\n      confirm_evaluations: -1\n"},
		{"unknown in page_on", "    alerting:\n      page_on: [down, unknown]\n"},
		{"excluded in page_on", "    alerting:\n      page_on: [excluded]\n"},
		{"a state that does not exist", "    alerting:\n      page_on: [flaky]\n"},
		{"healthy in page_on", "    alerting:\n      page_on: [healthy]\n"},
	} {
		be := decodeErr(t, alertingYAML(tc.block+"\n"))
		if be.Reason != ReasonDomainInvalid {
			t.Errorf("%s: reason = %s, want domain_invalid", tc.what, be.Reason)
		}
		if !strings.Contains(be.Msg, "service alert policy") {
			t.Errorf("%s: msg = %q, want the shared validator's wording", tc.what, be.Msg)
		}
	}
}

// `unknown` has its own switch, and that is the whole reason it cannot be listed in `page_on`.
func TestPageOnUnknownIsItsOwnSwitch(t *testing.T) {
	p := alertingOf(t, alertingYAML("    alerting:\n      page_on_unknown: true\n"))
	if p == nil || !p.PageOnUnknown {
		t.Fatalf("policy = %+v, want page_on_unknown on", p)
	}
}

// Every DECLARED field moves the hash, or a change to it would never reapply.
// The paging declaration is DELIBERATELY OUTSIDE the canonical hash, and this is the test that
// says so on purpose rather than by omission.
//
// The hash decides create/update/no-op, and the update branch creates a definition revision AND
// its evaluation epoch. §16.6a: paging fields bump NEITHER — they change who is paged, not what is
// measured, and an epoch bump would re-segment reliability history for an alerting edit. So the
// apply reconciles paging on every branch against the row itself, which needs no hash to notice a
// change, and this hash stays a statement about the DECLARATION of availability alone.
func TestAlertingIsOutsideTheCanonicalHash(t *testing.T) {
	base := hashOf(t, alertingYAML(specAlerting))
	for what, block := range map[string]string{
		"owns_paging":   strings.Replace(specAlerting, "owns_paging: true", "owns_paging: false", 1),
		"page_on":       strings.Replace(specAlerting, "page_on: [down]", "page_on: [down, degraded]", 1),
		"page_on empty": strings.Replace(specAlerting, "page_on: [down]", "page_on: []", 1),
		"page_on_unknown": strings.Replace(specAlerting,
			"page_on_unknown: false", "page_on_unknown: true", 1),
		"confirm_evaluations": strings.Replace(specAlerting,
			"confirm_evaluations: 2", "confirm_evaluations: 3", 1),
	} {
		if got := hashOf(t, alertingYAML(block)); got != base {
			t.Errorf("changing %s moved the canonical hash: an alerting edit would take the "+
				"declaration branch and create a definition revision and an epoch for it", what)
		}
	}
	// ...including against a bundle that declares no alerting at all: adding the block is not a
	// change to what availability MEANS, so existing bundles do not all reapply on upgrade.
	if hashOf(t, format2Bundle) != base {
		t.Error("declaring `alerting:` moved the canonical hash away from a bundle without it")
	}
}

// An absent block declares NOTHING about paging — it is not the default policy.
//
// The consequence, asserted here at the only place it can be: a service whose declaration
// carried `alerting:` and no longer does parses to a nil policy, which the apply writes nothing
// from. It neither disowns a service that has ownership on nor claims it for one that does not.
// A declaration that says nothing about paging is silence, exactly as a format-1 bundle's
// silence about `services` is not a request to delete them (§15.2).
func TestAbsentAlertingBlockDeclaresNothing(t *testing.T) {
	if p := alertingOf(t, format2Bundle); p != nil {
		t.Fatalf("an absent `alerting:` block parsed to %+v, want nil", *p)
	}
	// Silence and a declaration of the defaults are still different STATEMENTS — the first parses
	// to nil and writes nothing, the second parses to a policy the apply reconciles — even though
	// neither moves the canonical hash, which is about the declaration of availability.
	quiet := "    alerting:\n      owns_paging: false\n      page_on: [down]\n" +
		"      page_on_unknown: false\n      confirm_evaluations: 2\n"
	if p := alertingOf(t, alertingYAML(quiet)); p == nil {
		t.Error("declaring the defaults parsed to nil, which is what SILENCE parses to")
	}
}

// `page_on: []` is legal and is NOT the same as omitting the key: §16.6a calls it "explicitly
// page for no state", which dis-arms LIVE, while an omitted key takes the default {down}.
func TestEmptyPageOnIsADeclaration(t *testing.T) {
	p := alertingOf(t, alertingYAML("    alerting:\n      owns_paging: true\n      page_on: []\n"))
	if p == nil {
		t.Fatal("`page_on: []` parsed to nil")
	}
	if p.PageOn == nil {
		t.Error("page_on is nil; the empty declaration must round-trip to an empty text[], not to NULL")
	}
	if len(p.PageOn) != 0 {
		t.Errorf("page_on = %v, want the empty set", p.PageOn)
	}
	if p.Pages(domain.ServiceAlertDown) {
		t.Error("an empty `page_on` still pages for down")
	}
	omitted := alertingOf(t, alertingYAML("    alerting:\n      owns_paging: true\n"))
	if len(omitted.PageOn) != 1 {
		t.Errorf("omitting page_on gave %v, want the default [down]", omitted.PageOn)
	}
}
