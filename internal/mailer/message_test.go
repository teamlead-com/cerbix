package mailer

import (
	"bytes"
	"context"
	"mime"
	"net/mail"
	"net/smtp"
	"strings"
	"testing"
	"unicode/utf8"
)

// sec-hardening-3 T-3, S18: the message builder every mail goes through.

type capturedMail struct {
	called bool
	from   string
	to     []string
	msg    []byte
}

func captureMail(t *testing.T) *capturedMail {
	t.Helper()
	c := &capturedMail{}
	orig := sendMailFunc
	sendMailFunc = func(_ context.Context, _ string, _ smtp.Auth, from string, to []string, msg []byte) error {
		c.called, c.from, c.to, c.msg = true, from, to, msg
		return nil
	}
	t.Cleanup(func() { sendMailFunc = orig })
	return c
}

var fiveHeaders = []string{"Content-Type", "From", "Mime-Version", "Subject", "To"}

// parseMail parses a captured message and returns it with its decoded subject.
func parseMail(t *testing.T, raw []byte) (*mail.Message, string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the message does not parse: %v\n%s", err, raw)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("the subject does not decode: %v", err)
	}
	return m, subject
}

func headerNames(m *mail.Message) []string {
	var names []string
	for k := range m.Header {
		names = append(names, k)
	}
	return sortStrings(names)
}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

func bodyOf(t *testing.T, m *mail.Message) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := b.ReadFrom(m.Body); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\r\n")
}

// TestMailerSubjectCannotAddAHeader: the subscriber, confirmation and reset path. An incident title
// shaped like an Alertmanager `summary` with CR/LF added Bcc and Content-Type headers and replaced
// the body before; now the header set is exactly the five and the subject is one line.
func TestMailerSubjectCannotAddAHeader(t *testing.T) {
	c := captureMail(t)
	title := "[investigating] x\r\nBcc: victim@evil.example\r\nContent-Type: text/html\r\n\r\n<p>forged</p>"
	body := "Incident opened: x\n\nStatus: investigating\n"
	if err := New("smtp.example", 587, "", "", "status@example.com", "https://status.example").
		SendContext(context.Background(), "subscriber@example.com", title, body); err != nil {
		t.Fatal(err)
	}
	m, subject := parseMail(t, c.msg)
	if got := headerNames(m); strings.Join(got, ",") != strings.Join(fiveHeaders, ",") {
		t.Fatalf("headers = %v, want exactly %v\n%s", got, fiveHeaders, c.msg)
	}
	if subject != "[investigating] x" {
		t.Fatalf("subject = %q, want the first line", subject)
	}
	if got := bodyOf(t, m); got != body {
		t.Fatalf("body = %q, want the body given", got)
	}
	if strings.Contains(string(c.msg), "<p>forged</p>") {
		t.Fatalf("the injected HTML reached the message:\n%s", c.msg)
	}
}

func TestMailSubjectIsTheFirstLineAndBounded(t *testing.T) {
	t.Run("multi-line SLA report", func(t *testing.T) {
		c := captureMail(t)
		report := "📊 Weekly SLA report — Payments\n  7d : 99.912% (10070/10079 checks)\n  30d: no data"
		if err := New("smtp.example", 587, "", "", "status@example.com", "").
			SendContext(context.Background(), "ops@example.com", report, report); err != nil {
			t.Fatal(err)
		}
		m, subject := parseMail(t, c.msg)
		if subject != "📊 Weekly SLA report — Payments" {
			t.Fatalf("subject = %q, want the first line", subject)
		}
		got := strings.ReplaceAll(bodyOf(t, m), "\r\n", "\n")
		if got != report {
			t.Fatalf("body = %q, want all three lines", got)
		}
		assertFolded(t, c.msg)
	})
	t.Run("a 500-rune line", func(t *testing.T) {
		c := captureMail(t)
		long := strings.Repeat("word ", 100)
		if err := New("smtp.example", 587, "", "", "status@example.com", "").
			SendContext(context.Background(), "ops@example.com", long, "body"); err != nil {
			t.Fatal(err)
		}
		_, subject := parseMail(t, c.msg)
		if n := utf8.RuneCountInString(subject); n != 200 || !strings.HasSuffix(subject, "…") ||
			!strings.HasPrefix(long, strings.TrimSuffix(subject, "…")) {
			t.Fatalf("subject = %q (%d runes), want the first 199 runes and …", subject, n)
		}
		assertFolded(t, c.msg)
	})
}

// assertFolded: every header line is at most 78 octets.
func assertFolded(t *testing.T, raw []byte) {
	t.Helper()
	head := string(raw)
	if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
		head = head[:i]
	}
	for _, line := range strings.Split(head, "\r\n") {
		if len(line) > 78 {
			t.Fatalf("header line of %d octets: %q", len(line), line)
		}
	}
}

func TestMailSubjectIsEncodedWhenNotASCII(t *testing.T) {
	c := captureMail(t)
	if err := New("smtp.example", 587, "", "", "status@example.com", "").
		SendContext(context.Background(), "ops@example.com", "🔴 db is DOWN.", "body"); err != nil {
		t.Fatal(err)
	}
	head := string(c.msg[:bytes.Index(c.msg, []byte("\r\n\r\n"))])
	for i := 0; i < len(head); i++ {
		if head[i] > 0x7e {
			t.Fatalf("raw non-ASCII byte in the header block:\n%s", head)
		}
	}
	if !strings.Contains(head, "=?utf-8?q?") && !strings.Contains(head, "=?UTF-8?q?") && !strings.Contains(head, "=?utf-8?Q?") {
		t.Fatalf("the subject is not an RFC 2047 encoded-word:\n%s", head)
	}
	if _, subject := parseMail(t, c.msg); subject != "🔴 db is DOWN." {
		t.Fatalf("decoded subject = %q", subject)
	}
}

func TestMailerRefusesAnAddressItCannotSend(t *testing.T) {
	refused := []string{
		"Ops <ops@example.com>", "a@b,c@d", "a b@c", "üser@example.com",
		"ops@example.com\x00", "ops@example.com\r", "ops@example.com\nBcc: x@y", "ops@example.com\x7f",
	}
	for _, bad := range refused {
		for _, field := range []string{"from", "to"} {
			c := captureMail(t)
			from, to := "status@example.com", "ops@example.com"
			if field == "from" {
				from = bad
			} else {
				to = bad
			}
			err := New("smtp.example", 587, "", "", from, "").SendContext(context.Background(), to, "s", "b")
			if err == nil {
				t.Errorf("%s %q was accepted", field, bad)
				continue
			}
			if c.called {
				t.Errorf("%s %q: the sender was called", field, bad)
			}
			if !strings.Contains(err.Error(), field) || strings.Contains(err.Error(), strings.TrimSpace(bad)) {
				t.Errorf("%s %q: error %q, want it to name the field and not quote the value", field, bad, err)
			}
		}
	}
	for _, good := range []string{"a@b.example", "a@[192.0.2.1]"} {
		c := captureMail(t)
		if err := New("smtp.example", 587, "", "", good, "").SendContext(context.Background(), good, "s", "b"); err != nil || !c.called {
			t.Errorf("%q: err %v, sent %v; want sent", good, err, c.called)
		}
	}
}
