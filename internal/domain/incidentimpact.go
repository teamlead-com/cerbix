package domain

import (
	"fmt"
)

// FR-042 / NFR-035: incident impact vocabulary. Spec: docs/specs/func-incident-impact.md.

// Rank orders impact: none 0 < minor 1 < major 2 < critical 3; -1 for anything else (§4.3).
func (i IncidentImpact) Rank() int {
	switch i {
	case ImpactNone:
		return 0
	case ImpactMinor:
		return 1
	case ImpactMajor:
		return 2
	case ImpactCritical:
		return 3
	}
	return -1
}

// ImpactMapping says which impact an auto-opened service incident gets for each paging state (§4.1).
// The zero value means "not stated" and resolves to the D6 defaults.
type ImpactMapping struct {
	Degraded IncidentImpact `json:"degraded"`
	Down     IncidentImpact `json:"down"`
	Unknown  IncidentImpact `json:"unknown"`
}

// DefaultImpactMapping is the D6 default: degraded=minor, down=major, unknown=none.
func DefaultImpactMapping() ImpactMapping {
	return ImpactMapping{Degraded: ImpactMinor, Down: ImpactMajor, Unknown: ImpactNone}
}

// IsZero reports whether all three keys are empty.
func (m ImpactMapping) IsZero() bool {
	return m.Degraded == "" && m.Down == "" && m.Unknown == ""
}

// Resolved returns the defaults for a zero mapping and m unchanged otherwise (a partial mapping stays
// partial so Validate can refuse it).
func (m ImpactMapping) Resolved() ImpactMapping {
	if m.IsZero() {
		return DefaultImpactMapping()
	}
	return m
}

// Validate requires every key to be a valid impact and names the offending key.
func (m ImpactMapping) Validate() error {
	for _, k := range []struct {
		name string
		v    IncidentImpact
	}{{"degraded", m.Degraded}, {"down", m.Down}, {"unknown", m.Unknown}} {
		if !k.v.Valid() {
			return fmt.Errorf("impact_by_state.%s: unknown impact %q", k.name, k.v)
		}
	}
	return nil
}

// For returns the impact for a paging state; healthy and excluded have none.
func (m ImpactMapping) For(s ServiceAlertState) (IncidentImpact, bool) {
	switch s {
	case ServiceAlertDegraded:
		return m.Degraded, true
	case ServiceAlertDown:
		return m.Down, true
	case ServiceAlertUnknown:
		return m.Unknown, true
	}
	return "", false
}

// String renders the mapping in a fixed key order.
func (m ImpactMapping) String() string {
	return fmt.Sprintf("degraded=%s,down=%s,unknown=%s", m.Degraded, m.Down, m.Unknown)
}

// ImpactBasisSource says why an incident's impact is what it is (D8).
type ImpactBasisSource string

const (
	ImpactBasisServiceMapping ImpactBasisSource = "service_mapping"
	ImpactBasisProjectDefault ImpactBasisSource = "project_default"
	ImpactBasisManual         ImpactBasisSource = "manual"
	ImpactBasisAlertmanager   ImpactBasisSource = "alertmanager"
)

// ImpactBasisService is the service's name and slug as of the incident's opening.
type ImpactBasisService struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// ImpactBasis is NOT a field of Incident (D15): it is read by its own store call.
type ImpactBasis struct {
	Source  ImpactBasisSource   `json:"source"`
	Mapping *ImpactMapping      `json:"mapping,omitempty"`
	State   ServiceAlertState   `json:"state,omitempty"`
	Service *ImpactBasisService `json:"service,omitempty"`
}

// Validate mirrors incidents_impact_basis_chk exactly.
func (b ImpactBasis) Validate() error {
	switch b.Source {
	case ImpactBasisProjectDefault, ImpactBasisManual, ImpactBasisAlertmanager:
		if b.Mapping != nil || b.State != "" || b.Service != nil {
			return fmt.Errorf("impact basis: source %q carries nothing else", b.Source)
		}
		return nil
	case ImpactBasisServiceMapping:
		if b.Mapping == nil || b.Service == nil {
			return fmt.Errorf("impact basis: service_mapping needs mapping, state and service")
		}
		if err := b.Mapping.Validate(); err != nil {
			return fmt.Errorf("impact basis: %w", err)
		}
		switch b.State {
		case ServiceAlertDegraded, ServiceAlertDown, ServiceAlertUnknown:
		default:
			return fmt.Errorf("impact basis: state %q is not degraded, down or unknown", b.State)
		}
		return nil
	}
	return fmt.Errorf("impact basis: unknown source %q", b.Source)
}

// ImpactRaise is the optional from/to pair a raise carries on its incident event (§4.3).
type ImpactRaise struct {
	From IncidentImpact `json:"from"`
	To   IncidentImpact `json:"to"`
}

// ImpactRaisedMarker prefixes every system note a raise writes.
const ImpactRaisedMarker = "⬆ Impact raised:"

// ImpactRaisedNote is the timeline note for a raise.
func ImpactRaisedNote(from, to IncidentImpact, state ServiceAlertState) string {
	var tail string
	switch state {
	case ServiceAlertDegraded:
		tail = "the service is degraded."
	case ServiceAlertDown:
		tail = "the service is down."
	default:
		tail = "the service's state is unknown."
	}
	return fmt.Sprintf("%s %s → %s — %s", ImpactRaisedMarker, from, to, tail)
}

// ServiceIncidentOpeningNote is the opening note of an automatically opened service incident (§4.2).
func ServiceIncidentOpeningNote(state ServiceAlertState, confirmedOver int) string {
	word := "UNKNOWN"
	switch state {
	case ServiceAlertDown:
		word = "DOWN"
	case ServiceAlertDegraded:
		word = "DEGRADED"
	}
	return fmt.Sprintf("Opened automatically: service %s confirmed over %d evaluation(s).", word, confirmedOver)
}
