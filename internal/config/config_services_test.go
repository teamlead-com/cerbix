package config

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-003: an illegal value is REJECTED at startup, never silently mapped to a legal one. The
// first implementation clamped in the store, which meant the config the operator wrote and
// the config the system ran were different configs — a fail-fast violation that surfaced
// nowhere.
func TestServiceCapsAreRejectedNotReinterpreted(t *testing.T) {
	base := func() *Config {
		c := defaults()
		return c
	}

	// Defaults validate.
	if err := base().Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}

	for _, tc := range []struct {
		what string
		mut  func(*Config)
		want string
	}{
		{"negative per-project", func(c *Config) { c.Services.MaxServicesPerProject = -1 }, "at least 1"},
		{"zero per-monitor", func(c *Config) { c.Services.MaxServicesPerMonitor = 0 }, "at least 1"},
		{"over hard max per-project", func(c *Config) { c.Services.MaxServicesPerProject = domain.HardMaxServicesPerProject + 1 }, "hard maximum"},
		{"over hard max members", func(c *Config) { c.Services.MaxMembersPerRevision = domain.HardMaxMembersPerRevision + 1 }, "hard maximum"},
		{"over hard max per-monitor", func(c *Config) { c.Services.MaxServicesPerMonitor = domain.HardMaxServicesPerMonitor + 1 }, "hard maximum"},
	} {
		c := base()
		tc.mut(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted; the running config would differ from the written one", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name the rule (%q)", tc.what, err, tc.want)
		}
	}

	// The full legal range is accepted, so the rule is a bound and not a ban.
	c := base()
	c.Services.MaxServicesPerProject = domain.HardMaxServicesPerProject
	if err := c.Validate(); err != nil {
		t.Errorf("the hard maximum itself was rejected: %v", err)
	}
}
