package domain

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// MonitorType is the kind of check a monitor performs.
type MonitorType string

const (
	// MonitorHTTP checks an HTTP(S) endpoint (status/latency/body conditions).
	MonitorHTTP MonitorType = "http"
	// MonitorTCP checks that a TCP port accepts connections.
	MonitorTCP MonitorType = "tcp"
	// MonitorICMP checks host reachability with an ICMP echo (ping).
	MonitorICMP MonitorType = "icmp"
	// MonitorDNS checks that a hostname resolves (and how fast).
	MonitorDNS MonitorType = "dns"
	// MonitorTLS checks a TLS endpoint's certificate (handshake + days to expiry).
	MonitorTLS MonitorType = "tls"
	// MonitorGRPC checks a gRPC server's health (grpc.health.v1).
	MonitorGRPC MonitorType = "grpc"
	// MonitorComposite derives its status from child monitors (all/any).
	MonitorComposite MonitorType = "composite"
	// MonitorPostgres checks a PostgreSQL server (connect + a query, default SELECT 1).
	MonitorPostgres MonitorType = "postgres"
	// MonitorMySQL checks a MySQL/MariaDB server (connect + a query, default SELECT 1).
	MonitorMySQL MonitorType = "mysql"
	// MonitorRedis checks a Redis server (AUTH + PING).
	MonitorRedis MonitorType = "redis"
	// MonitorPromQL evaluates a PromQL query against a Prometheus server.
	MonitorPromQL MonitorType = "promql"
	// MonitorRabbitMQ checks a RabbitMQ broker (AMQP handshake, or the management HTTP API).
	MonitorRabbitMQ MonitorType = "rabbitmq"
	// MonitorWebSocket checks a WebSocket endpoint (the HTTP Upgrade handshake).
	MonitorWebSocket MonitorType = "websocket"
	// MonitorSSH checks an SSH server (connect + identification banner).
	MonitorSSH MonitorType = "ssh"
	// MonitorSynthetic runs a scripted multi-step HTTP scenario (login→act→assert).
	MonitorSynthetic MonitorType = "synthetic"
	// MonitorAsyncCanary runs ONE typed asynchronous transaction end to end — submit, correlate,
	// await a terminal result, assert — declared in a closed schema rather than a document
	// (FR-029, D-0218). It is deliberately not `synthetic` widened: an untyped document forces
	// every rule about it to guess, which is what D7 had to do and what cost FR-028 a round.
	MonitorAsyncCanary MonitorType = "async_canary"
	// MonitorPush is a passive dead-man's-switch: the target reports in.
	MonitorPush MonitorType = "push"
)

// Active reports whether the type is scheduled for periodic evaluation (network
// probers and composites), as opposed to the passive push type.
func (t MonitorType) Active() bool {
	return t == MonitorHTTP || t == MonitorTCP || t == MonitorICMP || t == MonitorDNS ||
		t == MonitorTLS || t == MonitorGRPC || t == MonitorComposite || t == MonitorPostgres || t == MonitorMySQL || t == MonitorRedis || t == MonitorPromQL ||
		t == MonitorRabbitMQ || t == MonitorWebSocket || t == MonitorSSH || t == MonitorSynthetic ||
		t == MonitorAsyncCanary
}

// NeedsTarget reports whether the type probes a single network target (composites
// derive from children; synthetic carries its own per-step URLs in the scenario).
func (t MonitorType) NeedsTarget() bool {
	// A canary's target is its workflow, exactly as a synthetic monitor's is its scenario: there is
	// no single address to probe, and `canSubmit` in the SPA demanding one is the defect FR-028's
	// editor arc found for `synthetic`.
	return t.Active() && t != MonitorComposite && t != MonitorSynthetic && t != MonitorAsyncCanary
}

// Valid reports whether t is a known monitor type.
func (t MonitorType) Valid() bool {
	switch t {
	case MonitorHTTP, MonitorTCP, MonitorICMP, MonitorDNS, MonitorTLS, MonitorGRPC, MonitorComposite, MonitorPostgres, MonitorMySQL, MonitorRedis, MonitorPromQL, MonitorRabbitMQ, MonitorWebSocket, MonitorSSH, MonitorSynthetic, MonitorAsyncCanary, MonitorPush:
		return true
	default:
		return false
	}
}

