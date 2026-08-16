package domain

import (
	"fmt"
	"math"
	"time"
)

// Burn-rule severities: page interrupts a human now, ticket waits for working
// hours. Delivery routing by severity is a follow-up; today it tags the message.
const (
	BurnSeverityPage   = "page"
	BurnSeverityTicket = "ticket"
)

// BurnRule is one multi-window burn-rate alert rule (Google SRE canon): it fires
// when the error-budget burn rate is at or over Threshold in BOTH windows — the
// long window filters noise, the short one confirms the burn is still happening.
// Firing is the server-owned edge latch (one alert per crossing).
type BurnRule struct {
	LongWindowSeconds  int     `json:"long_window_seconds"`
	ShortWindowSeconds int     `json:"short_window_seconds"`
	Threshold          float64 `json:"threshold"`
	Severity           string  `json:"severity"`
	Firing             bool    `json:"firing"`
}

// DefaultBurnRules is the canonical SRE pair: page 14.4x over (1h AND 5m),
// ticket 6x over (6h AND 30m).
func DefaultBurnRules() []BurnRule {
	return []BurnRule{
		{LongWindowSeconds: 3600, ShortWindowSeconds: 300, Threshold: 14.4, Severity: BurnSeverityPage},
		{LongWindowSeconds: 21600, ShortWindowSeconds: 1800, Threshold: 6, Severity: BurnSeverityTicket},
	}
}

// Key identifies a rule's configuration (not its latch) — used to carry Firing
// across edits that keep a rule unchanged.
func (r BurnRule) Key() string {
	return fmt.Sprintf("%s/%d/%d/%.4f", r.Severity, r.LongWindowSeconds, r.ShortWindowSeconds, r.Threshold)
}

// ValidateBurnRules enforces rule invariants: at most 4 rules; per rule a known
// severity, a positive threshold, and sane paired windows (1m ≤ short < long ≤ 7d).
func ValidateBurnRules(rules []BurnRule) error {
	if len(rules) > 4 {
		return fmt.Errorf("burn rules: at most 4 rules")
	}
	for i, r := range rules {
		if r.Severity != BurnSeverityPage && r.Severity != BurnSeverityTicket {
			return fmt.Errorf("burn rule %d: severity must be %q or %q", i+1, BurnSeverityPage, BurnSeverityTicket)
		}
		if r.Threshold <= 0 {
			return fmt.Errorf("burn rule %d: threshold must be positive", i+1)
		}
		if r.ShortWindowSeconds < 60 {
			return fmt.Errorf("burn rule %d: short window must be at least 60s", i+1)
		}
		if r.LongWindowSeconds <= r.ShortWindowSeconds {
			return fmt.Errorf("burn rule %d: long window must be longer than the short one", i+1)
		}
		if r.LongWindowSeconds > 7*24*3600 {
			return fmt.Errorf("burn rule %d: long window must be at most 7 days", i+1)
		}
	}
	return nil
}

// SLATarget is an SLO objective for a monitor or a project over a named window.
// BurnRules configure optional multi-window burn-rate alerting: the scheduler
// leader evaluates each rule over its window pair and alerts on edges.
type SLATarget struct {
	ID               string     `json:"id"`
	MonitorID        string     `json:"monitor_id,omitempty"`
	ProjectID        string     `json:"project_id,omitempty"`
	Objective        float64    `json:"objective"`
	Window           string     `json:"window"`
	BurnAlertEnabled bool       `json:"burn_alert_enabled"`
	BurnRules        []BurnRule `json:"burn_rules"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// AnyBurnFiring reports whether at least one burn rule is currently latched
// firing (the collapsed-row badge in the UI).
func (t SLATarget) AnyBurnFiring() bool {
	for _, r := range t.BurnRules {
		if r.Firing {
			return true
		}
	}
	return false
}

// MaintenanceWindow marks a planned time span excluded from SLA math. It applies
// to a single monitor (MonitorID set) or the whole project (MonitorID empty).
type MaintenanceWindow struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	MonitorID string    `json:"monitor_id,omitempty"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	// ArchivedAt marks a window removed from active inventory; CancelEffectiveAt is where
	// its EFFECT stops (for an archived future window: its own start — it never happened).
	// Every consumer reduces LEAST(ends_at, cancel_effective_at); raw ends_at lies for any
	// window an operator archived or annulled.
	ArchivedAt        *time.Time `json:"archived_at,omitempty"`
	CancelEffectiveAt *time.Time `json:"cancel_effective_at,omitempty"`
}

// PublicRedacted returns a copy safe for a public status page: internal
// project/monitor identifiers stripped, the window timing and reason kept.
func (m MaintenanceWindow) PublicRedacted() MaintenanceWindow {
	m.ProjectID = ""
	m.MonitorID = ""
	return m
}

// Validate enforces maintenance-window invariants.
func (m MaintenanceWindow) Validate() error {
	if m.ProjectID == "" {
		return fmt.Errorf("maintenance window: project_id is required")
	}
	if !m.EndsAt.After(m.StartsAt) {
		return fmt.Errorf("maintenance window: ends_at must be after starts_at")
	}
	return nil
}

// CanonicalObjective is the ONE objective rule for every SLA-target scope (monitor, project,
// service): objectives live in the OPEN interval (0,100) — D-0165. An objective of 100 means
// a zero error budget, and the shared budget/burn math's allowed<=0 sentinel would answer a
// total outage with 0× and fire no alert ([195] P0); a true zero-budget objective is a
// separately specified cross-scope change, not a value this rule may admit. The RAW input is
// judged first (>0 and <100 — 100.00004 is rejected as said, never rounded into range), then
// canonicalized half-up to FOUR decimal places (the numeric(7,4) representation), and the
// canonical value must remain inside (0,100) too — 99.99995 rounds to 100 and is rejected,
// 0.00001 rounds to zero and is rejected. The maximum admissible objective is 99.9999. The
// handler echoes exactly the canonical value, so the wire answer and the stored number can
// never differ.
func CanonicalObjective(v float64) (float64, error) {
	// NaN compares false against every bound, so an explicit check keeps the rule
	// fail-closed even for callers that bypass JSON (which cannot carry NaN).
	if math.IsNaN(v) || v <= 0 || v >= 100 {
		return 0, fmt.Errorf("objective must be within (0,100) — a zero error budget is not a supported configuration")
	}
	canonical := math.Round(v*10000) / 10000
	if canonical <= 0 || canonical >= 100 {
		return 0, fmt.Errorf("objective must be within (0,100) at four decimal places (maximum 99.9999)")
	}
	return canonical, nil
}
