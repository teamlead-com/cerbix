package config

import (
	"strings"
	"testing"
)

// TestConfigRefusesAMailFromItCannotSend (sec-hardening-3 T-3, §8 item 6): with mail.smtp_host set,
// mail.from must be present and a single bare address; the error names mail.from and does not quote
// the value. Without smtp_host the block is not checked. Before, an empty from silently switched mail
// off (MailConfig.Enabled) and a malformed one was accepted, to fail every send at the server.
func TestConfigRefusesAMailFromItCannotSend(t *testing.T) {
	block := func(host, from string) []byte {
		return []byte("mail:\n  smtp_host: \"" + host + "\"\n  smtp_port: 587\n  from: \"" + from + "\"\n  public_base_url: \"https://status.example\"\n")
	}
	for _, from := range []string{"", "Ops <ops@example.com>", "a b@c", "üser@example.com"} {
		_, err := Parse(block("smtp.example", from))
		if err == nil {
			t.Errorf("smtp_host with from %q was accepted", from)
			continue
		}
		if !strings.Contains(err.Error(), "mail.from") || (from != "" && strings.Contains(err.Error(), from)) {
			t.Errorf("from %q: error %q, want it to name mail.from and not quote the value", from, err)
		}
	}
	if _, err := Parse(block("smtp.example", "alerts@example.com")); err != nil {
		t.Errorf("a valid from was refused: %v", err)
	}
	for _, from := range []string{"", "Ops <ops@example.com>", "a b@c"} {
		if _, err := Parse(block("", from)); err != nil {
			t.Errorf("without smtp_host the block must not be checked; from %q refused: %v", from, err)
		}
	}
}
