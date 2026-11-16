package webhook

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/prober"
)

// TestWebhookDeliveryErrorNamesNoURL (sec-hardening-3 T-4, S21): the same as notify's test, for an
// org webhook with secrets in its path and query.
func TestWebhookDeliveryErrorNamesNoURL(t *testing.T) {
	const path, query = "PATHSECRET9f3", "QUERYSECRET7a1"
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer untrusted.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	alert := rawListener(t, []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28})
	banner := rawListener(t, []byte("SSH-2.0-OpenSSH_9.0\r\n"))
	open := prober.NewGuard(true, false)

	for _, tc := range []struct {
		name, url string
		guard     prober.Guard
		reason    string
	}{
		{"closed port", "http://" + closedAddr, open, "post failed: connection refused"},
		{"private address refused by the guard", plain.URL, prober.NewGuard(false, false), "post failed: destination refused by the delivery egress policy"},
		{"untrusted certificate", untrusted.URL, open, "post failed: certificate not accepted"},
		{"https at a plain HTTP server", strings.Replace(plain.URL, "http://", "https://", 1), open, "post failed: the server does not speak TLS"},
		{"fatal TLS alert", "https://" + alert, open, "post failed: TLS handshake failed"},
		{"bytes that are neither TLS nor HTTP", "https://" + banner, open, "post failed: TLS handshake failed"},
		{"a URL with a control byte", "https://hooks.example", open, "invalid URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := tc.url + "/hook/" + path + "?token=" + query
			if tc.reason == "invalid URL" {
				url = tc.url + "/hook/" + path + "\x7f?token=" + query
			}
			store := &fakeStore{hooks: []domain.Webhook{{ID: "wh1", URL: url, Secret: "topsecret", Enabled: true}}}
			ev := domain.IncidentEvent{Type: domain.EventIncidentOpened, Incident: domain.Incident{ID: "inc1", ProjectID: "p1", Title: "down"}}
			err := New(store, tc.guard.HTTPClient(5*time.Second)).Deliver(context.Background(), ev)
			if err == nil {
				t.Fatal("delivery to a failing endpoint succeeded")
			}
			text := err.Error()
			for _, secret := range []string{path, query} {
				if strings.Contains(text, secret) {
					t.Errorf("the error carries the planted secret %q: %s", secret, text)
				}
			}
			if !strings.Contains(text, "webhook wh1") || !strings.Contains(text, tc.reason) {
				t.Errorf("error %q, want the webhook id and %q", text, tc.reason)
			}
		})
	}
}

func rawListener(t *testing.T, reply []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_, _ = c.Write(reply)
				time.Sleep(100 * time.Millisecond)
			}(c)
		}
	}()
	return ln.Addr().String()
}
