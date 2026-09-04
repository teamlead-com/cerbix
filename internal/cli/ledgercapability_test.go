package cli

import "testing"

// FR-032: the generation-4 announcement follows the ROLE, never the envelope capability.
//
// Tying it to secrets left an envelope-disabled deployment — the default, since
// `secrets.enabled: false` is meant to change nothing else — with no ledger-capable worker, so the
// region never became eligible and the ordinary secretless monitor FR-032 exists for could not be
// scheduled onto the carrier at all.
func TestTheLedgerAnnouncementFollowsTheRoleAndNotTheEnvelopeCapability(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{
		{role: "worker", want: 1},
		{role: "all", want: 0},
		{role: "api", want: 0},
		{role: "scheduler", want: 0},
		{role: "agent", want: 0},
	} {
		t.Run(tc.role, func(t *testing.T) {
			if got := executorLedgerCapability(tc.role); got != tc.want {
				t.Fatalf("executorLedgerCapability(%q) = %d, want %d", tc.role, got, tc.want)
			}
		})
	}
}

// The point of the previous test stated as the deployment it protects: envelope enforcement OFF
// must still produce an announcing worker. The function takes no envelope argument precisely so
// that this cannot regress by someone threading one back in.
func TestAnEnvelopeDisabledDeploymentStillAnnouncesTheLedgerCarrier(t *testing.T) {
	if got := executorLedgerCapability("worker"); got != 1 {
		t.Fatalf("a worker announces %d with no envelope capability in play; "+
			"LiveLedgerJobRegions would stay false and FR-032 could schedule nothing", got)
	}
}