// MonitorStatus is the last observed state of a monitor.
type MonitorStatus string

const (
	StatusPending MonitorStatus = "pending"
	StatusUp      MonitorStatus = "up"
	StatusDown    MonitorStatus = "down"
)

// Monitor is a single check belonging to a project (the "service" being watched).
type Monitor struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	// Description is what the monitor is FOR, for the reader who did not create it (FR-030, D-0234).
	// Optional, plain text, at most MaxMonitorDescriptionRunes code points; empty means none, and every
	// surface renders exactly as before when it is empty.
	Description string `json:"description"`
	// Slug is project-unique and IMMUTABLE: the MaC reference key a service names a monitor
	// by, and the one identifier that is stable enough to put in a file. Renaming the
	// display name never changes it — making it renameable would turn it into a guarded
	// declaration mutation across every referencing service.
	Slug             string      `json:"slug"`
	Type             MonitorType `json:"type"`
	Target           string      `json:"target"`
	Method           string      `json:"method,omitempty"` // HTTP method (http monitors); empty = GET
	IntervalSeconds  int         `json:"interval_seconds"`
	TimeoutSeconds   int         `json:"timeout_seconds"`
	Retries          int         `json:"retries"`
	FailureThreshold int         `json:"failure_threshold"` // consecutive failed checks before "down" (confirmations); default 1
	// ConfirmIntervalSeconds accelerates ONLY the confirmation phase: after the
	// first failure (and until the down verdict or a recovery) probes run at this
	// interval instead of the main one, cutting time-to-alert from
	// interval×threshold to seconds. 0 = off; clamped to [5, interval]; effective
	// only with FailureThreshold > 1 on active non-push/composite types.
	ConfirmIntervalSeconds int `json:"confirm_interval_seconds"`
	// ConsecutiveFailures is the live confirmation counter (server-owned, set by
	// RecordCheckStatus); surfaced read-only so the UI can show "confirming N/M".
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// ExecutionRevision is the monitor's config generation (server-owned). The scheduler
	// snapshots it into each job; the prober echoes it into the result so RecordScheduledResult
	// can reject a result produced under a stale configuration. Read-only to clients.
	ExecutionRevision int64 `json:"execution_revision,omitempty"`
	// LastProbeError is executor/dispatch diagnostics only. It never represents target
	// liveness and is never exposed by public status pages.
	LastProbeErrorReason string     `json:"last_probe_error_reason,omitempty"`
	LastProbeErrorAt     *time.Time `json:"last_probe_error_at,omitempty"`
	// JobID is retained only for internal correlation and structured logs. The monitor API
	// exposes the bounded reason/time pair from the FR-020 contract, not queue identifiers.
	LastProbeErrorJobID string `json:"-"`
	// StateSequence is a per-monitor monotonic counter bumped on every applied
	// status transition. It rides along in the transition outbox event and is
	// checked at delivery so a stale DOWN (or reminder) can't fire after a newer
	// transition has superseded it. Server-owned, read-only to clients.
	StateSequence      int64             `json:"-"`
	RenotifySeconds    int               `json:"renotify_seconds"` // re-send the down alert every N seconds while down (0 = off)
	GraceSeconds       int               `json:"grace_seconds"`    // push: extra tolerance before "down"
	Conditions         []string          `json:"conditions"`
	Tags               []string          `json:"tags"`             // free-form labels (env:prod, team:x) for filtering
	Region             string            `json:"region"`           // worker-pool region that probes this monitor; default "core"
	Config             map[string]string `json:"config,omitempty"` // per-type config (e.g. composite children/mode)
	Enabled            bool              `json:"enabled"`
	AutoIncident       bool              `json:"auto_incident"`                  // open an incident automatically when this monitor goes down
	EscalationPolicyID string            `json:"escalation_policy_id,omitempty"` // on-call escalation ladder for down alerts (empty = flat notify)
	// DependsOn lists parent monitors (same project) this monitor depends on.
	// While any (transitive) parent is down, this monitor's alerts are suppressed
	// at delivery time; data keeps recording. The graph must stay acyclic.
	DependsOn []string      `json:"depends_on"`
	Status    MonitorStatus `json:"status"`
	PushToken string        `json:"push_token,omitempty"` // set for push monitors
	// SupersededByServiceID names the service that now expresses what this monitor was built to
	// express (FR-021 §15.5). It is an ANNOTATION, not a redirect: the monitor keeps probing,
	// keeps alerting and keeps its own history. ONE stored fact, rendered from both ends, so
	// there is no pair of links to fall out of sync.
	SupersededByServiceID string `json:"superseded_by_service_id,omitempty"`
	// RetiredAt is the LIFECYCLE statement — an explicit operator act, never a consequence of
	// building a service. `Enabled` remains the execution switch; retiring sets both, because
	// retired_at alone would leave a "retired" monitor probing and paging.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Retired reports whether an operator has explicitly ended this monitor's life.
