package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// agentLongPollHold bounds how long GET /agent/jobs holds an empty request waiting for a
// job (a max-hold fallback in case a NOTIFY is missed on a listener reconnect).
const agentLongPollHold = 20 * time.Second

// AgentRouter registers the HTTP-pull agent endpoints, gated by a shared bearer token
// (WithAgentToken). It is mounted outside the session-auth middleware because agents
// are not users. Without a token configured, the endpoints are disabled (404).
func (h *Handler) AgentRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agent/jobs", h.agentAuth(h.agentJobs))
	mux.HandleFunc("POST /api/v1/agent/results", h.agentAuth(h.agentResults))
	mux.HandleFunc("POST /api/v1/agent/backfill", h.agentAuth(h.agentBackfill))
	mux.HandleFunc("GET /api/v1/agent/tests", h.agentAuth(h.agentTests))
	mux.HandleFunc("POST /api/v1/agent/test-results", h.agentAuth(h.agentTestResult))
	mux.HandleFunc("POST /api/v1/agent/heartbeat", h.agentAuth(h.agentHeartbeat))
	return mux
}

// agentEnabled reports whether any agent auth (config or database) is configured.
func (h *Handler) agentEnabled() bool {
	return h.agentToken != "" || len(h.agentRegionTokens) > 0 || h.agentDBTokens
}

// agentDBAuthorized checks a bearer against database-managed agent tokens (a token
// authorizes exactly its region; an empty request region — results/backfill without one
// — accepts any live token).
func (h *Handler) agentDBAuthorized(ctx context.Context, bearer, region string) bool {
	if !h.agentDBTokens || bearer == "" {
		return false
	}
	tokenRegion, ok, err := h.store.ResolveAgentTokenRegion(ctx, store.HashToken(bearer))
	if err != nil {
		h.logger.Warn("agent_token_resolve_failed", "error", err.Error())
		return false
	}
	return ok && (region == "" || tokenRegion == region)
}

