package prober

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestDeliveryClientRefusesRedirects (sec-hardening-3 T-2, S17): the delivery client follows no
// redirect at all. A 3xx is answered as the response it is, and the deliverer reports its status.
func TestDeliveryClientRefusesRedirects(t *testing.T) {
	c := NewGuard(true, false).HTTPClient(time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("the delivery client has no redirect policy")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://next.example/landing", nil)
	via, _ := http.NewRequest(http.MethodPost, "https://hooks.example/token", nil)
	if err := c.CheckRedirect(req, []*http.Request{via}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect on the first hop = %v, want http.ErrUseLastResponse", err)
	}
}
