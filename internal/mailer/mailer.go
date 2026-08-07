// Package mailer sends plain-text email via SMTP for status-page subscriptions
// (confirmation and incident notifications).
package mailer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// dialTimeout bounds the TCP connect to the SMTP server; sessionDeadline bounds
// the whole conversation. Without these, an unreachable/misconfigured SMTP host
// makes net/smtp's Dial block on the OS TCP timeout (minutes), hanging the HTTP
// request that triggered the send (e.g. a status-page subscription).
const (
	dialTimeout     = 10 * time.Second
	sessionDeadline = 30 * time.Second
)

// sendMailFunc is the SMTP sender, a var so tests capture messages without a
// live server. The default is a timeout-bounded reimplementation of
// smtp.SendMail (which itself has no dial timeout).
var sendMailFunc = sendMailTimeout

// SendMailTimeout is the exported form of the timeout-bounded SMTP send, reused
// by internal/notify so email notification channels get the same dial/session
// deadlines instead of stdlib smtp.SendMail's unbounded blocking (which would
// stall the single outbox worker on a dead SMTP host).
func SendMailTimeout(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	return sendMailTimeout(addr, auth, from, to, msg)
}

// sendMailTimeout mirrors smtp.SendMail (EHLO → STARTTLS if offered → AUTH →
// MAIL/RCPT/DATA) but dials with a timeout and sets a session deadline so a
// dead SMTP endpoint fails fast instead of hanging the caller. Port 465 is
// treated as implicit TLS (SMTPS): the connection is wrapped in TLS before the
// SMTP greeting, since the server never speaks plaintext there (STARTTLS on 465
// deadlocks — client waits for a greeting, server waits for a TLS handshake).
func sendMailTimeout(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(sessionDeadline))

	implicitTLS := portStr == "465"
	if implicitTLS {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return err
		}
		conn = tlsConn
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok && !implicitTLS {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := mailFrom(c, from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// mailFrom issues MAIL FROM without the SMTPUTF8 parameter. net/smtp's
// Client.Mail appends SMTPUTF8 whenever the server advertises it — even for an
// all-ASCII envelope — and that flag then breaks forwarding through relays that
// don't support SMTPUTF8 (e.g. AWS SES), permanently bouncing the message. Our
// envelope addresses are ASCII, so SMTPUTF8 is never needed here. BODY=8BITMIME
// is kept when offered (widely supported; lets the UTF-8 body pass 8-bit clean).
func mailFrom(c *smtp.Client, from string) error {
	if strings.ContainsAny(from, "\r\n") {
		return errors.New("mailer: invalid from address")
	}
	cmd := "MAIL FROM:<%s>"
	if ok, _ := c.Extension("8BITMIME"); ok {
		cmd += " BODY=8BITMIME"
	}
	id, err := c.Text.Cmd(cmd, from)
	if err != nil {
		return err
	}
	c.Text.StartResponse(id)
	defer c.Text.EndResponse(id)
	_, _, err = c.Text.ReadResponse(250)
	return err
}

// Settings is a resolved SMTP endpoint. Enabled gates whether mail can be sent.
type Settings struct {
	Enabled       bool
	Host          string
	Port          int
	Username      string
	Password      string
	From          string
	PublicBaseURL string
}

func (s Settings) deliverable() bool {
	return s.Enabled && strings.TrimSpace(s.Host) != "" && strings.TrimSpace(s.From) != ""
}

// Mailer sends plain-text email. It resolves its endpoint per send so the SMTP
// configuration can be changed at runtime from the Settings UI.
type Mailer struct {
	resolve func() Settings
}

// New builds a static Mailer from explicit values (used by tests / config-only).
func New(host string, port int, username, password, from, baseURL string) *Mailer {
	s := Settings{Enabled: true, Host: host, Port: port, Username: username, Password: password, From: from, PublicBaseURL: baseURL}
	return &Mailer{resolve: func() Settings { return s }}
}

// NewLive builds a Mailer that resolves its endpoint per send (live-reconfigurable).
func NewLive(resolve func() Settings) *Mailer { return &Mailer{resolve: resolve} }

func (m *Mailer) settings() Settings {
	if m.resolve == nil {
		return Settings{}
	}
	return m.resolve()
}

// Enabled reports whether the mailer can currently deliver.
func (m *Mailer) Enabled() bool { return m.settings().deliverable() }

// BaseURL returns the public origin (no trailing slash).
func (m *Mailer) BaseURL() string { return strings.TrimRight(m.settings().PublicBaseURL, "/") }

// Send delivers a plain-text email to one recipient.
func (m *Mailer) Send(to, subject, body string) error {
	s := m.settings()
	if !s.deliverable() {
		return errors.New("mailer: not configured")
	}
	port := s.Port
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host)
	}
	msg := "From: " + s.From + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" + body + "\r\n"
	return sendMailFunc(fmt.Sprintf("%s:%d", s.Host, port), auth, s.From, []string{to}, []byte(msg))
}
