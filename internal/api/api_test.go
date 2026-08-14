package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/auth"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// fakeStore implements api.Store in memory for hermetic handler tests.
type fakePullTest struct {
	region  string
	payload []byte
	result  []byte
	claimed bool
}

type fakeStore struct {
	orgs               map[string]domain.Organization
	projects           map[string]domain.Project
	users              map[string]domain.User
	byOrg              map[string][]domain.Project
	userOrgs           map[string][]domain.Organization
	members            map[string][]domain.Membership
	monitors           map[string]domain.Monitor
	managed            map[string]store.FileManagement // monitor id → file provenance (ownership)
	managedProjects    map[string]bool                 // project id → owned by a file provider (blocks delete)
	managedOrgs        map[string]bool                 // org id → owns a file-managed project (blocks delete)
	diagnostics        []fakeDiag                      // file-provider bundle diagnostics
	passwords          map[string]string
	sessionsDeletedFor []string
	slaTargets         map[string]domain.SLATarget
	slaReports         map[string]bool // project id -> weekly report enabled
	oidcSettings       *domain.OIDCSettings
	instanceSettings   domain.InstanceSettings
	maintenance        map[string]domain.MaintenanceWindow
	incidents          map[string]domain.Incident
	escPolicies        map[string]domain.EscalationPolicy
	oncall             map[string]domain.OnCallSchedule
	overrides          map[string]domain.OnCallOverride
	pullJobs           map[string][][]byte
	pullSeq            int
	acked              []string
	pullTests          map[string]fakePullTest
	agentHeartbeats    map[string]string
	agentTokens        map[string]domain.AgentToken
	backfilled         []domain.Heartbeat
	incUpdates         map[string][]domain.IncidentUpdate
	postmortems        map[string]domain.Postmortem
	pages              map[string]domain.StatusPage
	components         map[string]domain.Component
	apiTokens          map[string]domain.ApiToken
	webhooks           map[string]domain.Webhook
	channels           map[string]domain.NotificationChannel
	monLinks           map[string][]string // monitor id -> channel ids
	deadOutbox         map[string]domain.OutboxEventView
	searchHits         []domain.SearchHit
	subscribers        map[string]domain.Subscriber // keyed by confirm token
	outboxEvents       []struct {
		Topic   string
		Payload []byte
	}
	audit []domain.AuditEntry
	totp  map[string]struct {
		secret  string
		enabled bool
	}
	recovery map[string]bool // userID|hash -> available

	secrets        map[string]map[string]*fakeSecret // project id → name → secret
	secretRefs     map[string]int                    // "projectID/name" → UI-managed ref count
	secretFileRefs map[string]int                    // "projectID/name" → file-managed ref count
	secretSeq      int
}

// fakeSecret backs the project secret inventory in memory. The value is kept
// only so tests can assert it never appears in any response body.
type fakeSecret struct {
	id        string
	value     string
	createdAt time.Time
	rotatedAt *time.Time
}