// agentAuthorized checks a bearer token against the agent auth policy. A request with a
// region (jobs/heartbeat) is authorized by the catch-all token or that region's token;
// a request without a region (results) is authorized by the catch-all or any configured
// per-region token (it is a valid agent, and results are routed by monitor id anyway).
func (h *Handler) agentAuthorized(bearer, region string) bool {
	eq := func(a, b string) bool { return b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
	if eq(bearer, h.agentToken) {
		return true
	}
	if region != "" {
		return eq(bearer, h.agentRegionTokens[region])
	}
	for _, t := range h.agentRegionTokens {
		if eq(bearer, t) {
			return true
		}
	}
	return false
}

// agentAuth wraps a handler with the agent-token check (constant-time, region-scoped).
func (h *Handler) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.agentEnabled() {
			http.NotFound(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		region := r.URL.Query().Get("region")
		if !h.agentAuthorized(got, region) && !h.agentDBAuthorized(r.Context(), got, region) {
			writeError(w, http.StatusUnauthorized, "invalid agent token for this region")
			return
		}
		next(w, r)
	}
}

// agentJobs claims up to `max` due jobs for the agent's region and returns their raw
// CheckJob payloads (the agent decodes and probes them). A job is delivered once.
func (h *Handler) agentJobs(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		writeError(w, http.StatusBadRequest, "region is required")
		return
	}
	max := 16
	if v := r.URL.Query().Get("max"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}
	payloads, err := h.store.ClaimPullJobs(r.Context(), region, max)
	if err != nil {
		h.serverError(w, "agent_claim_jobs", err)
		return
	}
	// Long-poll: if nothing is due, hold the request until a job is enqueued for this
	// region (LISTEN/NOTIFY) or the max hold elapses, then claim once more. This cuts
	// the query rate to near-zero while keeping dispatch near-instant, over plain HTTP.
	if len(payloads) == 0 && h.pullWaiter != nil {
		h.pullWaiter.Wait(r.Context(), region, agentLongPollHold)
		if payloads, err = h.store.ClaimPullJobs(r.Context(), region, max); err != nil {
			h.serverError(w, "agent_claim_jobs", err)
			return
		}
	}
	jobs := make([]json.RawMessage, 0, len(payloads))
	for _, p := range payloads {
		jobs = append(jobs, json.RawMessage(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// enforceRegionScope rejects (403) a result batch that contains a heartbeat for a
// monitor outside the agent's region — so a compromised region token cannot forge
// results for another region's monitors. Skipped when region is empty (catch-all token
// or an agent that does not send its region). Heartbeats for deleted monitors are
// tolerated (they fall out at insert). Returns false and writes the error on rejection.
func (h *Handler) enforceRegionScope(w http.ResponseWriter, r *http.Request, region string, hbs []domain.Heartbeat) bool {
	if region == "" || len(hbs) == 0 {
		return true
	}
	ids := make([]string, 0, len(hbs))
	seen := map[string]bool{}
	for _, hb := range hbs {
		if hb.MonitorID != "" && !seen[hb.MonitorID] {
			seen[hb.MonitorID] = true
			ids = append(ids, hb.MonitorID)
		}
	}
	regions, err := h.store.MonitorRegions(r.Context(), ids)
	if err != nil {
		h.serverError(w, "agent_monitor_regions", err)
		return false
	}
	for id, reg := range regions {
		if reg != region {
			writeError(w, http.StatusForbidden, "result for monitor "+id+" is outside region "+region)
			return false
		}
	}
	return true
}

// agentResults ingests heartbeats produced by the agent into the same pipeline as
// RabbitMQ results (via the result sink).
func (h *Handler) agentResults(w http.ResponseWriter, r *http.Request) {
	if h.results == nil {
		writeError(w, http.StatusNotImplemented, "result ingestion is not available")
		return
	}
	var body struct {
		Results []domain.Heartbeat `json:"results"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !h.enforceRegionScope(w, r, r.URL.Query().Get("region"), body.Results) {
		return
	}
	for _, hb := range body.Results {
		if err := h.results.PublishResult(r.Context(), hb); err != nil {
			h.serverError(w, "agent_publish_result", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(body.Results)})
}

// agentBackfill ingests HISTORICAL heartbeats buffered by an agent during a network
// outage. Unlike /results, these bypass the alert pipeline (status transitions,
// incidents, escalations) and only append to the heartbeats table for SLA/SLI
// continuity — replaying old down→up events must not fire alerts after the fact. It is
// idempotent, so a re-sent buffer is not double-counted.
func (h *Handler) agentBackfill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Results []domain.Heartbeat `json:"results"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !h.enforceRegionScope(w, r, r.URL.Query().Get("region"), body.Results) {
		return
	}
	inserted, err := h.store.InsertHeartbeatsBulk(r.Context(), body.Results)
	if err != nil {
		h.serverError(w, "agent_backfill", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inserted": inserted, "received": len(body.Results)})
}

// agentTests hands the agent one pending "Test connection" job for its region (if any)
// to run and report back — the pull-transport equivalent of the RabbitMQ test-RPC. The
// response is {"test": {"id","job"}} or {"test": null}.
func (h *Handler) agentTests(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		writeError(w, http.StatusBadRequest, "region is required")
		return
	}
	id, payload, ok, err := h.store.ClaimPullTest(r.Context(), region)
	if err != nil {
		h.serverError(w, "agent_claim_test", err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"test": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"test": map[string]any{"id": id, "job": json.RawMessage(payload)}})
}

// agentTestResult stores the heartbeat an agent produced for a test, which the waiting
// /monitors/test request then returns to the operator.
func (h *Handler) agentTestResult(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := h.store.SavePullTestResult(r.Context(), body.ID, body.Result); err != nil {
		h.serverError(w, "agent_test_result", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentHeartbeat records that an agent is alive for its region (so the region reads as
// live in the picker and the region-worker alert).
func (h *Handler) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	agentID := r.URL.Query().Get("agent_id")
	if region == "" || agentID == "" {
		writeError(w, http.StatusBadRequest, "region and agent_id are required")
		return
	}
	if err := h.store.RecordAgentHeartbeat(r.Context(), region, agentID); err != nil {
		h.serverError(w, "agent_heartbeat", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Agent-token admin (global-admin; database-managed alternative to config tokens) ---

// createAgentToken issues a new per-region agent token, returning the secret once.
func (h *Handler) createAgentToken(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	var body struct {
		Name   string `json:"name"`
		Region string `json:"region"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Region) == "" {
		writeError(w, http.StatusBadRequest, "name and region are required")
		return
	}
	plaintext := generateTokenSecret()
	created, err := h.store.CreateAgentToken(r.Context(), body.Name, body.Region, store.HashToken(plaintext))
	if err != nil {
		h.serverError(w, "create_agent_token", err)
		return
	}
	// The secret is shown only here — configure it as the agent's pull.token.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": created.ID, "name": created.Name, "region": created.Region, "token": plaintext,
	})
}

func (h *Handler) listAgentTokens(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	tokens, err := h.store.ListAgentTokens(r.Context())
	if err != nil {
		h.serverError(w, "list_agent_tokens", err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *Handler) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	if !h.requireGlobalAdmin(w, r) {
		return
	}
	if err := h.store.RevokeAgentToken(r.Context(), r.PathValue("tokenID")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		h.serverError(w, "revoke_agent_token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
