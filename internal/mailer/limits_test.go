package mailer

import (
	"bytes"
	"fmt"
	"net/mail"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// addressOfLength builds a syntactically valid address of exactly n octets (n >= 70).
func addressOfLength(n int) string {
	local := strings.Repeat("x", 64)
	rest := n - len(local) - 1
	var labels []string
	for rest > 0 {
		size := 63
		if rest < 63+1 {
			size = rest
		}
		labels = append(labels, strings.Repeat("d", size))
		rest -= size
		if rest > 0 {
			rest-- // the dot before the next label
		}
	}
	return local + "@" + strings.Join(labels, ".")
}

// TestToIsFoldedBetweenAddresses (code review of sec-hardening-3, P2-2; S18): a long recipient list is
// folded at the boundaries between addresses, so no header line exceeds 78 octets, and it unfolds to
// the same addresses. Before, 40 recipients made one To line of 1042 octets.
func TestToIsFoldedBetweenAddresses(t *testing.T) {
	var to []string
	for i := 0; i < 40; i++ {
		to = append(to, fmt.Sprintf("recipient%02d@example.com", i))
	}
	msg, err := BuildMessage("status@example.com", to, "s", "b")
	if err != nil {
		t.Fatal(err)
	}
	assertFolded(t, msg)
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	list, err := m.Header.AddressList("To")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range list {
		got = append(got, a.Address)
	}
	if strings.Join(got, ",") != strings.Join(to, ",") {
		t.Fatalf("To unfolds to %v, want %v", got, to)
	}
}

// TestAnAddressOver254OctetsIsRefusedOnEverySurface (code review of sec-hardening-3, P2-2): the address
// rule bounds an address at 254 octets (RFC 5321's path limit less its brackets), and every surface
// that applies the rule — the builder, the subscriber, the email channel, Settings → Mail and the
// startup check — refuses 255 octets and accepts 254.
func TestAnAddressOver254OctetsIsRefusedOnEverySurface(t *testing.T) {
	ok, long := addressOfLength(254), addressOfLength(255)
	if len(ok) != 254 || len(long) != 255 {
		t.Fatalf("fixture lengths %d/%d", len(ok), len(long))
	}
	surfaces := map[string]func(addr string) error{
		"builder": func(a string) error { _, err := BuildMessage(a, []string{"ops@example.com"}, "s", "b"); return err },
		"subscriber": func(a string) error {
			return domain.Subscriber{StatusPageID: "p", Email: a}.Validate()
		},
		"email channel": func(a string) error {
			return domain.NotificationChannel{ProjectID: "p", Name: "n", Type: domain.ChannelEmail,
				Config: map[string]string{"smtp_host": "mail", "from": "status@example.com", "to": a}}.Validate()
		},
		"Settings → Mail": func(a string) error {
			return domain.MailSettings{Enabled: true, SMTPHost: "smtp", SMTPPort: 587, From: a}.Validate()
		},
		"startup": func(a string) error {
			_, err := config.Parse([]byte("mail:\n  smtp_host: smtp.example\n  smtp_port: 587\n  from: \"" + a +
				"\"\n  public_base_url: \"https://status.example\"\n"))
			return err
		},
	}
	for name, check := range surfaces {
		if err := check(long); err == nil {
			t.Errorf("%s accepted a 255-octet address", name)
		}
		if err := check(ok); err != nil {
			t.Errorf("%s refused a 254-octet address: %v", name, err)
		}
	}
}