func seededStore() *fakeStore {
	fs := &fakeStore{
		orgs:        map[string]domain.Organization{},
		projects:    map[string]domain.Project{},
		users:       map[string]domain.User{},
		byOrg:       map[string][]domain.Project{},
		userOrgs:    map[string][]domain.Organization{},
		members:     map[string][]domain.Membership{},
		monitors:    map[string]domain.Monitor{},
		passwords:   map[string]string{},
		slaTargets:  map[string]domain.SLATarget{},
		slaReports:  map[string]bool{},
		maintenance: map[string]domain.MaintenanceWindow{},
		incidents:   map[string]domain.Incident{},
		incUpdates:  map[string][]domain.IncidentUpdate{},
		postmortems: map[string]domain.Postmortem{},
		pages:       map[string]domain.StatusPage{},
		components:  map[string]domain.Component{},
		apiTokens:   map[string]domain.ApiToken{},
		webhooks:    map[string]domain.Webhook{},
		channels:    map[string]domain.NotificationChannel{},
		monLinks:    map[string][]string{},
		deadOutbox:  map[string]domain.OutboxEventView{},
		subscribers: map[string]domain.Subscriber{},
	}
	o1 := domain.Organization{ID: "o1", Slug: "acme", Name: "Acme"}
	o2 := domain.Organization{ID: "o2", Slug: "globex", Name: "Globex"}
	fs.orgs[o1.ID], fs.orgs[o2.ID] = o1, o2
	p1 := domain.Project{ID: "p1", OrgID: "o1", Slug: "api", Name: "API"}
	p2 := domain.Project{ID: "p2", OrgID: "o1", Slug: "webhook", Name: "Webhook"}
	p3 := domain.Project{ID: "p3", OrgID: "o2", Slug: "file", Name: "File"}
	for _, p := range []domain.Project{p1, p2, p3} {
		fs.projects[p.ID] = p
		fs.byOrg[p.OrgID] = append(fs.byOrg[p.OrgID], p)
	}
	fs.users["u1"] = domain.User{ID: "u1", Email: "u1@x"}
	fs.monitors["mon1"] = domain.Monitor{ID: "mon1", ProjectID: "p1", Name: "api-health", Type: domain.MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	fs.monitors["mon3"] = domain.Monitor{ID: "mon3", ProjectID: "p3", Name: "globex", Type: domain.MonitorHTTP, Target: "https://y", IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true}
	fs.monitors["monp"] = domain.Monitor{ID: "monp", ProjectID: "p1", Name: "nightly-backup", Type: domain.MonitorPush, IntervalSeconds: 3600, Enabled: true, PushToken: "tok-push"}
	// A seeded open incident in p1, and a resolved one in p3 (other org).
	fs.incidents["inc1"] = domain.Incident{ID: "inc1", ProjectID: "p1", Title: "api degraded", Status: domain.IncidentInvestigating, Impact: domain.ImpactMajor, Source: domain.SourceManual}
	fs.incUpdates["inc1"] = []domain.IncidentUpdate{{ID: "iu1", IncidentID: "inc1", Status: domain.IncidentInvestigating, Body: "looking into it"}}
	fs.incidents["inc3"] = domain.Incident{ID: "inc3", ProjectID: "p3", Title: "file outage", Status: domain.IncidentResolved, Impact: domain.ImpactMinor, Source: domain.SourceManual}
	// Status pages in o1 across the three visibilities, one with a monitor-backed component.
	fs.pages["sp1"] = domain.StatusPage{ID: "sp1", OrgID: "o1", Slug: "acme-status", Title: "Acme", Visibility: domain.VisibilityPublic}
	fs.pages["sp2"] = domain.StatusPage{ID: "sp2", OrgID: "o1", Slug: "internal-status", Title: "Internal", Visibility: domain.VisibilityInternal}
	fs.pages["sp3"] = domain.StatusPage{ID: "sp3", OrgID: "o1", Slug: "secret-status", Title: "Secret", Visibility: domain.VisibilityUnlisted, UnlistedToken: "tok123"}
	fs.components["c1"] = domain.Component{ID: "c1", StatusPageID: "sp1", Name: "API", MonitorID: "mon1"}
	fs.apiTokens["at1"] = domain.ApiToken{ID: "at1", OrgID: "o1", Name: "ci", Role: domain.RoleEditor}
	fs.webhooks["wh1"] = domain.Webhook{ID: "wh1", OrgID: "o1", URL: "https://hook.example/x", Secret: "s", Enabled: true}
	fs.channels["nc1"] = domain.NotificationChannel{ID: "nc1", ProjectID: "p1", Type: domain.ChannelWebhook, Name: "ops", Config: map[string]string{"url": "https://hook.example/n"}, Enabled: true}
	fs.channels["nc3"] = domain.NotificationChannel{ID: "nc3", ProjectID: "p3", Type: domain.ChannelSlack, Name: "globex", Config: map[string]string{"url": "https://hook.example/c"}, Enabled: true}
	return fs
}

func (f *fakeStore) ListOrganizations(context.Context) ([]domain.Organization, error) {
	return []domain.Organization{f.orgs["o1"], f.orgs["o2"]}, nil
}
func (f *fakeStore) ListOrganizationsForUser(_ context.Context, userID string) ([]domain.Organization, error) {
	return f.userOrgs[userID], nil
}
func (f *fakeStore) CreateOrganization(_ context.Context, slug, name string) (domain.Organization, error) {
	for _, existing := range f.orgs {
		if existing.Slug == slug {
			return domain.Organization{}, store.ErrConflict
		}
	}
	o := domain.Organization{ID: "new", Slug: slug, Name: name}
	f.orgs[o.ID] = o
	return o, nil
}
func (f *fakeStore) GetOrganization(_ context.Context, id string) (domain.Organization, error) {
	o, ok := f.orgs[id]
	if !ok {
		return domain.Organization{}, store.ErrNotFound
	}
	return o, nil
}
func (f *fakeStore) DeleteOrganization(_ context.Context, orgID string) error {
	if _, ok := f.orgs[orgID]; !ok {
		return store.ErrNotFound
	}
	if f.managedOrgs[orgID] {
		return store.ErrManagedByFile
	}
	delete(f.orgs, orgID)
	return nil
}
func (f *fakeStore) ListProjectsByOrg(_ context.Context, orgID string) ([]domain.Project, error) {
	return f.byOrg[orgID], nil
}
func (f *fakeStore) CreateProject(_ context.Context, orgID, slug, name string) (domain.Project, error) {
	for _, existing := range f.projects {
		if existing.OrgID == orgID && existing.Slug == slug {
			return domain.Project{}, store.ErrConflict
		}
	}
	p := domain.Project{ID: "np", OrgID: orgID, Slug: slug, Name: name}
	f.projects[p.ID] = p
	return p, nil
}
func (f *fakeStore) GetProject(_ context.Context, id string) (domain.Project, error) {
	p, ok := f.projects[id]
	if !ok {
		return domain.Project{}, store.ErrNotFound
	}
	return p, nil
}
func (f *fakeStore) DeleteProject(_ context.Context, orgID, projectID string) error {
	p, ok := f.projects[projectID]
	if !ok || p.OrgID != orgID {
		return store.ErrNotFound
	}
	if f.managedProjects[projectID] {
		return store.ErrManagedByFile
	}
	delete(f.projects, projectID)
	return nil
}
func (f *fakeStore) CreateMembership(_ context.Context, m domain.Membership) (domain.Membership, error) {
	if _, ok := f.users[m.UserID]; !ok {
		return domain.Membership{}, store.ErrNotFound
	}
	m.ID = "m-new"
	return m, nil
}
func (f *fakeStore) ListOrgMembers(_ context.Context, orgID string) ([]domain.Member, error) {
	var out []domain.Member
	for _, m := range f.members[orgID] {
		u := f.users[m.UserID]
		out = append(out, domain.Member{Membership: m, Email: u.Email, DisplayName: u.DisplayName})
	}
	return out, nil
}
func (f *fakeStore) GetMembership(_ context.Context, id string) (domain.Membership, error) {
	for _, list := range f.members {
		for _, m := range list {
			if m.ID == id {
				return m, nil
			}
		}
	}
	return domain.Membership{}, store.ErrNotFound
}
func (f *fakeStore) UpdateMembershipRole(_ context.Context, id string, role domain.Role) (domain.Membership, error) {
	for orgID, list := range f.members {
		for i, m := range list {
			if m.ID == id {
				f.members[orgID][i].Role = role
				return f.members[orgID][i], nil
			}
		}
	}
	return domain.Membership{}, store.ErrNotFound
}
func (f *fakeStore) DeleteMembership(_ context.Context, id string) error {
	for orgID, list := range f.members {
		for i, m := range list {
			if m.ID == id {
				f.members[orgID] = append(list[:i], list[i+1:]...)
				return nil
			}
		}
	}
	return store.ErrNotFound
}
func (f *fakeStore) CountOrgAdmins(_ context.Context, orgID string) (int, error) {
	n := 0
	for _, m := range f.members[orgID] {
		if m.ProjectID == "" && m.Role == domain.RoleOrgAdmin {
			n++
		}
	}
	return n, nil
}
func (f *fakeStore) GetUser(_ context.Context, id string) (domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return domain.User{}, store.ErrNotFound
	}
	return u, nil
}
func (f *fakeStore) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	for _, u := range f.users {
		if u.Email == email {
			return u, nil
		}
	}
	return domain.User{}, store.ErrNotFound
}
func (f *fakeStore) ListAllUsers(_ context.Context, q string) ([]domain.AdminUser, error) {
	var out []domain.AdminUser
	for _, u := range f.users {
		if q != "" && !strings.Contains(strings.ToLower(u.Email+" "+u.DisplayName), strings.ToLower(q)) {
			continue
		}
		au := domain.AdminUser{User: u, AuthType: "local", Memberships: []domain.AdminUserMembership{}}
		for _, ms := range f.members {
			for _, m := range ms {
				if m.UserID == u.ID {
					au.Memberships = append(au.Memberships, domain.AdminUserMembership{OrgID: m.OrgID, ProjectID: m.ProjectID, Role: m.Role})
				}
			}
		}
		out = append(out, au)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}
func (f *fakeStore) SetGlobalAdmin(_ context.Context, id string, admin bool) error {
	u, ok := f.users[id]
	if !ok {
		return store.ErrNotFound
	}
	u.IsGlobalAdmin = admin
	f.users[id] = u
	return nil
}
func (f *fakeStore) DeleteUser(_ context.Context, id string) error {
	if _, ok := f.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.users, id)
	for orgID, ms := range f.members {
		kept := ms[:0]
		for _, m := range ms {
			if m.UserID != id {
				kept = append(kept, m)
			}
		}
		f.members[orgID] = kept
	}
	return nil
}
func (f *fakeStore) CountGlobalAdmins(_ context.Context) (int, error) {
	n := 0
	for _, u := range f.users {
		if u.IsGlobalAdmin {
			n++
		}
	}
	return n, nil
}
func (f *fakeStore) ListMonitorsByProject(_ context.Context, projectID string) ([]domain.Monitor, error) {
	var out []domain.Monitor
	for _, m := range f.monitors {
		if m.ProjectID == projectID {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeStore) ListRegions(_ context.Context) ([]string, error) {
	seen := map[string]bool{domain.DefaultRegion: true}
	out := []string{domain.DefaultRegion}
	for _, m := range f.monitors {
		if m.Region != "" && !seen[m.Region] {
			seen[m.Region] = true
			out = append(out, m.Region)
		}
	}
	return out, nil
}
func (f *fakeStore) CreateMonitor(_ context.Context, m domain.Monitor) (domain.Monitor, error) {
	m.ID = "mon-new"
	f.monitors[m.ID] = m
	return m, nil
}
func (f *fakeStore) GetMonitor(_ context.Context, id string) (domain.Monitor, error) {
	m, ok := f.monitors[id]
	if !ok {
		return domain.Monitor{}, store.ErrNotFound
	}
	return m, nil
}
func (f *fakeStore) GetMonitorByPushToken(_ context.Context, token string) (domain.Monitor, time.Time, error) {
	for _, m := range f.monitors {
		if m.PushToken == token {
			return m, time.Unix(1700000000, 0), nil // fixed ingress received_at for tests
		}
	}
	return domain.Monitor{}, time.Time{}, store.ErrNotFound
}
func (f *fakeStore) UpdateMonitor(_ context.Context, m domain.Monitor) (domain.Monitor, error) {
	if _, ok := f.monitors[m.ID]; !ok {
		return domain.Monitor{}, store.ErrNotFound
	}
	if _, managed := f.managed[m.ID]; managed {
		return domain.Monitor{}, store.ErrManagedByFile
	}
	f.monitors[m.ID] = m
	return m, nil
}
func (f *fakeStore) DeleteMonitor(_ context.Context, id string) error {
	if _, ok := f.monitors[id]; !ok {
		return store.ErrNotFound
	}
	if _, managed := f.managed[id]; managed {
		return store.ErrManagedByFile
	}
	delete(f.monitors, id)
	return nil
}
func (f *fakeStore) MonitorProvenance(_ context.Context, id string) (store.FileManagement, bool, error) {
	fm, ok := f.managed[id]
	return fm, ok, nil
}

// fakeDiag pairs a diagnostic with its org id for the org-scoped filter.
type fakeDiag struct {
	orgID string
	diag  store.FileProviderDiagnostic
}

func (f *fakeStore) FileProviderDiagnostics(_ context.Context, orgID string) ([]store.FileProviderDiagnostic, error) {
	var out []store.FileProviderDiagnostic
	for _, d := range f.diagnostics {
		if orgID == "" || d.orgID == orgID {
			out = append(out, d.diag)
		}
	}
	return out, nil
}
func (f *fakeStore) MonitorProvenanceBatch(_ context.Context, ids []string) (map[string]store.FileManagement, error) {
	out := map[string]store.FileManagement{}
	for _, id := range ids {
		if fm, ok := f.managed[id]; ok {
			out[id] = fm
		}
	}
	return out, nil
}
func (f *fakeStore) ReplaceMonitorDependencies(_ context.Context, monitorID, projectID string, parents []string) error {
	for _, p := range parents {
		parent, ok := f.monitors[p]
		if !ok || parent.ProjectID != projectID || p == monitorID {
			return store.ErrDependencyForeign
		}
		for _, gp := range parent.DependsOn { // one-level cycle check is enough for the fake
			if gp == monitorID {
				return store.ErrDependencyCycle
			}
		}
	}
	if m, ok := f.monitors[monitorID]; ok {
		m.DependsOn = parents
		f.monitors[monitorID] = m
	}
	return nil
}
func (f *fakeStore) ListRecentHeartbeats(_ context.Context, _ string, _ int) ([]domain.Heartbeat, error) {
	return nil, nil
}
func (f *fakeStore) PasswordHashByID(_ context.Context, id string) (string, error) {
	h, ok := f.passwords[id]
	if !ok {
		return "", store.ErrNotFound
	}
	return h, nil
}
func (f *fakeStore) SetPassword(_ context.Context, id, passwordHash string) error {
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[id] = passwordHash
	return nil
}
func (f *fakeStore) DeleteSessionsByUser(_ context.Context, userID, _ string) (int64, error) {
	f.sessionsDeletedFor = append(f.sessionsDeletedFor, userID)
	return 0, nil
}
func (f *fakeStore) MonitorSLI(_ context.Context, _ string, _ time.Time) (store.SLICounts, error) {
	return store.SLICounts{Total: 100, Up: 99, AvgLatencyMS: 12}, nil
}
func (f *fakeStore) ProjectSLI(_ context.Context, _ string, _ time.Time) (store.SLICounts, error) {
	return store.SLICounts{Total: 200, Up: 198, AvgLatencyMS: 15}, nil
}
func (f *fakeStore) MonitorDailyAvailability(_ context.Context, _ string, _ time.Time) ([]store.DailyAvailability, error) {
	return []store.DailyAvailability{{Up: 90, Total: 100, UptimePercent: 90}, {Up: 100, Total: 100, UptimePercent: 100}}, nil
}
func (f *fakeStore) ProjectDailyAvailability(_ context.Context, _ string, _ time.Time) ([]store.DailyAvailability, error) {
	return []store.DailyAvailability{{Up: 198, Total: 200, UptimePercent: 99}}, nil
}
func (f *fakeStore) UpsertMonitorSLATarget(_ context.Context, monitorID, window string, objective float64, burnAlert bool, rules []domain.BurnRule) (domain.SLATarget, error) {
	// Mirror the store contract: enabling with no rules seeds the SRE defaults.
	if burnAlert && rules == nil {
		if prev, ok := f.slaTargets[monitorID+"|"+window]; ok && len(prev.BurnRules) > 0 {
			rules = prev.BurnRules
		} else {
			rules = domain.DefaultBurnRules()
		}
	}
	t := domain.SLATarget{ID: "t", MonitorID: monitorID, Window: window, Objective: objective, BurnAlertEnabled: burnAlert, BurnRules: rules}
	f.slaTargets[monitorID+"|"+window] = t
	return t, nil
}
func (f *fakeStore) GetMonitorSLATarget(_ context.Context, monitorID, window string) (domain.SLATarget, error) {
	t, ok := f.slaTargets[monitorID+"|"+window]
	if !ok {
		return domain.SLATarget{}, store.ErrNotFound
	}
	return t, nil
}
func (f *fakeStore) SetProjectSLAReport(_ context.Context, projectID string, enabled bool) (bool, error) {
	f.slaReports[projectID] = enabled
	return enabled, nil
}
func (f *fakeStore) ProjectSLAReportEnabled(_ context.Context, projectID string) (bool, error) {
	return f.slaReports[projectID], nil
}
func (f *fakeStore) GetOIDCSettings(_ context.Context) (domain.OIDCSettings, error) {
	if f.oidcSettings == nil {
		return domain.OIDCSettings{}, store.ErrNotFound
	}
	return *f.oidcSettings, nil
}
func (f *fakeStore) GetInstanceSettings(_ context.Context) (domain.InstanceSettings, error) {
	return f.instanceSettings, nil
}
func (f *fakeStore) UpsertBranding(_ context.Context, b domain.Branding) error {
	f.instanceSettings.Branding = b
	return nil
}
func (f *fakeStore) UpsertAuthPolicy(_ context.Context, p domain.AuthPolicy) error {
	f.instanceSettings.AuthPolicy = p
	return nil
}
func (f *fakeStore) UpsertAlerting(_ context.Context, a domain.Alerting) error {
	f.instanceSettings.Alerting = a
	return nil
}
func (f *fakeStore) UpsertMonitorDefaults(_ context.Context, d domain.MonitorDefaults) error {
	f.instanceSettings.MonitorDefaults = d
	return nil
}
func (f *fakeStore) UpsertMail(_ context.Context, m domain.MailSettings) error {
	f.instanceSettings.Mail = m
	return nil
}
func (f *fakeStore) UpsertOIDCSettings(_ context.Context, s domain.OIDCSettings) error {
	cp := s
	f.oidcSettings = &cp
	return nil
}
func (f *fakeStore) CreateMaintenanceWindow(_ context.Context, mw domain.MaintenanceWindow) (domain.MaintenanceWindow, error) {
	mw.ID = "mw-new"
	f.maintenance[mw.ID] = mw
	return mw, nil
}
func (f *fakeStore) ListMaintenanceWindowsByProject(_ context.Context, projectID string) ([]domain.MaintenanceWindow, error) {
	var out []domain.MaintenanceWindow
	for _, mw := range f.maintenance {
		if mw.ProjectID == projectID {
			out = append(out, mw)
		}
	}
	return out, nil
}
func (f *fakeStore) GetMaintenanceWindow(_ context.Context, id string) (domain.MaintenanceWindow, error) {
	mw, ok := f.maintenance[id]
	if !ok {
		return domain.MaintenanceWindow{}, store.ErrNotFound
	}
	return mw, nil
}
func (f *fakeStore) DeleteMaintenanceWindow(_ context.Context, id string) error {
	if _, ok := f.maintenance[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.maintenance, id)
	return nil
}

func (f *fakeStore) CreateIncident(_ context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error) {
	inc.ID = "inc-new"
	inc.StartedAt = time.Now()
	f.incidents[inc.ID] = inc
	f.incUpdates[inc.ID] = append(f.incUpdates[inc.ID], domain.IncidentUpdate{
		ID: "iu-open", IncidentID: inc.ID, Status: inc.Status, Body: openingBody, Author: author,
	})
	return inc, nil
}
func (f *fakeStore) GetIncident(_ context.Context, id string) (domain.Incident, error) {
	inc, ok := f.incidents[id]
	if !ok {
		return domain.Incident{}, store.ErrNotFound
	}
	return inc, nil
}
func (f *fakeStore) AcknowledgeIncident(_ context.Context, id, by string) (domain.Incident, error) {
	inc, ok := f.incidents[id]
	if !ok || inc.Status == domain.IncidentResolved {
		return domain.Incident{}, store.ErrNotFound
	}
	if inc.AcknowledgedAt == nil {
		now := time.Now()
		inc.AcknowledgedAt, inc.AcknowledgedBy = &now, by
		f.incidents[id] = inc
	}
	return inc, nil
}

func (f *fakeStore) ClaimPullJobs(_ context.Context, region string, _, _ int) ([]store.PullJob, error) {
	if f.pullJobs == nil {
		return nil, nil
	}
	payloads := f.pullJobs[region]
	f.pullJobs[region] = nil
	out := make([]store.PullJob, 0, len(payloads))
	for _, p := range payloads {
		f.pullSeq++
		out = append(out, store.PullJob{Token: fmt.Sprintf("tok-%s-%d", region, f.pullSeq), Payload: p})
	}
	return out, nil
}

func (f *fakeStore) AckPullJobs(_ context.Context, tokens []string) error {
	f.acked = append(f.acked, tokens...)
	return nil
}
func (f *fakeStore) ClaimPullTest(_ context.Context, region string) (string, []byte, bool, error) {
	if f.pullTests == nil {
		return "", nil, false, nil
	}
	for id, pt := range f.pullTests {
		if pt.region == region && !pt.claimed {
			pt.claimed = true
			f.pullTests[id] = pt
			return id, pt.payload, true, nil
		}
	}
	return "", nil, false, nil
}
func (f *fakeStore) SavePullTestResult(_ context.Context, id, region string, result []byte) error {
	if pt, ok := f.pullTests[id]; ok && pt.region == region {
		pt.result = result
		f.pullTests[id] = pt
	}
	return nil
}

func (f *fakeStore) RecordAgentHeartbeat(_ context.Context, region, agentID string) error {
	if f.agentHeartbeats == nil {
		f.agentHeartbeats = map[string]string{}
	}
	f.agentHeartbeats[region] = agentID
	return nil
}
func (f *fakeStore) RecordHistoricalResults(_ context.Context, hbs []domain.Heartbeat) (int, int, error) {
	f.backfilled = append(f.backfilled, hbs...)
	return len(hbs), 0, nil
}
func (f *fakeStore) MonitorRegions(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if m, ok := f.monitors[id]; ok {
			r := m.Region
			if r == "" {
				r = "core"
			}
			out[id] = r
		}
	}
	return out, nil
}
func (f *fakeStore) CreateAgentToken(_ context.Context, name, region, hash string) (domain.AgentToken, error) {
	if f.agentTokens == nil {
		f.agentTokens = map[string]domain.AgentToken{}
	}
	t := domain.AgentToken{ID: "at-" + name, Name: name, Region: region}
	f.agentTokens[hash] = t
	return t, nil
}
func (f *fakeStore) ResolveAgentTokenRegion(_ context.Context, hash string) (string, bool, error) {
	t, ok := f.agentTokens[hash]
	return t.Region, ok, nil
}
func (f *fakeStore) ListAgentTokens(_ context.Context) ([]domain.AgentToken, error) {
	var out []domain.AgentToken
	for _, t := range f.agentTokens {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeStore) RevokeAgentToken(_ context.Context, id string) error {
	for h, t := range f.agentTokens {
		if t.ID == id {
			delete(f.agentTokens, h)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeStore) CreateEscalationPolicy(_ context.Context, p domain.EscalationPolicy) (domain.EscalationPolicy, error) {
	if f.escPolicies == nil {
		f.escPolicies = map[string]domain.EscalationPolicy{}
	}
	p.ID = "esc-" + p.Name
	f.escPolicies[p.ID] = p
	return p, nil
}
func (f *fakeStore) GetEscalationPolicy(_ context.Context, id string) (domain.EscalationPolicy, error) {
	p, ok := f.escPolicies[id]
	if !ok {
		return domain.EscalationPolicy{}, store.ErrNotFound
	}
	return p, nil
}
func (f *fakeStore) ListEscalationPolicies(_ context.Context, projectID string) ([]domain.EscalationPolicy, error) {
	var out []domain.EscalationPolicy
	for _, p := range f.escPolicies {
		if p.ProjectID == projectID {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeStore) UpdateEscalationPolicy(_ context.Context, p domain.EscalationPolicy) (domain.EscalationPolicy, error) {
	if _, ok := f.escPolicies[p.ID]; !ok {
		return domain.EscalationPolicy{}, store.ErrNotFound
	}
	f.escPolicies[p.ID] = p
	return p, nil
}
func (f *fakeStore) DeleteEscalationPolicy(_ context.Context, id string) error {
	if _, ok := f.escPolicies[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.escPolicies, id)
	return nil
}

func (f *fakeStore) CreateOnCallSchedule(_ context.Context, sc domain.OnCallSchedule) (domain.OnCallSchedule, error) {
	if f.oncall == nil {
		f.oncall = map[string]domain.OnCallSchedule{}
	}
	sc.ID = "oncall-" + sc.Name
	f.oncall[sc.ID] = sc
	return sc, nil
}
func (f *fakeStore) GetOnCallSchedule(_ context.Context, id string) (domain.OnCallSchedule, error) {
	sc, ok := f.oncall[id]
	if !ok {
		return domain.OnCallSchedule{}, store.ErrNotFound
	}
	return sc, nil
}
func (f *fakeStore) ListOnCallSchedules(_ context.Context, projectID string) ([]domain.OnCallSchedule, error) {
	var out []domain.OnCallSchedule
	for _, sc := range f.oncall {
		if sc.ProjectID == projectID {
			out = append(out, sc)
		}
	}
	return out, nil
}
func (f *fakeStore) UpdateOnCallSchedule(_ context.Context, sc domain.OnCallSchedule) (domain.OnCallSchedule, error) {
	if _, ok := f.oncall[sc.ID]; !ok {
		return domain.OnCallSchedule{}, store.ErrNotFound
	}
	f.oncall[sc.ID] = sc
	return sc, nil
}
func (f *fakeStore) DeleteOnCallSchedule(_ context.Context, id string) error {
	if _, ok := f.oncall[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.oncall, id)
	return nil
}
func (f *fakeStore) AddOnCallOverride(_ context.Context, o domain.OnCallOverride) (domain.OnCallOverride, error) {
	if f.overrides == nil {
		f.overrides = map[string]domain.OnCallOverride{}
	}
	o.ID = "ov-" + o.ScheduleID
	f.overrides[o.ID] = o
	return o, nil
}
func (f *fakeStore) ListOnCallOverrides(_ context.Context, scheduleID string) ([]domain.OnCallOverride, error) {
	var out []domain.OnCallOverride
	for _, o := range f.overrides {
		if o.ScheduleID == scheduleID {
			out = append(out, o)
		}
	}
	return out, nil
}
func (f *fakeStore) GetOnCallOverride(_ context.Context, id string) (domain.OnCallOverride, error) {
	o, ok := f.overrides[id]
	if !ok {
		return domain.OnCallOverride{}, store.ErrNotFound
	}
	return o, nil
}
func (f *fakeStore) DeleteOnCallOverride(_ context.Context, id string) error {
	if _, ok := f.overrides[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.overrides, id)
	return nil
}

func (f *fakeStore) FindOpenIncidentByExternalKey(_ context.Context, projectID, key string) (domain.Incident, error) {
	for _, inc := range f.incidents {
		if inc.ProjectID == projectID && inc.ExternalKey == key && inc.Status != domain.IncidentResolved {
			return inc, nil
		}
	}
	return domain.Incident{}, store.ErrNotFound
}
func (f *fakeStore) ListIncidentsByProject(_ context.Context, projectID string) ([]domain.Incident, error) {
	var out []domain.Incident
	for _, inc := range f.incidents {
		if inc.ProjectID == projectID {
			out = append(out, inc)
		}
	}
	return out, nil
}
func (f *fakeStore) AddIncidentUpdate(_ context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error) {
	upd.ID = "iu-new"
	f.incUpdates[upd.IncidentID] = append(f.incUpdates[upd.IncidentID], upd)
	if inc, ok := f.incidents[upd.IncidentID]; ok {
		inc.Status = upd.Status
		if upd.Status == domain.IncidentResolved && inc.ResolvedAt == nil {
			now := time.Now()
			inc.ResolvedAt = &now
		}
		f.incidents[upd.IncidentID] = inc
	}
	return upd, nil
}
func (f *fakeStore) ListIncidentUpdates(_ context.Context, incidentID string) ([]domain.IncidentUpdate, error) {
	return f.incUpdates[incidentID], nil
}
func (f *fakeStore) UpsertPostmortem(_ context.Context, incidentID, body, author string) (domain.Postmortem, error) {
	pm := domain.Postmortem{ID: "pm", IncidentID: incidentID, Body: body, Author: author}
	f.postmortems[incidentID] = pm
	return pm, nil
}
func (f *fakeStore) GetPostmortem(_ context.Context, incidentID string) (domain.Postmortem, error) {
	pm, ok := f.postmortems[incidentID]
	if !ok {
		return domain.Postmortem{}, store.ErrNotFound
	}
	return pm, nil
}

func (f *fakeStore) CreateStatusPage(_ context.Context, sp domain.StatusPage) (domain.StatusPage, error) {
	sp.ID = "sp-new"
	f.pages[sp.ID] = sp
	return sp, nil
}

func (f *fakeStore) UpdateStatusPage(_ context.Context, sp domain.StatusPage) (domain.StatusPage, error) {
	if _, ok := f.pages[sp.ID]; !ok {
		return domain.StatusPage{}, store.ErrNotFound
	}
	f.pages[sp.ID] = sp
	return sp, nil
}

func (f *fakeStore) DeleteStatusPage(_ context.Context, id string) error {
	if _, ok := f.pages[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.pages, id)
	return nil
}
func (f *fakeStore) GetStatusPage(_ context.Context, id string) (domain.StatusPage, error) {
	sp, ok := f.pages[id]
	if !ok {
		return domain.StatusPage{}, store.ErrNotFound
	}
	return sp, nil
}
func (f *fakeStore) GetStatusPageBySlug(_ context.Context, slug string) (domain.StatusPage, error) {
	for _, sp := range f.pages {
		if sp.Slug == slug {
			return sp, nil
		}
	}
	return domain.StatusPage{}, store.ErrNotFound
}
func (f *fakeStore) ListStatusPagesByOrg(_ context.Context, orgID string) ([]domain.StatusPage, error) {
	var out []domain.StatusPage
	for _, sp := range f.pages {
		if sp.OrgID == orgID {
			out = append(out, sp)
		}
	}
	return out, nil
}
func (f *fakeStore) CreateComponent(_ context.Context, c domain.Component) (domain.Component, error) {
	c.ID = "c-new"
	f.components[c.ID] = c
	return c, nil
}
func (f *fakeStore) ListComponentsByPage(_ context.Context, pageID string) ([]domain.Component, error) {
	var out []domain.Component
	for _, c := range f.components {
		if c.StatusPageID == pageID {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeStore) GetComponent(_ context.Context, id string) (domain.Component, error) {
	c, ok := f.components[id]
	if !ok {
		return domain.Component{}, store.ErrNotFound
	}
	return c, nil
}
func (f *fakeStore) DeleteComponent(_ context.Context, id string) error {
	if _, ok := f.components[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.components, id)
	return nil
}
func (f *fakeStore) ListOpenIncidentsByProject(_ context.Context, projectID string) ([]domain.Incident, error) {
	var out []domain.Incident
	for _, inc := range f.incidents {
		if inc.ProjectID == projectID && inc.Status != domain.IncidentResolved {
			out = append(out, inc)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateApiToken(_ context.Context, t domain.ApiToken, hash string) (domain.ApiToken, error) {
	t.ID = "tok-new"
	f.apiTokens[t.ID] = t
	return t, nil
}
func (f *fakeStore) ListApiTokensByOrg(_ context.Context, orgID string) ([]domain.ApiToken, error) {
	var out []domain.ApiToken
	for _, t := range f.apiTokens {
		if t.OrgID == orgID {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeStore) GetApiToken(_ context.Context, id string) (domain.ApiToken, error) {
	t, ok := f.apiTokens[id]
	if !ok {
		return domain.ApiToken{}, store.ErrNotFound
	}
	return t, nil
}
func (f *fakeStore) DeleteApiToken(_ context.Context, id string) error {
	if _, ok := f.apiTokens[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.apiTokens, id)
	return nil
}

func (f *fakeStore) CreateWebhook(_ context.Context, h domain.Webhook) (domain.Webhook, error) {
	h.ID = "wh-new"
	f.webhooks[h.ID] = h
	return h, nil
}
func (f *fakeStore) SetWebhookEnabled(_ context.Context, id string, enabled bool) error {
	h, ok := f.webhooks[id]
	if !ok {
		return store.ErrNotFound
	}
	h.Enabled = enabled
	f.webhooks[id] = h
	return nil
}
func (f *fakeStore) SetNotificationChannelEnabled(_ context.Context, id string, enabled bool) error {
	c, ok := f.channels[id]
	if !ok {
		return store.ErrNotFound
	}
	c.Enabled = enabled
	f.channels[id] = c
	return nil
}
func (f *fakeStore) GetWebhook(_ context.Context, id string) (domain.Webhook, error) {
	h, ok := f.webhooks[id]
	if !ok {
		return domain.Webhook{}, store.ErrNotFound
	}
	return h, nil
}
func (f *fakeStore) ListWebhooksByOrg(_ context.Context, orgID string) ([]domain.Webhook, error) {
	var out []domain.Webhook
	for _, h := range f.webhooks {
		if h.OrgID == orgID {
			out = append(out, h)
		}
	}
	return out, nil
}
func (f *fakeStore) DeleteWebhook(_ context.Context, id string) error {
	if _, ok := f.webhooks[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.webhooks, id)
	return nil
}

func (f *fakeStore) CreateNotificationChannel(_ context.Context, ch domain.NotificationChannel) (domain.NotificationChannel, error) {
	ch.ID = "nc-new"
	f.channels[ch.ID] = ch
	return ch, nil
}
func (f *fakeStore) GetNotificationChannel(_ context.Context, id string) (domain.NotificationChannel, error) {
	ch, ok := f.channels[id]
	if !ok {
		return domain.NotificationChannel{}, store.ErrNotFound
	}
	return ch, nil
}
func (f *fakeStore) ListChannelsByProject(_ context.Context, projectID string) ([]domain.NotificationChannel, error) {
	var out []domain.NotificationChannel
	for _, ch := range f.channels {
		if ch.ProjectID == projectID {
			out = append(out, ch)
		}
	}
	return out, nil
}
func (f *fakeStore) DeleteNotificationChannel(_ context.Context, id string) error {
	if _, ok := f.channels[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.channels, id)
	return nil
}
func (f *fakeStore) LinkMonitorChannel(_ context.Context, monitorID, channelID string) error {
	f.monLinks[monitorID] = append(f.monLinks[monitorID], channelID)
	return nil
}
func (f *fakeStore) UnlinkMonitorChannel(_ context.Context, monitorID, channelID string) error {
	kept := f.monLinks[monitorID][:0]
	for _, c := range f.monLinks[monitorID] {
		if c != channelID {
			kept = append(kept, c)
		}
	}
	f.monLinks[monitorID] = kept
	return nil
}
func (f *fakeStore) ListMonitorChannels(_ context.Context, monitorID string) ([]domain.NotificationChannel, error) {
	var out []domain.NotificationChannel
	for _, id := range f.monLinks[monitorID] {
		if ch, ok := f.channels[id]; ok {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (f *fakeStore) ListDeadOutbox(_ context.Context, limit int) ([]domain.OutboxEventView, error) {
	var out []domain.OutboxEventView
	for _, e := range f.deadOutbox {
		if len(out) >= limit {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeStore) ReplayDeadOutbox(_ context.Context, id string) error {
	if _, ok := f.deadOutbox[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.deadOutbox, id) // requeued
	return nil
}

func (f *fakeStore) ReplayAllDeadOutbox(_ context.Context) (int, error) {
	n := len(f.deadOutbox)
	f.deadOutbox = map[string]domain.OutboxEventView{}
	return n, nil
}

func (f *fakeStore) Search(_ context.Context, _ string, _ int, scope store.SearchScope) ([]domain.SearchHit, error) {
	if scope.AllOrgs {
		return f.searchHits, nil
	}
	orgs := map[string]bool{}
	for _, o := range scope.OrgIDs {
		orgs[o] = true
	}
	projs := map[string]bool{}
	for _, p := range scope.ProjectIDs {
		projs[p] = true
	}
	var out []domain.SearchHit
	for _, h := range f.searchHits {
		if orgs[h.OrgID] || projs[h.ProjectID] {
			out = append(out, h)
		}
	}
	return out, nil
}

func newHandler(fs *fakeStore) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).Router()
}

func newPublicHandler(fs *fakeStore) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).PublicRouter()
}

// newHandlerWithFPStatus builds a router with a file-provider runtime status source wired in,
// so diagnostics tests can assert the "providers" section of the admin response.
func newHandlerWithFPStatus(fs *fakeStore, src api.FileProviderStatusSource) http.Handler {
	return api.New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), 8).WithFileProviderStatus(src).Router()
}

// fakeFPStatus is a static FileProviderStatusSource for tests.
type fakeFPStatus []api.FileProviderRuntimeStatus

func (f fakeFPStatus) FileProviderRuntimeStatuses() []api.FileProviderRuntimeStatus { return f }

func do(h http.Handler, p authz.Principal, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var (
	globalAdmin = authz.Principal{UserID: "ga", IsGlobalAdmin: true}
	o1Admin     = authz.Principal{UserID: "oa", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleOrgAdmin}}}
	o1Viewer    = authz.Principal{UserID: "u1", Memberships: []domain.Membership{{OrgID: "o1", Role: domain.RoleViewer}}}
	p1Viewer    = authz.Principal{UserID: "pv", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleViewer}}}
	outsider    = authz.Principal{UserID: "out"}
)

func TestMe(t *testing.T) {
	h := newHandler(seededStore())
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me code = %d", rec.Code)
	}
}

func TestCreateOrganizationRequiresGlobalAdmin(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations", `{"slug":"x","name":"X"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("org admin create org = %d, want 403", rec.Code)
	}
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/organizations", `{"slug":"x","name":"X"}`); rec.Code != http.StatusCreated {
		t.Fatalf("global admin create org = %d, want 201", rec.Code)
	}
}

func TestGetOrganizationIsolation(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1", ""); rec.Code != http.StatusOK {
		t.Fatalf("member get own org = %d, want 200", rec.Code)
	}
	// Not a member of o2 → 404 (existence hidden).
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o2", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-member get org = %d, want 404", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/organizations/o1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider get org = %d, want 404", rec.Code)
	}
}

func TestListProjectsVisibilityFiltering(t *testing.T) {
	h := newHandler(seededStore())

	// Org-level viewer sees all projects in o1.
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1/projects", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list projects code = %d", rec.Code)
	}
	var orgViewerProjects []domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &orgViewerProjects)
	if len(orgViewerProjects) != 2 {
		t.Fatalf("org viewer should see 2 projects, got %d", len(orgViewerProjects))
	}

	// Project-scoped viewer sees only p1.
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/organizations/o1/projects", "")
	var projViewerProjects []domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &projViewerProjects)
	if len(projViewerProjects) != 1 || projViewerProjects[0].ID != "p1" {
		t.Fatalf("project viewer should see only p1, got %+v", projViewerProjects)
	}
}

