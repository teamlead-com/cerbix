package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestAgentFollowsNoRedirect (sec-hardening-3 T-2, S17): every endpoint of core answers 307 to another
// port on the same host. Nothing reaches it, and EACH call reports the status on its own — checked per
// call with its own log, so one call's line cannot hide another's silence (code review, P2-1). Before
// the fix every request arrived there WITH `Authorization: Bearer <agent token>`: net/http keeps the
// header on a redirect to the same host name, and its rule ignores the port.
func TestAgentFollowsNoRedirect(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Method+" "+r.URL.Path+" Authorization="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer b.Close()
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", b.URL+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer core.Close()

	newAgent := func() (*Agent, *bytes.Buffer) {
		var logs bytes.Buffer
		return New(core.URL, "AGENT-TOKEN", "pull1", fixedRunner{}, slog.New(slog.NewTextHandler(&logs, nil))), &logs
	}
	ctx := context.Background()

	t.Run("poll", func(t *testing.T) {
		a, logs := newAgent()
		a.poll(ctx)
		if !strings.Contains(logs.String(), "307") {
			t.Errorf("poll did not report the status: %q", logs.String())
		}
	})
	t.Run("heartbeat", func(t *testing.T) {
		a, logs := newAgent()
		a.heartbeat(ctx)
		if !strings.Contains(logs.String(), "agent_heartbeat_failed") || !strings.Contains(logs.String(), "307") {
			t.Errorf("heartbeat did not report the status: %q", logs.String())
		}
	})
	t.Run("claimTest", func(t *testing.T) {
		a, logs := newAgent()
		if _, _, _, ok := a.claimTest(ctx); ok {
			t.Error("claimTest claimed a test from a redirect")
		}
		if !strings.Contains(logs.String(), "agent_test_claim_failed") || !strings.Contains(logs.String(), "307") {
			t.Errorf("claimTest did not report the status: %q", logs.String())
		}
	})
	t.Run("postResults", func(t *testing.T) {
		a, _ := newAgent()
		err := a.postResults(ctx, []domain.Heartbeat{{MonitorID: "m1", Up: true}}, nil)
		if err == nil || !strings.Contains(err.Error(), "307") {
			t.Errorf("postResults = %v, want an error naming status 307", err)
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the redirect target received %d request(s): %v", len(leaked), leaked)
	}
}
