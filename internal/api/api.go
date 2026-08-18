// Package api serves the cerbix REST API over stdlib net/http routing. Every
// handler runs behind the auth middleware (a principal is in the request
// context) and enforces authorization via package authz. Tenant isolation is
// applied on every read and write: a caller only sees and mutates orgs/projects
// they are authorized for.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/teamlead-com/cerbix/internal/auth"
	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/settings"
	"github.com/teamlead-com/cerbix/internal/sla"
	"github.com/teamlead-com/cerbix/internal/store"
)

// Store is the persistence surface the API needs.
type Store interface {
	ListOrganizations(ctx context.Context) ([]domain.Organization, error)
	ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error)
	CreateOrganization(ctx context.Context, slug, name string) (domain.Organization, error)
	GetOrganization(ctx context.Context, id string) (domain.Organization, error)
	DeleteOrganization(ctx context.Context, orgID string) error
	ListProjectsByOrg(ctx context.Context, orgID string) ([]domain.Project, error)
	CreateProject(ctx context.Context, orgID, slug, name string) (domain.Project, error)
	GetProject(ctx context.Context, id string) (domain.Project, error)
	DeleteProject(ctx context.Context, orgID, projectID string) error
	ListProjectsForUser(ctx context.Context, userID string) ([]domain.Project, error)
	CreateMembership(ctx context.Context, m domain.Membership) (domain.Membership, error)
	ListOrgMembers(ctx context.Context, orgID string) ([]domain.Member, error)
	GetMembership(ctx context.Context, id string) (domain.Membership, error)
	UpdateMembershipRole(ctx context.Context, id string, role domain.Role) (domain.Membership, error)
	DeleteMembership(ctx context.Context, id string) error
	CountOrgAdmins(ctx context.Context, orgID string) (int, error)
	GetUser(ctx context.Context, id string) (domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (domain.User, error)
	ListAllUsers(ctx context.Context, q string) ([]domain.AdminUser, error)
	SetGlobalAdmin(ctx context.Context, id string, admin bool) error
	DeleteUser(ctx context.Context, id string) error
	CountGlobalAdmins(ctx context.Context) (int, error)
	ListMonitorsByProject(ctx context.Context, projectID string) ([]domain.Monitor, error)
	ListRegions(ctx context.Context) ([]string, error)
	CreateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error)
	UpdateMonitor(ctx context.Context, m domain.Monitor) (domain.Monitor, error)
	GetMonitor(ctx context.Context, id string) (domain.Monitor, error)
	GetMonitorByPushToken(ctx context.Context, token string) (domain.Monitor, time.Time, error)
	DeleteMonitor(ctx context.Context, id string) error
	MonitorProvenance(ctx context.Context, monitorID string) (store.FileManagement, bool, error)
	MonitorProvenanceBatch(ctx context.Context, monitorIDs []string) (map[string]store.FileManagement, error)
	FileProviderDiagnostics(ctx context.Context, orgID string) ([]store.FileProviderDiagnostic, error)
	ReplaceMonitorDependencies(ctx context.Context, monitorID, projectID string, parents []string) error
	ListRecentHeartbeats(ctx context.Context, monitorID string, limit int) ([]domain.Heartbeat, error)
	PasswordHashByID(ctx context.Context, id string) (string, error)
	SetPassword(ctx context.Context, id, passwordHash string) error
	DeleteSessionsByUser(ctx context.Context, userID, exceptToken string) (int64, error)
	SetTOTPSecret(ctx context.Context, userID, secret string) error
	GetTOTP(ctx context.Context, userID string) (secret string, enabled bool, err error)
	EnableTOTP(ctx context.Context, userID string) error
	DisableTOTP(ctx context.Context, userID string) error
	ReplaceRecoveryCodes(ctx context.Context, userID string, hashes []string) error
	MonitorSLI(ctx context.Context, monitorID string, since time.Time) (store.SLICounts, error)
	ProjectSLI(ctx context.Context, projectID string, since time.Time) (store.SLICounts, error)
	MonitorDailyAvailability(ctx context.Context, monitorID string, since time.Time) ([]store.DailyAvailability, error)
	ProjectDailyAvailability(ctx context.Context, projectID string, since time.Time) ([]store.DailyAvailability, error)
	UpsertMonitorSLATarget(ctx context.Context, monitorID, window string, objective float64, burnAlert bool, rules []domain.BurnRule) (domain.SLATarget, error)
	GetMonitorSLATarget(ctx context.Context, monitorID, window string) (domain.SLATarget, error)
	SetProjectSLAReport(ctx context.Context, projectID string, enabled bool) (bool, error)
	ProjectSLAReportEnabled(ctx context.Context, projectID string) (bool, error)
	GetOIDCSettings(ctx context.Context) (domain.OIDCSettings, error)
	UpsertOIDCSettings(ctx context.Context, s domain.OIDCSettings) error
	CreateMaintenanceWindowChecked(ctx context.Context, mw domain.MaintenanceWindow, previewID string, rawRetention time.Duration) (domain.MaintenanceWindow, error)
	PreviewMutationOf(ctx context.Context, projectID, monitorID, targetID string, mutation store.MaintenanceMutation, from, to time.Time, rawRetention time.Duration, createdBy string) (store.MaintenancePreview, error)
	ArchiveMaintenanceWindow(ctx context.Context, projectID, id string) error
	AnnulMaintenanceWindow(ctx context.Context, projectID, id, previewID string, rawRetention time.Duration) error
	ListMaintenanceWindowsByProject(ctx context.Context, projectID string) ([]domain.MaintenanceWindow, error)
	GetMaintenanceWindow(ctx context.Context, id string) (domain.MaintenanceWindow, error)
	CreateIncident(ctx context.Context, inc domain.Incident, openingBody, author string) (domain.Incident, error)
	GetIncident(ctx context.Context, id string) (domain.Incident, error)
	AcknowledgeIncident(ctx context.Context, id, by string) (domain.Incident, error)
	CreateEscalationPolicy(ctx context.Context, p domain.EscalationPolicy) (domain.EscalationPolicy, error)
	GetEscalationPolicy(ctx context.Context, id string) (domain.EscalationPolicy, error)
	ListEscalationPolicies(ctx context.Context, projectID string) ([]domain.EscalationPolicy, error)
	UpdateEscalationPolicy(ctx context.Context, p domain.EscalationPolicy) (domain.EscalationPolicy, error)
	DeleteEscalationPolicy(ctx context.Context, id string) error
	ClaimPullJobs(ctx context.Context, region string, max, leaseSeconds int) ([]store.PullJob, error)
	ClaimPullJobsV2(ctx context.Context, region string, max, leaseSeconds int) ([]store.PullJob, error)
	ClaimPullJobsV3(ctx context.Context, region string, max, leaseSeconds int) ([]store.PullJob, error)
	AckPullJobs(ctx context.Context, tokens []string) error
	ClaimPullTest(ctx context.Context, region string) (id string, payload []byte, protocolVersion int, ok bool, err error)
	ClaimPullTestV2(ctx context.Context, region string) (id string, payload []byte, protocolVersion int, ok bool, err error)
	ClaimPullTestV3(ctx context.Context, region string) (id string, payload []byte, protocolVersion int, ok bool, err error)
	SavePullTestResult(ctx context.Context, id, region string, result []byte) error
	RecordAgentHeartbeat(ctx context.Context, region, agentID string) error
	RecordAgentCapabilities(ctx context.Context, region, agentID string, credentialEnvelope int, credentialReady bool) error
	RecordHistoricalResults(ctx context.Context, hbs []domain.Heartbeat) (inserted, skipped int, err error)
	MonitorRegions(ctx context.Context, ids []string) (map[string]string, error)
	CreateAgentToken(ctx context.Context, name, region, hash string) (domain.AgentToken, error)
	ResolveAgentTokenRegion(ctx context.Context, hash string) (region string, ok bool, err error)
	ListAgentTokens(ctx context.Context) ([]domain.AgentToken, error)
	RevokeAgentToken(ctx context.Context, id string) error
	CreateOnCallSchedule(ctx context.Context, sc domain.OnCallSchedule) (domain.OnCallSchedule, error)
	GetOnCallSchedule(ctx context.Context, id string) (domain.OnCallSchedule, error)
	ListOnCallSchedules(ctx context.Context, projectID string) ([]domain.OnCallSchedule, error)
	UpdateOnCallSchedule(ctx context.Context, sc domain.OnCallSchedule) (domain.OnCallSchedule, error)
	DeleteOnCallSchedule(ctx context.Context, id string) error
	AddOnCallOverride(ctx context.Context, o domain.OnCallOverride) (domain.OnCallOverride, error)
	ListOnCallOverrides(ctx context.Context, scheduleID string) ([]domain.OnCallOverride, error)
	GetOnCallOverride(ctx context.Context, id string) (domain.OnCallOverride, error)
	DeleteOnCallOverride(ctx context.Context, id string) error
	FindOpenIncidentByExternalKey(ctx context.Context, projectID, key string) (domain.Incident, error)
	ListIncidentsByProject(ctx context.Context, projectID string) ([]domain.Incident, error)
	AddIncidentUpdate(ctx context.Context, upd domain.IncidentUpdate) (domain.IncidentUpdate, error)
	ListIncidentUpdates(ctx context.Context, incidentID string) ([]domain.IncidentUpdate, error)
	UpsertPostmortem(ctx context.Context, incidentID, body, author string) (domain.Postmortem, error)
	GetPostmortem(ctx context.Context, incidentID string) (domain.Postmortem, error)
	CreateStatusPage(ctx context.Context, sp domain.StatusPage) (domain.StatusPage, error)
	UpdateStatusPage(ctx context.Context, sp domain.StatusPage) (domain.StatusPage, error)
	DeleteStatusPage(ctx context.Context, id string) error
	GetStatusPage(ctx context.Context, id string) (domain.StatusPage, error)
	GetStatusPageBySlug(ctx context.Context, slug string) (domain.StatusPage, error)
	ListStatusPagesByOrg(ctx context.Context, orgID string) ([]domain.StatusPage, error)
	CreateComponent(ctx context.Context, c domain.Component) (domain.Component, error)
	ListComponentsByPage(ctx context.Context, pageID string) ([]domain.Component, error)
	GetComponent(ctx context.Context, id string) (domain.Component, error)
	DeleteComponent(ctx context.Context, id string) error
	// FR-021 §15.0/§15.5 — the status-page service projection and the composite lifecycle.
	UpdateComponent(ctx context.Context, orgID string, c domain.Component) (domain.Component, error)
	// ONE page-scoped snapshot over (project, service) pairs, plus three set-wise monitor reads:
	// an org-level page spans projects, so a per-project batch was never one snapshot ([314] P1-3).
	ServicePageProjections(ctx context.Context, refs []store.ServiceRef, withHistory bool) (map[string]store.ServicePageProjection, error)
	MonitorPageProjections(ctx context.Context, monitorIDs []string, since time.Time, withHistory bool) (map[string]store.MonitorPageProjection, error)
	PreviewComponentConversion(ctx context.Context, orgID, componentID string, target store.ComponentConversionTarget) (store.ComponentConversionPlan, error)
	ConfirmComponentConversion(ctx context.Context, orgID, componentID string, target store.ComponentConversionTarget, revision, pageGeneration int64, actor store.GraphActor) (domain.Component, error)
	SetMonitorSuccessor(ctx context.Context, projectID, monitorID, serviceID string, actor store.GraphActor) (domain.Monitor, error)
	RetireMonitor(ctx context.Context, projectID, monitorID string, actor store.GraphActor) (domain.Monitor, error)
	ReactivateMonitor(ctx context.Context, projectID, monitorID string, actor store.GraphActor) (domain.Monitor, error)
	ConvertCompositeToService(ctx context.Context, projectID, monitorID string, sli []string, actor store.GraphActor) (store.CompositeConversion, error)
	ListMonitorsSupersededBy(ctx context.Context, projectID, serviceID string) ([]domain.Monitor, error)
	ListOpenIncidentsByProject(ctx context.Context, projectID string) ([]domain.Incident, error)
	// The page's incident/maintenance context, batched across ALL its projects: an org page spans
	// projects, and a per-project loop made the unauthenticated render O(projects) ([318] P1-1).
	IncidentsForPage(ctx context.Context, projectIDs []string, since time.Time) (store.PageIncidents, error)
	MaintenanceForPage(ctx context.Context, projectIDs []string, now time.Time) ([]domain.MaintenanceWindow, error)
	IncidentTimelines(ctx context.Context, incidentIDs []string) (map[string][]domain.IncidentUpdate, error)
	PostmortemsForIncidents(ctx context.Context, incidentIDs []string) (map[string]domain.Postmortem, error)
	CreateApiToken(ctx context.Context, t domain.ApiToken, hash string) (domain.ApiToken, error)
	ListApiTokensByOrg(ctx context.Context, orgID string) ([]domain.ApiToken, error)
	GetApiToken(ctx context.Context, id string) (domain.ApiToken, error)
	DeleteApiToken(ctx context.Context, id string) error
	CreateWebhook(ctx context.Context, h domain.Webhook) (domain.Webhook, error)
	GetWebhook(ctx context.Context, id string) (domain.Webhook, error)
	SetWebhookEnabled(ctx context.Context, id string, enabled bool) error
	ListWebhooksByOrg(ctx context.Context, orgID string) ([]domain.Webhook, error)
	DeleteWebhook(ctx context.Context, id string) error
	CreateNotificationChannel(ctx context.Context, ch domain.NotificationChannel) (domain.NotificationChannel, error)
	GetNotificationChannel(ctx context.Context, id string) (domain.NotificationChannel, error)
	SetNotificationChannelEnabled(ctx context.Context, id string, enabled bool) error
	ListChannelsByProject(ctx context.Context, projectID string) ([]domain.NotificationChannel, error)
	DeleteNotificationChannel(ctx context.Context, id string) error
	LinkMonitorChannel(ctx context.Context, monitorID, channelID string) error
	UnlinkMonitorChannel(ctx context.Context, monitorID, channelID string) error
	ListMonitorChannels(ctx context.Context, monitorID string) ([]domain.NotificationChannel, error)
	ListDeadOutbox(ctx context.Context, limit int) ([]domain.OutboxEventView, error)
	ReplayDeadOutbox(ctx context.Context, id string) error
	ReplayAllDeadOutbox(ctx context.Context) (int, error)
	Search(ctx context.Context, q string, limit int, scope store.SearchScope) ([]domain.SearchHit, error)
	CreateSubscriber(ctx context.Context, sub domain.Subscriber) (domain.Subscriber, error)
	ListSubscribersByPage(ctx context.Context, pageID string) ([]domain.Subscriber, error)
	DeleteSubscriber(ctx context.Context, pageID, id string) error
	ConfirmSubscriber(ctx context.Context, token string) (domain.Subscriber, error)
	DeleteSubscriberByToken(ctx context.Context, token string) error
	EnqueueOutbox(ctx context.Context, topic string, payload []byte) error
	RecordAudit(ctx context.Context, e domain.AuditEntry) error
	ListAuditByOrg(ctx context.Context, orgID string, limit int) ([]domain.AuditEntry, error)
	// ListGlobalAudit reads the INSTANCE-level entries (org_id IS NULL) — a global admin's own
	// history, which no org listing may widen into.
	ListGlobalAudit(ctx context.Context, limit int) ([]domain.AuditEntry, error)
	CreateProjectSecret(ctx context.Context, actor store.SecretActor, projectID, name, value string) (store.ProjectSecret, error)
	UpdateProjectSecret(ctx context.Context, actor store.SecretActor, projectID, name string, newName, newValue *string) (renamed, rotated bool, repointed int, err error)
	DeleteProjectSecret(ctx context.Context, actor store.SecretActor, projectID, name string) error
	// Service reliability (FR-021 phase 1).
	ListServiceSummaries(ctx context.Context, projectID string) ([]store.ServiceSummary, error)
	CreateService(ctx context.Context, svc domain.Service) (domain.Service, error)
	ServiceDetail(ctx context.Context, projectID, serviceID string) (store.ServiceDetail, error)
	PutServiceDeclaration(ctx context.Context, projectID, serviceID string, decl domain.ServiceDeclaration, expectedRevision int64, opts store.DeclarationOptions) (domain.DefinitionRevision, domain.EvaluationEpoch, error)
	DeleteService(ctx context.Context, projectID, serviceID string) error
	// Service reliability reporting (FR-021 phase 2, iter-0141).
	ServiceReliabilityReport(ctx context.Context, projectID, serviceID string, window sla.Window) (domain.ServiceWindowReport, error)
	ServiceHealthNow(ctx context.Context, projectID, serviceID string) (domain.ServiceHealthNow, error)
	ServiceReliabilitySeries(ctx context.Context, projectID, serviceID string, from, to time.Time, step time.Duration) ([]domain.ReliabilitySeriesPoint, error)
	UpsertServiceSLATarget(ctx context.Context, projectID, serviceID, window string, objective float64) error
	// Alerting ownership (FR-021 phase 5, §16.6a). Both are transactional, audit inside the
	// transaction and enqueue the §16.4a lifecycle closes their edit orphans. The READ half is
	// asserted separately (see serviceAlertPolicyReader) because the store does not offer it yet.
	// The paging declaration (FR-021 §16.6a): the read is what a PATCH merges onto, and a merge
	// whose base was invented is how "leave ownership alone" becomes a silent disowning.
	ServiceAlertPolicy(ctx context.Context, projectID, serviceID string) (domain.ServiceAlertPolicy, error)
	// The COVERAGE that declaration is currently producing, per signal (§16.1/§16.6b). Read from
	// the delegation lookup's own predicates, so a badge cannot disagree with the delivery gate.
	ServiceAlertingState(ctx context.Context, projectID, serviceID string) (store.ServiceAlertingState, error)
	// The same question from the MONITOR's side: which of its own alerts a service is delivering
	// instead, and who. Read through the delivery gate's own lookup, never re-derived.
	MonitorDelegation(ctx context.Context, monitorID, projectID string) (store.MonitorDelegation, error)
	UpdateServiceAlertPolicy(ctx context.Context, projectID, serviceID string, patch store.ServiceAlertPolicyPatch, actor store.AlertActor) (domain.ServiceAlertPolicy, error)
	SetServiceBurnAlerting(ctx context.Context, projectID, serviceID, window string, enabled bool, rules []domain.BurnRule, actor store.AlertActor) error
	// Service impact graph + correlation reads (FR-021 phase 3, iter-0147).
	GetServiceDependencies(ctx context.Context, projectID, serviceID string) (store.ServiceGraphView, error)
	ReplaceServiceDependencies(ctx context.Context, projectID, serviceID string, parents []string, expectedGeneration int64, actor store.GraphActor) (store.ServiceGraphView, error)
	CreateServiceWithDependencies(ctx context.Context, svc domain.Service, parents []string, actor store.GraphActor) (domain.Service, error)
	ServiceNeighbourHealth(ctx context.Context, projectID string, serviceIDs []string) (map[string]domain.ServiceHealthNow, error)
	ListIncidentImpacts(ctx context.Context, projectID, incidentID string) ([]domain.ServiceImpactLink, error)

	ListProjectSecrets(ctx context.Context, projectID string) ([]store.ProjectSecret, error)
}

