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

// rank orders the lifecycle. It is unexported because the ORDER is the contract and the numbers are
// not: nothing outside this file should compare statuses by arithmetic.
func (s IncidentStatus) rank() int {
	switch s {
	case IncidentInvestigating:
		return 0
	case IncidentIdentified:
		return 1
	case IncidentMonitoring:
		return 2
	case IncidentResolved:
		return 3
	default:
		return -1
	}
}

// CanFollow reports whether s is a legal status for an update posted against an incident currently at
// `current`. Forward or staying put is legal; going BACKWARD is not.
//
// "Forward-flowing" was written down as the lifecycle's contract and then enforced only at its last
// step: the store refused writes once an incident was resolved, and nothing stopped `monitoring →
// identified` or `identified → investigating`. Those are not races — they are accepted, sequentially,
// through the ordinary API — and they make the public timeline read as if the operators went
// backwards. The race makes it worse rather than causing it: a plain comment carrying a status read
// moments earlier lands after somebody else moved the incident on, and silently reverts it.
func (s IncidentStatus) CanFollow(current IncidentStatus) bool {
	if current.Terminal() {
		return false
	}
	cr, sr := current.rank(), s.rank()
	if cr < 0 || sr < 0 {
		return false
	}
	return sr >= cr
}

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
	// The OTHER anchor is an internal id of exactly the same class as MonitorID (FR-022
	// invariant 11): a status page names its components by slug and name, and an
	// unauthenticated viewer has no use for the service's UUID. Adding an anchor without
	// adding it here is how a redaction list silently stops being complete.
	i.ServiceID = ""
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
	// An EMPTY status is legal and means "keep whatever the incident is at when this lands". It is
	// resolved inside the store's transaction, against the locked row. Materializing it in the
	// caller — reading the incident, then posting its status back — is what let a plain comment
	// revert a transition somebody else had already made.
	if u.Status != "" && !u.Status.Valid() {
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
