package outbox

import (
	"context"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// phoneFake is fakeStore's PhoneStore (FR-041a): scripted incident links, and the names a phone alert
// carries. Every read is counted in reads, so a test can prove a read did NOT happen.
type phoneFake struct {
	reads map[string]int // method name → calls
	// monitorLinks answers MonitorPhoneLink per monitor, one entry per call, the last one repeated:
	// a script of "the reconciler opens the incident on the third lookup".
	monitorLinks map[string][]store.PhoneLink
	serviceLinks map[string]store.PhoneLink
	linkCalls    []linkCall
	projects     map[string]domain.Project
	services     map[string]domain.Service // by service id
	incidents    map[string]domain.Incident
	refs         map[string]string // incident id → ref
	// monitorPhoneIDs and phoneChannels answer the phone-id-only reads the worker makes when the text
	// half's own channel read failed (Task 12 re-review N-1): a monitor's linked phone channels, and
	// which channel ids are phone channels. phoneIDsErr fails both.
	monitorPhoneIDs map[string][]string
	phoneChannels   map[string]bool
	phoneIDsErr     error
	// projectErr fails GetProject: the phone half cannot prepare its message.
	projectErr error
}

type linkCall struct {
	subject string // "monitor:<id>" or "service:<id>"
	since   time.Time
	at      time.Time
}

func (f *phoneFake) read(method string) {
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[method]++
}

func (f *phoneFake) MonitorPhoneLink(_ context.Context, monitorID string, since time.Time) (store.PhoneLink, error) {
	f.read("MonitorPhoneLink")
	f.linkCalls = append(f.linkCalls, linkCall{subject: "monitor:" + monitorID, since: since, at: time.Now()})
	script := f.monitorLinks[monitorID]
	if len(script) == 0 {
		return store.PhoneLink{}, nil
	}
	l := script[0]
	if len(script) > 1 {
		f.monitorLinks[monitorID] = script[1:]
	}
	return l, nil
}

func (f *phoneFake) ServicePhoneLink(_ context.Context, serviceID string, since time.Time) (store.PhoneLink, error) {
	f.read("ServicePhoneLink")
	f.linkCalls = append(f.linkCalls, linkCall{subject: "service:" + serviceID, since: since, at: time.Now()})
	return f.serviceLinks[serviceID], nil
}

func (f *phoneFake) GetProject(_ context.Context, id string) (domain.Project, error) {
	f.read("GetProject")
	if f.projectErr != nil {
		return domain.Project{}, f.projectErr
	}
	p, ok := f.projects[id]
	if !ok {
		return domain.Project{}, store.ErrNotFound
	}
	return p, nil
}

func (f *phoneFake) GetService(_ context.Context, projectID, id string) (domain.Service, error) {
	f.read("GetService")
	s, ok := f.services[id]
	if !ok || s.ProjectID != projectID {
		return domain.Service{}, store.ErrNotFound
	}
	return s, nil
}

func (f *phoneFake) GetIncident(_ context.Context, id string) (domain.Incident, error) {
	f.read("GetIncident")
	inc, ok := f.incidents[id]
	if !ok {
		return domain.Incident{}, store.ErrNotFound
	}
	return inc, nil
}

func (f *phoneFake) IncidentRef(_ context.Context, id string) (string, error) {
	f.read("IncidentRef")
	r, ok := f.refs[id]
	if !ok {
		return "", store.ErrNotFound
	}
	return r, nil
}

func (f *phoneFake) MonitorPhoneChannelIDs(_ context.Context, monitorID string) ([]string, error) {
	f.read("MonitorPhoneChannelIDs")
	if f.phoneIDsErr != nil {
		return nil, f.phoneIDsErr
	}
	return append([]string(nil), f.monitorPhoneIDs[monitorID]...), nil
}

func (f *phoneFake) PhoneChannelIDs(_ context.Context, ids []string) ([]string, error) {
	f.read("PhoneChannelIDs")
	if f.phoneIDsErr != nil {
		return nil, f.phoneIDsErr
	}
	var out []string
	for _, id := range ids {
		if f.phoneChannels[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
