package scheduler

import (
	"context"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// agentLivenessWindow is how recently a pull agent must have reported for its announcement to
// count: three missed 15-second heartbeats, which is a real outage rather than one slow poll.
//
// ONE value for every capability question asked of an agent (G1). It was a `canaryCapabilityWindow`
// constant whose own comment said "same 45s the credential readiness lookups use" beside three call
// sites that passed the literal — a bound stated four times, held together by that sentence.
const agentLivenessWindow = 45 * time.Second

// regionCapabilities is the ONE answer to "what can this region's executors consume", for one tick.
//
// G1. The leader held three independent resolvers for that question — credential readiness inline
// in the loop, the ledger carrier as a closure, and the canary announcement as a method — and each
// of them merged the SAME three sources (a live AMQP consumer, a pull agent's heartbeat, an
// executor inside this process) under the SAME pull-region exclusion, written out three times.
// A5, A6 and B5 are all that surface leaking: a queue-name helper that forgot the default region, a
// broker-admin stripper one generation behind, and an announcement that named the default region
// while the dispatcher it speaks for ignores the region entirely.
//
// What is consolidated is the RULE, not the timing. Each answer is still resolved where its inputs
// exist and at most once — the canary and the ledger lazily on first use, the credential set after
// the plain loop has said which regions have credentialed monitors due — because two of those
// resolution points have recorded P0s behind them and moving one would be a behaviour change
// wearing a refactor's clothes.
//
// The three rules this type states once:
//
//   - servedByAgents: a PULL region is executed by its agents and never by this process, so neither
//     an in-process executor nor an AMQP consumer is evidence about it. Both directions of that
//     have shipped as defects (party [99]/[100] for the canary, [102] for the credential carrier).
//   - servedHere: the converse, and it has to live beside it — a same-process executor IS this
//     binary, so its capability is ours by construction, and without the rule the ledger and the
//     envelope were inert in the commonest deployment.
//   - raise: the carrier is RAISED and never assigned, so no branch order can push an announced
//     generation back down.
type regionCapabilities struct {
	pull map[string]bool
	// carrier is the highest generation each region proved. Written only through `raise`.
	carrier map[string]int
	// credentialAMQP and credentialPull are envelope readiness, kept apart because they answer
	// about different executors and a consumer on one transport says nothing about the other.
	credentialAMQP map[string]bool
	credentialPull map[string]bool
}

func newRegionCapabilities(pull map[string]bool) *regionCapabilities {
	return &regionCapabilities{
		pull:           pull,
		carrier:        map[string]int{},
		credentialAMQP: map[string]bool{},
		credentialPull: map[string]bool{},
	}
}

// servedByAgents reports that this region's jobs go to `pull_jobs` and are executed by agents.
func (c *regionCapabilities) servedByAgents(region string) bool { return c.pull[region] }

// servedHere reports that this process executes the region itself — the exact negation, stated as
// one so the two cannot describe overlapping or disjoint sets by accident.
func (c *regionCapabilities) servedHere(region string) bool { return !c.pull[region] }

// raise moves a region's carrier UP and never down. A map any branch may assign into is a map whose
// value depends on branch order, which is how an announced generation 4 was silently pushed back to
// 3 by a credential branch that happened to run later.
func (c *regionCapabilities) raise(region string, generation int) {
	if c.carrier[region] < generation {
		c.carrier[region] = generation
	}
}

// carrierFor is the read side: the highest generation this region proved, zero when it proved none.
func (c *regionCapabilities) carrierFor(region string) int { return c.carrier[region] }

// credentialReady reports envelope readiness on the transport that actually serves the region.
func (c *regionCapabilities) credentialReady(region string) bool {
	if c.servedByAgents(region) {
		return c.credentialPull[region]
	}
	return c.credentialAMQP[region]
}

// resolveLedger answers FR-032 invariant 10k for every region, from the three sources.
//
// With `ledger.carrier_enabled` false this body never runs, so nothing anywhere stamps generation 4
// — the whole safety proof in-process, where there is no announcement to withhold (§16.1).
func (c *regionCapabilities) resolveLedger(ctx context.Context, s *Scheduler) {
	if !s.ledgerCarrier {
		return
	}
	// AMQP: something must be CONSUMING the v4 queue.
	if s.credentialLiveRegions != nil {
		if ready, err := s.credentialLiveRegions.LiveLedgerJobRegions(ctx); err != nil {
			s.logger.Warn("ledger_carrier_capability_lookup_failed", "error", err.Error())
		} else {
			for region := range ready {
				if c.servedHere(region) {
					c.raise(region, dispatch.ProtocolV4)
				}
			}
		}
	}
	// Pull: an AGENT must have declared it, which is a different question with a different source.
	// Announced on the heartbeat as `capabilities->>'ledger'`.
	if ready, err := s.store.LiveLedgerReadyAgentRegions(ctx, agentLivenessWindow); err != nil {
		s.logger.Warn("ledger_agent_capability_lookup_failed", "error", err.Error())
	} else {
		for region := range ready {
			if c.servedByAgents(region) {
				c.raise(region, dispatch.ProtocolV4)
			}
		}
	}
	// In-process: §13.0 requires this rule and its converse AT THE SAME PLACE, and without it the
	// ledger would be inert in the most common deployment.
	for region := range s.localLedgerRegions {
		if c.servedHere(region) {
			c.raise(region, dispatch.ProtocolV4)
		}
	}
}

// resolveCredential answers envelope readiness and raises the generation-3 carrier, for the regions
// that actually have a credentialed monitor due. It runs after the plain loop for that reason: the
// set is not known before it.
func (c *regionCapabilities) resolveCredential(ctx context.Context, s *Scheduler) {
	// Capability 1 is the floor for the generation-2 carrier; the floor rises with the emitted
	// generation, never independently of it.
	if ready, err := s.store.LiveCredentialReadyAgentRegions(ctx, agentLivenessWindow, dispatch.EnvelopeV1); err != nil {
		s.logger.Warn("credential_agent_capability_lookup_failed", "error", err.Error())
	} else {
		c.credentialPull = ready
	}
	if ready, err := s.store.LiveCredentialReadyAgentRegions(ctx, agentLivenessWindow, dispatch.EnvelopeV2); err != nil {
		s.logger.Warn("credential_agent_capability_lookup_failed", "error", err.Error())
	} else {
		for region := range ready {
			if c.servedByAgents(region) {
				c.raise(region, dispatch.ProtocolV3)
			}
		}
	}
	if s.credentialLiveRegions != nil {
		if ready, err := s.credentialLiveRegions.LiveCredentialJobRegions(ctx); err != nil {
			s.logger.Warn("credential_worker_capability_lookup_failed", "error", err.Error())
		} else {
			c.credentialAMQP = ready
		}
		if ready, err := s.credentialLiveRegions.LiveCredentialV3JobRegions(ctx); err != nil {
			s.logger.Warn("credential_worker_capability_lookup_failed", "error", err.Error())
		} else {
			for region := range ready {
				if c.servedHere(region) {
					c.raise(region, dispatch.ProtocolV3)
				}
			}
		}
	}
	for region := range s.localCredentialRegions {
		if c.servedByAgents(region) {
			continue
		}
		// A same-process executor IS this binary, so its capability is ours by construction — there
		// is no wire and no version skew to discover.
		c.credentialAMQP[region] = true
		c.raise(region, dispatch.ProtocolV3)
	}
}

// canaryTokensFor merges the announcement sources for one region, including this process's own
// (B5): `role=all` executes every non-pull region in-process, so an announcement enumerating the
// default region alone refused a canary in any other one, forever.
//
// A Scheduler method rather than one on the record, because the announcement map and the local
// half are properties of the ROLE rather than of a tick. It still takes the record, and that is the
// point: the pull-region exclusion is asked ONCE, of one owner, on every path that asks it.
func (s *Scheduler) canaryTokensFor(region string, announced map[string][]string, caps *regionCapabilities) []string {
	tokens := announced[region]
	if local := s.localCanaryTokenFor(region, caps); local != "" && !domain.CanaryCapabilityAnnounced(tokens, local) {
		tokens = append(append([]string(nil), tokens...), local)
	}
	return tokens
}
