package telephony

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/prober"
)

// deliveryClient is built exactly like notify.New's fallback: the same redirect refusal the egress
// guard's client has (sec-hardening-3 S17).
func deliveryClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: prober.RefuseRedirects}
}

// TestClassifyStatusFollowsTheTable is §4.5's classification: 2xx sent; 408, 429, 5xx unreachable;
// every other status, a 3xx included (D14), rejected.
func TestClassifyStatusFollowsTheTable(t *testing.T) {
	cases := map[int]Outcome{
		200: Sent, 201: Sent, 202: Sent, 204: Sent, 299: Sent,
		408: Unreachable, 429: Unreachable, 500: Unreachable, 502: Unreachable, 503: Unreachable,
		504: Unreachable, 599: Unreachable,
		300: Rejected, 301: Rejected, 302: Rejected, 303: Rejected, 307: Rejected, 308: Rejected,
		400: Rejected, 401: Rejected, 403: Rejected, 404: Rejected, 409: Rejected, 410: Rejected,
		413: Rejected, 422: Rejected, 100: Rejected, 600: Rejected,
	}
	for status, want := range cases {
		if got := classifyStatus(status); got != want {
			t.Errorf("classifyStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

// TestOutcomesAreTheDeliveryReasons pins the outcome values to the §4.5 reasons they become.
func TestOutcomesAreTheDeliveryReasons(t *testing.T) {
	if Sent != "sent" || Unreachable != "provider_unreachable" || Rejected != "provider_rejected" {
		t.Fatalf("outcomes drifted from the §4.5 reasons: %q %q %q", Sent, Unreachable, Rejected)
	}
}

// TestValidCodeIsTheD34Pattern: ^[A-Za-z0-9_.-]{1,32}$, nothing else.
func TestValidCodeIsTheD34Pattern(t *testing.T) {
	good := []string{"21211", "E_BALANCE", "a", "x.y-z_1", strings.Repeat("9", 32)}
	bad := []string{"", strings.Repeat("9", 33), "no money", "+12025550147", "a/b", "é", "a\nb", "<x>", "a,b"}
	for _, s := range good {
		if !validCode(s) {
			t.Errorf("validCode(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validCode(s) {
			t.Errorf("validCode(%q) = true, want false", s)
		}
	}
}

// assertNoText fails when any rendering of the result holds one of the given fragments — the D34
// rule that a provider's text is never kept.
func assertNoText(t *testing.T, r Result, fragments ...string) {
	t.Helper()
	dump := fmt.Sprintf("%+v %#v", r, r)
	for _, f := range fragments {
		if strings.Contains(dump, f) {
			t.Errorf("result keeps provider text %q: %s", f, dump)
		}
	}
}