func (m Monitor) Retired() bool { return m.RetiredAt != nil }

// Interval returns the configured check interval.
func (m Monitor) Interval() time.Duration { return time.Duration(m.IntervalSeconds) * time.Second }

// Timeout returns the per-check timeout.
func (m Monitor) Timeout() time.Duration { return time.Duration(m.TimeoutSeconds) * time.Second }

// ConfirmInterval returns the accelerated probe interval for the confirmation phase.
func (m Monitor) ConfirmInterval() time.Duration {
	return time.Duration(m.ConfirmIntervalSeconds) * time.Second
}

// ConfirmConfigured reports whether this monitor's configuration enables the
// accelerated confirmation phase at all (regardless of current state).
func (m Monitor) ConfirmConfigured() bool {
	return m.ConfirmIntervalSeconds > 0 && m.FailureThreshold > 1 &&
		m.Type != MonitorPush && m.Type != MonitorComposite && m.Type.Active()
}

// InConfirmPhase reports whether the monitor is currently mid-confirmation:
// still up, with at least one failure counted but no verdict yet. The scheduler
// probes such monitors at ConfirmInterval instead of Interval.
func (m Monitor) InConfirmPhase() bool {
	return m.ConfirmConfigured() && m.Status == StatusUp &&
		m.ConsecutiveFailures > 0 && m.ConsecutiveFailures < m.FailureThreshold
}

// MaxMonitorDescriptionRunes bounds Monitor.Description, counted as Unicode code points — the same
// count the form makes with `[...s].length` — so a Cyrillic sentence is as long as a Latin one to the
// person writing it. Set by the owner (200) with the approved mock, D-0234.
const MaxMonitorDescriptionRunes = 200

