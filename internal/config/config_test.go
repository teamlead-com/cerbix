package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("CERBIX_DB_PASS", "s3cr3t")
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	body := "database:\n  dsn: \"postgres://cerbix:${CERBIX_DB_PASS}@postgres:5432/cerbix?sslmode=disable\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(cfg.Database.DSN, "cerbix:s3cr3t@") {
		t.Fatalf("env not expanded in DSN: %q", cfg.Database.DSN)
	}
}

func TestParseMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: debug\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("default listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.HealthzPath != "/healthz" || cfg.Server.MetricsPath != "/metrics" {
		t.Fatalf("default paths = %#v", cfg.Server)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Fatalf("log = %#v", cfg.Log)
	}
}

// TestEgressPolicyDefaults locks the SSRF policy split: probe egress allows private
// (operator-chosen targets), notification egress denies private by default
// (editor-chosen destinations), and both block metadata. A regression here silently
// reopens SSRF on the alert-delivery path.
func TestEgressPolicyDefaults(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Prober.AllowPrivateIPs {
		t.Fatal("prober egress must allow private by default (monitors internal apps)")
	}
	if cfg.NotificationEgress.AllowPrivateIPs {
		t.Fatal("notification egress must DENY private by default (editor-controlled destinations)")
	}
	if cfg.Prober.AllowMetadataIPs || cfg.NotificationEgress.AllowMetadataIPs {
		t.Fatal("metadata IPs must be blocked by default on both policies")
	}
	// Explicit opt-in is honored (a deployment with genuine internal delivery endpoints).
	cfg, err = Parse([]byte("notification_egress:\n  allow_private_ips: true\n"))
	if err != nil || !cfg.NotificationEgress.AllowPrivateIPs {
		t.Fatalf("notification_egress override = %v err=%v", cfg.NotificationEgress.AllowPrivateIPs, err)
	}
}

// TestResultConfigDefaultsAndValidation locks the result-ingest config contract: absent
// block → secure enforce + 5m skew (default must not depend on the example file); partial
// block keeps the revision_mode default; strict enum + skew bounds.
func TestResultConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Result.RevisionMode != "enforce" {
		t.Fatalf("absent block: revision_mode = %q, want enforce", cfg.Result.RevisionMode)
	}
	if cfg.Result.AllowedSkew.Std() != 5*time.Minute {
		t.Fatalf("absent block: allowed_skew = %s, want 5m", cfg.Result.AllowedSkew.Std())
	}
	// Partial block: overriding skew must NOT reset revision_mode to empty.
	cfg, err = Parse([]byte("result:\n  allowed_skew: 1m\n"))
	if err != nil || cfg.Result.RevisionMode != "enforce" || cfg.Result.AllowedSkew.Std() != time.Minute {
		t.Fatalf("partial block: mode=%q skew=%s err=%v", cfg.Result.RevisionMode, cfg.Result.AllowedSkew.Std(), err)
	}
	if _, err := Parse([]byte("result:\n  revision_mode: observe\n")); err != nil {
		t.Fatalf("observe must be accepted: %v", err)
	}
	if _, err := Parse([]byte("result:\n  revision_mode: lax\n")); err == nil {
		t.Fatal("invalid revision_mode must be rejected")
	}
	if _, err := Parse([]byte("result:\n  allowed_skew: 2h\n")); err == nil {
		t.Fatal("allowed_skew > 1h must be rejected")
	}
	if _, err := Parse([]byte("result:\n  allowed_skew: 0s\n")); err == nil {
		t.Fatal("allowed_skew <= 0 must be rejected")
	}
}

func TestHeartbeatsRetentionDefaultAndValidation(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Heartbeats.RetentionDays != 30 {
		t.Fatalf("default retention_days = %d, want 30", cfg.Heartbeats.RetentionDays)
	}
	if _, err := Parse([]byte("heartbeats:\n  retention_days: 1\n")); err == nil {
		t.Fatal("retention_days below 2 should be rejected")
	}
	cfg, err = Parse([]byte("heartbeats:\n  retention_days: 7\n"))
	if err != nil || cfg.Heartbeats.RetentionDays != 7 {
		t.Fatalf("retention_days override = %d err=%v, want 7", cfg.Heartbeats.RetentionDays, err)
	}
}

func TestSecurityEncryptionKeyValidation(t *testing.T) {
	// Empty key = encryption off (valid).
	if _, err := Parse([]byte("log:\n  level: info\n")); err != nil {
		t.Fatalf("no key should be valid: %v", err)
	}
	// Non-base64 → rejected.
	if _, err := Parse([]byte("security:\n  encryption_key: \"not base64!!\"\n")); err == nil {
		t.Fatal("non-base64 key should be rejected")
	}
	// Base64 but wrong length (16 bytes) → rejected.
	if _, err := Parse([]byte("security:\n  encryption_key: \"AAAAAAAAAAAAAAAAAAAAAA==\"\n")); err == nil {
		t.Fatal("a 16-byte key should be rejected")
	}
	// Valid 32-byte base64 → accepted, decodes to 32 bytes.
	cfg, err := Parse([]byte("security:\n  encryption_key: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n"))
	if err != nil {
		t.Fatalf("valid 32-byte key rejected: %v", err)
	}
	if key, err := cfg.Security.EncryptionKeyBytes(); err != nil || len(key) != 32 {
		t.Fatalf("key bytes = %d err=%v, want 32", len(key), err)
	}
}

