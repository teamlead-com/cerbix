package notify

import (
	"bytes"
	"context"
	"mime"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type sentMail struct {
	called bool
	from   string
	to     []string
	msg    []byte
}

func captureChannelMail(t *testing.T) *sentMail {
	t.Helper()
	s := &sentMail{}
	orig := sendMailFunc
	sendMailFunc = func(_ context.Context, _ string, _ smtp.Auth, from string, to []string, msg []byte) error {
		s.called, s.from, s.to, s.msg = true, from, to, msg
		return nil
	}
	t.Cleanup(func() { sendMailFunc = orig })
	return s
}

func emailChannel(from, to string) domain.NotificationChannel {
	return domain.NotificationChannel{ID: "ec", Type: domain.ChannelEmail, Enabled: true,
		Config: map[string]string{"smtp_host": "mail", "smtp_port": "1025", "from": from, "to": to}}
}

// TestChannelMailCannotAddAHeader (sec-hardening-3 T-3, S18): a monitor whose name carries CR/LF goes
// DOWN; the channel mail has exactly the five headers, a one-line subject, and the whole alert text as
// its body. Before, the name added Bcc and Content-Type headers and made `<p>forged</p>` the body.
func TestChannelMailCannotAddAHeader(t *testing.T) {
	s := captureChannelMail(t)
	name := "x\r\nBcc: victim@evil.example\r\nContent-Type: text/html\r\n\r\n<p>forged</p>"
	d := New(&fakeStore{channels: []domain.NotificationChannel{emailChannel("cerbix@example.com", "ops@example.com")}}, nil)
	if err := d.Deliver(context.Background(), domain.Monitor{ID: "m1", Name: name}, false, nil); err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(s.msg))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, s.msg)
	}
	var names []string
	for k := range m.Header {
		names = append(names, k)
	}
	if len(names) != 5 || m.Header.Get("Bcc") != "" || m.Header.Get("Content-Type") != "text/plain; charset=UTF-8" {
		t.Fatalf("headers = %v (Content-Type %q), want exactly the five\n%s", names, m.Header.Get("Content-Type"), s.msg)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject != "🔴 x" {
		t.Fatalf("subject = %q (%v), want %q", subject, err, "🔴 x")
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(m.Body)
	if got := strings.TrimSuffix(body.String(), "\r\n"); got != Message(domain.Monitor{Name: name}, false) {
		t.Fatalf("body = %q, want the whole alert text", got)
	}
}

// TestChannelMailRefusesAnAddressItCannotSend (sec-hardening-3 T-3, S18): the channel's `from`, and
// each element of its comma-separated `to`, must be a single bare address. A refused one ends the
// delivery before any SMTP session, names the field and position, and mails nobody. A `from` with
// surrounding spaces is trimmed and sent, as the `to` elements always were.
func TestChannelMailRefusesAnAddressItCannotSend(t *testing.T) {
	// CR and LF sit INSIDE the value: surrounding whitespace, a trailing CR included, is trimmed before
	// the rule is applied and is never sent (§4.2), so only an interior one can reach a header.
	bad := []string{"ops@example.com\x00", "ops@example.com\rBcc: x@y", "ops@example.com\nBcc: x@y", "ops@example.com\x7f",
		"Ops <ops@example.com>", "a b@c", "üser@example.com"}
	for i, value := range bad {
		for _, field := range []string{"from", "to"} {
			t.Run(field+"/"+strconv.Itoa(i), func(t *testing.T) {
				s := captureChannelMail(t)
				ch := emailChannel("cerbix@example.com", "first@example.com, "+value)
				wantField := "to address 2"
				if field == "from" {
					ch = emailChannel(value, "first@example.com")
					wantField = "from"
				}
				err := New(&fakeStore{channels: []domain.NotificationChannel{ch}}, nil).
					Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false, nil)
				if err == nil {
					t.Fatalf("%s %q was accepted", field, value)
				}
				if s.called {
					t.Fatalf("%s %q: sendMailFunc was called (the valid recipient was mailed)", field, value)
				}
				if !strings.Contains(err.Error(), wantField) || strings.Contains(err.Error(), strings.TrimSpace(value)) {
					t.Fatalf("%s %q: error %q, want %q and not the value", field, value, err, wantField)
				}
			})
		}
	}
	t.Run("from with surrounding spaces", func(t *testing.T) {
		s := captureChannelMail(t)
		err := New(&fakeStore{channels: []domain.NotificationChannel{emailChannel("  ops@example.com  ", "a@example.com")}}, nil).
			Deliver(context.Background(), domain.Monitor{ID: "m1", Name: "API"}, false, nil)
		if err != nil || !s.called {
			t.Fatalf("err %v, sent %v; want sent", err, s.called)
		}
		m, perr := mail.ReadMessage(bytes.NewReader(s.msg))
		if perr != nil {
			t.Fatal(perr)
		}
		if s.from != "ops@example.com" || m.Header.Get("From") != "ops@example.com" {
			t.Fatalf("envelope sender %q, From header %q; want ops@example.com in both", s.from, m.Header.Get("From"))
		}
	})
}
