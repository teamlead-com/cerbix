package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// EscalationTargetType is what a policy step notifies: a fixed notification
// channel, or whoever is currently on call in a rotation.
type EscalationTargetType string

const (
	EscalationTargetChannel  EscalationTargetType = "channel"
	EscalationTargetSchedule EscalationTargetType = "schedule"
)

// Valid reports whether t is a known target type.
func (t EscalationTargetType) Valid() bool {
	return t == EscalationTargetChannel || t == EscalationTargetSchedule
}

// EscalationTarget is one recipient of a step: a notification channel or an
// on-call schedule (resolved to the currently-on-call channel at fire time).
type EscalationTarget struct {
	Type EscalationTargetType `json:"type"`
	ID   string               `json:"id"`
}

// EscalationStep is one rung of the ladder: after AfterSeconds from the incident
// start, notify Targets (unless the incident was acknowledged first).
type EscalationStep struct {
	AfterSeconds int                `json:"after_seconds"`
	Targets      []EscalationTarget `json:"targets"`
}

// EscalationPolicy is a per-project ordered ladder of notification steps applied
// to a down monitor's open auto-incident. Steps fire at increasing offsets from
// the incident start; acknowledgement or recovery stops the ladder. RepeatLast
// re-sends the final step on the monitor's renotify cadence until acknowledged.
type EscalationPolicy struct {
	ID         string           `json:"id"`
	ProjectID  string           `json:"project_id"`
	Name       string           `json:"name"`
	RepeatLast bool             `json:"repeat_last"`
	Steps      []EscalationStep `json:"steps"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// Validate enforces policy invariants (domain-owned): non-empty name/project, at
// least one step, non-decreasing offsets, and every step has valid targets.
func (p EscalationPolicy) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("escalation policy: name is required")
	}
	if p.ProjectID == "" {
		return fmt.Errorf("escalation policy: project_id is required")
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("escalation policy: at least one step is required")
	}
	prev := -1
	for i, s := range p.Steps {
		if s.AfterSeconds < 0 {
			return fmt.Errorf("escalation policy: step %d after_seconds must be >= 0", i+1)
		}
		if s.AfterSeconds < prev {
			return fmt.Errorf("escalation policy: step %d after_seconds must not decrease", i+1)
		}
		prev = s.AfterSeconds
		if len(s.Targets) == 0 {
			return fmt.Errorf("escalation policy: step %d has no targets", i+1)
		}
		for _, t := range s.Targets {
			if !t.Type.Valid() {
				return fmt.Errorf("escalation policy: step %d has unknown target type %q", i+1, t.Type)
			}
			if strings.TrimSpace(t.ID) == "" {
				return fmt.Errorf("escalation policy: step %d has an empty target id", i+1)
			}
		}
	}
	return nil
}

// OnCallOverride temporarily replaces who is on call for a schedule during a window
// (e.g. vacation cover): while StartsAt <= t < EndsAt, ChannelID is on call regardless
// of the rotation.
type OnCallOverride struct {
	ID         string    `json:"id"`
	ScheduleID string    `json:"schedule_id"`
	ChannelID  string    `json:"channel_id"`
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// Validate enforces override invariants (domain-owned).
func (o OnCallOverride) Validate() error {
	if strings.TrimSpace(o.ChannelID) == "" {
		return fmt.Errorf("on-call override: channel_id is required")
	}
	if !o.EndsAt.After(o.StartsAt) {
		return fmt.Errorf("on-call override: ends_at must be after starts_at")
	}
	return nil
}

// OnCallSchedule is a per-project rotation over notification channels: at any
// instant exactly one participant is on call, advancing every ShiftSeconds from
// AnchorAt. The rotation is a pure function of time (deterministic, no state).
// Overrides (if any, loaded with the schedule) take precedence during their window.
type OnCallSchedule struct {
	ID           string           `json:"id"`
	ProjectID    string           `json:"project_id"`
	Name         string           `json:"name"`
	ShiftSeconds int              `json:"shift_seconds"`
	AnchorAt     time.Time        `json:"anchor_at"`
	Participants []string         `json:"participants"` // ordered channel ids
	Overrides    []OnCallOverride `json:"overrides,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// Validate enforces schedule invariants (domain-owned).
func (s OnCallSchedule) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("on-call schedule: name is required")
	}
	if s.ProjectID == "" {
		return fmt.Errorf("on-call schedule: project_id is required")
	}
	if s.ShiftSeconds <= 0 {
		return fmt.Errorf("on-call schedule: shift_seconds must be > 0")
	}
	if len(s.Participants) == 0 {
		return fmt.Errorf("on-call schedule: at least one participant is required")
	}
	for i, c := range s.Participants {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("on-call schedule: participant %d is empty", i+1)
		}
	}
	return nil
}

// OnCall returns the channel id on call at instant t. An active override wins;
// otherwise the rotation index is floor((t - anchor)/shift) mod len, normalized so
// instants before the anchor still resolve.
func (s OnCallSchedule) OnCall(t time.Time) string {
	for _, o := range s.Overrides {
		if !t.Before(o.StartsAt) && t.Before(o.EndsAt) {
			return o.ChannelID
		}
	}
	n := len(s.Participants)
	if n == 0 {
		return ""
	}
	if s.ShiftSeconds <= 0 {
		return s.Participants[0]
	}
	elapsed := t.Sub(s.AnchorAt).Seconds()
	slot := int64(math.Floor(elapsed / float64(s.ShiftSeconds)))
	i := ((slot % int64(n)) + int64(n)) % int64(n)
	return s.Participants[i]
}