func TestGetProjectIsolation(t *testing.T) {
	h := newHandler(seededStore())
	// p1 viewer can see p1 but not p2 (same org) or p3 (other org).
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p1", ""); rec.Code != http.StatusOK {
		t.Fatalf("p1 viewer get p1 = %d, want 200", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p2", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("p1 viewer get p2 = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/projects/p3", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("p1 viewer get p3 = %d, want 404", rec.Code)
	}
}

// TestDeleteProjectAuthz covers FR-018 authorization + status mapping: only an org
// admin (or global admin) may delete; a project admin cannot; invisible/unknown ⇒ 404;
// a file-managed project ⇒ 409 (spec func-project-deletion §6/§11).
func TestDeleteProjectAuthz(t *testing.T) {
	p1Admin := authz.Principal{UserID: "pa", Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleProjectAdmin}}}

	// Denials — nothing is actually deleted here.
	h := newHandler(seededStore())
	if rec := do(h, outsider, http.MethodDelete, "/api/v1/projects/p1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider delete = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodDelete, "/api/v1/projects/p1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("project viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Admin, http.MethodDelete, "/api/v1/projects/p1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("project admin delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/projects/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project delete = %d, want 404", rec.Code)
	}

	// A file-provider-owned project is refused with 409 even for an org admin.
	fs := seededStore()
	fs.managedProjects = map[string]bool{"p1": true}
	if rec := do(newHandler(fs), o1Admin, http.MethodDelete, "/api/v1/projects/p1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete file-managed project = %d, want 409", rec.Code)
	}

	// Happy path: org admin deletes p1 (then it's gone); global admin deletes p2.
	h2 := newHandler(seededStore())
	if rec := do(h2, o1Admin, http.MethodDelete, "/api/v1/projects/p1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("org admin delete = %d, want 204", rec.Code)
	}
	if rec := do(h2, o1Admin, http.MethodGet, "/api/v1/projects/p1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted project = %d, want 404", rec.Code)
	}
	if rec := do(h2, globalAdmin, http.MethodDelete, "/api/v1/projects/p2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("global admin delete = %d, want 204", rec.Code)
	}
}

