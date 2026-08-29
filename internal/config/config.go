// Package config loads and strictly validates the cerbix service configuration.
//
// Configuration is strict-only: the loader fails fast on unknown keys, invalid
// values, or missing required fields. There is no runtime self-healing, silent
// downgrade, or warn-and-continue behavior. Defaults are applied only here, in
// the central loader, and only for optional fields.
package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Config is the validated configuration snapshot used by the runtime.
type Config struct {
	Server             ServerConfig             `yaml:"server"`
	Log                LogConfig                `yaml:"log"`
	Database           DatabaseConfig           `yaml:"database"`
	RabbitMQ           RabbitMQConfig           `yaml:"rabbitmq"`
	OIDC               OIDCConfig               `yaml:"oidc"`
	Local              LocalAuthConfig          `yaml:"local"`
	Session            SessionConfig            `yaml:"session"`
	Prober             ProberConfig             `yaml:"prober"`
	NotificationEgress NotificationEgressConfig `yaml:"notification_egress"`
	Result             ResultConfig             `yaml:"result"`
	Heartbeats         HeartbeatsConfig         `yaml:"heartbeats"`
	Security           SecurityConfig           `yaml:"security"`
	Secrets            SecretsConfig            `yaml:"secrets"`
	Services           ServicesConfig           `yaml:"services"`
	Mail               MailConfig               `yaml:"mail"`
	Pull               PullConfig               `yaml:"pull"`
	Providers          ProvidersConfig          `yaml:"providers"`
	Gate               GateConfig               `yaml:"gate"`
}

// PullConfig configures the HTTP-pull transport (an alternative to RabbitMQ for a geo
// that must not reach the broker). On the central side (api/scheduler): Regions lists
// the regions served by pull agents; Token is an optional catch-all secret that
// authorizes any region; Agents scopes a token to a single region (an agent with a
// per-region token can only claim/heartbeat that region). On the agent side
// (--role agent): ServerURL is the central base URL and Token the agent's secret; the
// region comes from --region.
type PullConfig struct {
	Regions   []string    `yaml:"regions"`
	Token     string      `yaml:"token"`
	Agents    []PullAgent `yaml:"agents"`
	ServerURL string      `yaml:"server_url"`
}

// PullAgent binds a per-region agent token: this token authorizes only Region.
type PullAgent struct {
	Region string `yaml:"region"`
	Token  string `yaml:"token"`
}

