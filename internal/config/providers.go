package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Monitoring-as-Code static provider configuration (spec func-monitoring-as-code §4).
// The provider DEFINITION is static — changing a provider's name, scope, directory, or
// limits requires a normal restart; only the file CONTENTS under `directory` are dynamic.
// Every role parses this schema identically so one validated config is reusable across a
// distributed deployment; only api/all own the component (role wiring, later iterations).

// ProvidersConfig is the optional `providers` block. Only file providers exist in v1.
type ProvidersConfig struct {
	File map[string]FileProviderConfig `yaml:"file"`
}

// FileProviderConfig is one named file provider under `providers.file.<name>`.
type FileProviderConfig struct {
	Directory         string              `yaml:"directory"`
	Debounce          Duration            `yaml:"debounce"`
	ResyncInterval    Duration            `yaml:"resync_interval"`
	OrphanGracePeriod Duration            `yaml:"orphan_grace_period"`
	Scope             ProviderScopeConfig `yaml:"scope"`
	Limits            ProviderLimits      `yaml:"limits"`
}

// ProviderScopeConfig fixes the tenant authority of a provider (§5). There is no implicit
// instance scope: `type` is required and exactly instance|organization|project.
type ProviderScopeConfig struct {
	Type         string `yaml:"type"`
	Organization string `yaml:"organization"`
	Project      string `yaml:"project"`
}

// ProviderLimits bounds one provider's work (§4/§17). Zero means "use the default".
type ProviderLimits struct {
	MaxFiles             int   `yaml:"max_files"`
	MaxFileBytes         int64 `yaml:"max_file_bytes"`
	MaxTotalBytes        int64 `yaml:"max_total_bytes"`
	MaxMonitorsPerBundle int   `yaml:"max_monitors_per_bundle"`
	MaxManagedMonitors   int   `yaml:"max_managed_monitors"`
}

// Provider scope types (§4.1).
const (
	ProviderScopeInstance     = "instance"
	ProviderScopeOrganization = "organization"
	ProviderScopeProject      = "project"
)

// Default provider tunables and the implementation-wide safety maxima (§4.1/§17).
const (
	defaultProviderDebounce     = 2 * time.Second
	defaultProviderResync       = 30 * time.Second
	defaultProviderOrphanGrace  = 30 * time.Second
	minProviderDebounce         = 100 * time.Millisecond
	maxProviderDebounce         = 30 * time.Second
	minProviderResync           = 5 * time.Second
	maxProviderResync           = time.Hour
	maxProviderOrphanGrace      = 24 * time.Hour
	maxConfiguredFileProviders  = 64
	defaultMaxFiles             = 1000
	defaultMaxFileBytes         = 1 << 20  // 1 MiB
	defaultMaxTotalBytes        = 16 << 20 // 16 MiB
	defaultMaxMonitorsPerBundle = 1000
	defaultMaxManagedMonitors   = 5000
	// Safety maxima: a configured limit may not exceed these regardless of operator intent.
	safetyMaxFiles             = 100000
	safetyMaxFileBytes         = 64 << 20  // 64 MiB
	safetyMaxTotalBytes        = 512 << 20 // 512 MiB
	safetyMaxMonitorsPerBundle = 100000
	safetyMaxManagedMonitors   = 1000000
)

var providerNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// normalizeProviders fills zero-valued per-provider fields with the contract defaults. Map
// entries cannot be pre-populated by defaults() (their keys are unknown before decode), so
// this runs after decode and before Validate.
func (c *Config) normalizeProviders() {
	for name, p := range c.Providers.File {
		if p.Debounce == 0 {
			p.Debounce = Duration(defaultProviderDebounce)
		}
		if p.ResyncInterval == 0 {
			p.ResyncInterval = Duration(defaultProviderResync)
		}
		// OrphanGracePeriod default only when the key is absent; 0 is a valid explicit value
		// (immediate disable after a valid absence). We cannot distinguish absent from 0 on a
		// scalar, so 0 keeps the default; an operator wanting true-zero sets it explicitly to a
		// sub-second value is NOT supported — 0 means default, documented in the example.
		if p.OrphanGracePeriod == 0 {
			p.OrphanGracePeriod = Duration(defaultProviderOrphanGrace)
		}
		if p.Limits.MaxFiles == 0 {
			p.Limits.MaxFiles = defaultMaxFiles
		}
		if p.Limits.MaxFileBytes == 0 {
			p.Limits.MaxFileBytes = defaultMaxFileBytes
		}
		if p.Limits.MaxTotalBytes == 0 {
			p.Limits.MaxTotalBytes = defaultMaxTotalBytes
		}
		if p.Limits.MaxMonitorsPerBundle == 0 {
			p.Limits.MaxMonitorsPerBundle = defaultMaxMonitorsPerBundle
		}
		if p.Limits.MaxManagedMonitors == 0 {
			p.Limits.MaxManagedMonitors = defaultMaxManagedMonitors
		}
		c.Providers.File[name] = p
	}
}