// TestDeleteOrganizationAuthz covers FR-019: global-admin only, 403 for everyone else
// (no existence leak), 404 for a global admin on an unknown org, 409 for a file-managed
// org (spec func-org-deletion §6/§10).
func TestDeleteOrganizationAuthz(t *testing.T) {
	// Non-global-admins are refused with 403 regardless of membership.
	h := newHandler(seededStore())
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/organizations/o1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("org admin delete org = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/organizations/o1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member delete org = %d, want 403", rec.Code)
	}
	if rec := do(h, outsider, http.MethodDelete, "/api/v1/organizations/o1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider delete org = %d, want 403", rec.Code)
	}

	// Global admin: 404 for an unknown org.
	if rec := do(h, globalAdmin, http.MethodDelete, "/api/v1/organizations/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("global admin delete unknown org = %d, want 404", rec.Code)
	}

	// File-provider-owned org ⇒ 409 even for a global admin.
	fs := seededStore()
	fs.managedOrgs = map[string]bool{"o1": true}
	if rec := do(newHandler(fs), globalAdmin, http.MethodDelete, "/api/v1/organizations/o1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("global admin delete file-managed org = %d, want 409", rec.Code)
	}

	// Happy path: global admin deletes o1; a re-delete then 404s (it's gone).
	h2 := newHandler(seededStore())
	if rec := do(h2, globalAdmin, http.MethodDelete, "/api/v1/organizations/o1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("global admin delete org = %d, want 204", rec.Code)
	}
	if rec := do(h2, globalAdmin, http.MethodDelete, "/api/v1/organizations/o1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete gone org = %d, want 404", rec.Code)
	}
}

