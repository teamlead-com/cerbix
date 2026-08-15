// Package agent is the HTTP-pull prober: a DB-less, broker-less process that claims
// check jobs for its region from the central API over HTTPS, probes them, and posts the
// resulting heartbeats back. It is the alternative to a RabbitMQ worker for a geo that
// must not reach the broker (only outbound HTTPS to the core is required).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
)

const (
	defaultPollEvery      = 2 * time.Second
	defaultHeartbeatEvery = 15 * time.Second
	// testPollEvery is how often the agent checks for a one-off "Test connection" probe
	// for its region (a rare, manual action, so a short fixed poll is enough).
	testPollEvery = time.Second
	claimBatch    = 16
	// bufferCap bounds the in-memory edge buffer of results held while the central API
	// is unreachable; oldest entries are dropped past this (a ring), so a long outage
	// cannot grow memory without bound.
	bufferCap = 10000
)

// Runner executes a single monitor check (implemented by *prober.Runner).
type Runner interface {
	Run(ctx context.Context, m domain.Monitor) domain.Heartbeat
}

type CredentialHealth interface {
	SetCredentialReady(ready bool, reason string)
	RecordExecutorProbeError(reason string)
}

// Agent polls the central API for its region's jobs and posts back results.
type Agent struct {
	serverURL      string
	token          string
	region         string
	agentID        string
	runner         Runner
	http           *http.Client
	logger         *slog.Logger
	pollEvery      time.Duration
	heartbeatEvery time.Duration

	// Edge buffer (accessed only from the single Run loop, so no locking): results held
	// while the API is unreachable, flushed as HISTORICAL backfill on reconnect.
	buf              []domain.Heartbeat
	dropped          int
	credentials      *dispatch.CredentialKeyring
	credentialReady  atomic.Bool
	credentialHealth CredentialHealth
}

// New builds an agent. serverURL is the central base URL (e.g. https://cerbix.core);
// token is the shared agent secret; region is this pool's name.
func New(serverURL, token, region string, runner Runner, logger *slog.Logger) *Agent {
	host, _ := os.Hostname()
	if host == "" {
		host = "agent"
	}
	return &Agent{
		serverURL:      serverURL,
		token:          token,
		region:         region,
		agentID:        host + "/" + region,
		runner:         runner,
		http:           &http.Client{Timeout: 30 * time.Second},
		logger:         logger,
		pollEvery:      defaultPollEvery,
		heartbeatEvery: defaultHeartbeatEvery,
	}
}

func (a *Agent) WithCredentialKeyring(ring *dispatch.CredentialKeyring) *Agent {
	a.credentials = ring
	a.credentialReady.Store(ring != nil)
	return a
}

func (a *Agent) WithCredentialHealth(health CredentialHealth) *Agent {
	a.credentialHealth = health
	if health != nil {
		reason := ""
		if a.credentials == nil {
			reason = domain.ProbeErrorNoDispatchKey
		}
		health.SetCredentialReady(a.credentials != nil, reason)
	}
	return a
}

func (a *Agent) recordCredentialFailure(reason string) {
	if reason != domain.ProbeErrorUnsupportedVersion {
		a.credentialReady.Store(false)
		if a.credentialHealth != nil {
			a.credentialHealth.SetCredentialReady(false, reason)
		}
	}
	if a.credentialHealth != nil {
		a.credentialHealth.RecordExecutorProbeError(reason)
	}
}

func (a *Agent) recordCredentialSuccess() {
	a.credentialReady.Store(true)
	if a.credentialHealth != nil {
		a.credentialHealth.SetCredentialReady(true, "")
	}
}

// Run polls and heartbeats until ctx is cancelled. Polling and heartbeating run on
// separate goroutines so a long-poll that holds the request server-side (up to ~20s)
// never starves the liveness heartbeat.
func (a *Agent) Run(ctx context.Context) {
	a.heartbeat(ctx)
	a.logger.Info("agent_started", "region", a.region, "server", a.serverURL, "agent_id", a.agentID)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.pollLoop(ctx) }()
	go func() { defer wg.Done(); a.heartbeatLoop(ctx) }()
	go func() { defer wg.Done(); a.testLoop(ctx) }()
	wg.Wait()
}

// testLoop answers one-off "Test connection" probes for this region.
func (a *Agent) testLoop(ctx context.Context) {
	t := time.NewTicker(testPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pollTest(ctx)
		}
	}
}