// Validate enforces monitor invariants (domain-owned).
func (m Monitor) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("monitor: name is required")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(m.Description)); n > MaxMonitorDescriptionRunes {
		return fmt.Errorf("monitor: description is %d characters, the limit is %d", n, MaxMonitorDescriptionRunes)
	}
	if m.ProjectID == "" {
		return fmt.Errorf("monitor: project_id is required")
	}
	if !m.Type.Valid() {
		return fmt.Errorf("monitor: unknown type %q", m.Type)
	}
	if m.Type.NeedsTarget() {
		if strings.TrimSpace(m.Target) == "" {
			return fmt.Errorf("monitor: target is required for %s monitors", m.Type)
		}
		if m.IntervalSeconds <= 0 {
			return fmt.Errorf("monitor: interval_seconds must be positive")
		}
		if m.TimeoutSeconds <= 0 {
			return fmt.Errorf("monitor: timeout_seconds must be positive")
		}
		if m.Retries < 0 {
			return fmt.Errorf("monitor: retries must not be negative")
		}
	}
	if m.Type == MonitorPush && m.IntervalSeconds <= 0 {
		// The interval is the expected heartbeat period (liveness window).
		return fmt.Errorf("monitor: interval_seconds must be positive for push monitors")
	}
	if m.Type == MonitorComposite {
		if m.IntervalSeconds <= 0 {
			return fmt.Errorf("monitor: interval_seconds must be positive for composite monitors")
		}
		if len(m.ChildIDs()) == 0 {
			return fmt.Errorf("monitor: a composite needs at least one child monitor")
		}
		if mode := m.Config["mode"]; mode != "" && mode != "all" && mode != "any" && mode != "quorum" {
			return fmt.Errorf("monitor: composite mode must be 'all', 'any' or 'quorum'")
		}
		if m.Config["mode"] == "quorum" {
			q := m.CompositeQuorum()
			if q < 1 || q > len(m.ChildIDs()) {
				return fmt.Errorf("monitor: composite quorum must be between 1 and the number of children (%d)", len(m.ChildIDs()))
			}
		}
	}
	if m.Type == MonitorSynthetic {
		if m.IntervalSeconds <= 0 {
			return fmt.Errorf("monitor: interval_seconds must be positive for synthetic monitors")
		}
		if m.TimeoutSeconds <= 0 {
			return fmt.Errorf("monitor: timeout_seconds must be positive for synthetic monitors")
		}
		sc, err := ParseScenario(m.Config)
		if err != nil {
			return err
		}
		if len(sc.Steps) > maxSyntheticSteps {
			return fmt.Errorf("monitor: a synthetic scenario may have at most %d steps", maxSyntheticSteps)
		}
		// FR-028 stage 2: a credential in a scenario is a named binding resolved from the
		// project inventory. Validated HERE so every write surface — UI, API, file provider
		// and test-before-save — passes through one rule rather than four.
		if _, err := ScenarioBindings(sc, m.Config); err != nil {
			return err
		}
	}
	// A scenario binding belongs to a SYNTHETIC monitor and to no other type. The key is
	// not a harmless unknown one: the store turns it into a `monitor_secret_refs` row, the
	// materializer builds an envelope field for it, and the dispatch gate raises the carrier
	// generation it demands. On any other type that produces a monitor this surface ACCEPTED
	// and dispatch then refuses — a failure moved away from the person who caused it. Found
	// in review of the shipped stage 2, where every helper acted on the key prefix alone.
	if m.Type != MonitorSynthetic {
		if refs := ScenarioSecretRefKeys(m.Config); len(refs) > 0 {
			return fmt.Errorf("monitor: %s is not a synthetic monitor and must not declare %s", m.Type, refs[0])
		}
	}
	if m.Type == MonitorAsyncCanary {
		if m.IntervalSeconds <= 0 || m.TimeoutSeconds <= 0 {
			return fmt.Errorf("monitor: interval_seconds and timeout_seconds must be positive for async_canary monitors")
		}
		// TYPE-SCOPED on purpose (FR-029 D9, and the D6b lesson from FR-028): an ordinary http
		// monitor with a 30 s interval and a 60 s timeout is legal today and common, so writing this
		// as a general rule would refuse configurations nobody meant it to.
		if m.IntervalSeconds < m.TimeoutSeconds {
			return fmt.Errorf("monitor: interval_seconds must be at least timeout_seconds for async_canary monitors (one probe may not overlap the next)")
		}
		w, err := ParseCanaryConfig(m.Config)
		if err != nil {
			return err
		}
		if err := ValidateCanaryWorkflow(w, m.TimeoutSeconds); err != nil {
			return err
		}
	}
	// A `canary_secret_*` reference belongs to an async_canary monitor and to nothing else — the
	// same rule, and the same reason, as FR-028's D6b: the key writes a `monitor_secret_refs` row,
	// adds an expected envelope field and joins the execution digest, so on any other type it
	// produces a monitor this surface ACCEPTED and dispatch then refuses.
	if m.Type != MonitorAsyncCanary {
		if refs := CanarySecretRefKeys(m.Config); len(refs) > 0 {
			return fmt.Errorf("monitor: %s is not an async_canary monitor and must not declare %s", m.Type, refs[0])
		}
	}
	// A URL-style target may not carry credentials in its userinfo, on ANY surface
	// (D-0145 addendum, 2026-09-01). Go's net/http turns https://user:pass@host into an
	// `Authorization: Basic` header by itself, so such a target WORKS — and the password is
	// then stored as plaintext in `monitors.target`, which Redacted() does not touch, so
	// every viewer of the project reads it in the monitor list. The file provider has always
	// refused this (D-0152); this is the same rule at the domain boundary, which is the only
	// place every write surface passes through.
	//
	// Narrower than the file provider on purpose: the parser also refuses secret-bearing
	// QUERY keys and URL-shaped targets that fail to parse, because a bundle must be PROVEN
	// free of secrets before it is applied. Here the rule is the one that can be decided
	// exactly — url.Parse populates User only for a real URL authority, so bare host:port,
	// ICMP hostnames and other non-URL targets never trip it. The message never echoes the
	// target.
	if u, err := url.Parse(strings.TrimSpace(m.Target)); err == nil && u.User != nil {
		return fmt.Errorf("monitor: target must not carry credentials in its URL userinfo; use a target without user:password")
	}
	if m.Type == MonitorHTTP && m.Method != "" && !httpMethods[m.Method] {
		return fmt.Errorf("monitor: unsupported HTTP method %q", m.Method)
	}
	if m.GraceSeconds < 0 {
		return fmt.Errorf("monitor: grace_seconds must not be negative")
	}
	region := m.Region
	if region == "" {
		region = DefaultRegion
	}
	if !regionSlug.MatchString(region) {
		return fmt.Errorf("monitor: region must match %s", regionSlug.String())
	}
	// Composite monitors read child statuses from the database, so they must run on
	// the central pool (a DB-less remote worker cannot evaluate them).
	if m.Type == MonitorComposite && region != DefaultRegion {
		return fmt.Errorf("monitor: composite monitors must run in region %q", DefaultRegion)
	}
	// Upper bounds: a single editor must not be able to tie up a fixed worker pool with a
	// pathological config (a multi-hour timeout, an enormous retry count, a huge interval).
	if m.IntervalSeconds > maxIntervalSeconds {
		return fmt.Errorf("monitor: interval_seconds must be at most %d", maxIntervalSeconds)
	}
	if m.TimeoutSeconds > maxTimeoutSeconds {
		return fmt.Errorf("monitor: timeout_seconds must be at most %d", maxTimeoutSeconds)
	}
	if m.Retries > maxRetries {
		return fmt.Errorf("monitor: retries must be at most %d", maxRetries)
	}
	if m.GraceSeconds > maxGraceSeconds {
		return fmt.Errorf("monitor: grace_seconds must be at most %d", maxGraceSeconds)
	}
	return nil
}

