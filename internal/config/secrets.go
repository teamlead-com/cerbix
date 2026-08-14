// Secret-inventory feature configuration (spec func-secret-inventory §4.1/§4.7,
// FR-020/NFR-015). Strict-only: every rule here fails config load or role startup —
// there is no warn-and-continue and no runtime self-healing for these contracts.
package config

import (
	"fmt"
	"regexp"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// SecretsConfig is the explicit feature switch for the project secret inventory.
// Disabled (the default) leaves every existing deployment untouched: the Secrets
// API answers 404 feature_disabled and `*_ref` bundles reject as bindable errors.
type SecretsConfig struct {
	Enabled bool `yaml:"enabled"`
	// DispatchEnvelope selects the credential transport mode: "" (legacy plaintext
	// dispatch for pre-existing inline-value monitors) or "enforced" (credential
	// fields ride only the encrypted envelope). `enabled: true` REQUIRES "enforced"
	// (spec §4.7): the inventory may never dispatch over legacy plaintext, so
	// during an expand rollout the keys ship first with enabled=false, and enabling
	// flips only after the v2 wire barrier is in place.
	DispatchEnvelope string `yaml:"dispatch_envelope"`
}

// EnvelopeEnforced reports whether credential envelopes are the required transport.
func (s SecretsConfig) EnvelopeEnforced() bool { return s.DispatchEnvelope == "enforced" }

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

// HasRegion reports whether region is covered by an explicit keyring or the default.
func (d DispatchConfig) HasRegion(region string) bool {
	if _, ok := d.Regions[region]; ok {
		return true
	}
	return d.Default != nil
}

// keyIDRe bounds dispatch key ids: they travel inside the envelope and its AAD, so
// they are slugs, not free text.
var keyIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// validateSecrets enforces the ROLE-AGNOSTIC rules at config load: the envelope-mode
// enum, the enabled→enforced coupling, and the structural validity of every supplied
// keyring. WHICH keys a process must (or must not) hold depends on --role, which the
// loader does not know — that lives in ValidateSecretsForRole, invoked fail-fast by
// cli before any runtime wiring.
func (c *Config) validateSecrets() error {
	if c.Secrets.DispatchEnvelope != "" && c.Secrets.DispatchEnvelope != "enforced" {
		return fmt.Errorf("secrets.dispatch_envelope must be empty or \"enforced\": %q", c.Secrets.DispatchEnvelope)
	}
	// The inventory may never dispatch over legacy plaintext (spec §4.7): keys can ship
	// ahead (enforced with enabled=false, the expand phase), but enabling without the
	// envelope barrier is a hard error, not a later wiring detail.
	if c.Secrets.Enabled && !c.Secrets.EnvelopeEnforced() {
		return fmt.Errorf("secrets.enabled requires secrets.dispatch_envelope: \"enforced\" (ref-based credentials never ride legacy plaintext dispatch)")
	}
	// Enforced mode promises ciphertext transport — meaningless without any keyring,
	// whatever the role (role-specific coverage is checked per role).
	if c.Secrets.EnvelopeEnforced() && !c.Security.Dispatch.Configured() {
		return fmt.Errorf("secrets.dispatch_envelope: \"enforced\" requires at least one security.dispatch keyring")
	}
	d := c.Security.Dispatch
	// Mixing an explicit per-region keyring with a default fallback means one key can
	// open payloads outside its region — allowed only with the acknowledged wider trust.
	if d.Default != nil && len(d.Regions) > 0 && !d.SharedTrustAcknowledged {
		return fmt.Errorf("security.dispatch.default alongside per-region keyrings requires security.dispatch.shared_trust_acknowledged: true (one key then opens more than one region's payloads)")
	}
	if d.SharedTrustAcknowledged && d.Default == nil {
		return fmt.Errorf("security.dispatch.shared_trust_acknowledged is set but there is no security.dispatch.default keyring")
	}
	for region, kr := range d.Regions {
		if !domain.ValidRegion(region) {
			return fmt.Errorf("security.dispatch.regions: region %q is not a valid region name", region)
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

// ValidateSecretsForRole enforces the role-dependent key presence/absence rules
// (spec §4.1/§4.7). Pure and directly testable; cli calls it fail-fast right after
// config load, for every role including the DB-less agent:
//
//   - all/api/scheduler (materializing roles): decrypt at-rest values and seal
//     envelopes, so `secrets.enabled` requires the at-rest master AND at least one
//     dispatch keyring; `enforced` alone (expand phase) likewise needs both — it
//     moves pre-existing inline-value monitors onto envelopes, which are sealed
//     from master-decrypted values.
//   - worker/agent (executors): must NEVER hold the at-rest master or its
//     previous keys, including while envelopes are disabled; when envelopes are
//     in play they additionally need the dispatch keyring covering THEIR region
//     (or the default). "Executors never receive the master"
//     is a hard boundary (NFR-015): a master key on an executor profile is a
//     misconfiguration, rejected rather than ignored.
//
// The role region (empty → domain.DefaultRegion) is validated even when a default
// keyring would cover anything — an invalid --region is a config error, not a
// routing surprise.
func (c *Config) ValidateSecretsForRole(role, region string) error {
	envelopes := c.Secrets.Enabled || c.Secrets.EnvelopeEnforced()
	switch role {
	case "all", "api", "scheduler":
		if !envelopes {
			return nil
		}
		if c.Security.EncryptionKey == "" {
			return fmt.Errorf("secrets: role %s requires security.encryption_key (materializing roles decrypt at-rest values; fail-closed, no plaintext fallback)", role)
		}
		if !c.Security.Dispatch.Configured() {
			return fmt.Errorf("secrets: role %s requires a security.dispatch keyring to seal credential envelopes", role)
		}
		return nil
	case "worker", "agent":
		if region == "" {
			region = domain.DefaultRegion
		}
		if !domain.ValidRegion(region) {
			return fmt.Errorf("secrets: --region %q is not a valid region name", region)
		}
		// The master key is never executor material, even while the feature and
		// envelope mode are disabled. Allowing it in an inactive profile would make
		// a later feature flip silently widen the worker's trust boundary.
		if c.Security.EncryptionKey != "" || len(c.Security.PreviousKeys) > 0 {
			return fmt.Errorf("secrets: role %s must NOT hold the at-rest master key (security.encryption_key/previous_keys) — executors receive only their region's dispatch keys", role)
		}
		if !envelopes {
			return nil
		}
		if !c.Security.Dispatch.HasRegion(region) {
			return fmt.Errorf("secrets: role %s in region %q has no security.dispatch keyring for it (configure security.dispatch.regions[%q] or a default)", role, region, region)
		}
		return nil
	default:
		return fmt.Errorf("secrets: unknown role %q", role)
	}
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