func (a *Agent) pollTest(ctx context.Context) {
	id, raw, carrierGen, ok := a.claimTest(ctx)
	if !ok {
		return
	}
	var job dispatch.CheckJob
	if err := json.Unmarshal(raw, &job); err != nil {
		a.logger.Error("agent_bad_test", "error", err.Error())
		return
	}
	// The test-RPC path crosses the SAME gate as scheduled jobs — a per-path variant is
	// how one of them ends up missing a rule.
	delivered := dispatch.DeliveredJob{Job: job, CarrierGeneration: a.testCarrierGeneration(carrierGen)}
	materialized, err := dispatch.ValidateAndMaterialize(a.credentials, delivered)
	if err != nil {
		reason := dispatch.CredentialProbeErrorReason(err)
		a.recordCredentialFailure(reason)
		a.logger.Error("agent_credential_test_rejected", "reason", reason,
			"carrier_generation", delivered.CarrierGeneration)
		_ = a.postTestResult(ctx, id, dispatch.ProbeErrorHeartbeat(job, reason))
		return
	}
	if materialized.UsedCredential {
		a.recordCredentialSuccess()
	}
	hb := a.runner.Run(ctx, materialized.Monitor)
	materialized.Cleanup()
	if err := a.postTestResult(ctx, id, hb); err != nil {
		a.logger.Warn("agent_test_result_failed", "error", err.Error())
	}
}

func (a *Agent) claimTest(ctx context.Context) (id string, job json.RawMessage, protocolVersion int, ok bool) {
	path := "/api/v1/agent/tests"
	if a.credentials != nil {
		path = "/api/v1/agent/v2/tests"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.serverURL+path+"?region="+a.region, nil)
	if err != nil {
		return "", nil, 0, false
	}
	a.auth(req)
	if a.credentials != nil {
		req.Header.Set("X-Cerbix-Credential-Envelope", "1")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", nil, 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil, 0, false
	}
	var out struct {
		Test *struct {
			ID              string          `json:"id"`
			Job             json.RawMessage `json:"job"`
			ProtocolVersion int             `json:"protocol_version"`
		} `json:"test"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Test == nil {
		return "", nil, 0, false
	}
	return out.Test.ID, out.Test.Job, out.Test.ProtocolVersion, true
}

func (a *Agent) postTestResult(ctx context.Context, id string, hb domain.Heartbeat) error {
	body, err := json.Marshal(map[string]any{"id": id, "result": hb})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.serverURL+"/api/v1/agent/test-results?region="+a.region, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.auth(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("test-results status %d", resp.StatusCode)
	}
	return nil
}

// pollLoop claims and executes jobs back-to-back (the server long-poll paces it when
// idle), with pollEvery as a floor between cycles.
func (a *Agent) pollLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		a.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.pollEvery):
		}
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.heartbeat(ctx)
		}
	}
}

func (a *Agent) poll(ctx context.Context) {
	jobs, tokens, protocolVersions, err := a.claim(ctx)
	if err != nil {
		a.logger.Warn("agent_claim_failed", "error", err.Error())
		return
	}
	if len(jobs) == 0 {
		return
	}
	results := make([]domain.Heartbeat, 0, len(jobs))
	// Ack ONLY the jobs we actually processed. A malformed job is NOT acked: its lease
	// lapses and it is re-delivered (and, if persistently poison, purged at its TTL) rather
	// than being silently deleted with no result — a correctness bug in the prior version,
	// which acked every claimed token including a skipped one.
	ackTokens := make([]string, 0, len(jobs))
	for i, raw := range jobs {
		var job dispatch.CheckJob
		if err := json.Unmarshal(raw, &job); err != nil {
			a.logger.Error("agent_bad_job", "error", err.Error())
			continue
		}
		// Every job crosses the same gate the AMQP worker uses, unconditionally: the
		// schema decides whether a credential is required, and the carrier generation is
		// the server's stamp rather than anything the payload says (§4.7, D-0160).
		delivered := dispatch.DeliveredJob{Job: job, CarrierGeneration: a.deliveredGeneration(protocolVersions, i)}
		materialized, err := dispatch.ValidateAndMaterialize(a.credentials, delivered)
		if err != nil {
			reason := dispatch.CredentialProbeErrorReason(err)
			a.recordCredentialFailure(reason)
			results = append(results, dispatch.ProbeErrorHeartbeat(job, reason))
			a.logger.Error("agent_credential_job_rejected", "monitor_id", job.Monitor.ID,
				"reason", reason, "carrier_generation", delivered.CarrierGeneration)
			if i < len(tokens) {
				ackTokens = append(ackTokens, tokens[i])
			}
			continue
		}
		if materialized.UsedCredential {
			a.recordCredentialSuccess()
		}
		results = append(results, a.runner.Run(ctx, materialized.Monitor))
		materialized.Cleanup()
		if i < len(tokens) {
			ackTokens = append(ackTokens, tokens[i])
		}
	}
	// The tokens ack the processed jobs: they ride along with the results POST so the server
	// deletes those leased jobs only once the results are accepted. If the POST fails we
	// buffer and do NOT ack — the leases lapse server-side and the jobs are re-delivered
	// rather than lost. Duplicate re-delivery is safe (RecordScheduledResult dedups).
	if err := a.postResults(ctx, results, ackTokens); err != nil {
		a.bufferResults(results)
		a.logger.Warn("agent_results_buffered", "error", err.Error(), "buffered", len(a.buf))
		return
	}
	a.flushBuffer(ctx) // connectivity is back — drain any buffered historical results
	a.logger.Info("agent_batch_done", "jobs", len(jobs), "results", len(results))
}

// bufferResults appends to the edge buffer, dropping the oldest past the cap (a ring).
func (a *Agent) bufferResults(hbs []domain.Heartbeat) {
	// probe_error is a current execution diagnostic, never historical/SLA data. A failed
	// live POST leaves its job unacked for redelivery; do not replay it through backfill.
	for _, hb := range hbs {
		if hb.ProbeError == nil {
			a.buf = append(a.buf, hb)
		}
	}
	if over := len(a.buf) - bufferCap; over > 0 {
		a.buf = a.buf[over:]
		a.dropped += over
		a.logger.Warn("agent_buffer_overflow", "dropped_total", a.dropped)
	}
}

// flushBuffer posts the edge buffer as HISTORICAL backfill; keeps it on failure to retry.
func (a *Agent) flushBuffer(ctx context.Context) {
	if len(a.buf) == 0 {
		return
	}
	if err := a.postBackfill(ctx, a.buf); err != nil {
		a.logger.Warn("agent_backfill_failed", "error", err.Error(), "buffered", len(a.buf))
		return
	}
	a.logger.Info("agent_backfill_flushed", "count", len(a.buf))
	a.buf = nil
}

func (a *Agent) claim(ctx context.Context) (jobs []json.RawMessage, tokens []string, protocolVersions []int, err error) {
	path := "/api/v1/agent/jobs"
	if a.credentials != nil {
		path = "/api/v1/agent/v2/jobs"
	}
	url := fmt.Sprintf("%s%s?region=%s&max=%d", a.serverURL, path, a.region, claimBatch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	a.auth(req)
	if a.credentials != nil {
		req.Header.Set("X-Cerbix-Credential-Envelope", "1")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, nil, fmt.Errorf("claim status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Jobs             []json.RawMessage `json:"jobs"`
		Tokens           []string          `json:"tokens"`
		ProtocolVersions []int             `json:"protocol_versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, nil, err
	}
	return out.Jobs, out.Tokens, out.ProtocolVersions, nil
}

