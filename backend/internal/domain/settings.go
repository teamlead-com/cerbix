package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Instance-wide settings groups. Each group is a self-validating value stored as a
// JSONB column in the instance_settings singleton. Configured=true means the group
// has been saved from the UI and overrides the config-file bootstrap / defaults.

// Announcement is a global banner shown to all users.
type Announcement struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	Level   string `json:"level"` // info | warning | critical
}

// Branding customizes the product name, accent color, and a global announcement.
type Branding struct {
	Configured   bool         `json:"configured"`
	ProductName  string       `json:"product_name"`
	AccentColor  string       `json:"accent_color"` // hex, e.g. #3b5bdb
	LogoURL      string       `json:"logo_url"`
	FooterText   string       `json:"footer_text"`
	SupportURL   string       `json:"support_url"`
	Announcement Announcement `json:"announcement"`
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate enforces branding invariants (domain-owned).
func (b Branding) Validate() error {
	if b.AccentColor != "" && !hexColor.MatchString(b.AccentColor) {
		return fmt.Errorf("branding: accent_color must be a 6-digit hex color like #3b5bdb")
	}
	if len(b.ProductName) > 60 {
		return fmt.Errorf("branding: product_name too long (max 60)")
	}
	if b.Announcement.Enabled && strings.TrimSpace(b.Announcement.Text) == "" {
		return fmt.Errorf("branding: announcement text is required when enabled")
	}
	if b.Announcement.Level != "" {
		switch b.Announcement.Level {
		case "info", "warning", "critical":
		default:
			return fmt.Errorf("branding: announcement level must be info, warning or critical")
		}
	}
	return nil
}

// TOTP enforcement modes.
const (
	TOTPNone   = "none"
	TOTPAdmins = "admins"
	TOTPAll    = "all"
)

// AuthPolicy is instance-wide login policy.
type AuthPolicy struct {
	Configured          bool     `json:"configured"`
	MinPasswordLen      int      `json:"min_password_len"`
	SessionTTLSeconds   int      `json:"session_ttl_seconds"`
	RequireTOTP         string   `json:"require_totp"` // none | admins | all
	AllowedEmailDomains []string `json:"allowed_email_domains"`
}

// Validate enforces auth-policy invariants.
func (p AuthPolicy) Validate() error {
	if p.MinPasswordLen < 6 || p.MinPasswordLen > 256 {
		return fmt.Errorf("auth_policy: min_password_len must be 6..256")
	}
	if p.SessionTTLSeconds < 300 {
		return fmt.Errorf("auth_policy: session_ttl_seconds must be at least 300")
	}
	switch p.RequireTOTP {
	case "", TOTPNone, TOTPAdmins, TOTPAll:
	default:
		return fmt.Errorf("auth_policy: require_totp must be none, admins or all")
	}
	return nil
}

// Normalize lower-cases and trims the allowed email domains.
func (p *AuthPolicy) Normalize() {
	out := make([]string, 0, len(p.AllowedEmailDomains))
	seen := map[string]bool{}
	for _, d := range p.AllowedEmailDomains {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	p.AllowedEmailDomains = out
	if p.RequireTOTP == "" {
		p.RequireTOTP = TOTPNone
	}
}

// EmailAllowed reports whether an email may sign in under this policy (empty allow
// list = any domain).
func (p AuthPolicy) EmailAllowed(email string) bool {
	if len(p.AllowedEmailDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	dom := strings.ToLower(email[at+1:])
	for _, d := range p.AllowedEmailDomains {
		if d == dom {
			return true
		}
	}
	return false
}

// TOTPRequiredFor reports whether TOTP is mandatory for a user of the given
// global-admin status under this policy.
func (p AuthPolicy) TOTPRequiredFor(isGlobalAdmin bool) bool {
	switch p.RequireTOTP {
	case TOTPAll:
		return true
	case TOTPAdmins:
		return isGlobalAdmin
	default:
		return false
	}
}

// GlobalSilence suppresses alert notifications instance-wide (incidents are still
// recorded). Until, when set, auto-expires the silence.
type GlobalSilence struct {
	Enabled bool       `json:"enabled"`
	Until   *time.Time `json:"until,omitempty"`
}

// Alerting holds instance-wide alerting controls.
type Alerting struct {
	Configured    bool          `json:"configured"`
	GlobalSilence GlobalSilence `json:"global_silence"`
}

// Silenced reports whether alert notifications are currently suppressed.
func (a Alerting) Silenced(now time.Time) bool {
	if !a.GlobalSilence.Enabled {
		return false
	}
	if a.GlobalSilence.Until != nil && now.After(*a.GlobalSilence.Until) {
		return false
	}
	return true
}

// Validate enforces alerting invariants.
func (a Alerting) Validate() error { return nil }

// MonitorDefaults are the default field values applied to a newly-created monitor
// when the request omits them.
type MonitorDefaults struct {
	Configured       bool `json:"configured"`
	IntervalSeconds  int  `json:"interval_seconds"`
	TimeoutSeconds   int  `json:"timeout_seconds"`
	Retries          int  `json:"retries"`
	FailureThreshold int  `json:"failure_threshold"`
	RenotifySeconds  int  `json:"renotify_seconds"`
	AutoIncident     bool `json:"auto_incident"`
}

// Validate enforces monitor-default invariants.
func (d MonitorDefaults) Validate() error {
	if d.IntervalSeconds < 5 {
		return fmt.Errorf("monitor_defaults: interval_seconds must be at least 5")
	}
	if d.TimeoutSeconds < 1 {
		return fmt.Errorf("monitor_defaults: timeout_seconds must be at least 1")
	}
	if d.Retries < 0 || d.FailureThreshold < 1 || d.RenotifySeconds < 0 {
		return fmt.Errorf("monitor_defaults: retries/failure_threshold/renotify out of range")
	}
	return nil
}

// MailSettings is the instance-wide SMTP configuration used for password-reset and
// status-page subscription email. SMTPPassword is encrypted at rest.
type MailSettings struct {
	Configured    bool   `json:"configured"`
	Enabled       bool   `json:"enabled"`
	SMTPHost      string `json:"smtp_host"`
	SMTPPort      int    `json:"smtp_port"`
	SMTPUsername  string `json:"smtp_username"`
	SMTPPassword  string `json:"smtp_password,omitempty"` // never emitted to clients; see MailSettingsView
	From          string `json:"from"`
	PublicBaseURL string `json:"public_base_url"`
}

// Validate enforces mail-settings invariants.
func (m MailSettings) Validate() error {
	if !m.Enabled {
		return nil
	}
	if strings.TrimSpace(m.SMTPHost) == "" || strings.TrimSpace(m.From) == "" {
		return fmt.Errorf("mail: smtp_host and from are required when enabled")
	}
	if m.SMTPPort < 0 || m.SMTPPort > 65535 {
		return fmt.Errorf("mail: smtp_port out of range")
	}
	return nil
}

// Deliverable reports whether the settings can actually send mail.
func (m MailSettings) Deliverable() bool {
	return m.Enabled && strings.TrimSpace(m.SMTPHost) != "" && strings.TrimSpace(m.From) != ""
}

// Port returns the SMTP port, defaulting to 587 when unset.
func (m MailSettings) Port() int {
	if m.SMTPPort == 0 {
		return 587
	}
	return m.SMTPPort
}

// InstanceSettings aggregates every group (one instance_settings row).
type InstanceSettings struct {
	Branding        Branding        `json:"branding"`
	AuthPolicy      AuthPolicy      `json:"auth_policy"`
	Alerting        Alerting        `json:"alerting"`
	MonitorDefaults MonitorDefaults `json:"monitor_defaults"`
	Mail            MailSettings    `json:"mail"`
}