// Mailer sends status-page subscription emails. Optional; nil means email is not
// configured, and subscription endpoints report as much.
type Mailer interface {
	Send(to, subject, body string) error
	BaseURL() string
}

// Metrics is the optional observability surface the API records into. It is
// nil-safe: handlers guard on it, so tests and embed-only modes can omit it.
type Metrics interface {
	RecordIncidentOpened()
	// RecordStatusPageUnreadableComponent counts a status-page component whose ACTIVE service could
	// not be read (§15.0, invariant 71a). It is a FAILED read, not absent measurement, and a log
	// alone leaves it invisible to anything watching trends.
	RecordStatusPageUnreadableComponent()
}

// ResultSink publishes a heartbeat into the ingestion pipeline. Used by the AGENT results
// endpoint so a remote-prober result flows through the scheduled ingest path (status
// update, notifications, incidents). Optional and nil-safe. (Push no longer uses this —
// see PushRecorder.)
type ResultSink interface {
	PublishResult(ctx context.Context, hb domain.Heartbeat) error
}

// PushRecorder applies a push (dead-man's-switch) heartbeat via its dedicated trusted
// entrypoint — NOT the shared ResultSink/dispatcher. receivedAt is the ingress DB clock
// from the token lookup; observedAt is the optional raw client timestamp (zero = absent).
// Satisfied by *ingest.PushRecorder. Optional and nil-safe.
type PushRecorder interface {
	Record(ctx context.Context, monitorID string, up bool, msg string, receivedAt, observedAt time.Time)
}

