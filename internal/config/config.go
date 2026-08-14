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
	Mail               MailConfig               `yaml:"mail"`
	Pull               PullConfig               `yaml:"pull"`
	Providers          ProvidersConfig          `yaml:"providers"`
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

// HeartbeatsConfig controls raw heartbeat retention. RetentionDays bounds how many
// days of raw heartbeats are kept (older daily partitions are dropped); it also
// bounds the daily-availability rollup's recompute window, so the rollup never
// touches days whose raw data has been dropped (long-range availability lives in
// the frozen rollup rows).
type HeartbeatsConfig struct {
	RetentionDays int `yaml:"retention_days"`
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
