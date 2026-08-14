package config

import (
	"strings"
	"testing"
)

// b64Key32 is a valid base64 32-byte key for tests ("0123456789abcdef0123456789abcdef").
const b64Key32 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// b64Key32B is a second, distinct valid key ("fedcba9876543210fedcba9876543210").
const b64Key32B = "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA="

func baseSecretsConfig() *Config {
	c := defaultsConfig()
	c.Security.EncryptionKey = b64Key32
	return c
}

// defaultsConfig builds a minimal valid config the way Load would.
func defaultsConfig() *Config {
	return defaults()
}

func TestSecretsDisabledByDefaultIsValid(t *testing.T) {
	c := defaultsConfig()
	if c.Secrets.Enabled {
		t.Fatal("secrets must be disabled by default")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestSecretsEnabledRequiresEnforcedEnvelope(t *testing.T) {
	// enabled with the legacy transport mode is a hard error: ref credentials never
	// ride legacy plaintext dispatch (§4.7 wire barrier is not a later wiring detail).
	c := defaultsConfig()
	c.Secrets.Enabled = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "enforced") {
		t.Fatalf("enabled without dispatch_envelope=enforced must fail, got %v", err)
	}
	c.Secrets.DispatchEnvelope = "enforced"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "dispatch keyring") {
		t.Fatalf("enforced without any keyring must fail, got %v", err)
	}
	c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{
		"geo2": {Primary: DispatchKeyEntry{ID: "geo2-2026a", Key: b64Key32B}},
	}
	// Load-time validation is role-agnostic: NO master key required here (a worker's
	// config must validate without one); presence rules are per-role below.
	if err := c.Validate(); err != nil {
		t.Fatalf("enabled+enforced with a keyring must validate role-agnostically: %v", err)
	}
}