// Handler holds API dependencies.
type Handler struct {
	store             Store
	logger            *slog.Logger
	renderCache       *statusPageCache
	minPasswordLen    int
	metrics           Metrics
	results           ResultSink
	pushRecorder      PushRecorder
	mailer            Mailer
	eventSrc          EventSource
	oidc              OIDCController
	settings          *settings.Service
	liveRegions       LiveRegionSource
	tester            RegionTester
	agentToken        string                   // optional catch-all agent token (authorizes any region)
	agentRegionTokens map[string]string        // per-region agent tokens (region → token)
	agentDBTokens     bool                     // also resolve agent tokens from the database
	pullWaiter        PullWaiter               // long-poll wake source (LISTEN/NOTIFY); nil = no long-poll
	fpStatus          FileProviderStatusSource // process-local file-provider runtime status; nil = none
	secretsEnabled    bool                     // project secret inventory feature switch (cfg.Secrets.Enabled)
	// heartbeatRetentionDays bounds how far back a retroactive maintenance mutation can be
	// repaired. It is a settings question, so the store does not answer it: the store takes
	// the resolved floor as an argument and refuses anything older.
	heartbeatRetentionDays int
}

// PullWaiter blocks until a pull job is enqueued for a region (or the max hold / request
// context elapses), letting GET /agent/jobs long-poll. Implemented by *store.PullNotifier.
type PullWaiter interface {
	Wait(ctx context.Context, region string, max time.Duration)
}

