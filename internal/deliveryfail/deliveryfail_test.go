package deliveryfail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/prober"
)

// FR-041a review P3-3: Classes is the closed set Class draws from — every class is reachable, and
// nothing Class returns is outside it — so a caller that admits only Classes (the test SMS's
// transport_class) drops nothing the classifier can say.
func TestClassesIsWhatClassReturns(t *testing.T) {
	_, egressErr := prober.NewGuard(false, false).HTTPClient(time.Second).Get("http://127.0.0.1:1/")
	if egressErr == nil {
		t.Fatal("the deny-private guard reached loopback")
	}
	cases := map[string]error{
		ClassEgressRefused:   egressErr,
		ClassCertificate:     fmt.Errorf("post: %w", &tls.CertificateVerificationError{Err: errors.New("x")}),
		ClassNotTLS:          fmt.Errorf("post: %w", http.ErrSchemeMismatch),
		ClassTLSHandshake:    &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")},
		ClassTimeout:         fmt.Errorf("post: %w", context.DeadlineExceeded),
		ClassDNS:             &net.DNSError{Err: "no such host", Name: "sms.example"},
		ClassRefused:         &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		ClassConnectionFails: errors.New("EOF"),
	}
	if len(Classes()) != len(cases) {
		t.Fatalf("Classes() has %d entries, the classifier %d branches", len(Classes()), len(cases))
	}
	for want, err := range cases {
		got := Class(err)
		if got != want {
			t.Errorf("Class(%v) = %q, want %q", err, got, want)
		}
		if !IsClass(got) {
			t.Errorf("Class returned %q, which is not in Classes()", got)
		}
	}
	if IsClass("dial tcp 203.0.113.9:443: free text") || IsClass("") {
		t.Fatal("IsClass admits free text")
	}
}
