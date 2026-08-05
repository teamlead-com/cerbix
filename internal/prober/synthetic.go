package prober

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// syntheticProber runs a scripted multi-step HTTP scenario. Steps execute in order
// sharing a variable context (extracted values interpolated as {{var}}); the whole
// scenario runs within the monitor's timeout budget. It reports Connected=false with a
// step-scoped message on the first connection error, failed assert, or missing extract
// — messages name the step and the check, never the request/response bodies, so secrets
// in the scenario do not leak into the heartbeat.
type syntheticProber struct{ client *http.Client }

var substRe = regexp.MustCompile(`{{\s*([a-zA-Z0-9_]+)\s*}}`)

func subst(s string, vars map[string]string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return substRe.ReplaceAllStringFunc(s, func(m string) string {
		name := substRe.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

func stepLabel(i int, st domain.SyntheticStep) string {
	if st.Name != "" {
		return fmt.Sprintf("step %d (%s)", i+1, st.Name)
	}
	return fmt.Sprintf("step %d", i+1)
}

func (p syntheticProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	sc, err := domain.ParseScenario(m.Config)
	if err != nil {
		return Result{Connected: false, Msg: err.Error()}
	}
	vars := map[string]string{}
	lastCode := 0
	for i, st := range sc.Steps {
		method := st.Method
		if method == "" {
			method = http.MethodGet
		}
		var body io.Reader
		if b := subst(st.Body, vars); b != "" {
			body = strings.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, subst(st.URL, vars), body)
		if err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: stepLabel(i, st) + ": bad request"}
		}
		for k, v := range st.Headers {
			req.Header.Set(k, subst(v, vars))
		}
		stepStart := time.Now()
		resp, err := p.client.Do(req)
		if err != nil {
			return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: stepLabel(i, st) + ": " + err.Error()}
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		lastCode = resp.StatusCode
		latency := time.Since(stepStart).Milliseconds()

		for _, a := range st.Assert {
			if why := checkAssert(a, resp.StatusCode, string(respBody), resp.Header, latency); why != "" {
				return Result{Connected: false, Code: resp.StatusCode, LatencyMS: elapsedMS(start), Msg: stepLabel(i, st) + ": " + why}
			}
		}
		for _, e := range st.Extract {
			v, ok := extractValue(e, resp.StatusCode, string(respBody), resp.Header)
			if !ok {
				return Result{Connected: false, Code: resp.StatusCode, LatencyMS: elapsedMS(start), Msg: stepLabel(i, st) + ": could not extract " + e.Var}
			}
			vars[e.Var] = v
		}
	}
	return Result{Connected: true, Code: lastCode, LatencyMS: elapsedMS(start)}
}

// checkAssert returns "" if the assert holds, or a short (secret-free) reason if not.
func checkAssert(a domain.SyntheticAssert, status int, body string, header http.Header, latency int64) string {
	switch a.That {
	case "status":
		if !compareNum(float64(status), a.Op, a.Value) {
			return fmt.Sprintf("assert status %s %s (got %d)", opOrEq(a.Op), a.Value, status)
		}
	case "latency_ms":
		if !compareNum(float64(latency), a.Op, a.Value) {
			return fmt.Sprintf("assert latency_ms %s %s (got %d)", opOrEq(a.Op), a.Value, latency)
		}
	case "body_contains":
		if !strings.Contains(body, a.Value) {
			return "assert body_contains failed"
		}
	case "json":
		got, ok := jsonPath(body, a.Path)
		if !ok || !compareStr(got, a.Op, a.Value) {
			return fmt.Sprintf("assert json %s %s %s failed", a.Path, opOrEq(a.Op), a.Value)
		}
	}
	return ""
}

func extractValue(e domain.SyntheticExtract, status int, body string, header http.Header) (string, bool) {
	switch e.From {
	case "status":
		return strconv.Itoa(status), true
	case "header":
		v := header.Get(e.Path)
		return v, v != ""
	case "body":
		return body, true
	case "json":
		return jsonPath(body, e.Path)
	}
	return "", false
}

func opOrEq(op string) string {
	if op == "" {
		return "eq"
	}
	return op
}

// compareNum compares a numeric actual against want using op (default eq).
func compareNum(actual float64, op, want string) bool {
	w, err := strconv.ParseFloat(strings.TrimSpace(want), 64)
	if err != nil {
		return false
	}
	switch op {
	case "", "eq":
		return actual == w
	case "ne":
		return actual != w
	case "lt":
		return actual < w
	case "gt":
		return actual > w
	}
	return false
}

// compareStr compares a string actual against want using op (default eq).
func compareStr(actual, op, want string) bool {
	switch op {
	case "", "eq":
		return actual == want
	case "ne":
		return actual != want
	case "contains":
		return strings.Contains(actual, want)
	case "lt", "gt":
		return compareNum(atof(actual), op, want)
	}
	return false
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// jsonPath returns the value at a dot path (map keys and numeric slice indexes) in the
// JSON body, rendered as a string. ok=false if the body isn't JSON or the path misses.
func jsonPath(body, path string) (string, bool) {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return "", false
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			continue
		}
		switch cur := v.(type) {
		case map[string]any:
			nv, ok := cur[seg]
			if !ok {
				return "", false
			}
			v = nv
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(cur) {
				return "", false
			}
			v = cur[idx]
		default:
			return "", false
		}
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "", true
	default:
		b, _ := json.Marshal(t)
		return string(b), true
	}
}