// OIDCController lets the settings API rebuild the live OIDC provider after a
// config change (implemented by *auth.Authenticator).
type OIDCController interface {
	SyncOIDC(ctx context.Context) error
	OIDCActive() bool
}

// LiveRegionSource reports which worker-pool regions currently have a live worker
// (a consumer on checks.jobs.<region>). Implemented by *mqadmin.Client.
type LiveRegionSource interface {
	LiveJobRegions(ctx context.Context) (map[string]bool, error)
}

// RegionTester runs a one-off probe (the create-form "Test connection") in the
// monitor's region and returns the result. The inproc build runs it locally; the
// AMQP build (implemented by *dispatch.AMQP) dispatches it to a worker in that region
// so a geo target is tested from its own region, not from core. It errors when no
// worker in the region answers.
type RegionTester interface {
	RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error)
}

// New builds an API handler. minPasswordLen bounds local password changes
// until the settings service is wired — from then on the live auth policy
// (DB → config → default) is authoritative, so a UI change applies instantly.
func New(store Store, logger *slog.Logger, minPasswordLen int) *Handler {
	if minPasswordLen < 1 {
		minPasswordLen = 8
	}
	return &Handler{store: store, logger: logger, minPasswordLen: minPasswordLen,
		renderCache: newStatusPageCache()}
}