// Resource bounds on a monitor's schedule/probe config. These cap the load one monitor can
// place on the shared worker pool; they are generous (a day interval, a 5-minute timeout)
// so no realistic config is rejected, while a pathological one is.
const (
	maxIntervalSeconds = 86400 // 1 day
	maxTimeoutSeconds  = 300   // 5 minutes
	maxRetries         = 10
	maxGraceSeconds    = 86400 // 1 day
	maxSyntheticSteps  = 50
)

// ChildIDs returns a composite monitor's child monitor IDs (config["children"]
// is a comma-separated list).
func (m Monitor) ChildIDs() []string {
	var out []string
	for _, id := range strings.Split(m.Config["children"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// CompositeMode returns a composite's aggregation mode: "all" (default — every
// child must be up), "any" (at least one up), or "quorum" (down only when at
// least CompositeQuorum children vote down — the multi-region set, D-0101).
func (m Monitor) CompositeMode() string {
	switch m.Config["mode"] {
	case "any":
		return "any"
	case "quorum":
		return "quorum"
	default:
		return "all"
	}
}

// CompositeQuorum returns the down-vote threshold M for mode "quorum": the
// composite goes down only when >= M children are not up. 0 when unset/invalid
// (Validate rejects that for quorum mode).
func (m Monitor) CompositeQuorum() int {
	q := 0
	for _, c := range m.Config["quorum"] {
		if c < '0' || c > '9' {
			return 0
		}
		q = q*10 + int(c-'0')
		if q > 1000 {
			return 0
		}
	}
	return q
}

// httpMethods are the request methods an HTTP monitor may use.
var httpMethods = map[string]bool{
	"GET": true, "POST": true, "HEAD": true, "PUT": true, "DELETE": true,
}

// The protection a monitor config key gets is THREE classifications, not one set. They were
// one set until FR-028, and that is how a scenario's bearer token ended up cleartext at rest
// and in every viewer's monitor list: the single name `SecretMonitorConfigKeys` meant both
// "encrypt this" and "never return this", so a key that needs the first and not the second
// had nowhere to live (D-0216).
//
// The classifications are declarative and closed. They are NOT an authorization mechanism:
// the decision is `ActionProjectWrite`, taken by the handler, which then SELECTS a reader.
// A mode inferred from request contents would be an authorization decision made by the
// caller of the API.
var (
	// EncryptedMonitorConfigKeys are ciphertext in the column, covered by rotation and by
	// the startup backfill.
	EncryptedMonitorConfigKeys = map[string]bool{"password": true, SyntheticScenarioKey: true}

	// WriteOnlyMonitorConfigKeys never leave the server, in any mode: a client that wants to
	// change one sends a new value, and an empty submission keeps what is stored.
	WriteOnlyMonitorConfigKeys = map[string]bool{"password": true}

	// WriterOnlyMonitorConfigKeys are returned to a principal who may WRITE the monitor and
	// withheld from everyone else. A scenario is the check's own definition — an operator who
	// may edit the monitor must be able to read and change it — while it may carry a
	// credential until FR-028 stage 2 lands, so a read-only viewer does not get it.
	WriterOnlyMonitorConfigKeys = map[string]bool{SyntheticScenarioKey: true}

	// SecretMonitorConfigKeys is the write-only set under its historical name, kept only for
	// callers that mean exactly "never return this". New code names the classification it
	// means; this alias exists so the split did not become a rename of forty call sites.
	SecretMonitorConfigKeys = WriteOnlyMonitorConfigKeys
)

// WithoutWriterOnlyConfig removes the keys a read-only principal may not see — today the
// synthetic scenario. It is a separate step from Redacted() for the same reason
// WithoutPushToken is: what a VIEWER may not see is a different question from what NOBODY
// may see, and collapsing them is the mistake FR-028 exists to correct.
func (m Monitor) WithoutWriterOnlyConfig() Monitor {
	if len(m.Config) == 0 {
		return m
	}
	cfg := make(map[string]string, len(m.Config))
	for k, v := range m.Config {
		if WriterOnlyMonitorConfigKeys[k] {
			continue
		}
		cfg[k] = v
	}
	m.Config = cfg
	return m
}

// Redacted returns a copy of the monitor with secret config values blanked, safe
// to return to clients.
func (m Monitor) Redacted() Monitor {
	if len(m.Config) == 0 {
		return m
	}
	cfg := make(map[string]string, len(m.Config))
	for k, v := range m.Config {
		if SecretMonitorConfigKeys[k] && v != "" {
			cfg[k] = ""
		} else {
			cfg[k] = v
		}
	}
	m.Config = cfg
	return m
}

// WithoutPushToken blanks the push heartbeat token. The token is a bearer
// capability for the public push endpoint (anyone holding it can post UP/DOWN),
// so it must not be returned to a read-only viewer — only to a caller who can
// already write the monitor. Applied on top of Redacted() in the read handlers.
func (m Monitor) WithoutPushToken() Monitor {
	m.PushToken = ""
	return m
}

// Normalize applies defaults and clears fields irrelevant to the monitor type:
// an HTTP monitor defaults to (and upper-cases) GET; non-HTTP monitors carry no
// method; grace applies only to push monitors.
func (m *Monitor) Normalize() {
	// FR-030 D2: whitespace is trimmed and a whitespace-only description is no description.
	m.Description = strings.TrimSpace(m.Description)
	// FR-029 D3e: the stored workflow is ONE CANONICAL string, and canonicalization belongs to the
	// server on every write surface — not to each client.
	//
	// The file provider called `CanaryConfig` and so always stored the canonical form; the API path
	// only PARSED and VALIDATED the document and stored whatever string the caller sent. Two monitors
	// with the same workflow but a different key order therefore had different stored documents and
	// different semantic hashes, so a Monitoring-as-Code re-apply over an API-created canary read as
	// CHANGED forever, and D3e's "one canonical string" was false of that surface. Found while
	// building the phase-F form, which is an API client and would have produced exactly that.
	//
	// A document that does not parse is left untouched: `Normalize` cannot fail, and the refusal is
	// `Validate`'s to make with a message that names the position.
	if m.Type == MonitorAsyncCanary && m.Config[CanaryWorkflowKey] != "" {
		if w, err := ParseCanaryConfig(m.Config); err == nil {
			if doc, cerr := CanaryCanonicalJSON(w); cerr == nil {
				m.Config[CanaryWorkflowKey] = doc
			}
		}
	}
	if m.Type == MonitorHTTP {
		m.Method = strings.ToUpper(strings.TrimSpace(m.Method))
		if m.Method == "" {
			m.Method = "GET"
		}
	} else {
		m.Method = ""
	}
	if m.Type != MonitorPush {
		m.GraceSeconds = 0
	}
	if m.FailureThreshold < 1 {
		m.FailureThreshold = 1 // at least one failed check declares down
	}
	if m.Type == MonitorPush {
		// A push (dead-man's-switch) timeout is a single definitive signal — the
		// target simply stopped reporting. It must not be confirmation-gated, or a
		// threshold of N would delay "down" by N missed intervals.
		m.FailureThreshold = 1
	}
	// Confirm interval: 0 = off; otherwise clamp to [5s, interval] so the
	// confirmation phase can neither hammer the target nor outlast the main rhythm.
	switch {
	case m.ConfirmIntervalSeconds <= 0:
		m.ConfirmIntervalSeconds = 0
	case m.ConfirmIntervalSeconds < 5:
		m.ConfirmIntervalSeconds = 5
	case m.IntervalSeconds > 0 && m.ConfirmIntervalSeconds > m.IntervalSeconds:
		m.ConfirmIntervalSeconds = m.IntervalSeconds
	}
	if m.Type == MonitorPush || m.Type == MonitorComposite {
		m.ConfirmIntervalSeconds = 0 // no active probing to accelerate
	}
	m.Tags = normalizeTags(m.Tags)
	m.Region = strings.ToLower(strings.TrimSpace(m.Region))
	if m.Region == "" {
		m.Region = DefaultRegion
	}
	// Composite monitors are pinned to the central pool (see Validate).
	if m.Type == MonitorComposite {
		m.Region = DefaultRegion
	}
	m.EscalationPolicyID = strings.TrimSpace(m.EscalationPolicyID)
}

// DefaultRegion is the central worker pool that probes monitors with no explicit
// region (and the only pool allowed to run composite monitors).
const DefaultRegion = "core"

// regionSlug bounds region names (worker-pool labels).
var regionSlug = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ValidRegion reports whether s is a well-formed region name. The domain owns this
// rule (single owner); config/cli validation reuses it rather than duplicating the
// pattern.
func ValidRegion(s string) bool { return regionSlug.MatchString(s) }

// maxTags / maxTagLen bound label sprawl.
const (
	maxTags   = 20
	maxTagLen = 40
)

// normalizeTags trims, drops empty/over-long, de-duplicates (case-insensitively,
// keeping first spelling), and caps the count — so tags stay a clean set.
func normalizeTags(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > maxTagLen {
			continue
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// Heartbeat is one recorded check result (a time-series point).
type Heartbeat struct {
	MonitorID string    `json:"monitor_id"`
	Ts        time.Time `json:"ts"`
	Up        bool      `json:"up"`
	LatencyMS int64     `json:"latency_ms"`
	Code      int       `json:"code"`
	Msg       string    `json:"msg"`
	// ExecutionRevision is the monitor config generation this probe ran under (stamped by
	// the prober from the job's monitor). RecordScheduledResult rejects a result whose
	// revision no longer matches the monitor's current one. 0 = not carried (push/legacy).
	ExecutionRevision int64 `json:"execution_revision,omitempty"`
	// JobID and JobIssuedAt are the job this result answers, carried back so a result can be
	// correlated with the dispatch that asked for it (func-result-protocol §9, deferred there with
	// "not here" and delivered in iter-0155). Both are stamped by the CORE — the id and the issue
	// instant come from the database in the same statement that materializes the job — so
	// `observed_at >= job_issued_at` compares an executor's clock against the core's, which is the
	// only comparison that can detect a skewed region. Empty/zero on a push or legacy result, and
	// then the ordering check does not apply.
	JobID       string    `json:"job_id,omitempty"`
	JobIssuedAt time.Time `json:"job_issued_at,omitempty"`
	// CanaryRunKey is the SCHEDULED RUN this result answers, carried back so the in-flight slot is
	// released by the run that took it and not merely by monitor id. Without it a LATE result from
	// run 1 — arriving after run 1's lease expired and run 2 claimed the slot — deleted run 2's row
	// and let run 3 start alongside it, which is the one thing the lease exists to prevent
	// (reviewer P0-3). Empty for every other monitor type, and empty from an executor older than
	// this release: such a result releases nothing and the slot returns at its TTL instead.
	CanaryRunKey string `json:"canary_run_key,omitempty"`
	// ProbeError is the typed non-liveness result member used when an executor cannot
	// authenticate/materialize a credential envelope. When set, the ingest path records
	// diagnostics only: no heartbeat, status, SLA, incident, or transition mutation.
	ProbeError *ProbeError `json:"probe_error,omitempty"`
}

const (
	ProbeErrorNoDispatchKey      = "no_dispatch_key"
	ProbeErrorUnknownKeyID       = "unknown_key_id"
	ProbeErrorDecryptAuthFailed  = "decrypt_auth_failed"
	ProbeErrorUnsupportedVersion = "unsupported_version"
)

// ProbeError is a bounded wire-safe execution diagnostic. It intentionally omits key ids,
// ciphertext and detailed crypto errors so it cannot become a diagnostic oracle.
type ProbeError struct {
	Reason string `json:"reason"`
	JobID  string `json:"job_id,omitempty"`
}

func (e ProbeError) Error() string { return "probe_error: " + e.Reason }

func ValidProbeErrorReason(reason string) bool {
	switch reason {
	case ProbeErrorNoDispatchKey, ProbeErrorUnknownKeyID, ProbeErrorDecryptAuthFailed, ProbeErrorUnsupportedVersion:
		return true
	default:
		return false
	}
}

// StatusFor maps an up/down boolean to a MonitorStatus.
func StatusFor(up bool) MonitorStatus {
	if up {
		return StatusUp
	}
	return StatusDown
}

// MonitorSlugPattern is the shape a monitor slug must have: project-unique, immutable, and
// readable enough to be typed into a bundle by hand.
var monitorSlugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidMonitorSlug reports whether s is a well-formed monitor slug.
func ValidMonitorSlug(s string) bool { return monitorSlugRe.MatchString(s) }

// MonitorSlugPattern exposes the pattern for API schemas and error messages.
func MonitorSlugPattern() string { return monitorSlugRe.String() }

// NormalizeMonitorSlug derives a candidate slug from a display name or a provider source
// uid. It is deliberately the SAME derivation the backfill migration performs, so a monitor
// created today and one adopted from an old row land on the same shape.
//
// It does not guarantee uniqueness — that is the store's job, under the project lock, since
// only the database can answer it.
func NormalizeMonitorSlug(in string) string {
	s := strings.ToLower(in)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 55 {
		out = strings.TrimRight(out[:55], "-")
	}
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = strings.TrimRight("monitor-"+out, "-")
	}
	return out
}