func TestCreateProjectAuthz(t *testing.T) {
	h := newHandler(seededStore())
	// Outsider → 404 (org hidden).
	if rec := do(h, outsider, http.MethodPost, "/api/v1/organizations/o1/projects", `{"slug":"s","name":"n"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider create project = %d, want 404", rec.Code)
	}
	// In-org viewer → 403 (insufficient role).
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/organizations/o1/projects", `{"slug":"s","name":"n"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create project = %d, want 403", rec.Code)
	}
	// Org admin → 201.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/projects", `{"slug":"s","name":"n"}`); rec.Code != http.StatusCreated {
		t.Fatalf("org admin create project = %d, want 201", rec.Code)
	}
}

func TestMeUnauthorizedWithoutPrincipal(t *testing.T) {
	h := newHandler(seededStore())
	// No principal in context.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without principal = %d, want 401", rec.Code)
	}
}

func TestListOrganizationsBranches(t *testing.T) {
	fs := seededStore()
	fs.userOrgs["u1"] = []domain.Organization{fs.orgs["o1"]}
	h := newHandler(fs)

	rec := do(h, globalAdmin, http.MethodGet, "/api/v1/organizations", "")
	var all []domain.Organization
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all) != 2 {
		t.Fatalf("global admin should see all orgs, got %d", len(all))
	}

	rec = do(h, o1Viewer, http.MethodGet, "/api/v1/organizations", "")
	var mine []domain.Organization
	_ = json.Unmarshal(rec.Body.Bytes(), &mine)
	if len(mine) != 1 || mine[0].ID != "o1" {
		t.Fatalf("member should see only o1, got %+v", mine)
	}
}

