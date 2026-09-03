package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Execution-side helpers for FR-029: the names the envelope uses, the idempotency key, and the
// bounds on a value the TARGET controls. They live in `domain` so the core and the executor cannot
// disagree about them — the same reason FR-028 derives its envelope field name from a function
// rather than from a convention two packages remember separately.

// CanaryBindingField is the envelope field that carries one binding's value, and the config key the
// dispatch gate injects it under for one execution. It is never persisted: the stored document holds
// a marker, and the gate's cleanup removes this key after the probe.
func CanaryBindingField(binding string) string { return canarySecretPrefix + binding }

// CanaryBindingFromField is the inverse, for the gate's injection and cleanup.
func CanaryBindingFromField(field string) (string, bool) {
	if !strings.HasPrefix(field, canarySecretPrefix) || strings.HasSuffix(field, canarySecretSuffix) {
		return "", false
	}
	name := strings.TrimPrefix(field, canarySecretPrefix)
	if !canaryBindingName.MatchString(name) {
		return "", false
	}
	return name, true
}

// CanaryRunKey is the config key carrying the SCHEDULED RUN this job belongs to. The scheduler sets
// it when it materializes the job (phase D); the executor derives the idempotency key from it.
//
// It is deliberately not the job id: a redelivered AMQP message, a re-claimed pull job after a lease
// expiry and a transport retry are all the SAME execution and must carry the same key, while the next
// scheduled run must carry a different one (D8).
const CanaryRunKey = "canary_run"

// CanaryRunKeyAt is the SCHEDULED WINDOW a run belongs to — floor(unix / interval) — and is the ONE
// place that formula lives.
//
// It used to be written inline in the credential materializer, which meant the PLAIN dispatch path
// never produced one: a canary with no bindings never reaches that materializer, so it claimed its
// in-flight slot with an empty run key, and a slot keyed by nothing cannot be released by key
// (reviewer P0-3). Both dispatch paths now stamp the run through this function.
func CanaryRunKeyAt(intervalSeconds int, at time.Time) string {
	if intervalSeconds <= 0 {
		return ""
	}
	return strconv.FormatInt(at.Unix()/int64(intervalSeconds), 10)
}

// CanaryIdempotencyKey derives the stable key. An empty run key yields an empty result, and the
// executor then sends NO `Idempotency-Key` header at all — an unstable key would be worse than none,
// because it would look like protection while creating a second external task on every retry.
//
// Half of this guarantee is the TARGET's, and the specification says so rather than implying
// otherwise: cerbix guarantees the same key for the same execution; whether a second submit with that
// key creates a second task is the target's contract.
func CanaryIdempotencyKey(monitorID string, executionRevision int64, runKey string) string {
	if strings.TrimSpace(runKey) == "" || strings.TrimSpace(monitorID) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cerbix-canary\x00" + monitorID + "\x00" +
		strconv.FormatInt(executionRevision, 10) + "\x00" + runKey))
	return hex.EncodeToString(sum[:16])
}

// ValidateCanaryCorrelationID bounds a value that came from the TARGET and is about to go into a URL
// this product then requests. Length, encoding and control characters are checked BEFORE it is used;
// the percent-encoding that keeps `/`, `?`, `#` and `@` from changing the request's shape happens at
// substitution (D4).
//
// The error text names the fault and never echoes the value: a validation message leaks exactly as a
// log line does.
func ValidateCanaryCorrelationID(raw string) error {
	if raw == "" {
		return fmt.Errorf("correlation id is empty")
	}
	if len(raw) > CanaryMaxCorrelationBytes {
		return fmt.Errorf("correlation id is longer than %d bytes", CanaryMaxCorrelationBytes)
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("correlation id is not valid UTF-8")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("correlation id contains a control character")
		}
	}
	return nil
}

// CanaryBindingsOf returns the binding names a stored config declares, sorted. The dispatch gate and
// the materializer both use it, so the set they build and the set the executor expects come from one
// place.
func CanaryBindingsOf(config map[string]string) []string {
	var out []string
	for _, key := range CanarySecretRefKeys(config) {
		binding, _ := CanaryBindingFromRefKey(key)
		out = append(out, binding)
	}
	return out
}
