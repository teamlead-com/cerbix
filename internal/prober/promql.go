package prober

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// promqlProber evaluates a PromQL query against a Prometheus server (through the
// guarded HTTP client). Success (no conditions) = the query returns a value; the
// scalar value is exposed as [RESULT] so `[RESULT] < 0.9` can assert a threshold.
// Config keys: query (the PromQL expression) and, for `auth: basic`, username + the
// materialized password. Target is the Prometheus base URL.
type promqlProber struct{ client *http.Client }

func (p promqlProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	query := strings.TrimSpace(m.Config["query"])
	if query == "" {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no query configured"}
	}
	u, err := promQueryURL(m.Target, query)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad target: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: msgRequestNotBuilt}
	}
	// Optional basic auth (D-0215): the credential arrives materialized in Config by the
	// dispatch gate, exactly as it does for the RabbitMQ management probe. An absent
	// username means the `none` variant — the unauthenticated Prometheus that was the only
	// supported shape before — so the header is not sent at all rather than sent empty.
	if user := m.Config["username"]; user != "" {
		req.SetBasicAuth(user, m.Config["password"])
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// The composed URL carries the operator's target, which may hold a credential in its
		// query — and the query cerbix appends besides. Neither goes into the message.
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: probeFailure(err, u)}
	}
	defer func() { _ = resp.Body.Close() }()
	lat := elapsedMS(start)
	if resp.StatusCode != http.StatusOK {
		return Result{Connected: false, LatencyMS: lat, Code: resp.StatusCode, Msg: fmt.Sprintf("prometheus status %d", resp.StatusCode)}
	}
	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	// The answer is read through the same 1 MiB bound as every other HTTP body a prober reads
	// (sec-hardening-2 S8; review of iter-0212, P2-1): a body that never completes ends the probe
	// at the bound, not at its deadline.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&out); err != nil {
		return Result{Connected: false, LatencyMS: lat, Msg: "decode: " + err.Error()}
	}
	if out.Status != "success" {
		return Result{Connected: false, LatencyMS: lat, Msg: "query status: " + out.Status}
	}
	value, ok := promValue(out.Data.ResultType, out.Data.Result)
	if !ok {
		return Result{Connected: false, LatencyMS: lat, Msg: "query returned no value"}
	}
	return Result{Connected: true, LatencyMS: lat, Value: value}
}

// promQueryURL builds the instant-query URL on the operator's base URL (sec-hardening-2 H-2 S2,
// external review of iter-0210): the base is parsed again by domain.ParsePromQLTarget — the parser
// the write surfaces run — before anything is dialed, and the URL is a url.URL, not a
// concatenation. The base path (a reverse-proxy prefix) is kept with its percent-encoding, joined
// to /api/v1/query by joinURLPath. Userinfo of a row stored before D-0145 stays on the same host,
// as before.
func promQueryURL(target, query string) (string, error) {
	base, err := domain.ParsePromQLTarget(target)
	if err != nil {
		return "", err
	}
	out := url.URL{Scheme: base.Scheme, User: base.User, Host: base.Host, RawQuery: url.Values{"query": {query}}.Encode()}
	out.Path, out.RawPath = joinURLPath(base, promQueryPath)
	return out.String(), nil
}

// promQueryPath is the instant-query endpoint, appended to the operator's base path.
var promQueryPath = &url.URL{Path: "/api/v1/query"}

// promValue extracts a scalar float from a Prometheus query result: a "scalar"
// ([ts, "value"]) or the first sample of a "vector" ([{value:[ts,"value"]}]).
func promValue(resultType string, raw json.RawMessage) (float64, bool) {
	switch resultType {
	case "scalar":
		var pair []any
		if json.Unmarshal(raw, &pair) == nil && len(pair) == 2 {
			return parseSample(pair[1])
		}
	case "vector":
		var samples []struct {
			Value []any `json:"value"`
		}
		if json.Unmarshal(raw, &samples) == nil && len(samples) > 0 && len(samples[0].Value) == 2 {
			return parseSample(samples[0].Value[1])
		}
	}
	return 0, false
}

func parseSample(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}
