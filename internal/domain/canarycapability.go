package domain

import (
	"strconv"
	"strings"
)

// FR-029 invariant 6 — a canary reaches an executor that ANNOUNCED it can run one.
//
// The capability is a SET of `workflow_kind@version`, not a boolean, for the reason D-0160 already
// established for credential envelopes: an executor that can open generation 1 is not evidence of
// readiness for a region core is about to emit generation 2 into. A canary has the same shape — a
// future `async_transaction_v2` must not land on a runner that only knows v1 — so the announcement
// carries the version and the scheduler asks for the one it is about to emit.
//
// This exists as a typed value rather than as a bare string because the announcement crosses THREE
// wires (an agent heartbeat's JSONB, an AMQP queue name, and an in-process registration) and a
// spelling that drifts on one of them fails silently: the region simply looks incapable forever.

// CanaryWorkflowVersion is the version of `CanaryWorkflowKind` this binary executes. It moves when
// the executor's contract changes in a way an older runner cannot honour — not when the schema gains
// an optional field, which an older runner ignores harmlessly.
const CanaryWorkflowVersion = 1

// CanaryCapabilityKey is the key an executor announces under, inside the capabilities document it
// already sends. One key, so a reader never has to know how many workflow kinds exist.
const CanaryCapabilityKey = "workflow_kinds"

// CanaryCapabilityToken is the announced form: `<kind>@<version>`. The scheduler compares tokens
// rather than parsing versions, so a runner announcing a kind it does not have cannot be mistaken
// for one that does.
func CanaryCapabilityToken(kind string, version int) string {
	return kind + "@" + strconv.Itoa(version)
}

// CanaryCapabilityOfThisBinary is what an executor built from this source announces.
func CanaryCapabilityOfThisBinary() string {
	return CanaryCapabilityToken(CanaryWorkflowKind, CanaryWorkflowVersion)
}

// CanaryCapabilityRequiredBy reports the token a JOB needs. It is derived from the workflow the job
// carries, so a document of a future kind asks for a runner that announces that kind — the scheduler
// never assumes the only kind is the one it happens to know.
func CanaryCapabilityRequiredBy(w CanaryWorkflow) string {
	return CanaryCapabilityToken(w.Kind, CanaryWorkflowVersion)
}

// CanaryCapabilityAnnounced reports whether an announcement covers the required token.
//
// Exact match, deliberately. A runner announcing `async_transaction_v1@1` does not satisfy a job
// needing `@2`, and the reverse is equally refused: a NEWER runner does not silently accept an older
// contract it may have stopped honouring. Both directions are a `capability_mismatch`, which is a
// bounded per-monitor DOWN and not a queue that grows.
func CanaryCapabilityAnnounced(announced []string, required string) bool {
	for _, a := range announced {
		if a == required {
			return true
		}
	}
	return false
}

// CanaryCapabilityRequiredByConfig is the token a stored MONITOR needs, and the only place that rule
// is written. Three call sites derive it — the scheduler's dispatch filter, the pull job's required
// capability, and the AMQP queue a job is published to — and three spellings of one rule is exactly
// the drift this file exists to prevent: they would disagree only for the documents that matter, the
// ones of an unfamiliar kind.
//
// A config that does not parse falls back to this binary's token. It is not a guess about the
// document: an unparseable canary fails at the executor's own gate with a reason of its own, and
// answering "the runner I know" here keeps that failure where it can be read instead of turning it
// into a capability refusal that names the wrong problem.
func CanaryCapabilityRequiredByConfig(config map[string]string) string {
	if w, err := ParseCanaryConfig(config); err == nil {
		return CanaryCapabilityRequiredBy(w)
	}
	return CanaryCapabilityOfThisBinary()
}

// ── The AMQP half of the same announcement ─────────────────────────────────────────────────────

// CanaryQueueSuffix is what follows `checks.canary.` in a canary queue name: the capability TOKEN,
// then the region. The token is in the name on purpose — a queue is the announcement on the AMQP
// side, so a worker that speaks a different version binds a different queue and core can see the
// difference instead of publishing into a queue whose consumers cannot honour the contract.
//
// A token contains no dot (kind is `[a-z0-9_]`, version is digits), so the FIRST dot separates the
// two halves and a region containing dots still round-trips.
func CanaryQueueSuffix(token, region string) string {
	return token + "." + region
}

// SplitCanaryQueueSuffix is the exact inverse, and the only place the split rule is written. It
// refuses anything that is not `<token>.<region>` with both halves non-empty rather than guessing,
// so an unrelated queue that happens to start with the prefix announces nothing.
func SplitCanaryQueueSuffix(suffix string) (token, region string, ok bool) {
	i := strings.IndexByte(suffix, '.')
	if i <= 0 || i == len(suffix)-1 {
		return "", "", false
	}
	return suffix[:i], suffix[i+1:], true
}
