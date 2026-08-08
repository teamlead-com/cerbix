package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// projectAccess loads a project and checks the principal may act on it. It
// writes 404 (hidden) or 403 and returns ok=false on failure.
func (h *Handler) projectAccess(w http.ResponseWriter, r *http.Request, projectID string, action authz.Action) (domain.Project, bool) {
	p, _ := h.principal(r)
	proj, err := h.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Project{}, false
	}
	if err != nil {
		h.serverError(w, "get_project", err)
		return domain.Project{}, false
	}
	if !p.VisibleProject(proj.OrgID, proj.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Project{}, false
	}
	if !p.Can(action, proj.OrgID, proj.ID) {
		writeError(w, http.StatusForbidden, "forbidden")
		return domain.Project{}, false
	}
	return proj, true
}

// monitorAccess loads a monitor and its project, then checks access. It returns
// the project too so callers can make role-aware decisions (e.g. whether to reveal
// the push token) without re-fetching it.
func (h *Handler) monitorAccess(w http.ResponseWriter, r *http.Request, action authz.Action) (domain.Monitor, domain.Project, bool) {
	mon, err := h.store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return domain.Monitor{}, domain.Project{}, false
	}
	if err != nil {
		h.serverError(w, "get_monitor", err)
		return domain.Monitor{}, domain.Project{}, false
	}
	proj, ok := h.projectAccess(w, r, mon.ProjectID, action)
	if !ok {
		return domain.Monitor{}, domain.Project{}, false
	}
	return mon, proj, true
}

// principalCan reports whether the request's principal may perform action on the
// given org/project — a non-erroring predicate (unlike projectAccess, which writes
// a 403). Used for role-aware response shaping.
func (h *Handler) principalCan(r *http.Request, action authz.Action, orgID, projectID string) bool {
	p, ok := h.principal(r)
	return ok && p.Can(action, orgID, projectID)
}

func (h *Handler) listMonitors(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectRead)
	if !ok {
		return
	}
	monitors, err := h.store.ListMonitorsByProject(r.Context(), proj.ID)
	if err != nil {
		h.serverError(w, "list_monitors", err)
		return
	}
	canWrite := h.principalCan(r, authz.ActionProjectWrite, proj.OrgID, proj.ID)
	for i := range monitors {
		monitors[i] = monitors[i].Redacted() // never return secret config to the client
		if !canWrite {
			monitors[i] = monitors[i].WithoutPushToken() // viewers must not get the push bearer token
		}
	}
	writeJSON(w, http.StatusOK, monitors)
}

type regionView struct {
	Name string `json:"name"`
	Live bool   `json:"live"` // has an active worker (consumer on checks.jobs.<name>)
}

// listRegions returns the worker-pool regions for the monitor form's region picker:
// the regions already in use (always incl. core) unioned with regions that have a
// live worker (from RabbitMQ), each flagged with its live status. Any authenticated
// caller may read it (regions are instance-wide infra labels, not tenant data).
func (h *Handler) listRegions(w http.ResponseWriter, r *http.Request) {
	inUse, err := h.store.ListRegions(r.Context())
	if err != nil {
		h.serverError(w, "list_regions", err)
		return
	}
	live := map[string]bool{}
	if h.liveRegions != nil {
		if l, lerr := h.liveRegions.LiveJobRegions(r.Context()); lerr == nil {
			live = l
		} else {
			h.logger.Warn("live_regions_unavailable", "error", lerr.Error())
		}
	}
	seen := map[string]bool{}
	out := []regionView{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, regionView{Name: name, Live: live[name]})
	}
	for _, n := range inUse {
		add(n) // already sorted, core first
	}
	// Live-only regions (a worker up before any monitor is assigned), appended sorted.
	liveOnly := make([]string, 0, len(live))
	for n := range live {
		if !seen[n] {
			liveOnly = append(liveOnly, n)
		}
	}
	sort.Strings(liveOnly)
	for _, n := range liveOnly {
		add(n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"regions": out})
}

