package dispatch

import (
	"sync"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// CredentialFailuresBeforeUnready is how many CONSECUTIVE authentication failures mark an
// executor credential-unready.
//
// §4.7 degrades readiness on a PERSISTENT key mismatch, not on the first one, and the
// distinction is operational rather than pedantic: one corrupt, transplanted or
// retired-key payload is a per-job diagnostic, while readiness is what routes work to a
// whole executor. Treating the first as the second turns a single bad message into a
// regional outage — and during a dispatch-key rotation an old envelope arriving under a
// retired key id is expected traffic, not a fault.
const CredentialFailuresBeforeUnready = 2

// CredentialHealth turns a stream of per-job outcomes into one executor-level readiness
// verdict. It exists so the AMQP worker, its test-RPC callback and the pull agent share ONE
// rule: they had three, and only one of them had been updated to the spec's wording, which
// is the sort of divergence that is invisible until an operator compares two regions.
//
// Safe for concurrent use: a worker pool observes from every probe goroutine.
type CredentialHealth struct {
	mu       sync.Mutex
	failures int
}

// Failure records a rejected job and reports whether the executor should now be marked
// credential-UNREADY. It never flips readiness back on — only Success does that.
func (h *CredentialHealth) Failure(reason string) bool {
	switch reason {
	case domain.ProbeErrorUnsupportedVersion:
		// A generation we do not understand says nothing about our keys.
		return false
	case domain.ProbeErrorNoDispatchKey:
		// Holding no keyring at all is a configuration fact established at startup, not a
		// property of one payload: repeating it tells nobody anything new.
		h.mu.Lock()
		h.failures = CredentialFailuresBeforeUnready
		h.mu.Unlock()
		return true
	default:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.failures++
		return h.failures >= CredentialFailuresBeforeUnready
	}
}

// Success records a credential that opened, clearing the streak so an isolated failure
// later starts from scratch.
func (h *CredentialHealth) Success() {
	h.mu.Lock()
	h.failures = 0
	h.mu.Unlock()
}