// validateProviders enforces the static provider contract (§4.1). Structural only: the
// directory-exists + DB-present checks are role-specific (api/all) and run at startup wiring,
// not here, so a scheduler/worker can parse the same config without touching the directory.
func (c *Config) validateProviders() error {
	if len(c.Providers.File) == 0 {
		return nil
	}
	if len(c.Providers.File) > maxConfiguredFileProviders {
		return fmt.Errorf("providers.file: at most %d providers (got %d)", maxConfiguredFileProviders, len(c.Providers.File))
	}
	// Canonical directory → provider, to reject overlapping roots deterministically.
	roots := make(map[string]string, len(c.Providers.File))
	for name, p := range c.Providers.File {
		if !providerNameRe.MatchString(name) {
			return fmt.Errorf("providers.file: invalid provider name %q (want %s)", name, providerNameRe.String())
		}
		if err := p.validate(name); err != nil {
			return err
		}
		clean := filepath.Clean(p.Directory)
		for existing, other := range roots {
			if clean == existing || isSubpath(existing, clean) || isSubpath(clean, existing) {
				return fmt.Errorf("providers.file.%s: directory %q overlaps provider %q root %q", name, p.Directory, other, existing)
			}
		}
		roots[clean] = name
	}
	return nil
}

func (p FileProviderConfig) validate(name string) error {
	dir := filepath.Clean(p.Directory)
	if p.Directory == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("providers.file.%s: directory must be an absolute path", name)
	}
	if dir == string(filepath.Separator) {
		return fmt.Errorf("providers.file.%s: directory must not be the filesystem root", name)
	}
	switch p.Scope.Type {
	case ProviderScopeInstance:
		if p.Scope.Organization != "" || p.Scope.Project != "" {
			return fmt.Errorf("providers.file.%s: instance scope must not set organization/project", name)
		}
	case ProviderScopeOrganization:
		if p.Scope.Organization == "" {
			return fmt.Errorf("providers.file.%s: organization scope requires scope.organization", name)
		}
		if p.Scope.Project != "" {
			return fmt.Errorf("providers.file.%s: organization scope must not set scope.project", name)
		}
	case ProviderScopeProject:
		if p.Scope.Organization == "" || p.Scope.Project == "" {
			return fmt.Errorf("providers.file.%s: project scope requires scope.organization and scope.project", name)
		}
	case "":
		return fmt.Errorf("providers.file.%s: scope.type is required (instance|organization|project — no implicit scope)", name)
	default:
		return fmt.Errorf("providers.file.%s: scope.type must be instance|organization|project, got %q", name, p.Scope.Type)
	}
	if d := p.Debounce.Std(); d < minProviderDebounce || d > maxProviderDebounce {
		return fmt.Errorf("providers.file.%s: debounce must be in [100ms, 30s], got %s", name, d)
	}
	if d := p.ResyncInterval.Std(); d < minProviderResync || d > maxProviderResync {
		return fmt.Errorf("providers.file.%s: resync_interval must be in [5s, 1h], got %s", name, d)
	}
	if d := p.OrphanGracePeriod.Std(); d < 0 || d > maxProviderOrphanGrace {
		return fmt.Errorf("providers.file.%s: orphan_grace_period must be in [0, 24h], got %s", name, d)
	}
	return p.Limits.validate(name)
}

func (l ProviderLimits) validate(name string) error {
	for _, c := range []struct {
		field string
		val   int64
		max   int64
	}{
		{"max_files", int64(l.MaxFiles), safetyMaxFiles},
		{"max_file_bytes", l.MaxFileBytes, safetyMaxFileBytes},
		{"max_total_bytes", l.MaxTotalBytes, safetyMaxTotalBytes},
		{"max_monitors_per_bundle", int64(l.MaxMonitorsPerBundle), safetyMaxMonitorsPerBundle},
		{"max_managed_monitors", int64(l.MaxManagedMonitors), safetyMaxManagedMonitors},
	} {
		if c.val <= 0 {
			return fmt.Errorf("providers.file.%s: limits.%s must be positive", name, c.field)
		}
		if c.val > c.max {
			return fmt.Errorf("providers.file.%s: limits.%s exceeds the safety maximum %d", name, c.field, c.max)
		}
	}
	return nil
}

// isSubpath reports whether child is inside parent (both cleaned absolute paths).
func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
