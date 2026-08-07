package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/config"
)

// TestNotificationEgressGuardWiring is a composition test over the actual cli wiring
// (notificationEgressGuard), not just config values or the guard mechanism in
// isolation. httptest binds 127.0.0.1 (a private address), so with the default
// notification_egress policy (deny-private) delivery must be blocked — and a
// regression that rewired the guard back to cfg.Prober (allow-private by default)
// would let it through, failing this test.
func TestNotificationEgressGuardWiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	def, err := config.Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("parse default: %v", err)
	}
	if _, err := notificationEgressGuard(def).HTTPClient(3 * time.Second).Get(srv.URL); err == nil {
		t.Fatal("default notification egress must block delivery to a loopback address")
	}

	open, err := config.Parse([]byte("notification_egress:\n  allow_private_ips: true\n"))
	if err != nil {
		t.Fatalf("parse opt-in: %v", err)
	}
	resp, err := notificationEgressGuard(open).HTTPClient(3 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("explicit opt-in must allow loopback delivery: %v", err)
	}
	_ = resp.Body.Close()
}