// testMonitor runs a one-off probe against a monitor spec (before it is saved) and
// returns the result. The probe is dispatched to a worker in the spec's region, so a
// geo target is tested from its own region rather than from core. If no worker in that
// region answers, it returns 502. Push/composite monitors are not testable.
func (h *Handler) testMonitor(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	if h.tester == nil {
		writeError(w, http.StatusNotImplemented, "probing is not available in this deployment")
		return
	}
	var body struct {
		Type            string            `json:"type"`
		Target          string            `json:"target"`
		Method          string            `json:"method"`
		Region          string            `json:"region"`
		TimeoutSeconds  int               `json:"timeout_seconds"`
		IntervalSeconds int               `json:"interval_seconds"`
		Conditions      []string          `json:"conditions"`
		Config          map[string]string `json:"config"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	interval := body.IntervalSeconds
	if interval <= 0 {
		interval = 60 // irrelevant to a one-off probe, but domain validation wants it positive
	}
	timeout := body.TimeoutSeconds
	if timeout <= 0 {
		timeout = 10
	}
	m := domain.Monitor{
		ProjectID: proj.ID, Name: "probe-test", Type: domain.MonitorType(body.Type), Target: body.Target,
		Method: body.Method, Region: body.Region, IntervalSeconds: interval, TimeoutSeconds: timeout, Retries: 0,
		Conditions: body.Conditions, Config: body.Config,
	}
	m.Normalize()
	if m.Type == domain.MonitorPush || m.Type == domain.MonitorComposite {
		writeError(w, http.StatusBadRequest, "this monitor type cannot be tested")
		return
	}
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hb, err := h.tester.RunTest(r.Context(), m)
	if err != nil {
		// No worker in the target region (or the RPC failed): the probe result is
		// unknown, not "down" — surface it distinctly so the operator can bring up a
		// worker rather than mistaking it for a target outage.
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"up": hb.Up, "latency_ms": hb.LatencyMS, "code": hb.Code, "msg": hb.Msg,
	})
}

func (h *Handler) createMonitor(w http.ResponseWriter, r *http.Request) {
	proj, ok := h.projectAccess(w, r, r.PathValue("projectID"), authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		Name               string            `json:"name"`
		Type               string            `json:"type"`
		Target             string            `json:"target"`
		Method             string            `json:"method"`
		IntervalSeconds    int               `json:"interval_seconds"`
		TimeoutSeconds     int               `json:"timeout_seconds"`
		Retries            *int              `json:"retries"` // nil = instance default; 0 is a real value
		FailureThreshold   int               `json:"failure_threshold"`
		ConfirmInterval    *int              `json:"confirm_interval_seconds"` // nil = default 10; 0 = off
		RenotifySeconds    *int              `json:"renotify_seconds"`         // nil = instance default; 0 = never re-notify
		GraceSeconds       int               `json:"grace_seconds"`
		Conditions         []string          `json:"conditions"`
		Tags               []string          `json:"tags"`
		Region             string            `json:"region"`
		Config             map[string]string `json:"config"`
		Enabled            *bool             `json:"enabled"`
		AutoIncident       *bool             `json:"auto_incident"`
		EscalationPolicyID string            `json:"escalation_policy_id"`
		DependsOn          []string          `json:"depends_on"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// Apply instance-wide monitor defaults for any omitted field.
	def := domain.MonitorDefaults{IntervalSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 1, AutoIncident: true}
	if h.settings != nil {
		def = h.settings.MonitorDefaults()
	}
	if body.IntervalSeconds == 0 {
		body.IntervalSeconds = def.IntervalSeconds
	}
	if body.TimeoutSeconds == 0 {
		body.TimeoutSeconds = def.TimeoutSeconds
	}
	// Pointer-typed so an explicit 0 survives (0 retries / never re-notify are
	// real choices, unlike the zero-sentinel int fields above).
	retries := def.Retries
	if body.Retries != nil {
		retries = *body.Retries
	}
	if body.FailureThreshold == 0 {
		body.FailureThreshold = def.FailureThreshold
	}
	renotify := def.RenotifySeconds
	if body.RenotifySeconds != nil {
		renotify = *body.RenotifySeconds
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	autoIncident := def.AutoIncident // instance default; opt out per monitor
	if body.AutoIncident != nil {
		autoIncident = *body.AutoIncident
	}
	confirmInterval := 10 // accelerated confirmation-phase default; explicit 0 disables
	if body.ConfirmInterval != nil {
		confirmInterval = *body.ConfirmInterval
	}
	m := domain.Monitor{
		ProjectID:              proj.ID,
		Name:                   body.Name,
		Type:                   domain.MonitorType(body.Type),
		Target:                 body.Target,
		Method:                 body.Method,
		IntervalSeconds:        body.IntervalSeconds,
		TimeoutSeconds:         body.TimeoutSeconds,
		Retries:                retries,
		FailureThreshold:       body.FailureThreshold,
		ConfirmIntervalSeconds: confirmInterval,
		RenotifySeconds:        renotify,
		GraceSeconds:           body.GraceSeconds,
		Conditions:             body.Conditions,
		Tags:                   body.Tags,
		Region:                 body.Region,
		Config:                 body.Config,
		Enabled:                enabled,
		AutoIncident:           autoIncident,
		EscalationPolicyID:     body.EscalationPolicyID,
	}
	m.Normalize()
	// Push monitors are passive: give them a secret token for their heartbeat URL.
	if m.Type == domain.MonitorPush {
		m.PushToken = generatePushToken()
	}
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.compositeChildrenOK(w, r, m, proj.ID) {
		return
	}
	if !h.escalationPolicyInProject(w, r, m.EscalationPolicyID, proj.ID) {
		return
	}
	created, err := h.store.CreateMonitor(r.Context(), m)
	if err != nil {
		h.serverError(w, "create_monitor", err)
		return
	}
	if len(body.DependsOn) > 0 {
		if !h.applyDependencies(w, r, &created, proj.ID, body.DependsOn) {
			// Keep the create atomic from the client's view: roll the monitor back.
			_ = h.store.DeleteMonitor(r.Context(), created.ID)
			return
		}
	}
	h.logEvent(r, "monitor_created", "monitor_id", created.ID, "name", created.Name, "type", string(created.Type), "project_id", created.ProjectID)
	writeJSON(w, http.StatusCreated, created.Redacted())
}

// applyDependencies replaces a monitor's parent set, translating the store's
// typed graph errors into 400s. On success it refreshes m.DependsOn.
func (h *Handler) applyDependencies(w http.ResponseWriter, r *http.Request, m *domain.Monitor, projectID string, parents []string) bool {
	err := h.store.ReplaceMonitorDependencies(r.Context(), m.ID, projectID, parents)
	switch {
	case errors.Is(err, store.ErrDependencyCycle):
		writeError(w, http.StatusBadRequest, "dependencies would create a cycle")
		return false
	case errors.Is(err, store.ErrDependencyForeign):
		writeError(w, http.StatusBadRequest, "dependencies must be other monitors of the same project")
		return false
	case err != nil:
		h.serverError(w, "replace_dependencies", err)
		return false
	}
	m.DependsOn = dedupeIDs(parents)
	return true
}

// dedupeIDs mirrors the store's normalization for the response body.
func dedupeIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// compositeChildrenOK verifies a composite monitor's children all exist within
// the same project (tenant isolation). A no-op for non-composite monitors.
func (h *Handler) compositeChildrenOK(w http.ResponseWriter, r *http.Request, m domain.Monitor, projectID string) bool {
	if m.Type != domain.MonitorComposite {
		return true
	}
	for _, id := range m.ChildIDs() {
		child, err := h.store.GetMonitor(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && child.ProjectID != projectID) {
			writeError(w, http.StatusBadRequest, "composite child is not a monitor in this project")
			return false
		}
		if err != nil {
			h.serverError(w, "get_child_monitor", err)
			return false
		}
	}
	return true
}

func (h *Handler) getMonitor(w http.ResponseWriter, r *http.Request) {
	mon, proj, ok := h.monitorAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	out := mon.Redacted()
	// The push token is a bearer capability — reveal it only to a caller who can
	// write the monitor; a read-only viewer must not be able to forge heartbeats.
	if !h.principalCan(r, authz.ActionProjectWrite, proj.OrgID, proj.ID) {
		out = out.WithoutPushToken()
	}
	writeJSON(w, http.StatusOK, out)
}

// updateMonitor applies a partial update to a monitor (editor+). Type and
// push_token are immutable; only the provided fields change.
func (h *Handler) updateMonitor(w http.ResponseWriter, r *http.Request) {
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	var body struct {
		Name               *string            `json:"name"`
		Target             *string            `json:"target"`
		Method             *string            `json:"method"`
		IntervalSeconds    *int               `json:"interval_seconds"`
		TimeoutSeconds     *int               `json:"timeout_seconds"`
		Retries            *int               `json:"retries"`
		FailureThreshold   *int               `json:"failure_threshold"`
		ConfirmInterval    *int               `json:"confirm_interval_seconds"`
		RenotifySeconds    *int               `json:"renotify_seconds"`
		GraceSeconds       *int               `json:"grace_seconds"`
		Conditions         *[]string          `json:"conditions"`
		Tags               *[]string          `json:"tags"`
		Region             *string            `json:"region"`
		Config             *map[string]string `json:"config"`
		Enabled            *bool              `json:"enabled"`
		AutoIncident       *bool              `json:"auto_incident"`
		EscalationPolicyID *string            `json:"escalation_policy_id"`
		DependsOn          *[]string          `json:"depends_on"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Config != nil {
		// Preserve unchanged secrets: an empty submitted secret keeps the stored one
		// (the client never receives it, so it can't resend it).
		old := mon.Config
		next := *body.Config
		for k := range domain.SecretMonitorConfigKeys {
			if next[k] == "" && old[k] != "" {
				next[k] = old[k]
			}
		}
		mon.Config = next
	}
	if body.Name != nil {
		mon.Name = *body.Name
	}
	if body.Target != nil {
		mon.Target = *body.Target
	}
	if body.Method != nil {
		mon.Method = *body.Method
	}
	if body.IntervalSeconds != nil {
		mon.IntervalSeconds = *body.IntervalSeconds
	}
	if body.TimeoutSeconds != nil {
		mon.TimeoutSeconds = *body.TimeoutSeconds
	}
	if body.Retries != nil {
		mon.Retries = *body.Retries
	}
	if body.FailureThreshold != nil {
		mon.FailureThreshold = *body.FailureThreshold
	}
	if body.ConfirmInterval != nil {
		mon.ConfirmIntervalSeconds = *body.ConfirmInterval
	}
	if body.RenotifySeconds != nil {
		mon.RenotifySeconds = *body.RenotifySeconds
	}
	if body.GraceSeconds != nil {
		mon.GraceSeconds = *body.GraceSeconds
	}
	if body.Conditions != nil {
		mon.Conditions = *body.Conditions
	}
	if body.Tags != nil {
		mon.Tags = *body.Tags
	}
	if body.Region != nil {
		mon.Region = *body.Region
	}
	if body.Enabled != nil {
		mon.Enabled = *body.Enabled
	}
	if body.AutoIncident != nil {
		mon.AutoIncident = *body.AutoIncident
	}
	if body.EscalationPolicyID != nil {
		mon.EscalationPolicyID = *body.EscalationPolicyID
	}
	mon.Normalize()
	if err := mon.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.compositeChildrenOK(w, r, mon, mon.ProjectID) {
		return
	}
	if !h.escalationPolicyInProject(w, r, mon.EscalationPolicyID, mon.ProjectID) {
		return
	}
	// Validate + replace the dependency edges before touching the row, so a
	// cycle/foreign parent rejects the whole update with the fields unchanged.
	if body.DependsOn != nil {
		if !h.applyDependencies(w, r, &mon, mon.ProjectID, *body.DependsOn) {
			return
		}
	}
	updated, err := h.store.UpdateMonitor(r.Context(), mon)
	if err != nil {
		if h.rejectIfManaged(w, r, mon.ID, err) {
			return
		}
		h.serverError(w, "update_monitor", err)
		return
	}
	h.logEvent(r, "monitor_updated", "monitor_id", updated.ID, "name", updated.Name, "enabled", updated.Enabled, "project_id", updated.ProjectID)
	writeJSON(w, http.StatusOK, updated.Redacted())
}

// rejectIfManaged maps store.ErrManagedByFile to 409 with tenant-safe provenance (spec §8):
// a file-managed monitor's declarative fields are read-only through normal CRUD. Returns
// true when it handled the error.
func (h *Handler) rejectIfManaged(w http.ResponseWriter, r *http.Request, monitorID string, err error) bool {
	if !errors.Is(err, store.ErrManagedByFile) {
		return false
	}
	body := map[string]any{"error": "managed_by_file"}
	if fm, ok, perr := h.store.MonitorProvenance(r.Context(), monitorID); perr == nil && ok {
		body["management"] = map[string]any{
			"source": "file", "provider": fm.Provider, "uid": fm.UID,
			"path": fm.SourcePath, "read_only": true,
		}
	}
	writeJSON(w, http.StatusConflict, body)
	return true
}

func (h *Handler) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectWrite)
	if !ok {
		return
	}
	if err := h.store.DeleteMonitor(r.Context(), mon.ID); err != nil {
		if h.rejectIfManaged(w, r, mon.ID, err) {
			return
		}
		h.serverError(w, "delete_monitor", err)
		return
	}
	h.logEvent(r, "monitor_deleted", "monitor_id", mon.ID, "name", mon.Name, "project_id", mon.ProjectID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listHeartbeats(w http.ResponseWriter, r *http.Request) {
	mon, _, ok := h.monitorAccess(w, r, authz.ActionProjectRead)
	if !ok {
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			limit = n
		}
	}
	hbs, err := h.store.ListRecentHeartbeats(r.Context(), mon.ID, limit)
	if err != nil {
		h.serverError(w, "list_heartbeats", err)
		return
	}
	writeJSON(w, http.StatusOK, hbs)
}

// effectiveMonitorDefaults returns the resolved instance defaults (DB → config
// seed → built-ins) for any authenticated user: the monitor form prefills from
// them so Settings → Monitor defaults actually shapes UI-created monitors.
func (h *Handler) effectiveMonitorDefaults(w http.ResponseWriter, r *http.Request) {
	def := domain.MonitorDefaults{IntervalSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 1, AutoIncident: true}
	if h.settings != nil {
		def = h.settings.MonitorDefaults()
	}
	writeJSON(w, http.StatusOK, def)
}
