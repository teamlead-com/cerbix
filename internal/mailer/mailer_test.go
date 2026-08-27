package mailer

import (
	"context"
	"net/smtp"
	"testing"
)

func TestLiveMailerResolvesPerSend(t *testing.T) {
	var captured struct {
		addr, from string
		to         []string
	}
	orig := sendMailFunc
	sendMailFunc = func(_ context.Context, addr string, _ smtp.Auth, from string, to []string, _ []byte) error {
		captured.addr, captured.from, captured.to = addr, from, to
		return nil
	}
	t.Cleanup(func() { sendMailFunc = orig })

	cur := Settings{}
	m := NewLive(func() Settings { return cur })

	// Empty settings → not deliverable, Send errors, no dispatch.
	if m.Enabled() {
		t.Fatal("empty settings should not be enabled")
	}
	if err := m.Send("x@y", "s", "b"); err == nil {
		t.Fatal("send with no config should error")
	}

	// Configure live → Send resolves the new endpoint; port defaults to 587.
	cur = Settings{Enabled: true, Host: "smtp.example", From: "a@x", PublicBaseURL: "https://c/"}
	if !m.Enabled() {
		t.Fatal("configured settings should be enabled")
	}
	if m.BaseURL() != "https://c" {
		t.Fatalf("BaseURL = %q, want trailing slash trimmed", m.BaseURL())
	}
	if err := m.Send("x@y", "s", "b"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if captured.addr != "smtp.example:587" || captured.from != "a@x" || len(captured.to) != 1 {
		t.Fatalf("captured = %+v", captured)
	}
}
