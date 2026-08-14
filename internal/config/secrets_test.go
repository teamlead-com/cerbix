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

func TestSecretsEnabledRequiresMasterKeyAndKeyring(t *testing.T) {
	c := defaultsConfig()
	c.Secrets.Enabled = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "security.encryption_key") {
		t.Fatalf("enabled without master key must fail with the key hint, got %v", err)
	}
	c.Security.EncryptionKey = b64Key32
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "dispatch keyring") {
		t.Fatalf("enabled without any dispatch keyring must fail, got %v", err)
	}
	c.Security.Dispatch.Regions = map[string]DispatchRegionKeys{
		"geo2": {Primary: DispatchKeyEntry{ID: "geo2-2026a", Key: b64Key32B}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("enabled with master key + region keyring must validate: %v", err)
	}
}

func TestDispatchEnvelopeModeStrict(t *testing.T) {
	c := baseSecretsConfig()
	c.Secrets.DispatchEnvelope = "bogus"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "dispatch_envelope") {
		t.Fatalf("bogus dispatch_envelope must fail, got %v", err)
	}
	c.Secrets.DispatchEnvelope = "enforced"
	if err := c.Validate(); err != nil {
		t.Fatalf("enforced must validate: %v", err)
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