func TestCreateOrganizationValidation(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/organizations", `{"slug":"","name":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty fields = %d, want 400", rec.Code)
	}
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/organizations", `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", rec.Code)
	}
}

// TestDuplicateSlugConflict proves a duplicate slug is a clean 409, not a raw 500
// leaking the DB unique-constraint violation.
func TestDuplicateSlugConflict(t *testing.T) {
	h := newHandler(seededStore())
	// "acme" is o1's slug.
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/organizations", `{"slug":"acme","name":"Dup"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate org slug = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	// "api" is p1's slug within o1.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/projects", `{"slug":"api","name":"Dup"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate project slug = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	// A fresh slug still succeeds.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/projects", `{"slug":"brand-new","name":"New"}`); rec.Code != http.StatusCreated {
		t.Fatalf("fresh project slug = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
}

func TestListMembersAuthz(t *testing.T) {
	fs := seededStore()
	fs.members["o1"] = []domain.Membership{{ID: "m1", OrgID: "o1", UserID: "u1", Role: domain.RoleViewer}}
	h := newHandler(fs)

	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/organizations/o1/members", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer list members = %d, want 403", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/organizations/o1/members", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider list members = %d, want 404", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/organizations/o1/members", ""); rec.Code != http.StatusOK {
		t.Fatalf("org admin list members = %d, want 200", rec.Code)
	}
}

func TestGetProjectNotFound(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, globalAdmin, http.MethodGet, "/api/v1/projects/unknown", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project = %d, want 404", rec.Code)
	}
}

func TestAddMemberUnknownUser(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"user_id":"ghost","role":"viewer"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown user = %d, want 400", rec.Code)
	}
}

func TestMonitorSLAAndTarget(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	// Read SLA (no target yet) → 200, no objective in windows.
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/monitors/mon1/sla", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("monitor sla = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "error_budget") {
		t.Fatal("no target set → no error_budget expected")
	}

	// Viewer cannot set target.
	if rec := do(h, o1Viewer, http.MethodPut, "/api/v1/monitors/mon1/sla-target", `{"objective":99.9,"window":"30d"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer set target = %d, want 403", rec.Code)
	}
	// Invalid objective / window.
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/sla-target", `{"objective":0,"window":"30d"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad objective = %d, want 400", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/sla-target", `{"objective":99.9,"window":"99y"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad window = %d, want 400", rec.Code)
	}
	// Admin sets a valid target; SLA read now includes error budget.
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/sla-target", `{"objective":99.9,"window":"30d"}`); rec.Code != http.StatusOK {
		t.Fatalf("set target = %d, want 200", rec.Code)
	}
	rec = do(h, o1Viewer, http.MethodGet, "/api/v1/monitors/mon1/sla", "")
	if !strings.Contains(rec.Body.String(), "error_budget") || !strings.Contains(rec.Body.String(), "objective") {
		t.Fatalf("expected objective+error_budget after target set: %s", rec.Body.String())
	}

	// Enabling burn-rate alerting persists and surfaces in the SLA read.
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/monitors/mon1/sla-target", `{"objective":99.9,"window":"30d","burn_alert":true}`); rec.Code != http.StatusOK {
		t.Fatalf("set burn target = %d, want 200", rec.Code)
	}
	if !fs.slaTargets["mon1|30d"].BurnAlertEnabled {
		t.Fatal("burn_alert=true not persisted on the target")
	}
	rec = do(h, o1Viewer, http.MethodGet, "/api/v1/monitors/mon1/sla", "")
	if !strings.Contains(rec.Body.String(), "burn_alert") {
		t.Fatalf("expected burn_alert in SLA read: %s", rec.Body.String())
	}
}