// carrierGeneration returns the SERVER's stamp for the i-th claimed row. A capable claim
// mixes generations, so the generation is transport metadata, never something to read out
// of the payload — that part is attacker-editable (func-secret-inventory §4.7, D-0160). A
// server that did not stamp (older core during a rolling upgrade) yields 0, which callers
// treat as "unknown, do not use as authority".
func carrierGeneration(versions []int, i int) int {
	if i < 0 || i >= len(versions) {
		return 0
	}
	return versions[i]
}

// deliveredGeneration resolves the carrier generation for the i-th claimed job. The
// server's stamp wins; when it is absent (a core that predates stamping) the ENDPOINT the
// agent itself chose to poll is the fallback — that is still out-of-band knowledge the
// agent holds, and it is never the payload.
func (a *Agent) deliveredGeneration(versions []int, i int) int {
	if gen := carrierGeneration(versions, i); gen > 0 {
		return gen
	}
	return a.claimGeneration()
}

func (a *Agent) testCarrierGeneration(stamped int) int {
	if stamped > 0 {
		return stamped
	}
	return a.claimGeneration()
}

// claimGeneration is the generation of the claim endpoint this agent polls, which follows
// from its own capability — not from anything a server or a payload asserts.
func (a *Agent) claimGeneration() int {
	if a.credentials != nil {
		return dispatch.ProtocolV2
	}
	return dispatch.ProtocolV1
}

// postResults reports live results and acks (via ack tokens) the claimed jobs they
// complete, in one request.
func (a *Agent) postResults(ctx context.Context, results []domain.Heartbeat, ack []string) error {
	return a.postHeartbeats(ctx, "/api/v1/agent/results?region="+a.region, results, ack)
}

// postBackfill sends buffered results to the historical (SLA-only) endpoint. Buffered
// results were never acked, so their jobs already re-leased/expired — no ack here.
func (a *Agent) postBackfill(ctx context.Context, results []domain.Heartbeat) error {
	return a.postHeartbeats(ctx, "/api/v1/agent/backfill?region="+a.region, results, nil)
}

func (a *Agent) postHeartbeats(ctx context.Context, path string, results []domain.Heartbeat, ack []string) error {
	payload := map[string]any{"results": results}
	if len(ack) > 0 {
		payload["ack"] = ack
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.serverURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.auth(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s status %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) heartbeat(ctx context.Context) {
	url := fmt.Sprintf("%s/api/v1/agent/heartbeat?region=%s&agent_id=%s", a.serverURL, a.region, a.agentID)
	capability := 0
	if a.credentials != nil {
		capability = 1
	}
	body, err := json.Marshal(map[string]any{
		"capabilities":     map[string]int{"credential_envelope": capability},
		"credential_ready": capability == 1 && a.credentialReady.Load(),
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	a.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		a.logger.Warn("agent_heartbeat_failed", "error", err.Error())
		return
	}
	_ = resp.Body.Close()
}

func (a *Agent) auth(req *http.Request) { req.Header.Set("Authorization", "Bearer "+a.token) }
