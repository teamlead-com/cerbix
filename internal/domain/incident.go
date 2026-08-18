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
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	MonitorID string `json:"monitor_id,omitempty"` // set for auto-incidents from a monitor
	// ServiceID is the OTHER anchor (FR-022, D-0170). At most one anchor is set — the schema enforces
	// it — and an incident with neither is a project-level record, which is what a manual incident has
	// always been. The discriminator is read EXPLICITLY everywhere: phase 4 paid for the alternative
	// when `monitor_id != ""` as an implicit discriminator published a converted component's old monitor.
	ServiceID      string         `json:"service_id,omitempty"`
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

// PublicRedacted returns a copy safe for an unauthenticated status page: it strips the
// internal identifiers (its own id, the incident id) and the author (a user id/name that
// an outside viewer must not see). Body, status, and timestamp — what a timeline shows —
// are kept.
func (u IncidentUpdate) PublicRedacted() IncidentUpdate {
	u.ID = ""
	u.IncidentID = ""
	u.Author = ""
	return u
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

// PublicRedacted returns a copy safe for an unauthenticated status page: internal ids and
// the author are stripped; the published body + timestamp are kept.
func (p Postmortem) PublicRedacted() Postmortem {
	p.ID = ""
	p.IncidentID = ""
	p.Author = ""
	return p
}

// IncidentMember is one member of a service AS OF the instant its incident opened (FR-022 invariant 13).
// It is a snapshot, not a reference: the monitor it names may be gone by the time a postmortem is read,
// and the postmortem still has to be able to say who was in the service when it broke.
type IncidentMember struct {
	MonitorID string `json:"monitor_id"`
	Name      string `json:"name"`
	// Roles is a LIST because one monitor can hold several at once — a member is commonly both
	// operational context and an SLI, and the declaration stores a row per role. A postmortem listing
	// the same monitor twice reads as a duplicate rather than as a fact, so the roles are aggregated
	// here and the monitor appears once.
	Roles []string `json:"roles,omitempty"`
}
