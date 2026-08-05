package prober

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// promqlProber evaluates a PromQL query against a Prometheus server (through the
// guarded HTTP client). Success (no conditions) = the query returns a value; the
// scalar value is exposed as [RESULT] so `[RESULT] < 0.9` can assert a threshold.
// Config keys: query (the PromQL expression). Target is the Prometheus base URL.
type promqlProber struct{ client *http.Client }

func (p promqlProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	query := strings.TrimSpace(m.Config["query"])
	if query == "" {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "no query configured"}
	}
	base := strings.TrimRight(m.Target, "/")
	u := base + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad request: " + err.Error()}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer resp.Body.Close()
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
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
