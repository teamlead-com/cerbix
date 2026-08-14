package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
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
	// MonitorPush is a passive dead-man's-switch: the target reports in.
	MonitorPush MonitorType = "push"
)

// Active reports whether the type is scheduled for periodic evaluation (network
// probers and composites), as opposed to the passive push type.
func (t MonitorType) Active() bool {
	return t == MonitorHTTP || t == MonitorTCP || t == MonitorICMP || t == MonitorDNS ||
		t == MonitorTLS || t == MonitorGRPC || t == MonitorComposite || t == MonitorPostgres || t == MonitorMySQL || t == MonitorRedis || t == MonitorPromQL ||
		t == MonitorRabbitMQ || t == MonitorWebSocket || t == MonitorSSH || t == MonitorSynthetic
}

// NeedsTarget reports whether the type probes a single network target (composites
// derive from children; synthetic carries its own per-step URLs in the scenario).
func (t MonitorType) NeedsTarget() bool {
	return t.Active() && t != MonitorComposite && t != MonitorSynthetic
}

// Valid reports whether t is a known monitor type.
func (t MonitorType) Valid() bool {
	switch t {
	case MonitorHTTP, MonitorTCP, MonitorICMP, MonitorDNS, MonitorTLS, MonitorGRPC, MonitorComposite, MonitorPostgres, MonitorMySQL, MonitorRedis, MonitorPromQL, MonitorRabbitMQ, MonitorWebSocket, MonitorSSH, MonitorSynthetic, MonitorPush:
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
	ID               string      `json:"id"`
	ProjectID        string      `json:"project_id"`
	Name             string      `json:"name"`
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
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

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

// Validate enforces monitor invariants (domain-owned).
func (m Monitor) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("monitor: name is required")
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

// SecretMonitorConfigKeys are config keys treated as secrets: encrypted at rest
// and never returned in API responses (single source of truth for store + api).
var SecretMonitorConfigKeys = map[string]bool{"password": true}

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
}

// StatusFor maps an up/down boolean to a MonitorStatus.
func StatusFor(up bool) MonitorStatus {
	if up {
		return StatusUp
	}
	return StatusDown
}
