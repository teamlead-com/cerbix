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
	// CompNoData is measurement ABSENT (FR-021 §15.0): nothing sealed, no SLI declared, a
	// monitor never confirmed either way, a manual component whose operator said nothing.
	// It is NOT part of the severity ladder below — asking whether "we do not know" is better
	// or worse than "declared maintenance" is a false comparison, so the summary keeps the two
	// apart as measured-versus-unmeasured instead of ordering them.
	CompNoData ComponentStatus = "no_data"
)

// Valid reports whether c is a known component status.
func (c ComponentStatus) Valid() bool {
	switch c {
	case CompOperational, CompDegraded, CompPartialOutage, CompMajorOutage, CompMaintenance, CompNoData:
		return true
	default:
		return false
	}
}

// Measured reports whether the status is an actual observation of the service's state.
// CompNoData — and any unrecognized value — is not: the summary counts those separately and
// NEVER folds them into the ladder, which is what keeps an unknown from rolling up as health.
func (c ComponentStatus) Measured() bool {
	switch c {
	case CompOperational, CompDegraded, CompPartialOutage, CompMajorOutage, CompMaintenance:
		return true
	default:
		return false
	}
}

// severity orders MEASURED component statuses from best to worst for summary rollup.
// Maintenance is informational and never worse than an outage. Unmeasured values never reach
// this function (Summarize filters them), so there is no honest default to return — an
// unrecognized value is treated as the worst measured state rather than as health, so a future
// enum value can never silently mean "fine".
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
		return 5
	}
}

// ComponentStatusFromMonitor maps a monitor's last observed state to a component status.
//
// PENDING is a monitor that has never been confirmed either way, so it maps to CompNoData —
// NOT to operational as it did before FR-021 phase 4. That old mapping shipped the unknown as
// health on public pages, which is the defect §17 enumerates as one of three intentional
// changes to existing public output.
func ComponentStatusFromMonitor(s MonitorStatus) ComponentStatus {
	switch s {
	case StatusDown:
		return CompMajorOutage
	case StatusUp:
		return CompOperational
	case StatusPending:
		return CompNoData
	default:
		return CompNoData
	}
}

// PageSummaryState is the headline discriminator of a status page (§15.0). It exists because
// one ComponentStatus cannot express "operational, but part of this page was not measured".
type PageSummaryState string

const (
	SummaryOperational PageSummaryState = "operational"
	SummaryImpaired    PageSummaryState = "impaired" // any measured status worse than operational
	SummaryNoData      PageSummaryState = "no_data"  // nothing on the page was measured
	SummaryEmpty       PageSummaryState = "empty"    // no components at all
)

// PageSummary is the total, fail-closed summary of a page's components.
type PageSummary struct {
	// Status is the worst MEASURED status, kept a ComponentStatus so existing clients keep
	// parsing the field they already read. It is CompNoData when nothing was measured.
	Status ComponentStatus `json:"summary"`
	// UnmeasuredCount is how many components reported no measurement.
	UnmeasuredCount int `json:"unmeasured_count"`
	// State is the headline discriminator.
	State PageSummaryState `json:"summary_state"`
}

// Summarize is the ONE total summarizer (§15.0, invariant 67). It classifies every component as
// measured or unmeasured — an unrecognized enum value counts as UNMEASURED, so it can never
// contribute severity 0 — and reports both halves.
//
// An EMPTY page reports SummaryEmpty with CompNoData, not operational: before phase 4,
// SummaryStatus(nil) returned operational, so a page with nothing configured asserted that
// everything was fine. That is the second of the three inherited lies §17 enumerates.
func Summarize(statuses []ComponentStatus) PageSummary {
	if len(statuses) == 0 {
		return PageSummary{Status: CompNoData, State: SummaryEmpty}
	}
	worst := CompOperational
	measured := 0
	unmeasured := 0
	for _, s := range statuses {
		if !s.Measured() {
			unmeasured++
			continue
		}
		measured++
		if s.severity() > worst.severity() {
			worst = s
		}
	}
	switch {
	case measured == 0:
		return PageSummary{Status: CompNoData, UnmeasuredCount: unmeasured, State: SummaryNoData}
	case worst != CompOperational:
		return PageSummary{Status: worst, UnmeasuredCount: unmeasured, State: SummaryImpaired}
	default:
		return PageSummary{Status: CompOperational, UnmeasuredCount: unmeasured, State: SummaryOperational}
	}
}

// SummaryStatus is retained for callers that only need the worst measured status.
// Prefer Summarize: this function cannot express the unmeasured half.
func SummaryStatus(statuses []ComponentStatus) ComponentStatus {
	return Summarize(statuses).Status
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
	ID           string `json:"id"`
	StatusPageID string `json:"status_page_id"`
	// OrgID is the row's OWN tenant identity: before phase 4 a component reached its org only
	// through its page, so a direct writer could bind another org's service to it (§15.0 P0).
	OrgID       string `json:"org_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	GroupName   string `json:"group,omitempty"`
	Position    int    `json:"position"`
	// Source is the ACTIVE binding (FR-021 §15.0): the discriminator, never the presence
	// of a column. The inactive bindings below stay DORMANT so a conversion can be
	// reverted without re-choosing what it replaced.
	Source ComponentSource `json:"source"`
	// SourceProject is the project of the BINDINGS — deliberately not the page's scope, since
	// an org-level page legitimately holds components from several projects.
	SourceProject string          `json:"source_project,omitempty"`
	MonitorID     string          `json:"monitor_id,omitempty"`
	ServiceID     string          `json:"service_id,omitempty"`
	ManualStatus  ComponentStatus `json:"manual_status,omitempty"`
	// Revision is the structural CAS half of the conversion preview: it is compared inside
	// the confirming transaction so an operator cannot apply consent to a changed component.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ComponentSource names which binding a component actually renders from.
type ComponentSource string

const (
	// Prefixed because domain already has incident SourceManual/SourceAuto: a component's
	// source and an incident's source are different vocabularies and must not be mistakable.
	ComponentSourceMonitor ComponentSource = "monitor"
	ComponentSourceService ComponentSource = "service"
	ComponentSourceManual  ComponentSource = "manual"
)

// ValidComponentSource reports whether s is one of the three declared sources. There is no
// default: a component that does not say what it renders from cannot be rendered honestly.
func ValidComponentSource(s ComponentSource) bool {
	switch s {
	case ComponentSourceMonitor, ComponentSourceService, ComponentSourceManual:
		return true
	}
	return false
}

// Validate enforces component invariants: a name, and a manual status an operator is actually
// allowed to STATE. A component with no binding and no status is valid and renders `no_data` — it
// is the honest description of "the operator has not spoken yet", and before FR-021 phase 4 that
// same row rendered `operational`.
//
// The `no_data` refusal lives HERE, not only in the store and the transport ([314] P2-1): it is a
// business rule about what an operator may claim, so the domain owns it and every writer inherits
// it. The database CHECK stays as the backstop for a direct writer, not as the definition.
func (c Component) Validate() error {
	if c.StatusPageID == "" {
		return fmt.Errorf("component: status_page_id is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("component: name is required")
	}
	if c.ManualStatus != "" {
		if !c.ManualStatus.Valid() {
			return fmt.Errorf("component: unknown manual status %q", c.ManualStatus)
		}
		if !c.ManualStatus.Measured() {
			return fmt.Errorf("component: %q is computed when measurement is absent and cannot be set",
				c.ManualStatus)
		}
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