func TestProjectSLAReportToggle(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	// Default off, and it surfaces in the project SLA read.
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p1/sla", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sla_report_weekly":false`) {
		t.Fatalf("default report state = %d %s", rec.Code, rec.Body.String())
	}
	// Viewer cannot toggle.
	if rec := do(h, o1Viewer, http.MethodPut, "/api/v1/projects/p1/sla-report", `{"enabled":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer toggle = %d, want 403", rec.Code)
	}
	// Admin enables it; state persists and reads back.
	if rec := do(h, o1Admin, http.MethodPut, "/api/v1/projects/p1/sla-report", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("admin toggle = %d, want 200", rec.Code)
	}
	if !fs.slaReports["p1"] {
		t.Fatal("report flag not persisted")
	}
	rec = do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p1/sla", "")
	if !strings.Contains(rec.Body.String(), `"sla_report_weekly":true`) {
		t.Fatalf("enabled report not reflected: %s", rec.Body.String())
	}
}

func TestMonitorSLAIsolation(t *testing.T) {
	h := newHandler(seededStore())
	// mon3 is in p3 (org o2); an o1 member must not see its SLA.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/monitors/mon3/sla", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org monitor sla = %d, want 404", rec.Code)
	}
}

func TestProjectSLA(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p1/sla", ""); rec.Code != http.StatusOK {
		t.Fatalf("project sla = %d, want 200", rec.Code)
	}
	if rec := do(h, outsider, http.MethodGet, "/api/v1/projects/p1/sla", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider project sla = %d, want 404", rec.Code)
	}
}

func TestMaintenanceCRUD(t *testing.T) {
	h := newHandler(seededStore())
	body := `{"starts_at":"2026-01-01T00:00:00Z","ends_at":"2026-01-01T01:00:00Z","reason":"upgrade"}`

	// Viewer cannot create.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/projects/p1/maintenance", body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create maintenance = %d, want 403", rec.Code)
	}
	// Admin creates a project-level window.
	rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/maintenance", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create maintenance = %d, want 201", rec.Code)
	}
	// Monitor-scoped window with a foreign monitor → 400.
	bad := `{"monitor_id":"mon3","starts_at":"2026-01-01T00:00:00Z","ends_at":"2026-01-01T01:00:00Z"}`
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/maintenance", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign monitor maintenance = %d, want 400", rec.Code)
	}
	// Invalid range → 400.
	badRange := `{"starts_at":"2026-01-01T01:00:00Z","ends_at":"2026-01-01T00:00:00Z"}`
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/maintenance", badRange); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range = %d, want 400", rec.Code)
	}
	// List, then delete.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p1/maintenance", ""); rec.Code != http.StatusOK {
		t.Fatalf("list maintenance = %d, want 200", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/maintenance/mw-new", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete maintenance = %d, want 204", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/maintenance/ghost", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", rec.Code)
	}
}

func TestChangePassword(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	old, _ := auth.HashPassword("oldpass12")
	fs.passwords["u1"] = old // o1Viewer.UserID == "u1"

	// Wrong current password → 400.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/me/password", `{"current_password":"nope","new_password":"newpass12"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong current = %d, want 400", rec.Code)
	}
	// New password too short → 400.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/me/password", `{"current_password":"oldpass12","new_password":"short"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("short new = %d, want 400", rec.Code)
	}
	// Success → 204 and hash updated.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/me/password", `{"current_password":"oldpass12","new_password":"newpass12"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("change = %d, want 204", rec.Code)
	}
	if ok, _ := auth.VerifyPassword(fs.passwords["u1"], "newpass12"); !ok {
		t.Fatal("password not updated to new value")
	}
	// Non-local user (no password) → 400.
	if rec := do(h, globalAdmin, http.MethodPost, "/api/v1/me/password", `{"current_password":"x","new_password":"newpass12"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-local user = %d, want 400", rec.Code)
	}
}

func TestMonitorListAndCreateAuthz(t *testing.T) {
	h := newHandler(seededStore())
	// Viewer can list monitors in a visible project.
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p1/monitors", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer list monitors = %d, want 200", rec.Code)
	}
	// Viewer cannot create (needs write).
	body := `{"name":"n","type":"http","target":"https://z","interval_seconds":60,"timeout_seconds":10}`
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/projects/p1/monitors", body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create monitor = %d, want 403", rec.Code)
	}
	// Org admin can create.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", body); rec.Code != http.StatusCreated {
		t.Fatalf("admin create monitor = %d, want 201", rec.Code)
	}
	// Invalid monitor rejected.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/monitors", `{"name":"","type":"http"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid monitor = %d, want 400", rec.Code)
	}
}

func TestMonitorIsolationAcrossProjects(t *testing.T) {
	h := newHandler(seededStore())
	// p1 viewer can get mon1 (in p1) but not mon3 (in p3, another org).
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/monitors/mon1", ""); rec.Code != http.StatusOK {
		t.Fatalf("p1 viewer get mon1 = %d, want 200", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/monitors/mon3", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("p1 viewer get mon3 = %d, want 404", rec.Code)
	}
	// Listing p3 monitors as an o1 member → 404 (project hidden).
	if rec := do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p3/monitors", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("o1 viewer list p3 monitors = %d, want 404", rec.Code)
	}
}

func TestMonitorDeleteAuthz(t *testing.T) {
	h := newHandler(seededStore())
	if rec := do(h, o1Viewer, http.MethodDelete, "/api/v1/monitors/mon1", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete = %d, want 403", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodDelete, "/api/v1/monitors/mon1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204", rec.Code)
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/monitors/mon1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}

func TestAddMemberAuthz(t *testing.T) {
	h := newHandler(seededStore())
	body := `{"user_id":"u1","role":"viewer"}`
	// Viewer cannot manage members.
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/organizations/o1/members", body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer add member = %d, want 403", rec.Code)
	}
	// Org admin can.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", body); rec.Code != http.StatusCreated {
		t.Fatalf("org admin add member = %d, want 201", rec.Code)
	}
	// Invalid role rejected (domain validation) → 400.
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/organizations/o1/members", `{"user_id":"u1","role":"superuser"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid role = %d, want 400", rec.Code)
	}
}

