package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// [318] P1-2 — the TTL map is only half a rate bound. These pin the half that was missing: the
// entry actually EXPIRES, and N simultaneous cold requests cost ONE render.
//
// This is an internal test on purpose: the property is about how many times the expensive function
// runs, which no HTTP-level assertion can see.

func TestRenderCacheExpiresOnItsClock(t *testing.T) {
	c := newStatusPageCache()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	var renders int32
	render := func() ([]byte, bool, error) {
		atomic.AddInt32(&renders, 1)
		return []byte("body"), true, nil
	}

	if _, hit, _ := c.do("k", render); hit {
		t.Fatal("the first request was reported as a cache hit")
	}
	if _, hit, _ := c.do("k", render); !hit {
		t.Fatal("the second request inside the TTL was not served from the cache")
	}
	if got := atomic.LoadInt32(&renders); got != 1 {
		t.Fatalf("renders = %d inside the TTL, want 1", got)
	}

	// One microsecond BEFORE expiry: still a hit. The boundary is where an off-by-one lives.
	now = now.Add(statusPageCacheTTL - time.Microsecond)
	if _, hit, _ := c.do("k", render); !hit {
		t.Fatal("an entry expired early")
	}
	// AT expiry: the entry is gone and the work runs again.
	now = now.Add(time.Microsecond)
	if _, hit, _ := c.do("k", render); hit {
		t.Fatal("an expired entry was served: the TTL never actually elapses")
	}
	if got := atomic.LoadInt32(&renders); got != 2 {
		t.Fatalf("renders = %d after expiry, want 2", got)
	}
}

func TestRenderCacheCoalescesConcurrentColdMisses(t *testing.T) {
	c := newStatusPageCache()
	var renders int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	render := func() ([]byte, bool, error) {
		if atomic.AddInt32(&renders, 1) == 1 {
			started <- struct{}{}
		}
		<-release // hold the render open so every caller arrives while it is in flight
		return []byte("body"), true, nil
	}

	const callers = 24
	var wg sync.WaitGroup
	bodies := make([][]byte, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _, err := c.do("k", render)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			bodies[i] = body
		}(i)
	}
	<-started
	// Everyone else is now either waiting on the in-flight call or about to.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&renders); got != 1 {
		t.Fatalf("renders = %d for %d simultaneous cold requests — every TTL boundary is a stampede",
			got, callers)
	}
	for i, b := range bodies {
		if string(b) != "body" {
			t.Fatalf("caller %d got %q: a coalesced waiter did not receive the result", i, b)
		}
	}
}

// A refusal or an error must NOT be stored: one bad moment would otherwise be served for the whole
// TTL, and the next caller could not tell.
func TestRenderCacheDoesNotStoreRefusals(t *testing.T) {
	c := newStatusPageCache()
	var renders int32
	refuse := func() ([]byte, bool, error) {
		atomic.AddInt32(&renders, 1)
		return []byte("over limit"), false, nil // cacheable = false
	}
	for i := 0; i < 3; i++ {
		if _, hit, _ := c.do("k", refuse); hit {
			t.Fatalf("request %d was served a cached REFUSAL", i)
		}
	}
	if got := atomic.LoadInt32(&renders); got != 3 {
		t.Fatalf("renders = %d, want 3: a refusal was cached", got)
	}
}

// Different access shapes never share bytes, which is what keeps an unlisted page's render
// unreachable without its token.
func TestRenderCacheKeysSeparateAccessShapes(t *testing.T) {
	pub := statusPageCacheKey("page", true, "")
	tok := statusPageCacheKey("page", true, "secret")
	authed := statusPageCacheKey("page", false, "")
	if pub == tok || pub == authed || tok == authed {
		t.Fatalf("keys collide: public=%q token=%q authenticated=%q", pub, tok, authed)
	}
}
