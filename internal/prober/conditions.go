package prober

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Values are the placeholder values a condition can reference, derived from a
// probe Result.
type Values struct {
	Status         int
	ResponseTimeMS int64
	Body           string
	Connected      bool
	CertExpiryDays int64
	Result         float64 // PromQL query value, for [RESULT]
}

// A condition is "[PLACEHOLDER] OP VALUE" (declarative). Supported placeholders:
// [STATUS], [RESPONSE_TIME] (ms), [CONNECTED] (1/0), [CERT_EXPIRY] (days, TLS), [RESULT] (PromQL value),
// [BODY]. Numeric operators:
// == != < <= > >= ; string operators (BODY): == != contains matches (regex).

// EvaluateAll reports whether every condition passes. On failure it returns the
// first failing condition text.
func EvaluateAll(conditions []string, v Values) (bool, string, error) {
	for _, c := range conditions {
		ok, err := evaluate(c, v)
		if err != nil {
			return false, c, err
		}
		if !ok {
			return false, c, nil
		}
	}
	return true, "", nil
}

func evaluate(cond string, v Values) (bool, error) {
	cond = strings.TrimSpace(cond)
	if !strings.HasPrefix(cond, "[") {
		return false, fmt.Errorf("condition must start with a [PLACEHOLDER]: %q", cond)
	}
	end := strings.Index(cond, "]")
	if end < 0 {
		return false, fmt.Errorf("unterminated placeholder: %q", cond)
	}
	name := cond[1:end]
	rest := strings.TrimSpace(cond[end+1:])
	op, valStr, err := splitOpValue(rest)
	if err != nil {
		return false, fmt.Errorf("%q: %w", cond, err)
	}

	switch name {
	case "STATUS":
		return compareInt(int64(v.Status), op, valStr)
	case "RESPONSE_TIME":
		return compareInt(v.ResponseTimeMS, op, valStr)
	case "CONNECTED":
		return compareInt(boolToInt(v.Connected), op, valStr)
	case "CERT_EXPIRY":
		return compareInt(v.CertExpiryDays, op, valStr)
	case "RESULT":
		return compareFloat(v.Result, op, valStr)
	case "BODY":
		return compareString(v.Body, op, valStr)
	default:
		return false, fmt.Errorf("unknown placeholder [%s]", name)
	}
}

func splitOpValue(s string) (op, val string, err error) {
	fields := strings.SplitN(s, " ", 2)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("expected 'OP VALUE', got %q", s)
	}
	op = fields[0]
	val = strings.TrimSpace(fields[1])
	val = strings.Trim(val, `"`)
	return op, val, nil
}

func compareFloat(actual float64, op, valStr string) (bool, error) {
	want, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return false, fmt.Errorf("expected numeric value, got %q", valStr)
	}
	switch op {
	case "==":
		return actual == want, nil
	case "!=":
		return actual != want, nil
	case "<":
		return actual < want, nil
	case "<=":
		return actual <= want, nil
	case ">":
		return actual > want, nil
	case ">=":
		return actual >= want, nil
	default:
		return false, fmt.Errorf("unsupported numeric operator %q", op)
	}
}

func compareInt(actual int64, op, valStr string) (bool, error) {
	want, err := strconv.ParseInt(valStr, 10, 64)
	if err != nil {
		// Allow true/false for CONNECTED-style comparisons.
		switch strings.ToLower(valStr) {
		case "true":
			want = 1
		case "false":
			want = 0
		default:
			return false, fmt.Errorf("expected integer value, got %q", valStr)
		}
	}
	switch op {
	case "==":
		return actual == want, nil
	case "!=":
		return actual != want, nil
	case "<":
		return actual < want, nil
	case "<=":
		return actual <= want, nil
	case ">":
		return actual > want, nil
	case ">=":
		return actual >= want, nil
	default:
		return false, fmt.Errorf("unsupported numeric operator %q", op)
	}
}

func compareString(actual, op, val string) (bool, error) {
	switch op {
	case "==":
		return actual == val, nil
	case "!=":
		return actual != val, nil
	case "contains":
		return strings.Contains(actual, val), nil
	case "matches":
		re, err := regexp.Compile(val)
		if err != nil {
			return false, fmt.Errorf("invalid regex %q: %w", val, err)
		}
		return re.MatchString(actual), nil
	default:
		return false, fmt.Errorf("unsupported string operator %q", op)
	}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