// TestValidateSecretsForRole is the role/key separation matrix (§4.1/§4.7): materializing
// roles need master+keyring; executors need their region's keyring and must NOT hold the
// master; the role region is validated even when a default keyring exists.
func TestValidateSecretsForRole(t *testing.T) {
	ring := DispatchRegionKeys{Primary: DispatchKeyEntry{ID: "k1", Key: b64Key32B}}
	build := func(mut func(*Config)) *Config {
		c := defaultsConfig()
		c.Secrets.Enabled = true
		c.Secrets.DispatchEnvelope = "enforced"
		c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{"geo2": ring}
		mut(c)
		return c
	}
	cases := []struct {
		name    string
		cfg     *Config
		role    string
		region  string
		wantErr string // "" = ok
	}{
		{"disabled any role ok", defaultsConfig(), "worker", "geo2", ""},
		{"disabled executor still rejects master", func() *Config {
			c := defaultsConfig()
			c.Security.EncryptionKey = b64Key32
			return c
		}(), "worker", "geo2", "must NOT hold the at-rest master"},
		{"all requires master", build(func(c *Config) {}), "all", "", "encryption_key"},
		{"api requires master", build(func(c *Config) {}), "api", "", "encryption_key"},
		{"scheduler with master+ring ok", build(func(c *Config) { c.Security.EncryptionKey = b64Key32 }), "scheduler", "", ""},
		{"materializer needs a keyring", build(func(c *Config) {
			c.Security.EncryptionKey = b64Key32
			c.Security.Dispatch = DispatchConfig{}
			c.Secrets.Enabled = false
			c.Secrets.DispatchEnvelope = "enforced"
		}), "all", "", "dispatch keyring"},
		{"worker exact region ok (no master)", build(func(c *Config) {}), "worker", "geo2", ""},
		{"agent exact region ok (no master)", build(func(c *Config) {}), "agent", "geo2", ""},
		{"worker region mismatch fails", build(func(c *Config) {}), "worker", "geo9", "no security.dispatch keyring"},
		{"worker default covers empty region", build(func(c *Config) {
			c.Security.Dispatch = DispatchConfig{Default: &DispatchRegionKeys{Primary: DispatchKeyEntry{ID: "d1", Key: b64Key32B}}}
		}), "worker", "", ""},
		{"executor must not hold master", build(func(c *Config) { c.Security.EncryptionKey = b64Key32 }), "agent", "geo2", "must NOT hold the at-rest master"},
		{"executor must not hold previous keys", build(func(c *Config) { c.Security.PreviousKeys = []string{b64Key32} }), "worker", "geo2", "must NOT hold the at-rest master"},
		{"invalid role region fails even with default", build(func(c *Config) {
			c.Security.Dispatch = DispatchConfig{Default: &DispatchRegionKeys{Primary: DispatchKeyEntry{ID: "d1", Key: b64Key32B}}}
		}), "agent", "Bad Region", "not a valid region"},
		{"unknown role fails", build(func(c *Config) {}), "controller", "", "unknown role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateSecretsForRole(tc.role, tc.region)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDispatchEnvelopeModeStrict(t *testing.T) {
	c := baseSecretsConfig()
	c.Secrets.DispatchEnvelope = "bogus"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "dispatch_envelope") {
		t.Fatalf("bogus dispatch_envelope must fail, got %v", err)
	}
	// enforced promises ciphertext transport: without any keyring it is meaningless config.
	c.Secrets.DispatchEnvelope = "enforced"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "dispatch keyring") {
		t.Fatalf("enforced without keys must fail, got %v", err)
	}
	c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{
		"geo2": {Primary: DispatchKeyEntry{ID: "k1", Key: b64Key32B}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("enforced with a keyring must validate: %v", err)
	}
}

func TestDispatchKeyringValidation(t *testing.T) {
	mk := func(kr DispatchRegionKeys) *Config {
		c := baseSecretsConfig()
		c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{"geo2": kr}
		return c
	}
	// Bad key id.
	if err := mk(DispatchRegionKeys{Primary: DispatchKeyEntry{ID: "Bad_ID", Key: b64Key32}}).Validate(); err == nil {
		t.Fatal("non-slug key id must fail")
	}
	// Bad key material.
	if err := mk(DispatchRegionKeys{Primary: DispatchKeyEntry{ID: "k1", Key: "short"}}).Validate(); err == nil {
		t.Fatal("non-32-byte key must fail")
	}
	// Duplicate id within the ring.
	if err := mk(DispatchRegionKeys{
		Primary:  DispatchKeyEntry{ID: "k1", Key: b64Key32},
		Previous: []DispatchKeyEntry{{ID: "k1", Key: b64Key32B}},
	}).Validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate key id must fail, got %v", err)
	}
	// Duplicate key bytes under distinct ids.
	if err := mk(DispatchRegionKeys{
		Primary:  DispatchKeyEntry{ID: "k1", Key: b64Key32},
		Previous: []DispatchKeyEntry{{ID: "k2", Key: b64Key32}},
	}).Validate(); err == nil || !strings.Contains(err.Error(), "distinct keys") {
		t.Fatalf("duplicate key bytes must fail, got %v", err)
	}
	// Bad region slug.
	c := baseSecretsConfig()
	c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{
		"Bad Region": {Primary: DispatchKeyEntry{ID: "k1", Key: b64Key32}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("invalid region slug must fail")
	}
	// Valid ring with rotation entry.
	if err := mk(DispatchRegionKeys{
		Primary:  DispatchKeyEntry{ID: "geo2-2026b", Key: b64Key32},
		Previous: []DispatchKeyEntry{{ID: "geo2-2026a", Key: b64Key32B}},
	}).Validate(); err != nil {
		t.Fatalf("valid keyring must pass: %v", err)
	}
}

func TestDispatchDefaultTrustRules(t *testing.T) {
	// default alone (single trust domain): valid, no acknowledgement needed.
	c := baseSecretsConfig()
	c.Security.Dispatch.Default = &DispatchRegionKeys{Primary: DispatchKeyEntry{ID: "d1", Key: b64Key32B}}
	if err := c.Validate(); err != nil {
		t.Fatalf("default-only keyring must validate: %v", err)
	}
	// default + per-region keys without acknowledgement: rejected.
	c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{
		"geo2": {Primary: DispatchKeyEntry{ID: "k1", Key: b64Key32}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "shared_trust_acknowledged") {
		t.Fatalf("default+regions without acknowledgement must fail, got %v", err)
	}
	c.Security.Dispatch.SharedTrustAcknowledged = true
	if err := c.Validate(); err != nil {
		t.Fatalf("acknowledged mixed mode must validate: %v", err)
	}
	// acknowledgement without a default is meaningless config → rejected (strict-only).
	c2 := baseSecretsConfig()
	c2.Security.Dispatch.SharedTrustAcknowledged = true
	if err := c2.Validate(); err == nil {
		t.Fatal("shared_trust_acknowledged without a default keyring must fail")
	}
}