// ServerConfig holds HTTP listener settings.
type ServerConfig struct {
	Listen      string `yaml:"listen"`
	HealthzPath string `yaml:"healthz_path"`
	ReadyzPath  string `yaml:"readyz_path"`
	MetricsPath string `yaml:"metrics_path"`
	// TrustedProxyCount is how many reverse-proxy hops sit in front of cerbix
	// (e.g. 1 for a lone Traefik, 2 behind Cloudflare→Traefik). The rate-limiter's
	// client IP is taken that many entries from the right of the
	// X-Forwarded-For + peer chain, so a client can't spoof its way into a fresh
	// bucket. 0 (default) trusts NO XFF and keys on the direct peer — safe against
	// spoofing, but keys every request behind a proxy to the proxy's IP, so set
	// this to your real hop count in production.
	TrustedProxyCount int `yaml:"trusted_proxy_count"`
	// TrustedProxyCIDRs lists the networks (CIDR notation) that our own reverse
	// proxies live in. When non-empty this SUPERSEDES TrustedProxyCount: the
	// rate-limiter honors X-Forwarded-For only when the direct peer is inside one
	// of these networks, then walks the chain right-to-left skipping addresses
	// that are themselves trusted proxies — the first untrusted address is the
	// client. A request reaching cerbix directly (peer not in any trusted CIDR)
	// has its XFF ignored entirely, so it can't forge a limiter bucket even in a
	// dual-path deployment where both the proxy and the origin are reachable.
	TrustedProxyCIDRs []string `yaml:"trusted_proxy_cidrs"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DatabaseConfig holds the Postgres connection. Optional in early iterations:
// when DSN is empty the service runs without a database (scaffold mode).
type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

// RabbitMQConfig holds the broker connection. Optional in early iterations:
// when URL is empty the service uses the in-process dispatcher.
type RabbitMQConfig struct {
	URL string `yaml:"url"`
	// ManagementURL is the RabbitMQ HTTP management API base (e.g.
	// http://user:pass@host:15672). Optional: when empty it is derived from URL
	// by scheme (amqp→http://host:15672, amqps→https://host:15671 — a non-standard
	// AMQP port is not carried over). Used by the
	// API to list live worker-pool regions (queues with active consumers).
	ManagementURL string `yaml:"management_url"`
}

// OIDCConfig holds the OpenID Connect provider settings. Any OIDC-compliant
// issuer works (Keycloak, Auth0, Okta, Google, Entra ID, …). Optional: when
// Issuer is empty, OIDC login is disabled (scaffold / local-only mode).
type OIDCConfig struct {
	Issuer                string   `yaml:"issuer"`
	ClientID              string   `yaml:"client_id"`
	ClientSecret          string   `yaml:"client_secret"`
	RedirectURL           string   `yaml:"redirect_url"`
	Scopes                []string `yaml:"scopes"`
	PostLogoutRedirectURL string   `yaml:"post_logout_redirect_url"`
	// ButtonLabel is the text shown on the OIDC sign-in button (e.g. "Continue
	// with Okta"). Defaults to "Continue with SSO".
	ButtonLabel string `yaml:"button_label"`
	// BootstrapAdminEmails are promoted to global admin on login (JIT).
	BootstrapAdminEmails []string `yaml:"bootstrap_admin_emails"`
}

// Enabled reports whether OIDC login is configured.
func (o OIDCConfig) Enabled() bool { return o.Issuer != "" }

// LocalAuthConfig controls the built-in username/password login, available
// alongside or instead of an OIDC provider.
type LocalAuthConfig struct {
	Enabled           bool `yaml:"enabled"`
	MinPasswordLength int  `yaml:"min_password_length"`
	// LoginRateLimitPerMinute bounds local-login attempts per client IP per
	// minute (brute-force mitigation). 0 disables the limit.
	LoginRateLimitPerMinute int `yaml:"login_rate_limit_per_minute"`
}

// SessionConfig controls server-side session cookies.
type SessionConfig struct {
	CookieName string   `yaml:"cookie_name"`
	TTL        Duration `yaml:"ttl"`
	Secure     bool     `yaml:"secure"`
}

// ProberConfig is the SSRF policy for probe targets. cerbix monitors internal
// services, so allow_private_ips defaults on; allow_metadata_ips defaults off to
// block link-local / cloud instance metadata (169.254.169.254).
type ProberConfig struct {
	AllowPrivateIPs  bool `yaml:"allow_private_ips"`
	AllowMetadataIPs bool `yaml:"allow_metadata_ips"`
}

// NotificationEgressConfig is a SEPARATE SSRF policy for OUTBOUND alert delivery
// (webhooks, Slack/notify HTTP, SMTP). Unlike probe egress it defaults to deny-private:
// a probe target is chosen by an operator monitoring internal services, but a
// notification destination can be set by a project editor, so it must not be able to
// reach loopback/RFC1918/ULA/CGNAT/metadata by default. Opt back in per deployment only
// when internal delivery endpoints are genuinely required. (OIDC discovery/JWKS/token is
// a distinct, operator-trusted path and is NOT governed by this policy — see D-0141.)
type NotificationEgressConfig struct {
	AllowPrivateIPs  bool `yaml:"allow_private_ips"`
	AllowMetadataIPs bool `yaml:"allow_metadata_ips"`
}

// ResultConfig governs result ingest (spec func-result-protocol). AllowedSkew bounds how
// far ahead of statement_timestamp() a scheduled result's observed_at may be before it is
// quarantined. RevisionMode selects the active execution_revision gate policy (enforce |
// observe); the default is the secure `enforce`, including when the whole `result:` block
// is absent (the default must not depend on which example file was copied — see D-0142 /
// spec §10).
type ResultConfig struct {
	AllowedSkew  Duration `yaml:"allowed_skew"`
	RevisionMode string   `yaml:"revision_mode"` // enforce | observe
}

// ServicesConfig bounds service fan-out (func-service-reliability §10.10).
//
// Each value has a default and a HARD MAXIMUM the domain owns. Validate REJECTS a value
// outside [1, hard max] at startup: silently mapping an illegal value to a legal one would
// mean the config the operator wrote and the config the system runs are different configs,
// which is exactly what FR-003's fail-fast contract forbids. The store additionally refuses
// to run past the hard maxima as defense in depth.
type ServicesConfig struct {
	// MaxServicesPerProject caps declaration growth (default 50, hard max 200).
	MaxServicesPerProject int `yaml:"max_services_per_project"`
	// MaxMembersPerRevision caps aggregation width (default 50, hard max 200).
	MaxMembersPerRevision int `yaml:"max_members_per_revision"`
	// MaxServicesPerMonitor caps epoch and ingest fan-out from one monitor write
	// (default 10, hard max 25).
	MaxServicesPerMonitor int `yaml:"max_services_per_monitor"`
}

// HeartbeatsConfig controls raw heartbeat retention. RetentionDays bounds how many
// days of raw heartbeats are kept (older daily partitions are dropped); it also
// bounds the daily-availability rollup's recompute window, so the rollup never
// touches days whose raw data has been dropped (long-range availability lives in
// the frozen rollup rows).
type HeartbeatsConfig struct {
	RetentionDays int `yaml:"retention_days"`
}

// GateConfig bounds the reliability gate (func-reliability-gate §5a): how many decisions a
// process and a principal may have in flight, how fast either may ask, how long a decision
// transaction may run, and how the decision ledger's daily partitions are created and
// removed. Every key has a default, a minimum and a maximum the spec owns, and Validate
// REJECTS a value outside them at startup, naming the key and the range — the config the
// operator wrote is the config that runs. The bounds are PROCESS-LOCAL by contract: every
// api/all replica enforces its own copies, so the cluster allowance scales with replicas.
type GateConfig struct {
	// EvaluateInflightProcess caps decisions in flight per PROCESS; the (n+1)th is 429
	// `process_inflight` before any transaction. Default 8, range 1..64.
	EvaluateInflightProcess int `yaml:"evaluate_inflight_process"`
	// EvaluateInflightPrincipal caps decisions in flight per principal (token or user);
	// 429 `principal_inflight`. Default 2, range 1..16.
	EvaluateInflightPrincipal int `yaml:"evaluate_inflight_principal"`
	// EvaluateRatePrincipalPerMinute is a token bucket per principal: capacity = the value,
	// refill = value/60 per second; drained → 429 `principal_rate`. Default 10, range 1..600.
	EvaluateRatePrincipalPerMinute int `yaml:"evaluate_rate_principal_per_minute"`
	// EvaluateRateProcessPerMinute is the process-wide bucket, same algorithm; drained → 429
	// `process_rate`. Default 60, range 1..600. Raising it raises the ledger's capacity
	// table (§5a) — read that first.
	EvaluateRateProcessPerMinute int `yaml:"evaluate_rate_process_per_minute"`
	// EvaluateTxBudgetMs is the begin-through-commit budget of the decision transaction,
	// applied through the store's deadline wrapper. Default 5000, range 500..30000.
	EvaluateTxBudgetMs int `yaml:"evaluate_tx_budget_ms"`
	// DecisionRetentionDays is the ledger retention (D10): daily partitions whose upper bound
	// is at or before now − retention are detached, then dropped a pass later. Default 90,
	// range 7..365.
	DecisionRetentionDays int `yaml:"decision_retention_days"`
	// DecisionPartitionLeadDays is how many days of partitions are kept created ahead — the
	// writable horizon the gauge measures against. Default 7, range 2..30.
	DecisionPartitionLeadDays int `yaml:"decision_partition_lead_days"`
	// DecisionPartitionCreateMax is how many days are created (create + attach) per
	// maintenance pass, nearest horizon first. Default 3, range 1..8; load refuses
	// create_max × floor(86400 / purge_every seconds) < 2.
	DecisionPartitionCreateMax int `yaml:"decision_partition_create_max"`
	// DecisionPurgeEvery is the maintenance cadence on the gate's own fenced session.
	// Default 1h, range 5m..24h.
	DecisionPurgeEvery Duration `yaml:"decision_purge_every"`
	// DecisionPurgeMaxPartitions is how many removal STAGE-OPS (finalize, drop, detach) a
	// pass performs; steady state is two a day. Default 8, range 1..48; load refuses
	// max × floor(86400 / purge_every seconds) < 4.
	DecisionPurgeMaxPartitions int `yaml:"decision_purge_max_partitions"`
}

// SecurityConfig controls encryption of secrets stored at rest (webhook signing
// secrets, notification-channel credentials). EncryptionKey is the primary
// base64-encoded 32-byte (AES-256) key used to encrypt; empty disables encryption
// (secrets stored as plaintext, the pre-existing behavior). PreviousKeys are
// additional keys tried on decrypt during a rotation — data still encrypted under
// an old key stays readable until `cerbix reencrypt` migrates it to the primary.
type SecurityConfig struct {
	EncryptionKey string   `yaml:"encryption_key"`
	PreviousKeys  []string `yaml:"previous_keys"`
	// Dispatch holds the per-region dispatch keyrings for credential envelopes
	// (spec func-secret-inventory §4.7). SEPARATE from the at-rest keyring above:
	// executors receive only their region's dispatch keys, never the master.
	Dispatch DispatchConfig `yaml:"dispatch"`
	// AdminEmail/AdminPassword define the instance's global admin account,
	// created once on an empty system at first startup (requires local login to
	// be enabled). The password is never generated or logged; rotate it via the
	// UI/API after first login.
	AdminEmail    string `yaml:"admin_email"`
	AdminPassword string `yaml:"admin_password"`
}

// MailConfig configures outbound email for status-page subscribers (confirmation
// and incident notifications). Optional: when unset, subscription endpoints
// report that email is not configured. public_base_url builds confirm/unsubscribe
// links, so it is required when mail is enabled.
type MailConfig struct {
	SMTPHost      string `yaml:"smtp_host"`
	SMTPPort      int    `yaml:"smtp_port"`
	SMTPUsername  string `yaml:"smtp_username"`
	SMTPPassword  string `yaml:"smtp_password"`
	From          string `yaml:"from"`
	PublicBaseURL string `yaml:"public_base_url"`
}

// Enabled reports whether outbound email is configured.
func (m MailConfig) Enabled() bool { return m.SMTPHost != "" && m.From != "" }

// EncryptionKeyBytes decodes and validates the primary key, or returns nil when
// none is set. Errors on a present-but-malformed key.
func (s SecurityConfig) EncryptionKeyBytes() ([]byte, error) {
	if s.EncryptionKey == "" {
		return nil, nil
	}
	return decodeKey("security.encryption_key", s.EncryptionKey)
}

// Keys returns the full keyring (primary first, then previous keys) for building a
// Cipher, or nil when encryption is disabled. All keys are validated; previous_keys
// without a primary is a configuration error.
func (s SecurityConfig) Keys() ([][]byte, error) {
	primary, err := s.EncryptionKeyBytes()
	if err != nil {
		return nil, err
	}
	if primary == nil {
		if len(s.PreviousKeys) > 0 {
			return nil, fmt.Errorf("security.previous_keys set without security.encryption_key")
		}
		return nil, nil
	}
	keys := [][]byte{primary}
	for i, pk := range s.PreviousKeys {
		k, err := decodeKey(fmt.Sprintf("security.previous_keys[%d]", i), pk)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func decodeKey(name, b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64: %w", name, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes, got %d", name, len(key))
	}
	return key, nil
}

// Duration is a time.Duration that unmarshals from a YAML string like "24h".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

var (
	validLogLevels  = map[string]bool{"debug": true, "info": true, "error": true, "critical": true}
	validLogFormats = map[string]bool{"json": true, "text": true}
)

// Load reads and validates the config file at path. It fails fast on any
// contract violation and never returns a partially-usable config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	// Expand ${VAR} / $VAR from the process environment before parsing, so secrets
	// (DB/broker passwords, tokens) can be injected at runtime — e.g. from a compose
	// .env in production — instead of being committed to the config file. A literal
	// '$' in a value must be escaped as '$$'. Expansion is a single pass, so injected
	// values are never re-expanded.
	expanded, err := expandEnvStrict(string(data))
	if err != nil {
		return nil, err
	}
	return Parse([]byte(expanded))
}

// expandEnvStrict expands ${VAR}/$VAR from the environment but, unlike os.ExpandEnv, FAILS
// on an UNDEFINED variable instead of silently substituting "" — a silent blank could
// disable a security-critical setting (encryption key, tokens, bootstrap admin) without
// notice. A defined-but-empty variable is allowed (that is an explicit choice). The '$$'
// escape and a bare trailing '$' are preserved as a literal '$'.
func expandEnvStrict(s string) (string, error) {
	var missing []string
	out := os.Expand(s, func(key string) string {
		// os.Expand passes "$" for the '$$' escape (a shell special var); yield a literal $.
		// Any other non-env-name token (e.g. $@) is left as-is rather than treated as a var.
		if key == "$" {
			return "$"
		}
		if !isEnvName(key) {
			return "$" + key
		}
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		missing = append(missing, key)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config: undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// isEnvName reports whether key is a POSIX-style environment variable name
// ([A-Za-z_][A-Za-z0-9_]*), i.e. a real variable reference rather than a shell special.
func isEnvName(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Parse validates raw YAML bytes into a Config. Unknown keys are rejected.
func Parse(data []byte) (*Config, error) {
	cfg := defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	// Per-entry provider defaults can only be applied after decode (map keys are unknown to
	// defaults()); they run before Validate so the bounds check sees resolved values.
	cfg.normalizeProviders()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:      ":8080",
			HealthzPath: "/healthz",
			ReadyzPath:  "/readyz",
			MetricsPath: "/metrics",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		OIDC: OIDCConfig{
			Scopes:      []string{"openid", "email", "profile"},
			ButtonLabel: "Continue with SSO",
		},
		Local: LocalAuthConfig{
			MinPasswordLength:       8,
			LoginRateLimitPerMinute: 10,
		},
		Session: SessionConfig{
			CookieName: "cerbix_session",
			TTL:        Duration(24 * time.Hour),
			Secure:     true,
		},
		Prober: ProberConfig{
			AllowPrivateIPs:  true,  // this tool monitors internal apps
			AllowMetadataIPs: false, // block cloud-metadata / link-local
		},
		NotificationEgress: NotificationEgressConfig{
			AllowPrivateIPs:  false, // editor-controlled destinations: deny internal by default
			AllowMetadataIPs: false,
		},
		Result: ResultConfig{
			AllowedSkew:  Duration(5 * time.Minute),
			RevisionMode: "enforce", // secure default even when the result: block is absent
		},
		Heartbeats: HeartbeatsConfig{
			RetentionDays: 30,
		},
		Services: ServicesConfig{
			MaxServicesPerProject: domain.DefaultMaxServicesPerProject,
			MaxMembersPerRevision: domain.DefaultMaxMembersPerRevision,
			MaxServicesPerMonitor: domain.DefaultMaxServicesPerMonitor,
		},
		Gate: GateConfig{
			EvaluateInflightProcess:        8,
			EvaluateInflightPrincipal:      2,
			EvaluateRatePrincipalPerMinute: 10,
			EvaluateRateProcessPerMinute:   60,
			EvaluateTxBudgetMs:             5000,
			DecisionRetentionDays:          90,
			DecisionPartitionLeadDays:      7,
			DecisionPartitionCreateMax:     3,
			DecisionPurgeEvery:             Duration(time.Hour),
			DecisionPurgeMaxPartitions:     8,
		},
	}
}

// Validate enforces the config contract. Each rule has a single owner here at
// the infra/bootstrap boundary; business rules live in their own layers.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return fmt.Errorf("server.listen must not be empty")
	}
	for name, p := range map[string]string{
		"server.healthz_path": c.Server.HealthzPath,
		"server.readyz_path":  c.Server.ReadyzPath,
		"server.metrics_path": c.Server.MetricsPath,
	} {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("%s must start with '/': %q", name, p)
		}
	}
	if !validLogLevels[c.Log.Level] {
		return fmt.Errorf("log.level must be one of debug|info|error|critical: %q", c.Log.Level)
	}
	if !validLogFormats[c.Log.Format] {
		return fmt.Errorf("log.format must be one of json|text: %q", c.Log.Format)
	}
	// OIDC is optional, but if enabled all required fields and a database (for
	// sessions/JIT users) must be present.
	if c.OIDC.Enabled() {
		if c.OIDC.ClientID == "" || c.OIDC.RedirectURL == "" {
			return fmt.Errorf("oidc.client_id and oidc.redirect_url are required when oidc.issuer is set")
		}
		if c.Database.DSN == "" {
			return fmt.Errorf("database.dsn is required when oidc is enabled (sessions and users need Postgres)")
		}
	}
	// Service fan-out caps (func-service-reliability §10.10). Fail fast: a value the domain
	// forbids is REJECTED at startup, never silently mapped to something legal — the config
	// the operator wrote and the config the system runs must be the same config.
	for _, cap := range []struct {
		name string
		v    int
		hard int
	}{
		{"services.max_services_per_project", c.Services.MaxServicesPerProject, domain.HardMaxServicesPerProject},
		{"services.max_members_per_revision", c.Services.MaxMembersPerRevision, domain.HardMaxMembersPerRevision},
		{"services.max_services_per_monitor", c.Services.MaxServicesPerMonitor, domain.HardMaxServicesPerMonitor},
	} {
		if cap.v < 1 {
			return fmt.Errorf("%s must be at least 1, got %d", cap.name, cap.v)
		}
		if cap.v > cap.hard {
			return fmt.Errorf("%s must not exceed the hard maximum %d, got %d", cap.name, cap.hard, cap.v)
		}
	}
	if err := c.validateGate(); err != nil {
		return err
	}

	if c.Local.Enabled {
		if c.Database.DSN == "" {
			return fmt.Errorf("database.dsn is required when local login is enabled")
		}
		if c.Local.MinPasswordLength < 1 {
			return fmt.Errorf("local.min_password_length must be positive")
		}
		if c.Security.AdminPassword != "" && len(c.Security.AdminPassword) < c.Local.MinPasswordLength {
			return fmt.Errorf("security.admin_password is shorter than local.min_password_length")
		}
		if c.Security.AdminPassword != "" && c.Security.AdminEmail == "" {
			return fmt.Errorf("security.admin_email is required when security.admin_password is set")
		}
	}
	if c.Session.TTL.Std() <= 0 {
		return fmt.Errorf("session.ttl must be positive")
	}
	if strings.TrimSpace(c.Session.CookieName) == "" {
		return fmt.Errorf("session.cookie_name must not be empty")
	}
	// Retention also bounds the rollup recompute window, so a day of margin is the
	// practical minimum.
	if c.Heartbeats.RetentionDays < 2 {
		return fmt.Errorf("heartbeats.retention_days must be at least 2")
	}
	// Result ingest (spec func-result-protocol). A skew larger than an hour would defeat the
	// future-clock guard; a non-positive one is meaningless. RevisionMode is strict-enum.
	if s := c.Result.AllowedSkew.Std(); s <= 0 || s > time.Hour {
		return fmt.Errorf("result.allowed_skew must be > 0 and <= 1h: %s", s)
	}
	if c.Result.RevisionMode != "enforce" && c.Result.RevisionMode != "observe" {
		return fmt.Errorf("result.revision_mode must be enforce|observe: %q", c.Result.RevisionMode)
	}
	if _, err := c.Server.TrustedProxyNets(); err != nil {
		return err
	}
	if err := c.validateProviders(); err != nil {
		return err
	}
	if _, err := c.Security.Keys(); err != nil {
		return err
	}
	if err := c.validateSecrets(); err != nil {
		return err
	}
	if c.Mail.Enabled() {
		if c.Mail.SMTPPort <= 0 {
			return fmt.Errorf("mail.smtp_port must be positive when mail is enabled")
		}
		if strings.TrimSpace(c.Mail.PublicBaseURL) == "" {
			return fmt.Errorf("mail.public_base_url is required when mail is enabled (it builds confirm/unsubscribe links)")
		}
	}
	return nil
}

// Gate bounds (func-reliability-gate §5a). The purge cadence is bounded separately because it
// is a duration; the two cross-checks below it are the spec's, and each names both keys.
const (
	gatePurgeEveryMin = 5 * time.Minute
	gatePurgeEveryMax = 24 * time.Hour
	// gateCreatePerDayMin: create_max × passes per day must cover one day's partition plus
	// catch-up.
	gateCreatePerDayMin = 2
	// gatePurgePerDayMin: purge_max × passes per day must be STRICTLY more than the two
	// stage-ops a day of steady state (detach today's eligible day, drop yesterday's detached
	// one), so a backlog converges instead of holding still.
	gatePurgePerDayMin = 4
)

// validateGate refuses any of the ten gate.* keys outside its range, then the two cross-checks
// of §5a, naming the key(s) and the range every time.
func (c *Config) validateGate() error {
	g := c.Gate
	for _, b := range []struct {
		name     string
		v        int
		min, max int
	}{
		{"gate.evaluate_inflight_process", g.EvaluateInflightProcess, 1, 64},
		{"gate.evaluate_inflight_principal", g.EvaluateInflightPrincipal, 1, 16},
		{"gate.evaluate_rate_principal_per_minute", g.EvaluateRatePrincipalPerMinute, 1, 600},
		{"gate.evaluate_rate_process_per_minute", g.EvaluateRateProcessPerMinute, 1, 600},
		{"gate.evaluate_tx_budget_ms", g.EvaluateTxBudgetMs, 500, 30000},
		{"gate.decision_retention_days", g.DecisionRetentionDays, 7, 365},
		{"gate.decision_partition_lead_days", g.DecisionPartitionLeadDays, 2, 30},
		{"gate.decision_partition_create_max", g.DecisionPartitionCreateMax, 1, 8},
		{"gate.decision_purge_max_partitions", g.DecisionPurgeMaxPartitions, 1, 48},
	} {
		if b.v < b.min || b.v > b.max {
			return fmt.Errorf("%s must be between %d and %d, got %d", b.name, b.min, b.max, b.v)
		}
	}
	every := g.DecisionPurgeEvery.Std()
	if every < gatePurgeEveryMin || every > gatePurgeEveryMax {
		return fmt.Errorf("gate.decision_purge_every must be between %s and %s, got %s", gatePurgeEveryMin, gatePurgeEveryMax, every)
	}
	// floor(86 400 / every_seconds): the maintenance passes one day holds.
	passesPerDay := int((24 * time.Hour) / every)
	if n := g.DecisionPartitionCreateMax * passesPerDay; n < gateCreatePerDayMin {
		return fmt.Errorf("gate.decision_partition_create_max × floor(86400 / gate.decision_purge_every) must be at least %d "+
			"(one day's partition plus catch-up), got %d × %d = %d: raise gate.decision_partition_create_max or shorten gate.decision_purge_every",
			gateCreatePerDayMin, g.DecisionPartitionCreateMax, passesPerDay, n)
	}
	if n := g.DecisionPurgeMaxPartitions * passesPerDay; n < gatePurgePerDayMin {
		return fmt.Errorf("gate.decision_purge_max_partitions × floor(86400 / gate.decision_purge_every) must be at least %d "+
			"(strictly more than the two stage-ops a day of steady state), got %d × %d = %d: raise gate.decision_purge_max_partitions or shorten gate.decision_purge_every",
			gatePurgePerDayMin, g.DecisionPurgeMaxPartitions, passesPerDay, n)
	}
	return nil
}

// TrustedProxyNets parses TrustedProxyCIDRs into networks, returning an error on
// any malformed entry (called from Validate so a bad CIDR fails Load). An empty
// list yields a nil slice, which selects the hop-count model in the rate-limiter.
func (s ServerConfig) TrustedProxyNets() ([]*net.IPNet, error) {
	if len(s.TrustedProxyCIDRs) == 0 {
		return nil, nil
	}
	nets := make([]*net.IPNet, 0, len(s.TrustedProxyCIDRs))
	for _, c := range s.TrustedProxyCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxy_cidrs: invalid CIDR %q: %w", c, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// HTTPReadTimeout is a conservative default used by the HTTP server.
const HTTPReadTimeout = 15 * time.Second