func TestSecurityPreviousKeys(t *testing.T) {
	valid := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	other := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

	// previous_keys without a primary is an error.
	if _, err := Parse([]byte("security:\n  previous_keys: [\"" + valid + "\"]\n")); err == nil {
		t.Fatal("previous_keys without encryption_key should be rejected")
	}
	// primary + a valid previous key → keyring of 2 (primary first).
	cfg, err := Parse([]byte("security:\n  encryption_key: \"" + valid + "\"\n  previous_keys: [\"" + other + "\"]\n"))
	if err != nil {
		t.Fatalf("valid rotation config rejected: %v", err)
	}
	keys, err := cfg.Security.Keys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("keyring len = %d err=%v, want 2", len(keys), err)
	}
	// a malformed previous key is rejected.
	if _, err := Parse([]byte("security:\n  encryption_key: \"" + valid + "\"\n  previous_keys: [\"bad!!\"]\n")); err == nil {
		t.Fatal("malformed previous key should be rejected")
	}
}

func TestOIDCButtonLabelDefault(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.OIDC.ButtonLabel != "Continue with SSO" {
		t.Fatalf("default oidc.button_label = %q, want %q", cfg.OIDC.ButtonLabel, "Continue with SSO")
	}
	cfg, _ = Parse([]byte("oidc:\n  button_label: \"Continue with Okta\"\n"))
	if cfg.OIDC.ButtonLabel != "Continue with Okta" {
		t.Fatalf("override oidc.button_label = %q", cfg.OIDC.ButtonLabel)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("server:\n  bogus: 1\n"))
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestValidateRejectsBadLogLevel(t *testing.T) {
	_, err := Parse([]byte("log:\n  level: verbose\n"))
	if err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("expected log.level error, got %v", err)
	}
}

func TestValidateRejectsNonSlashPath(t *testing.T) {
	_, err := Parse([]byte("server:\n  metrics_path: metrics\n"))
	if err == nil || !strings.Contains(err.Error(), "server.metrics_path") {
		t.Fatalf("expected metrics_path error, got %v", err)
	}
}

func TestValidateRejectsEmptyListen(t *testing.T) {
	_, err := Parse([]byte("server:\n  listen: \"\"\n"))
	if err == nil || !strings.Contains(err.Error(), "server.listen") {
		t.Fatalf("expected listen error, got %v", err)
	}
}

func TestValidateRejectsBadTrustedProxyCIDR(t *testing.T) {
	_, err := Parse([]byte("server:\n  trusted_proxy_cidrs:\n    - 10.0.0.0/8\n    - not-a-cidr\n"))
	if err == nil || !strings.Contains(err.Error(), "server.trusted_proxy_cidrs") {
		t.Fatalf("expected trusted_proxy_cidrs error, got %v", err)
	}
}

func TestTrustedProxyNetsParses(t *testing.T) {
	cfg, err := Parse([]byte("server:\n  trusted_proxy_cidrs:\n    - 10.0.0.0/8\n    - 172.16.0.0/12\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	nets, err := cfg.Server.TrustedProxyNets()
	if err != nil {
		t.Fatalf("TrustedProxyNets: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("want 2 nets, got %d", len(nets))
	}
}

func TestValidateOIDCPartial(t *testing.T) {
	_, err := Parse([]byte("oidc:\n  issuer: https://keycloak.example/realms/x\n"))
	if err == nil || !strings.Contains(err.Error(), "oidc.client_id") {
		t.Fatalf("expected oidc partial error, got %v", err)
	}
}

func TestValidateOIDCRequiresDatabase(t *testing.T) {
	yaml := "oidc:\n" +
		"  issuer: https://keycloak.example/realms/x\n" +
		"  client_id: cerbix\n" +
		"  redirect_url: https://cerbix.example/auth/callback\n"
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "database.dsn is required") {
		t.Fatalf("expected database requirement error, got %v", err)
	}
}

func TestValidateOIDCComplete(t *testing.T) {
	yaml := "oidc:\n" +
		"  issuer: https://keycloak.example/realms/x\n" +
		"  client_id: cerbix\n" +
		"  redirect_url: https://cerbix.example/auth/callback\n" +
		"database:\n" +
		"  dsn: postgres://localhost/cerbix\n"
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.OIDC.Enabled() {
		t.Fatal("oidc should be enabled")
	}
}

func TestSessionDefaultsAndTTLParsing(t *testing.T) {
	cfg, err := Parse([]byte("session:\n  ttl: 2h\n  secure: false\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Session.CookieName != "cerbix_session" {
		t.Fatalf("default cookie name = %q", cfg.Session.CookieName)
	}
	if cfg.Session.TTL.Std() != 2*time.Hour {
		t.Fatalf("ttl = %v, want 2h", cfg.Session.TTL.Std())
	}
	if cfg.Session.Secure {
		t.Fatal("secure should be false as configured")
	}
}

func TestSessionRejectsBadTTL(t *testing.T) {
	if _, err := Parse([]byte("session:\n  ttl: nonsense\n")); err == nil {
		t.Fatal("expected error for bad ttl")
	}
}

func TestLocalRequiresDatabase(t *testing.T) {
	_, err := Parse([]byte("local:\n  enabled: true\n"))
	if err == nil || !strings.Contains(err.Error(), "database.dsn is required when local login") {
		t.Fatalf("expected local db requirement, got %v", err)
	}
}

func TestAdminPasswordLength(t *testing.T) {
	yaml := "database:\n  dsn: postgres://x/y\n" +
		"local:\n  enabled: true\n" +
		"security:\n  admin_email: a@x\n  admin_password: short\n"
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "shorter than") {
		t.Fatalf("expected short-password error, got %v", err)
	}
}

func TestAdminPasswordNeedsEmail(t *testing.T) {
	yaml := "database:\n  dsn: postgres://x/y\n" +
		"local:\n  enabled: true\n" +
		"security:\n  admin_password: longenough1\n"
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "admin_email is required") {
		t.Fatalf("expected email-required error, got %v", err)
	}
}

func TestLocalValid(t *testing.T) {
	yaml := "database:\n  dsn: postgres://x/y\n" +
		"local:\n  enabled: true\n" +
		"security:\n  admin_email: a@x\n  admin_password: longenough1\n"
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("valid local config rejected: %v", err)
	}
	if !cfg.Local.Enabled || cfg.Local.MinPasswordLength != 8 {
		t.Fatalf("local config = %+v", cfg.Local)
	}
}

