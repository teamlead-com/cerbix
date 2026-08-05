package domain

import (
	"fmt"
	"strings"
	"time"
)

// Visibility controls who may view a status page.
type Visibility string

const (
	// VisibilityPublic: viewable by anyone, no session.
	VisibilityPublic Visibility = "public"
	// VisibilityInternal: viewable only by authenticated org members.
	VisibilityInternal Visibility = "internal"
	// VisibilityUnlisted: viewable by anyone holding the secret token.
	VisibilityUnlisted Visibility = "unlisted"
)

// Valid reports whether v is a known visibility.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPublic, VisibilityInternal, VisibilityUnlisted:
		return true
	default:
		return false
	}
}

// ComponentStatus is the displayed state of a status-page component.
type ComponentStatus string

const (
	CompOperational   ComponentStatus = "operational"
	CompDegraded      ComponentStatus = "degraded"
	CompPartialOutage ComponentStatus = "partial_outage"
	CompMajorOutage   ComponentStatus = "major_outage"
	CompMaintenance   ComponentStatus = "maintenance"
)

// Valid reports whether c is a known component status.
func (c ComponentStatus) Valid() bool {
	switch c {
	case CompOperational, CompDegraded, CompPartialOutage, CompMajorOutage, CompMaintenance:
		return true
	default:
		return false
	}
}

// severity orders component statuses from best to worst for summary rollup.
// Maintenance is treated as informational and never worse than an outage.
func (c ComponentStatus) severity() int {
	switch c {
	case CompOperational:
		return 0
	case CompMaintenance:
		return 1
	case CompDegraded:
		return 2
	case CompPartialOutage:
		return 3
	case CompMajorOutage:
		return 4
	default:
		return 0
	}
}

// ComponentStatusFromMonitor maps a monitor's last observed state to a component
// status. Pending (never checked) reads as operational until proven otherwise.
func ComponentStatusFromMonitor(s MonitorStatus) ComponentStatus {
	switch s {
	case StatusDown:
		return CompMajorOutage
	case StatusUp, StatusPending:
		return CompOperational
	default:
		return CompOperational
	}
}

// SummaryStatus returns the worst of the given component statuses (the overall
// page status). Empty input is operational.
func SummaryStatus(statuses []ComponentStatus) ComponentStatus {
	worst := CompOperational
	for _, s := range statuses {
		if s.severity() > worst.severity() {
			worst = s
		}
	}
	return worst
}

// StatusPage is a public/internal page aggregating component statuses for an
// organization (or one of its projects).
type StatusPage struct {
	ID            string     `json:"id"`
	OrgID         string     `json:"org_id"`
	ProjectID     string     `json:"project_id,omitempty"`
	Slug          string     `json:"slug"`
	Title         string     `json:"title"`
	Visibility    Visibility `json:"visibility"`
	UnlistedToken string     `json:"unlisted_token,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Validate enforces status-page invariants.
func (p StatusPage) Validate() error {
	if p.OrgID == "" {
		return fmt.Errorf("status page: org_id is required")
	}
	if strings.TrimSpace(p.Slug) == "" {
		return fmt.Errorf("status page: slug is required")
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("status page: title is required")
	}
	if !p.Visibility.Valid() {
		return fmt.Errorf("status page: unknown visibility %q", p.Visibility)
	}
	return nil
}

// Component is one line on a status page. It tracks a monitor (status derived) or
// is driven manually (ManualStatus set).
type Component struct {
	ID           string          `json:"id"`
	StatusPageID string          `json:"status_page_id"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	GroupName    string          `json:"group,omitempty"`
	Position     int             `json:"position"`
	MonitorID    string          `json:"monitor_id,omitempty"`
	ManualStatus ComponentStatus `json:"manual_status,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Validate enforces component invariants: a name, and either a monitor binding or
// a valid manual status (or neither → defaults to operational at render time).
func (c Component) Validate() error {
	if c.StatusPageID == "" {
		return fmt.Errorf("component: status_page_id is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("component: name is required")
	}
	if c.ManualStatus != "" && !c.ManualStatus.Valid() {
		return fmt.Errorf("component: unknown manual status %q", c.ManualStatus)
	}
	return nil
}

// Subscriber is an email subscriber to a status page. ConfirmedAt is set once the
// double opt-in confirmation link is followed. ConfirmToken is secret and never
// serialized.
type Subscriber struct {
	ID           string     `json:"id"`
	StatusPageID string     `json:"status_page_id"`
	Email        string     `json:"email"`
	ConfirmToken string     `json:"-"`
	ConfirmedAt  *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Validate checks a subscriber has a plausible email and a page.
func (s Subscriber) Validate() error {
	e := strings.TrimSpace(s.Email)
	if !strings.Contains(e, "@") || strings.HasPrefix(e, "@") || strings.HasSuffix(e, "@") {
		return fmt.Errorf("subscriber: invalid email")
	}
	if s.StatusPageID == "" {
		return fmt.Errorf("subscriber: status_page_id is required")
	}
	return nil
}
