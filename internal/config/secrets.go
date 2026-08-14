// Secret-inventory feature configuration (spec func-secret-inventory §4.1/§4.7,
// FR-020/NFR-015). Strict-only: every rule here fails config load — there is no
// warn-and-continue and no runtime self-healing for these contracts.
package config

import (
	"fmt"
	"regexp"
)

// SecretsConfig is the explicit feature switch for the project secret inventory.
// Disabled (the default) leaves every existing deployment untouched: the Secrets
// API answers 404 feature_disabled and `*_ref` bundles reject as bindable errors.
// Enabling it requires the at-rest master key AND a dispatch keyring on the roles
// that materialize or execute credentialed jobs — enforced per-role at startup in
// cli (the role is a CLI flag, unknown at config load).
type SecretsConfig struct {
	Enabled bool `yaml:"enabled"`
	// DispatchEnvelope selects the credential transport mode. Allowed values:
	// "" (legacy plaintext dispatch for pre-existing inline-value monitors) and
	// "enforced" (credential fields ride only the encrypted envelope). Monitors
	// with `*_ref` secrets require "enforced" — that wiring lands with the
	// envelope itself (spec §4.7 rollout, iteration 2); the value is validated
	// now so configs are stable across the rollout.
	DispatchEnvelope string `yaml:"dispatch_envelope"`
}

// DispatchKeyEntry is one dispatch key: a stable id (selects the key on decrypt;
// part of the envelope AAD) and a base64-encoded 32-byte AES-256 key.
type DispatchKeyEntry struct {
	ID  string `yaml:"id"`
	Key string `yaml:"key"`
}

// DispatchRegionKeys is one region's dispatch keyring: core encrypts with Primary;
// executors decrypt by key id (Primary or Previous), so a dispatch-key rotation can
// drain queued/DLQ payloads before the old key is retired (its own runbook —
// at-rest `reencrypt` does not apply to payloads).
type DispatchRegionKeys struct {
	Primary  DispatchKeyEntry   `yaml:"primary"`
	Previous []DispatchKeyEntry `yaml:"previous"`
}

// DispatchConfig maps regions to dispatch keyrings. The at-rest master key NEVER
// plays this role: a leaked executor must expose at most its own region's retained
// payloads (spec §4.7 trust/exposure statements, D-0155). Default is a fallback
// keyring for deployments that genuinely run one trust domain; combining it with
// per-region keys requires an explicit SharedTrustAcknowledged, which widens the
// recorded exposure statement.
type DispatchConfig struct {
	Regions                 map[string]DispatchRegionKeys `yaml:"regions"`
	Default                 *DispatchRegionKeys           `yaml:"default"`
	SharedTrustAcknowledged bool                          `yaml:"shared_trust_acknowledged"`
}

// Configured reports whether any dispatch keyring is present.
func (d DispatchConfig) Configured() bool {
	return len(d.Regions) > 0 || d.Default != nil
}

// keyIDRe bounds dispatch key ids: they travel inside the envelope and its AAD, so
// they are slugs, not free text.
var keyIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// regionRe mirrors domain's region-slug bound (worker-pool labels) for the keyring map.
var regionRe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// validateSecrets enforces the structural rules for the secrets feature and the
// dispatch keyrings. Role-dependent presence rules (materializing roles need the
// master key + a keyring; executors need their region's keyring) live in cli where
// the role is known.
func (c *Config) validateSecrets() error {
	if c.Secrets.DispatchEnvelope != "" && c.Secrets.DispatchEnvelope != "enforced" {
		return fmt.Errorf("secrets.dispatch_envelope must be empty or \"enforced\": %q", c.Secrets.DispatchEnvelope)
	}
	if c.Secrets.Enabled && c.Security.EncryptionKey == "" {
		return fmt.Errorf("secrets.enabled requires security.encryption_key (the inventory is fail-closed: no plaintext fallback)")
	}
	d := c.Security.Dispatch
	if c.Secrets.Enabled && !d.Configured() {
		return fmt.Errorf("secrets.enabled requires at least one security.dispatch keyring (regions or default)")
	}
	// Mixing an explicit per-region keyring with a default fallback means one key can
	// open payloads outside its region — allowed only with the acknowledged wider trust.
	if d.Default != nil && len(d.Regions) > 0 && !d.SharedTrustAcknowledged {
		return fmt.Errorf("security.dispatch.default alongside per-region keyrings requires security.dispatch.shared_trust_acknowledged: true (one key then opens more than one region's payloads)")
	}
	if d.SharedTrustAcknowledged && d.Default == nil {
		return fmt.Errorf("security.dispatch.shared_trust_acknowledged is set but there is no security.dispatch.default keyring")
	}
	for region, kr := range d.Regions {
		if !regionRe.MatchString(region) {
			return fmt.Errorf("security.dispatch.regions: region %q must match %s", region, regionRe.String())
		}
		if err := validateKeyring(fmt.Sprintf("security.dispatch.regions[%s]", region), kr); err != nil {
			return err
		}
	}
	if d.Default != nil {
		if err := validateKeyring("security.dispatch.default", *d.Default); err != nil {
			return err
		}
	}
	return nil
}

// validateKeyring checks one keyring: every entry has a slug id and a 32-byte key;
// ids are unique AND key bytes are distinct within the ring (a duplicated key under
// two ids would silently defeat a rotation).
func validateKeyring(name string, kr DispatchRegionKeys) error {
	entries := append([]DispatchKeyEntry{kr.Primary}, kr.Previous...)
	seenID := make(map[string]bool, len(entries))
	seenKey := make(map[string]bool, len(entries))
	for i, e := range entries {
		where := fmt.Sprintf("%s.primary", name)
		if i > 0 {
			where = fmt.Sprintf("%s.previous[%d]", name, i-1)
		}
		if !keyIDRe.MatchString(e.ID) {
			return fmt.Errorf("%s.id must match %s: %q", where, keyIDRe.String(), e.ID)
		}
		key, err := decodeKey(where+".key", e.Key)
		if err != nil {
			return err
		}
		if seenID[e.ID] {
			return fmt.Errorf("%s: key id %q is duplicated within the keyring", name, e.ID)
		}
		seenID[e.ID] = true
		if seenKey[string(key)] {
			return fmt.Errorf("%s: key bytes of %q duplicate another entry (distinct ids must carry distinct keys)", name, e.ID)
		}
		seenKey[string(key)] = true
	}
	return nil
}
