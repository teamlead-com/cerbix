package domain

import (
	"fmt"
	"strings"
	"time"
)

// IncidentStatus is a point in an incident's lifecycle. The lifecycle is
// forward-flowing toward Resolved, which is terminal.
type IncidentStatus string

const (
	// IncidentInvestigating: the issue is acknowledged, cause unknown.
	IncidentInvestigating IncidentStatus = "investigating"
	// IncidentIdentified: the cause is understood, a fix is in progress.
	IncidentIdentified IncidentStatus = "identified"
	// IncidentMonitoring: a fix is applied and being watched.
	IncidentMonitoring IncidentStatus = "monitoring"
	// IncidentResolved: the incident is over. Terminal.
	IncidentResolved IncidentStatus = "resolved"
)

// Valid reports whether s is a known incident status.
func (s IncidentStatus) Valid() bool {
	switch s {
	case IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is an end state (no further updates allowed).
func (s IncidentStatus) Terminal() bool { return s == IncidentResolved }

// IncidentImpact grades how much the incident affects users.
type IncidentImpact string

const (
	ImpactNone     IncidentImpact = "none"
	ImpactMinor    IncidentImpact = "minor"
	ImpactMajor    IncidentImpact = "major"
	ImpactCritical IncidentImpact = "critical"
)

// Valid reports whether i is a known impact level.
func (i IncidentImpact) Valid() bool {
	switch i {
	case ImpactNone, ImpactMinor, ImpactMajor, ImpactCritical:
		return true
	default:
		return false
	}
}

// IncidentSource records how an incident was opened.
type IncidentSource string

const (
	// SourceManual: opened by a user in the UI (cookie session).
	SourceManual IncidentSource = "manual"
	// SourceAPI: opened by a service-account/API caller.
	SourceAPI IncidentSource = "api"
	// SourceAuto: opened automatically from a monitor going down.
	SourceAuto IncidentSource = "auto"
)

// Valid reports whether s is a known incident source.
func (s IncidentSource) Valid() bool {
	switch s {
	case SourceManual, SourceAPI, SourceAuto:
		return true
	default:
		return false
	}
}

// Incident is a tracked disruption within a project, communicated through a
// timeline of updates and (once resolved) an optional postmortem.
type Incident struct {
	ID             string         `json:"id"`
	ProjectID      string         `json:"project_id"`
	MonitorID      string         `json:"monitor_id,omitempty"`   // set for auto-incidents from a monitor
	ExternalKey    string         `json:"external_key,omitempty"` // correlates an externally-sourced incident (e.g. an Alertmanager fingerprint)
	Title          string         `json:"title"`
	Status         IncidentStatus `json:"status"`
	Impact         IncidentImpact `json:"impact"`
	Source         IncidentSource `json:"source"`
	StartedAt      time.Time      `json:"started_at"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
	AcknowledgedAt *time.Time     `json:"acknowledged_at,omitempty"` // set when someone takes ownership; stops escalation
	AcknowledgedBy string         `json:"acknowledged_by,omitempty"`
	// AcknowledgedByName resolves the actor for display (detail endpoint only).
	AcknowledgedByName string `json:"acknowledged_by_name,omitempty"`
	// EscalationStep / LastEscalatedAt expose how far the escalation engine has
	// walked the policy — what an on-call responder weighs before acknowledging.
	EscalationStep  int        `json:"escalation_step,omitempty"`
	LastEscalatedAt *time.Time `json:"last_escalated_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// PublicRedacted returns a copy safe to serialize on a public (unauthenticated)
// status page: it strips internal identifiers and the acknowledging actor that an
// outside viewer must not see — the project/monitor UUIDs, the external correlation
// key (e.g. an Alertmanager fingerprint), and who acknowledged it. Title, status,
// impact, and timing (what a status page actually shows) are kept.
func (i Incident) PublicRedacted() Incident {
	i.ProjectID = ""
	i.MonitorID = ""
	i.ExternalKey = ""
	i.AcknowledgedBy = ""
	i.AcknowledgedByName = ""
	return i
}

// Validate enforces incident invariants (domain-owned).
func (i Incident) Validate() error {
	if strings.TrimSpace(i.Title) == "" {
		return fmt.Errorf("incident: title is required")
	}
	if i.ProjectID == "" {
		return fmt.Errorf("incident: project_id is required")
	}
	if !i.Status.Valid() {
		return fmt.Errorf("incident: unknown status %q", i.Status)
	}
	if !i.Impact.Valid() {
		return fmt.Errorf("incident: unknown impact %q", i.Impact)
	}
	if !i.Source.Valid() {
		return fmt.Errorf("incident: unknown source %q", i.Source)
	}
	return nil
}

// IncidentUpdate is one entry on an incident's timeline. Each update carries the
// status the incident is in as of that entry, plus a markdown body.
type IncidentUpdate struct {
	ID         string         `json:"id"`
	IncidentID string         `json:"incident_id"`
	Status     IncidentStatus `json:"status"`
	Body       string         `json:"body"`
	Author     string         `json:"author"`
	CreatedAt  time.Time      `json:"created_at"`
}

// Validate enforces incident-update invariants.
func (u IncidentUpdate) Validate() error {
	if u.IncidentID == "" {
		return fmt.Errorf("incident update: incident_id is required")
	}
	if !u.Status.Valid() {
		return fmt.Errorf("incident update: unknown status %q", u.Status)
	}
	return nil
}

// Postmortem is the published analysis attached to a resolved incident.
type Postmortem struct {
	ID          string    `json:"id"`
	IncidentID  string    `json:"incident_id"`
	Body        string    `json:"body"`
	Author      string    `json:"author"`
	PublishedAt time.Time `json:"published_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
