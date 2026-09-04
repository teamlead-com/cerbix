package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-032 invariant 10k — with `ledger.carrier_enabled` false, NOTHING stamps generation 4.
//
// Asserted on `lead()`'s resolved carrier map, which reaches the materializer and is captured by
// the fake as `carrierPolicies`, rather than once per transport: all three transports read what
// the producer stamped, so one assertion at the source covers three sinks while three sink-side
// assertions would still miss the producer.

// announcingLedgerRegions announces generation 4 for every region it holds. It exists so the
// inertness proof faces the hard case: an executor that HAS announced, with the flag still off.
type announcingLedgerRegions map[string]bool

func (a announcingLedgerRegions) LiveCredentialJobRegions(context.Context) (map[string]bool, error) {
	return a, nil
}

func (a announcingLedgerRegions) LiveCredentialV3JobRegions(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (a announcingLedgerRegions) LiveLedgerJobRegions(context.Context) (map[string]bool, error) {
	return a, nil
}

func (a announcingLedgerRegions) LiveCanaryJobRegions(context.Context) (map[string][]string, error) {
	return map[string][]string{}, nil
}

func highestCarrier(t *testing.T, fs *fakeStore) int {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	highest := 0
	for _, policy := range fs.carrierPolicies {
		for _, generation := range policy {
			if generation > highest {
				highest = generation
			}
		}
	}
	return highest
}

// The three execution contexts §16.1 distinguishes, each with the flag off and an executor that
// HAS announced generation 4. The map must stay at or below 3 in every one of them.
func TestTheFalseLedgerFlagStampsNothingAboveThreeOnAnyTransport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pullRegions []string
	}{
		{name: "amqp-region", pullRegions: nil},
		{name: "pull-region", pullRegions: []string{"edge"}},
		{name: "role-all-in-process", pullRegions: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := domain.Monitor{
				ID: "ledger-monitor", Type: domain.MonitorPostgres, Target: "db:5432",
				Region: "edge", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
			}
			fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
			s := New(fs, dispatch.NewInProc(2), testLogger()).
				WithCredentialEnvelopes(true).
				WithCredentialLiveRegions(announcingLedgerRegions{"edge": true})
			// WithLedgerCarrier is deliberately NOT called: absent and false must be
			// indistinguishable downstream, which is the other half of the config contract.
			if tc.pullRegions != nil {
				s = s.WithPullRegions(tc.pullRegions)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go s.Run(ctx)
			waitUntil(t, 3*time.Second, "a carrier policy to be resolved", func() bool {
				fs.mu.Lock()
				defer fs.mu.Unlock()
				return len(fs.carrierPolicies) > 0
			})
			if got := highestCarrier(t, fs); got > dispatch.ProtocolV3 {
				t.Fatalf("the resolved carrier map stamped generation %d with the ledger flag off "+
					"and an ANNOUNCING executor; announcement alone must not promote a region", got)
			}
		})
	}
}

// The converse, without which the test above cannot distinguish an inert flag from a scheduler
// that never raises anything: with the flag ON and the same announcement, the map reaches 4.
// B1 cannot produce this state — its config refuses `true` — so the flag is set directly here,
// which is exactly why the config refusal is tested separately in internal/config.
func TestTheLedgerFlagOnRaisesTheAnnouncedRegion(t *testing.T) {
	monitor := domain.Monitor{
		ID: "ledger-monitor", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "edge", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
	s := New(fs, dispatch.NewInProc(2), testLogger()).
		WithCredentialEnvelopes(true).
		WithLedgerCarrier(true).
		WithCredentialLiveRegions(announcingLedgerRegions{"edge": true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.Run(ctx)
	waitUntil(t, 3*time.Second, "generation 4 to be resolved", func() bool {
		return highestCarrier(t, fs) >= dispatch.ProtocolV4
	})
}

// The PULL half of the announcement is a different question with a different source: an AGENT
// declares `capabilities->>'ledger'` on its heartbeat. With the flag on, that promotes the region;
// with it off, nothing does — the same proof as the AMQP half, asked of the other transport.
func TestAPullRegionIsRaisedOnlyByItsAgentsAndOnlyWithTheFlagOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
		want int
	}{
		{name: "flag-off-announced-agent-stays-at-three", flag: false, want: dispatch.ProtocolV3},
		{name: "flag-on-announced-agent-is-raised", flag: true, want: dispatch.ProtocolV4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := domain.Monitor{
				ID: "ledger-monitor", Type: domain.MonitorPostgres, Target: "db:5432",
				Region: "edge", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
			}
			fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor},
				ledgerReadyPullRegions: map[string]bool{"edge": true}}
			s := New(fs, dispatch.NewInProc(2), testLogger()).
				WithCredentialEnvelopes(true).
				WithPullRegions([]string{"edge"}).
				WithCredentialLiveRegions(staticCredentialRegions{})
			if tc.flag {
				s = s.WithLedgerCarrier(true)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go s.Run(ctx)
			waitUntil(t, 3*time.Second, "a carrier policy to be resolved", func() bool {
				fs.mu.Lock()
				defer fs.mu.Unlock()
				return len(fs.carrierPolicies) > 0
			})
			if tc.flag {
				waitUntil(t, 3*time.Second, "the announced pull region to be raised", func() bool {
					return highestCarrier(t, fs) >= dispatch.ProtocolV4
				})
				return
			}
			if got := highestCarrier(t, fs); got > tc.want {
				t.Fatalf("a pull region with an ANNOUNCING agent was stamped generation %d with the "+
					"ledger flag off; announcement alone must not promote a region", got)
			}
		})
	}
}

// A region served by AGENTS is not promoted on the strength of the in-process runner, even with
// the flag on: `scheduler.go` skips pull regions in the local branch because an in-process
// executor "is no evidence" about the agent that will claim the row. Same shape as the V3 failure
// that comment records.
func TestAPullRegionIsNotRaisedByTheLocalExecutor(t *testing.T) {
	monitor := domain.Monitor{
		ID: "ledger-monitor", Type: domain.MonitorPostgres, Target: "db:5432",
		Region: "edge", Enabled: true, IntervalSeconds: 60, TimeoutSeconds: 5,
	}
	fs := &fakeStore{leader: true, monitors: []domain.Monitor{monitor}}
	s := New(fs, dispatch.NewInProc(2), testLogger()).
		WithCredentialEnvelopes(true).
		WithLedgerCarrier(true).
		WithPullRegions([]string{"edge"}).
		WithCredentialLiveRegions(announcingLedgerRegions{"edge": true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.Run(ctx)
	waitUntil(t, 3*time.Second, "a carrier policy to be resolved", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.carrierPolicies) > 0
	})
	if got := highestCarrier(t, fs); got > dispatch.ProtocolV3 {
		t.Fatalf("a pull-served region was raised to generation %d by the AMQP announcement path; "+
			"its agents, not this process, decide what it can receive", got)
	}
}