// effectiveMinPasswordLen resolves the live policy value, falling back to the
// construction-time config value when no settings service is attached.
func (h *Handler) effectiveMinPasswordLen() int {
	if h.settings != nil {
		if n := h.settings.AuthPolicy().MinPasswordLen; n > 0 {
			return n
		}
	}
	return h.minPasswordLen
}

// WithMetrics attaches an observability recorder and returns the handler for
// chaining. Optional: without it, metric hooks are no-ops.
func (h *Handler) WithMetrics(m Metrics) *Handler {
	h.metrics = m
	return h
}

// WithResultSink attaches the pipeline result publisher used by the agent results
// endpoint. Optional and nil-safe.
func (h *Handler) WithResultSink(s ResultSink) *Handler {
	h.results = s
	return h
}

// WithPushRecorder attaches the dedicated push-result recorder used by the push endpoint.
// Optional and nil-safe (the endpoint reports 501 when unset).
func (h *Handler) WithPushRecorder(p PushRecorder) *Handler {
	h.pushRecorder = p
	return h
}

// WithMailer attaches the subscription email sender. Optional and nil-safe: the
// subscribe endpoint reports 503 when no mailer is configured.
func (h *Handler) WithMailer(m Mailer) *Handler {
	h.mailer = m
	return h
}

// WithEvents attaches the realtime event source for the SSE stream. Optional and
// nil-safe: the events endpoint reports 501 when unset.
func (h *Handler) WithEvents(s EventSource) *Handler {
	h.eventSrc = s
	return h
}

// WithOIDC wires the live OIDC controller so saving settings can rebuild the
// provider. Without it, the OIDC settings endpoints report not-configurable.
func (h *Handler) WithOIDC(o OIDCController) *Handler {
	h.oidc = o
	return h
}

// WithSettings wires the instance-settings service (branding, auth policy,
// alerting, monitor defaults). Without it, those endpoints report not-configurable.
func (h *Handler) WithSettings(s *settings.Service) *Handler {
	h.settings = s
	return h
}

// WithLiveRegions wires the RabbitMQ management lookup so the region picker can flag
// which pools have a live worker. Without it, regions report live=false.
func (h *Handler) WithLiveRegions(s LiveRegionSource) *Handler {
	h.liveRegions = s
	return h
}

// WithTester wires the region-aware test runner so the create form can test a monitor
// before saving, from the monitor's own region. Without it, test returns 501.
func (h *Handler) WithTester(t RegionTester) *Handler {
	h.tester = t
	return h
}

// WithFileProviderStatus wires this process's file-provider runtime status source into the
// global-admin diagnostics (leadership/last-scan/configured providers). Nil = none.
func (h *Handler) WithFileProviderStatus(src FileProviderStatusSource) *Handler {
	h.fpStatus = src
	return h
}

// WithHeartbeatRetention sets how many days of raw heartbeats are kept, which is what makes
// a retroactive maintenance mutation repairable or not.
func (h *Handler) WithHeartbeatRetention(days int) *Handler {
	h.heartbeatRetentionDays = days
	return h
}

