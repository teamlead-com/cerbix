package store

import (
	"context"
	"errors"
	"testing"
)

// FR-021 §16.1/§16.6b — the badge and the delivery gate must be the SAME answer.
//
// This is the whole risk of having a badge at all. A UI that decided "armed" its own way would
// eventually say ARMED while delivery said otherwise, and it would be believed, because a badge is
// what an operator looks at when deciding whether they still need their own alerts. So the read is
// composed from the delegation lookup's own predicate constants, and this table asserts the
// consequence across every dis-arming cause: same service, same instant, same conclusion.
func TestAlertingStateAgreesWithWhatDeliveryDecides(t *testing.T) {
	for _, tc := range []struct {
		name string
		// break makes the service fail exactly one clause.
		breaks   func(t *testing.T, st *Store, ctx context.Context, f armFixture)
		wantLive string // "" means still armed
		wantBurn string
	}{
		{
			name:   "fully armed",
			breaks: func(*testing.T, *Store, context.Context, armFixture) {},
		},
		{
			name: "ownership off",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `UPDATE services SET owns_paging = false WHERE id = $1`, f.serviceID)
			},
			wantLive: AlertReasonNotOwned, wantBurn: AlertReasonNotOwned,
		},
		{
			name: "policy pages nothing",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx,
					`UPDATE services SET page_on = '{}', page_on_unknown = false WHERE id = $1`, f.serviceID)
			},
			// A generation bump rides along with any paging edit, so BURN dis-arms too and says
			// exactly why: its verdicts are for the configuration that was just replaced. That is
			// the safe direction and precisely what the delegation lookup concludes.
			wantLive: AlertReasonPolicyPagesNothing, wantBurn: AlertReasonGenerationChanged,
		},
		{
			name: "never evaluated",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `DELETE FROM service_alert_state`)
			},
			wantLive: AlertReasonNeverEvaluated,
		},
		{
			name: "stale lease",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `UPDATE service_alert_state SET lease_until = now() - interval '1 minute'`)
			},
			wantLive: AlertReasonStaleLease,
		},
		{
			name: "evaluation error",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `UPDATE service_alert_state SET last_error = 'projection failed'`)
			},
			wantLive: AlertReasonEvaluationError,
		},
		{
			name: "unroutable",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `UPDATE notification_channels SET enabled = false WHERE project_id = $1`, f.projectID)
			},
			wantLive: AlertReasonUnroutable, wantBurn: AlertReasonUnroutable,
		},
		{
			name: "burn held",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `UPDATE service_burn_alert_state SET last_verdict = 'hold'`)
			},
			wantBurn: AlertReasonHeld,
		},
		{
			name: "burn target disabled",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `UPDATE sla_targets SET burn_alert_enabled = false WHERE id = $1`, f.targetID)
			},
			wantBurn: AlertReasonNoEnabledTarget,
		},
		{
			name: "a declared rule with no verdict",
			breaks: func(t *testing.T, st *Store, ctx context.Context, f armFixture) {
				exec(t, st, ctx, `DELETE FROM service_burn_alert_state`)
			},
			wantBurn: AlertReasonRuleUnevaluated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := serviceSchemaStore(t)
			f := armedService(t, st, ctx)
			tc.breaks(t, st, ctx, f)

			got, err := st.ServiceAlertingState(ctx, f.projectID, f.serviceID)
			if err != nil {
				t.Fatalf("state: %v", err)
			}
			if got.Live.Armed != (tc.wantLive == "") || got.Live.Reason != tc.wantLive {
				t.Fatalf("live = armed:%v reason:%q, want reason %q",
					got.Live.Armed, got.Live.Reason, tc.wantLive)
			}
			if got.Burn.Armed != (tc.wantBurn == "") || got.Burn.Reason != tc.wantBurn {
				t.Fatalf("burn = armed:%v reason:%q, want reason %q",
					got.Burn.Armed, got.Burn.Reason, tc.wantBurn)
			}

			// The agreement itself: what a member monitor's delivery would conclude.
			if live := delegated(t, st, ctx, f, DelegationLive); live != got.Live.Armed {
				t.Fatalf("the LIVE badge says armed=%v while delivery says %v — a badge that "+
					"disagrees with the gate is worse than no badge, because it is believed",
					got.Live.Armed, live)
			}
			if burn := delegated(t, st, ctx, f, DelegationBurn); burn != got.Burn.Armed {
				t.Fatalf("the BURN badge says armed=%v while delivery says %v", got.Burn.Armed, burn)
			}
		})
	}
}

// Tenant scoping, on the same terms as every other read of this surface.
func TestAlertingStateRefusesForeignAndMalformedIdentifiers(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	f := armedService(t, st, ctx)
	for _, tc := range []struct{ name, project, service string }{
		{"foreign project", f.serviceID, f.serviceID},
		{"malformed project", "not-a-uuid", f.serviceID},
		{"foreign service", f.projectID, f.projectID},
		{"malformed service", f.projectID, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.ServiceAlertingState(ctx, tc.project, tc.service); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s answered %v, want ErrNotFound", tc.name, err)
			}
		})
	}
}

func exec(t *testing.T, st *Store, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