func TestDefaultSessionSecure(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Session.Secure {
		t.Fatal("session.secure should default to true")
	}
}

// TestExpandEnvStrict proves undefined vars fail loudly (no silent security downgrade),
// defined/defined-empty expand, and $$ stays a literal $.
func TestExpandEnvStrict(t *testing.T) {
	t.Setenv("CBX_DEFINED", "val")
	t.Setenv("CBX_EMPTY", "")
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: ${CBX_UNDEFINED_XYZ}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("undefined env var must fail Load, not silently blank")
	}
	// Defined + defined-empty expand; $$ stays a literal $.
	got, err := expandEnvStrict("a=${CBX_DEFINED} b=${CBX_EMPTY} c=$$literal")
	if err != nil {
		t.Fatalf("strict expand: %v", err)
	}
	if got != "a=val b= c=$literal" {
		t.Fatalf("expand = %q, want %q", got, "a=val b= c=$literal")
	}
}

func TestProvidersFileValid(t *testing.T) {
	cfg, err := Parse([]byte("providers:\n  file:\n    platform:\n      directory: /etc/cerbix/monitoring.d\n      scope:\n        type: instance\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := cfg.Providers.File["platform"]
	if !ok {
		t.Fatal("provider not parsed")
	}
	// Defaults applied by normalizeProviders.
	if p.Debounce.Std().String() != "2s" || p.ResyncInterval.Std().String() != "30s" {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if p.Limits.MaxFiles != 1000 || p.Limits.MaxManagedMonitors != 5000 {
		t.Fatalf("limit defaults not applied: %+v", p.Limits)
	}
}

func TestProvidersFileRejections(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"bad name", "providers:\n  file:\n    BadName:\n      directory: /x\n      scope: {type: instance}\n", "invalid provider name"},
		{"relative dir", "providers:\n  file:\n    p:\n      directory: rel/dir\n      scope: {type: instance}\n", "absolute path"},
		{"root dir", "providers:\n  file:\n    p:\n      directory: /\n      scope: {type: instance}\n", "filesystem root"},
		{"missing scope", "providers:\n  file:\n    p:\n      directory: /x\n", "scope.type is required"},
		{"bad scope", "providers:\n  file:\n    p:\n      directory: /x\n      scope: {type: cluster}\n", "scope.type must be"},
		{"instance with org", "providers:\n  file:\n    p:\n      directory: /x\n      scope: {type: instance, organization: acme}\n", "must not set"},
		{"org without org", "providers:\n  file:\n    p:\n      directory: /x\n      scope: {type: organization}\n", "requires scope.organization"},
		{"project without project", "providers:\n  file:\n    p:\n      directory: /x\n      scope: {type: project, organization: acme}\n", "requires scope.organization and scope.project"},
		{"bad debounce", "providers:\n  file:\n    p:\n      directory: /x\n      debounce: 5m\n      scope: {type: instance}\n", "debounce must be"},
		{"overlap roots", "providers:\n  file:\n    a:\n      directory: /etc/cerbix/mon\n      scope: {type: instance}\n    b:\n      directory: /etc/cerbix/mon/sub\n      scope: {type: instance}\n", "overlaps"},
		{"limit over max", "providers:\n  file:\n    p:\n      directory: /x\n      scope: {type: instance}\n      limits: {max_files: 999999999}\n", "safety maximum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
