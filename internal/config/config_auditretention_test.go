package config

import (
	"strings"
	"testing"
	"time"
)

func TestAuditRetentionDefaultsAndBounds(t *testing.T) {
	if err := defaults().Validate(); err != nil {
		t.Fatalf("defaults validate: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"retention low", func(c *Config) { c.Audit.RetentionDays = 29 }, "audit.retention_days"},
		{"retention high", func(c *Config) { c.Audit.RetentionDays = 3651 }, "audit.retention_days"},
		{"cadence low", func(c *Config) { c.Audit.PurgeEvery = Duration(4*time.Minute + 59*time.Second) }, "audit.purge_every"},
		{"cadence high", func(c *Config) { c.Audit.PurgeEvery = Duration(24*time.Hour + time.Second) }, "audit.purge_every"},
		{"batch low", func(c *Config) { c.Audit.PurgeBatchRows = 99 }, "audit.purge_batch_rows"},
		{"batch high", func(c *Config) { c.Audit.PurgeBatchRows = 10001 }, "audit.purge_batch_rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			tc.mutate(c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestAuditRetentionUnknownKeyIsRejected(t *testing.T) {
	if _, err := Parse([]byte("audit:\n  unknown: 1\n")); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("Parse() = %v, want unknown audit key refusal", err)
	}
}
