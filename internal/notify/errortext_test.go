package notify

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

// Distinctive values planted in every place a delivery URL can hold a secret.
const (
	plantedPath  = "PATHSECRET9f3"
	plantedQuery = "QUERYSECRET7a1"
	plantedBot   = "123456:BOTSECRET5c2"
)

// failureServer is one way a delivery can fail before any HTTP response, and the reason it must name.
type failureServer struct {
	name   string
	base   string // scheme://host:port of the failing endpoint
	guard  prober.Guard
	reason string
}

// rawListener answers every connection by reading once and writing `reply`, then closing.
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

func failureServers(t *testing.T) []failureServer {
	t.Helper()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(untrusted.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(plain.Close)
	open := prober.NewGuard(true, false)
	return []failureServer{
		{"closed port", "http://" + closedAddr, open, "connection refused"},
		{"private address refused by the guard", plain.URL, prober.NewGuard(false, false), "destination refused by the delivery egress policy"},
		{"untrusted certificate", untrusted.URL, open, "certificate not accepted"},
		{"https at a plain HTTP server", strings.Replace(plain.URL, "http://", "https://", 1), open, "the server does not speak TLS"},
		{"fatal TLS alert", "https://" + rawListener(t, []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28}), open, "TLS handshake failed"},
		{"bytes that are neither TLS nor HTTP", "https://" + rawListener(t, []byte("SSH-2.0-OpenSSH_9.0\r\n")), open, "TLS handshake failed"},
	}
}

func assertNoPlantedSecret(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range []string{plantedPath, plantedQuery, "BOTSECRET5c2"} {
		if strings.Contains(text, secret) {
			t.Errorf("%s carries the planted secret %q: %s", where, secret, text)
		}
	}
}

// TestDeliveryErrorNamesNoURL (sec-hardening-3 T-4, S21): a delivery that fails before any response
// returns text with the channel id and a reason from a closed set, and never a part of the URL. Before,
// it wrapped the client's *url.Error, whose text quotes the full URL — a Slack path, a tokened webhook
// URL, a Telegram bot token — into last_error, the log and the dead-letter API.
func TestDeliveryErrorNamesNoURL(t *testing.T) {
	origAPI := telegramAPI
	t.Cleanup(func() { telegramAPI = origAPI })
	channelsFor := func(base string) []domain.NotificationChannel {
		return []domain.NotificationChannel{
			{ID: "nc-webhook", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": base + "/hook/" + plantedPath + "?token=" + plantedQuery}},
			{ID: "nc-slack", Type: domain.ChannelSlack, Enabled: true, Config: map[string]string{"url": base + "/services/" + plantedPath}},
			{ID: "nc-telegram", Type: domain.ChannelTelegram, Enabled: true, Config: map[string]string{"bot_token": plantedBot, "chat_id": "42"}},
		}
	}
	for _, fs := range failureServers(t) {
		t.Run(fs.name, func(t *testing.T) {
			telegramAPI = fs.base
			for _, ch := range channelsFor(fs.base) {
				d := New(&fakeStore{channels: []domain.NotificationChannel{ch}}, fs.guard.HTTPClient(5*time.Second))
				err := d.Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false)
				if err == nil {
					t.Errorf("%s: delivery to a failing endpoint succeeded", ch.ID)
					continue
				}
				text := err.Error()
				assertNoPlantedSecret(t, ch.ID, text)
				if !strings.Contains(text, "channel "+ch.ID) || !strings.Contains(text, "post failed: "+fs.reason) {
					t.Errorf("%s: error %q, want the channel id and %q", ch.ID, text, "post failed: "+fs.reason)
				}
			}
		})
	}
	t.Run("a URL with a control byte", func(t *testing.T) {
		telegramAPI = "https://api.telegram.org"
		for _, ch := range []domain.NotificationChannel{
			{ID: "nc-webhook", Type: domain.ChannelWebhook, Enabled: true, Config: map[string]string{"url": "https://hooks.example/" + plantedPath + "\x7f?token=" + plantedQuery}},
			{ID: "nc-telegram", Type: domain.ChannelTelegram, Enabled: true, Config: map[string]string{"bot_token": plantedBot + "\x7f", "chat_id": "42"}},
		} {
			err := New(&fakeStore{channels: []domain.NotificationChannel{ch}}, nil).Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false)
			if err == nil {
				t.Errorf("%s: a URL with a control byte was sent", ch.ID)
				continue
			}
			assertNoPlantedSecret(t, ch.ID, err.Error())
			if !strings.Contains(err.Error(), "channel "+ch.ID) || !strings.Contains(err.Error(), "invalid URL") {
				t.Errorf("%s: error %q, want the channel id and \"invalid URL\"", ch.ID, err.Error())
			}
		}
	})
}
