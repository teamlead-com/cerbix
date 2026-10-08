package api

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-021 §15.0 — the short-TTL public render cache, "as the rate bound" ([314] P1-7).
//
// The hard ceiling bounds ONE request. It does nothing about repetition, and the public render is
// the one surface an unauthenticated caller can ask for as often as they like: each request is a
// report snapshot plus four more statements plus 90 days of strips. So a page's rendered bytes are
// reused for a few seconds.
//
// Three properties make this safe rather than merely fast:
//
//   - PRIVACY: the key carries the page id AND the exact access shape (public vs authenticated,
//     and the unlisted token when one was used). An unlisted page's bytes can never be served to a
//     request that did not present its token, and an authenticated render — which carries operator
//     diagnostics — can never be served to the public endpoint.
//   - FRESHNESS: the TTL is short and absolute. There is no invalidation hook, deliberately: an
//     operator's edit competes with a few seconds of staleness, while a hook that must be called
//     from every write path is a hook that will one day not be called, and a page silently pinned
//     to old bytes is worse than one that is five seconds behind.
//   - BOUNDEDNESS: the cache itself cannot become the leak. Entries are dropped on read when
//     expired, and the whole map is cleared when it grows past a hard cap, so an attacker cycling
//     slugs pays memory for at most that many entries.
//   - CONCURRENCY: a TTL map alone is not a rate bound ([318] P1-2). N simultaneous cold requests
//     all miss, all render, and the expensive work happens N times — every TTL boundary becomes a
//     stampede, which is precisely the burst the bound exists to stop. Requests for the same key
//     therefore COALESCE: one renders, the rest wait for that result.
const (
	// statusPageCacheTTL is short enough that an operator publishing an incident update does not
	// wait for it meaningfully, and long enough to collapse a burst.
	statusPageCacheTTL = 5 * time.Second
	// statusPageCacheMaxEntries bounds the cache under slug cycling.
	statusPageCacheMaxEntries = 512
)

type statusPageCacheEntry struct {
	body    []byte
	expires time.Time
}

// statusPageCache is a tiny TTL map. It holds SERIALIZED bytes, not view structs: re-encoding on
// every hit would give back most of what the cache saves, and bytes cannot be mutated by a
// subsequent handler the way a shared struct could.
type statusPageCache struct {
	mu      sync.Mutex
	entries map[string]statusPageCacheEntry
	// inflight is the coalescing half: one render per key at a time, everyone else waits on it.
	inflight map[string]*renderCall
	// now is injectable so the TTL is testable without sleeping.
	now func() time.Time
}

// renderCall is one in-progress render other callers can wait for.
type renderCall struct {
	done chan struct{}
	body []byte
	err  error
	// cacheable is false for a render that must not be stored (a refusal, an error): the waiters
	// still get the answer, and nothing is kept.
	cacheable bool
}

func newStatusPageCache() *statusPageCache {
	return &statusPageCache{
		entries:  map[string]statusPageCacheEntry{},
		inflight: map[string]*renderCall{},
		now:      time.Now,
	}
}

// do returns cached bytes, or runs `render` ONCE for this key while every concurrent caller for the
// same key waits for that single result.
//
// The lock is never held across the render — that would serialize the whole endpoint — so the
// sequence is: check the cache, claim the key, render outside the lock, publish to the waiters.
func (c *statusPageCache) do(key string, render func() (body []byte, cacheable bool, err error)) ([]byte, bool, error) {
	if c == nil {
		body, _, err := render()
		return body, false, err
	}
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if c.now().Before(e.expires) {
			c.mu.Unlock()
			return e.body, true, nil
		}
		delete(c.entries, key)
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		// A coalesced answer is reported as a HIT: the caller did not do the work.
		return call.body, true, call.err
	}
	call := &renderCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	call.body, call.cacheable, call.err = render()

	c.mu.Lock()
	delete(c.inflight, key)
	if call.err == nil && call.cacheable {
		if len(c.entries) >= statusPageCacheMaxEntries {
			c.entries = make(map[string]statusPageCacheEntry, statusPageCacheMaxEntries)
		}
		c.entries[key] = statusPageCacheEntry{body: call.body, expires: c.now().Add(statusPageCacheTTL)}
	}
	c.mu.Unlock()
	close(call.done)
	return call.body, false, call.err
}

// The endpoints that share the render cache. The endpoint is a FIELD of the key, never a suffix.
const (
	cacheRender  = "render"
	cacheHistory = "history"
)

// statusPageCacheKey binds cached bytes to the endpoint and the access shape that produced them.
//
// An unlisted page's token is part of the key, so its bytes are unreachable without it. A public
// page's token is checked by nothing, so it is NOT part of the key: otherwise every invented token
// would be a new entry (iter-0203 review R1-1). The fields are encoded as a JSON array, so a
// caller-chosen value containing a delimiter cannot spell another key — concatenating `|`-separated
// fields let `?token=|history|<month>|` on the render produce the history's key.
func statusPageCacheKey(endpoint string, sp domain.StatusPage, public bool, token string, parts ...string) string {
	shape := "authed"
	if public {
		shape = "public"
	}
	if sp.Visibility != domain.VisibilityUnlisted {
		token = ""
	}
	fields := append([]string{endpoint, sp.ID, shape, token}, parts...)
	b, err := json.Marshal(fields)
	if err != nil { // a []string always marshals; refuse to share bytes if it ever did not
		panic("status page cache key: " + err.Error())
	}
	return string(b)
}