// rawRetention is how long raw heartbeats are kept. The store resolves the actual floor on
// the DATABASE clock inside the transaction that uses it — passing a process-computed
// timestamp from here let replica clock skew accept a purged range or reject a whole one.
func (h *Handler) rawRetention() time.Duration {
	days := h.heartbeatRetentionDays
	if days <= 0 {
		days = 90
	}
	return time.Duration(days) * 24 * time.Hour
}

// WithSecretsEnabled sets the project-secret-inventory feature switch
// (cfg.Secrets.Enabled). Off (the default), every secrets endpoint answers
// 404 feature_disabled (spec func-secret-inventory §4.1).
func (h *Handler) WithSecretsEnabled(enabled bool) *Handler {
	h.secretsEnabled = enabled
	return h
}

// WithAgentToken sets the optional catch-all agent bearer token (authorizes any
// region). Empty (the default) leaves it unset.
func (h *Handler) WithAgentToken(token string) *Handler {
	h.agentToken = token
	return h
}

// WithAgentRegionTokens sets per-region agent tokens: an agent presenting one may only
// claim/heartbeat that region. The agent endpoints are enabled if a catch-all token OR
// any per-region token is configured.
func (h *Handler) WithAgentRegionTokens(tokens map[string]string) *Handler {
	h.agentRegionTokens = tokens
	return h
}

// WithPullWaiter enables long-polling on GET /agent/jobs: when the queue is empty the
// request is held until a job is enqueued for the region (or the max hold elapses).
func (h *Handler) WithPullWaiter(w PullWaiter) *Handler {
	h.pullWaiter = w
	return h
}

// WithAgentDBTokens also resolves agent tokens from the database (issued/revoked at
// runtime), in addition to any config tokens. It enables the agent endpoints and the
// agent-token admin API.
func (h *Handler) WithAgentDBTokens() *Handler {
	h.agentDBTokens = true
	return h
}