func (f *fakeStore) ListSubscribersByPage(_ context.Context, pageID string) ([]domain.Subscriber, error) {
	var out []domain.Subscriber
	for _, sub := range f.subscribers {
		if sub.StatusPageID == pageID {
			out = append(out, sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}
func (f *fakeStore) DeleteSubscriber(_ context.Context, pageID, id string) error {
	// The fake map is keyed by confirm token; match on the row id.
	for tok, sub := range f.subscribers {
		if sub.ID == id && sub.StatusPageID == pageID {
			delete(f.subscribers, tok)
			return nil
		}
	}
	return store.ErrNotFound
}
func (f *fakeStore) CreateSubscriber(_ context.Context, sub domain.Subscriber) (domain.Subscriber, error) {
	// Emulate ON CONFLICT (page,email): reuse an existing row, re-issue the token.
	for tok, s := range f.subscribers {
		if s.StatusPageID == sub.StatusPageID && s.Email == sub.Email {
			delete(f.subscribers, tok)
			s.ConfirmToken = sub.ConfirmToken
			f.subscribers[s.ConfirmToken] = s
			return s, nil
		}
	}
	sub.ID = "sub-" + sub.ConfirmToken
	f.subscribers[sub.ConfirmToken] = sub
	return sub, nil
}

func (f *fakeStore) ConfirmSubscriber(_ context.Context, token string) (domain.Subscriber, error) {
	s, ok := f.subscribers[token]
	if !ok {
		return domain.Subscriber{}, store.ErrNotFound
	}
	if s.ConfirmedAt == nil {
		now := time.Now()
		s.ConfirmedAt = &now
		f.subscribers[token] = s
	}
	return s, nil
}

func (f *fakeStore) DeleteSubscriberByToken(_ context.Context, token string) error {
	if _, ok := f.subscribers[token]; !ok {
		return store.ErrNotFound
	}
	delete(f.subscribers, token)
	return nil
}

func (f *fakeStore) EnqueueOutbox(_ context.Context, topic string, payload []byte) error {
	f.outboxEvents = append(f.outboxEvents, struct {
		Topic   string
		Payload []byte
	}{topic, payload})
	return nil
}

// fakeMailer records sent messages for assertions.
type fakeMailer struct {
	sent []struct{ To, Subject, Body string }
}

func (m *fakeMailer) Send(to, subject, body string) error {
	m.sent = append(m.sent, struct{ To, Subject, Body string }{to, subject, body})
	return nil
}
func (m *fakeMailer) BaseURL() string { return "https://status.example" }

func (f *fakeStore) ListProjectsForUser(_ context.Context, userID string) ([]domain.Project, error) {
	var out []domain.Project
	for _, o := range f.userOrgs[userID] {
		out = append(out, f.byOrg[o.ID]...)
	}
	return out, nil
}

func (f *fakeStore) RecordAudit(_ context.Context, e domain.AuditEntry) error {
	e.ID = "au-new"
	f.audit = append([]domain.AuditEntry{e}, f.audit...) // newest first
	return nil
}
func (f *fakeStore) ListAuditByOrg(_ context.Context, orgID string, limit int) ([]domain.AuditEntry, error) {
	var out []domain.AuditEntry
	for _, e := range f.audit {
		if e.OrgID == orgID {
			out = append(out, e)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) totpState() map[string]struct {
	secret  string
	enabled bool
} {
	if f.totp == nil {
		f.totp = map[string]struct {
			secret  string
			enabled bool
		}{}
	}
	return f.totp
}
func (f *fakeStore) SetTOTPSecret(_ context.Context, userID, secret string) error {
	f.totpState()[userID] = struct {
		secret  string
		enabled bool
	}{secret, false}
	return nil
}
func (f *fakeStore) GetTOTP(_ context.Context, userID string) (string, bool, error) {
	s := f.totpState()[userID]
	return s.secret, s.enabled, nil
}
func (f *fakeStore) EnableTOTP(_ context.Context, userID string) error {
	s := f.totpState()[userID]
	s.enabled = true
	f.totpState()[userID] = s
	return nil
}
func (f *fakeStore) DisableTOTP(_ context.Context, userID string) error {
	delete(f.totpState(), userID)
	return nil
}
func (f *fakeStore) ReplaceRecoveryCodes(_ context.Context, userID string, hashes []string) error {
	if f.recovery == nil {
		f.recovery = map[string]bool{}
	}
	for _, h := range hashes {
		f.recovery[userID+"|"+h] = true
	}
	return nil
}

// Project secret inventory (map-backed, honoring the store's typed errors).
// Mutations record their audit entry themselves, mirroring the real store's
// audit-in-tx behavior (spec §5): the handler no longer writes secret audit rows.


func fakeSecretValueInvalid(value string) bool {
	return len(value) == 0 || len(value) > 4096 || !utf8.ValidString(value)
}

// recordSecretAudit is the fake's stand-in for the store-level in-tx audit row.
func (f *fakeStore) recordSecretAudit(projectID string, actor store.SecretActor, action, target string) {
	org := ""
	if p, ok := f.projects[projectID]; ok {
		org = p.OrgID
	}
	f.audit = append([]domain.AuditEntry{{
		ID: "au-new", OrgID: org, ActorUserID: actor.ActorUserID, ViaToken: actor.ViaToken,
		Action: action, Target: target,
	}}, f.audit...)
}

func (f *fakeStore) projectSecrets(projectID string) map[string]*fakeSecret {
	if f.secrets == nil {
		f.secrets = map[string]map[string]*fakeSecret{}
	}
	if f.secrets[projectID] == nil {
		f.secrets[projectID] = map[string]*fakeSecret{}
	}
	return f.secrets[projectID]
}

func (f *fakeStore) secretRefCounts(projectID, name string) (ui, file int) {
	key := projectID + "/" + name
	return f.secretRefs[key], f.secretFileRefs[key]
}

func (f *fakeStore) CreateProjectSecret(_ context.Context, actor store.SecretActor, projectID, name, value string) (store.ProjectSecret, error) {
	if !domain.ValidSecretName(name) {
		return store.ProjectSecret{}, store.ErrSecretNameInvalid
	}
	if fakeSecretValueInvalid(value) {
		return store.ProjectSecret{}, store.ErrSecretValueInvalid
	}
	if _, ok := f.projects[projectID]; !ok {
		return store.ProjectSecret{}, store.ErrNotFound
	}
	ps := f.projectSecrets(projectID)
	if _, ok := ps[name]; ok {
		return store.ProjectSecret{}, store.ErrSecretExists
	}
	if len(ps) >= 100 {
		return store.ProjectSecret{}, store.ErrSecretQuota
	}
	f.secretSeq++
	s := &fakeSecret{id: fmt.Sprintf("sec-%d", f.secretSeq), value: value, createdAt: time.Unix(1700000000, 0)}
	ps[name] = s
	f.recordSecretAudit(projectID, actor, "secret.create", name)
	return store.ProjectSecret{ID: s.id, Name: name, CreatedAt: s.createdAt}, nil
}

func (f *fakeStore) UpdateProjectSecret(_ context.Context, actor store.SecretActor, projectID, name string, newName, newValue *string) (renamed, rotated bool, repointed int, err error) {
	ps := f.projectSecrets(projectID)
	s, ok := ps[name]
	if !ok {
		return false, false, 0, store.ErrNotFound
	}
	if newValue != nil && fakeSecretValueInvalid(*newValue) {
		return false, false, 0, store.ErrSecretValueInvalid
	}
	if newName != nil && !domain.ValidSecretName(*newName) {
		return false, false, 0, store.ErrSecretNameInvalid
	}
	if newName != nil && *newName != name {
		if _, file := f.secretRefCounts(projectID, name); file > 0 {
			return false, false, 0, store.SecretRenamedInUseError{Count: file}
		}
		if _, exists := ps[*newName]; exists {
			return false, false, 0, store.ErrSecretExists
		}
	}
	if newValue != nil {
		s.value = *newValue
		now := time.Unix(1700000100, 0)
		s.rotatedAt = &now
		rotated = true
	}
	if newName != nil && *newName != name {
		delete(ps, name)
		ps[*newName] = s
		ui, _ := f.secretRefCounts(projectID, name)
		repointed = ui
		if f.secretRefs != nil {
			delete(f.secretRefs, projectID+"/"+name)
			f.secretRefs[projectID+"/"+*newName] = ui
		}
		renamed = true
	}
	if renamed || rotated {
		target := name
		if renamed {
			target = name + " → " + *newName
		}
		f.recordSecretAudit(projectID, actor, "secret.update",
			fmt.Sprintf("%s · renamed=%t rotated=%t repointed=%d", target, renamed, rotated, repointed))
	}
	return renamed, rotated, repointed, nil
}

func (f *fakeStore) DeleteProjectSecret(_ context.Context, actor store.SecretActor, projectID, name string) error {
	ps := f.projectSecrets(projectID)
	if _, ok := ps[name]; !ok {
		return store.ErrNotFound
	}
	if ui, file := f.secretRefCounts(projectID, name); ui+file > 0 {
		return store.SecretInUseError{Count: ui + file}
	}
	delete(ps, name)
	f.recordSecretAudit(projectID, actor, "secret.delete", name)
	return nil
}

func (f *fakeStore) ListProjectSecrets(_ context.Context, projectID string) ([]store.ProjectSecret, error) {
	ps := f.projectSecrets(projectID)
	names := make([]string, 0, len(ps))
	for n := range ps {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]store.ProjectSecret, 0, len(names))
	for _, n := range names {
		s := ps[n]
		ui, file := f.secretRefCounts(projectID, n)
		out = append(out, store.ProjectSecret{
			ID: s.id, Name: n, CreatedAt: s.createdAt, RotatedAt: s.rotatedAt,
			UsedByTotal: ui + file, UsedByFileManaged: file,
		})
	}
	return out, nil
}
