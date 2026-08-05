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
	buf     []domain.Heartbeat
	dropped int
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
	id, raw, ok := a.claimTest(ctx)
	if !ok {
		return
	}
	var job dispatch.CheckJob
	if err := json.Unmarshal(raw, &job); err != nil {
		a.logger.Error("agent_bad_test", "error", err.Error())
		return
	}
	hb := a.runner.Run(ctx, job.Monitor)
	if err := a.postTestResult(ctx, id, hb); err != nil {
		a.logger.Warn("agent_test_result_failed", "error", err.Error())
	}
}

func (a *Agent) claimTest(ctx context.Context) (id string, job json.RawMessage, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.serverURL+"/api/v1/agent/tests?region="+a.region, nil)
	if err != nil {
		return "", nil, false
	}
	a.auth(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, false
	}
	var out struct {
		Test *struct {
			ID  string          `json:"id"`
			Job json.RawMessage `json:"job"`
		} `json:"test"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Test == nil {
		return "", nil, false
	}
	return out.Test.ID, out.Test.Job, true
}

func (a *Agent) postTestResult(ctx context.Context, id string, hb domain.Heartbeat) error {
	body, err := json.Marshal(map[string]any{"id": id, "result": hb})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.serverURL+"/api/v1/agent/test-results", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.auth(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
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
	jobs, err := a.claim(ctx)
	if err != nil {
		a.logger.Warn("agent_claim_failed", "error", err.Error())
		return
	}
	if len(jobs) == 0 {
		return
	}
	results := make([]domain.Heartbeat, 0, len(jobs))
	for _, raw := range jobs {
		var job dispatch.CheckJob
		if err := json.Unmarshal(raw, &job); err != nil {
			a.logger.Error("agent_bad_job", "error", err.Error())
			continue
		}
		results = append(results, a.runner.Run(ctx, job.Monitor))
	}
	if err := a.postResults(ctx, results); err != nil {
		// API unreachable: keep this cycle's results as historical (buffered). Live
		// status will be driven by fresh probes once connectivity returns.
		a.bufferResults(results)
		a.logger.Warn("agent_results_buffered", "error", err.Error(), "buffered", len(a.buf))
		return
	}
	a.flushBuffer(ctx) // connectivity is back — drain any buffered historical results
	a.logger.Info("agent_batch_done", "jobs", len(jobs), "results", len(results))
}

// bufferResults appends to the edge buffer, dropping the oldest past the cap (a ring).
func (a *Agent) bufferResults(hbs []domain.Heartbeat) {
	a.buf = append(a.buf, hbs...)
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

func (a *Agent) claim(ctx context.Context) ([]json.RawMessage, error) {
	url := fmt.Sprintf("%s/api/v1/agent/jobs?region=%s&max=%d", a.serverURL, a.region, claimBatch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	a.auth(req)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("claim status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

func (a *Agent) postResults(ctx context.Context, results []domain.Heartbeat) error {
	return a.postHeartbeats(ctx, "/api/v1/agent/results?region="+a.region, results)
}

// postBackfill sends buffered results to the historical (SLA-only) endpoint.
func (a *Agent) postBackfill(ctx context.Context, results []domain.Heartbeat) error {
	return a.postHeartbeats(ctx, "/api/v1/agent/backfill?region="+a.region, results)
}

func (a *Agent) postHeartbeats(ctx context.Context, path string, results []domain.Heartbeat) error {
	body, err := json.Marshal(map[string]any{"results": results})
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s status %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) heartbeat(ctx context.Context) {
	url := fmt.Sprintf("%s/api/v1/agent/heartbeat?region=%s&agent_id=%s", a.serverURL, a.region, a.agentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return
	}
	a.auth(req)
	resp, err := a.http.Do(req)
	if err != nil {
		a.logger.Warn("agent_heartbeat_failed", "error", err.Error())
		return
	}
	_ = resp.Body.Close()
}

func (a *Agent) auth(req *http.Request) { req.Header.Set("Authorization", "Bearer "+a.token) }