// Router registers all API routes (patterns carry the full /api/v1 prefix so the
// handler can be mounted directly behind auth middleware).
func (h *Handler) Router() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", h.me)
	mux.HandleFunc("GET /api/v1/version", h.version)
	mux.HandleFunc("POST /api/v1/me/password", h.changePassword)
	mux.HandleFunc("POST /api/v1/me/totp/enroll", h.totpEnroll)
	mux.HandleFunc("POST /api/v1/me/totp/enable", h.totpEnable)
	mux.HandleFunc("POST /api/v1/me/totp/disable", h.totpDisable)
	mux.HandleFunc("GET /api/v1/organizations", h.listOrganizations)
	mux.HandleFunc("POST /api/v1/organizations", h.createOrganization)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}", h.getOrganization)
	mux.HandleFunc("DELETE /api/v1/organizations/{orgID}", h.deleteOrganization)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/projects", h.listProjects)
	mux.HandleFunc("POST /api/v1/organizations/{orgID}/projects", h.createProject)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/members", h.listMembers)
	mux.HandleFunc("POST /api/v1/organizations/{orgID}/members", h.addMember)
	mux.HandleFunc("PATCH /api/v1/organizations/{orgID}/members/{membershipID}", h.updateMember)
	mux.HandleFunc("DELETE /api/v1/organizations/{orgID}/members/{membershipID}", h.removeMember)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/audit", h.listAudit)
	mux.HandleFunc("GET /api/v1/projects/{projectID}", h.getProject)
	mux.HandleFunc("DELETE /api/v1/projects/{projectID}", h.deleteProject)
	mux.HandleFunc("GET /api/v1/regions", h.listRegions)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/monitors", h.listMonitors)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/monitors", h.createMonitor)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/monitors/test", h.testMonitor)
	mux.HandleFunc("GET /api/v1/monitors/{monitorID}", h.getMonitor)
	mux.HandleFunc("PATCH /api/v1/monitors/{monitorID}", h.updateMonitor)
	mux.HandleFunc("DELETE /api/v1/monitors/{monitorID}", h.deleteMonitor)
	mux.HandleFunc("GET /api/v1/monitors/{monitorID}/heartbeats", h.listHeartbeats)
	mux.HandleFunc("GET /api/v1/monitors/{monitorID}/sla", h.monitorSLA)
	mux.HandleFunc("PUT /api/v1/monitors/{monitorID}/sla-target", h.setMonitorSLATarget)
	mux.HandleFunc("GET /api/v1/monitors/{monitorID}/availability", h.monitorAvailability)
	// FR-021 §15.5 composite lifecycle: the annotation, the explicit retire/reactivate pair, and
	// the composite→service conversion. Each is its own act; none is a side effect of another.
	mux.HandleFunc("PUT /api/v1/monitors/{monitorID}/successor", h.setMonitorSuccessor)
	mux.HandleFunc("POST /api/v1/monitors/{monitorID}/retire", h.retireMonitor)
	mux.HandleFunc("POST /api/v1/monitors/{monitorID}/reactivate", h.reactivateMonitor)
	mux.HandleFunc("POST /api/v1/monitors/{monitorID}/convert-to-service", h.convertCompositeToService)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/availability", h.projectAvailability)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/sla", h.projectSLA)
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/sla-report", h.setProjectSLAReport)
	// Service reliability (FR-021). Phase 1: the resource, its declaration and the state of
	// materialization. Phase 2 (iter-0141): the reporting endpoints below — the detail still
	// embeds no number (an embedded zero would be worse than no field), the numbers live on
	// their own routes with spec-§11 honesty statuses.
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services", h.listServices)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/services", h.createService)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}", h.getService)
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/services/{serviceID}/declaration", h.putServiceDeclaration)
	mux.HandleFunc("DELETE /api/v1/projects/{projectID}/services/{serviceID}", h.deleteService)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}/dependencies", h.getServiceDependencies)
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/services/{serviceID}/dependencies", h.putServiceDependencies)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}/reliability", h.serviceReliability)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}/health", h.serviceHealth)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}/reliability/series", h.serviceReliabilitySeries)
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/services/{serviceID}/sla-target", h.setServiceSLATarget)
	// Phase 5 (§16.6a): the alerting-ownership write surface. The LIVE paging declaration and one
	// SLA target's burn declaration are separate routes because they are separate transactions
	// with separate audit actions and separate lifecycle closes — and because the objective write
	// above must stay exactly what it was.
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}/alerting", h.getServiceAlerting)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/services/{serviceID}/alerting/state", h.getServiceAlertingState)
	mux.HandleFunc("PATCH /api/v1/projects/{projectID}/services/{serviceID}/alerting", h.patchServiceAlerting)
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/services/{serviceID}/sla-target/burn-alerting", h.setServiceBurnAlerting)

	mux.HandleFunc("GET /api/v1/projects/{projectID}/secrets", h.listSecrets)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/secrets", h.createSecret)
	mux.HandleFunc("PATCH /api/v1/projects/{projectID}/secrets/{name}", h.updateSecret)
	mux.HandleFunc("DELETE /api/v1/projects/{projectID}/secrets/{name}", h.deleteSecret)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/maintenance", h.listMaintenance)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/maintenance", h.createMaintenance)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/maintenance/preview", h.previewMaintenance)
	mux.HandleFunc("DELETE /api/v1/maintenance/{maintenanceID}", h.deleteMaintenance)
	mux.HandleFunc("POST /api/v1/maintenance/{maintenanceID}/annul", h.annulMaintenance)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/incidents", h.listIncidents)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/incidents", h.createIncident)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/alerts/alertmanager", h.alertmanagerWebhook)
	mux.HandleFunc("GET /api/v1/incidents/{incidentID}", h.getIncident)
	mux.HandleFunc("GET /api/v1/incidents/{incidentID}/updates", h.listIncidentUpdates)
	mux.HandleFunc("POST /api/v1/incidents/{incidentID}/updates", h.addIncidentUpdate)
	mux.HandleFunc("GET /api/v1/incidents/{incidentID}/postmortem", h.getPostmortem)
	mux.HandleFunc("PUT /api/v1/incidents/{incidentID}/postmortem", h.putPostmortem)
	mux.HandleFunc("POST /api/v1/incidents/{incidentID}/acknowledge", h.acknowledgeIncident)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/escalation-policies", h.listEscalationPolicies)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/escalation-policies", h.createEscalationPolicy)
	mux.HandleFunc("PUT /api/v1/escalation-policies/{policyID}", h.updateEscalationPolicy)
	mux.HandleFunc("DELETE /api/v1/escalation-policies/{policyID}", h.deleteEscalationPolicy)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/oncall-schedules", h.listOnCallSchedules)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/oncall-schedules", h.createOnCallSchedule)
	mux.HandleFunc("PUT /api/v1/oncall-schedules/{scheduleID}", h.updateOnCallSchedule)
	mux.HandleFunc("DELETE /api/v1/oncall-schedules/{scheduleID}", h.deleteOnCallSchedule)
	mux.HandleFunc("GET /api/v1/agent-tokens", h.listAgentTokens)
	mux.HandleFunc("POST /api/v1/agent-tokens", h.createAgentToken)
	mux.HandleFunc("DELETE /api/v1/agent-tokens/{tokenID}", h.revokeAgentToken)
	mux.HandleFunc("GET /api/v1/oncall-schedules/{scheduleID}/current", h.currentOnCall)
	mux.HandleFunc("GET /api/v1/oncall-schedules/{scheduleID}/overrides", h.listOverrides)
	mux.HandleFunc("POST /api/v1/oncall-schedules/{scheduleID}/overrides", h.addOverride)
	mux.HandleFunc("DELETE /api/v1/oncall-overrides/{overrideID}", h.deleteOverride)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/status-pages", h.listStatusPages)
	mux.HandleFunc("POST /api/v1/organizations/{orgID}/status-pages", h.createStatusPage)
	mux.HandleFunc("GET /api/v1/status-pages/{pageID}", h.getStatusPage)
	mux.HandleFunc("PATCH /api/v1/status-pages/{pageID}", h.updateStatusPage)
	mux.HandleFunc("DELETE /api/v1/status-pages/{pageID}", h.deleteStatusPage)
	mux.HandleFunc("GET /api/v1/status-pages/{pageID}/render", h.renderStatusPageAuthed)
	mux.HandleFunc("GET /api/v1/status-pages/{pageID}/feed", h.renderFeedAuthed)
	mux.HandleFunc("GET /api/v1/status-pages/{pageID}/subscribers", h.listSubscribers)
	mux.HandleFunc("DELETE /api/v1/status-pages/{pageID}/subscribers/{subscriberID}", h.deleteSubscriber)
	mux.HandleFunc("GET /api/v1/status-pages/{pageID}/components", h.listComponents)
	mux.HandleFunc("POST /api/v1/status-pages/{pageID}/components", h.createComponent)
	mux.HandleFunc("DELETE /api/v1/components/{componentID}", h.deleteComponent)
	// FR-021 §15.0: a component's presentation is editable; its SOURCE changes only through the
	// previewed, CAS-fenced conversion below.
	mux.HandleFunc("PATCH /api/v1/components/{componentID}", h.updateComponent)
	mux.HandleFunc("POST /api/v1/components/{componentID}/conversion/preview", h.previewComponentConversion)
	mux.HandleFunc("POST /api/v1/components/{componentID}/conversion", h.confirmComponentConversion)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/tokens", h.listApiTokens)
	mux.HandleFunc("POST /api/v1/organizations/{orgID}/tokens", h.createApiToken)
	mux.HandleFunc("DELETE /api/v1/tokens/{tokenID}", h.deleteApiToken)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/webhooks", h.listWebhooks)
	mux.HandleFunc("POST /api/v1/organizations/{orgID}/webhooks", h.createWebhook)
	mux.HandleFunc("PATCH /api/v1/webhooks/{webhookID}", h.updateWebhook)
	mux.HandleFunc("DELETE /api/v1/webhooks/{webhookID}", h.deleteWebhook)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/notification-channels", h.listChannels)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/notification-channels", h.createChannel)
	mux.HandleFunc("PATCH /api/v1/notification-channels/{channelID}", h.updateChannel)
	mux.HandleFunc("DELETE /api/v1/notification-channels/{channelID}", h.deleteChannel)
	mux.HandleFunc("GET /api/v1/monitors/{monitorID}/notifications", h.listMonitorChannels)
	mux.HandleFunc("POST /api/v1/monitors/{monitorID}/notifications", h.linkMonitorChannel)
	mux.HandleFunc("DELETE /api/v1/monitors/{monitorID}/notifications/{channelID}", h.unlinkMonitorChannel)
	mux.HandleFunc("GET /api/v1/search", h.search)
	mux.HandleFunc("GET /api/v1/events", h.events)
	mux.HandleFunc("GET /api/v1/settings/oidc", h.getOIDCSettings)
	mux.HandleFunc("PUT /api/v1/settings/oidc", h.setOIDCSettings)
	mux.HandleFunc("GET /api/v1/settings/branding", h.getBranding)
	mux.HandleFunc("PUT /api/v1/settings/branding", h.putBranding)
	mux.HandleFunc("GET /api/v1/settings/auth-policy", h.getAuthPolicy)
	mux.HandleFunc("PUT /api/v1/settings/auth-policy", h.putAuthPolicy)
	mux.HandleFunc("GET /api/v1/settings/alerting", h.getAlerting)
	mux.HandleFunc("PUT /api/v1/settings/alerting", h.putAlerting)
	mux.HandleFunc("GET /api/v1/settings/monitor-defaults", h.getMonitorDefaults)
	// Effective defaults for any authenticated user — the monitor form prefills
	// from these (the admin-gated settings endpoint above is for editing them).
	mux.HandleFunc("GET /api/v1/monitor-defaults", h.effectiveMonitorDefaults)
	mux.HandleFunc("PUT /api/v1/settings/monitor-defaults", h.putMonitorDefaults)
	mux.HandleFunc("GET /api/v1/settings/mail", h.getMail)
	mux.HandleFunc("PUT /api/v1/settings/mail", h.putMail)
	mux.HandleFunc("GET /api/v1/admin/audit", h.listGlobalAudit)
	mux.HandleFunc("GET /api/v1/admin/users", h.listAllUsers)
	mux.HandleFunc("PATCH /api/v1/admin/users/{userID}", h.updateAdminUser)
	mux.HandleFunc("DELETE /api/v1/admin/users/{userID}", h.deleteAdminUser)
	mux.HandleFunc("GET /api/v1/admin/file-providers", h.listFileProvidersAdmin)
	mux.HandleFunc("GET /api/v1/organizations/{orgID}/file-providers", h.listOrgFileProviders)
	mux.HandleFunc("GET /api/v1/admin/outbox/dead", h.listDeadOutbox)
	mux.HandleFunc("POST /api/v1/admin/outbox/dead/replay-all", h.replayAllDeadOutbox)
	mux.HandleFunc("POST /api/v1/admin/outbox/dead/{eventID}/replay", h.replayDeadOutbox)
	return mux
}

// PublicRouter registers the unauthenticated status-page rendering endpoint. It
// is mounted outside the auth middleware; the handler enforces visibility itself.
func (h *Handler) PublicRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/public/status-pages/{slug}", h.renderStatusPagePublic)
	mux.HandleFunc("GET /api/v1/public/status-pages/{slug}/feed", h.renderFeedPublic)
	mux.HandleFunc("POST /api/v1/public/status-pages/{slug}/subscribers", h.subscribe)
	mux.HandleFunc("POST /api/v1/public/subscriptions/{token}/confirm", h.confirmSubscription)
	mux.HandleFunc("DELETE /api/v1/public/subscriptions/{token}", h.unsubscribe)
	mux.HandleFunc("POST /api/v1/public/push/{token}", h.pushHeartbeat)
	mux.HandleFunc("GET /api/v1/public/branding", h.publicBranding)
	return maxBytes(mux, publicMaxBody)
}

func (h *Handler) principal(r *http.Request) (authz.Principal, bool) {
	return auth.PrincipalFrom(r.Context())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
