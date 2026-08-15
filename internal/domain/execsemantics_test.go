package domain

import (
	"reflect"
	"strings"
	"testing"
)

// The guard the whole projection rests on: every field of Monitor has an explicit answer.
// A field added later without one lands on SemanticUnclassified and fails here, rather
// than defaulting into the epoch (churn on every rename) or silently out of it (a target
// change that produces no epoch, which is the failure this classification exists to
// prevent).
func TestMonitorFieldsAreExhaustivelyClassified(t *testing.T) {
	rt := reflect.TypeOf(Monitor{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if ClassifyMonitorField(name) == SemanticUnclassified {
			t.Errorf("Monitor.%s has no semantic classification.\n"+
				"Add it to monitorFieldClass in execsemantics.go: SemanticEvaluation if it changes what\n"+
				"endpoint or operation produced heartbeat.up (or when a missing observation goes stale),\n"+
				"otherwise the reason it is out.", name)
		}
	}
}

// The classification table must not name fields that no longer exist, or it stops being a
// description of the type and becomes folklore.
func TestClassificationHasNoStaleEntries(t *testing.T) {
	rt := reflect.TypeOf(Monitor{})
	live := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		live[rt.Field(i).Name] = true
	}
	for name := range monitorFieldClass {
		if !live[name] {
			t.Errorf("monitorFieldClass classifies %q, which is not a field of Monitor", name)
		}
	}
}

func baseMonitor() Monitor {
	return Monitor{
		ID: "m1", ProjectID: "p1", Name: "checkout http",
		Type: MonitorHTTP, Target: "https://checkout.example.com/healthz", Method: "GET",
		IntervalSeconds: 30, TimeoutSeconds: 5, Retries: 1,
		Region: "core", Enabled: true, Conditions: []string{"status==200"},
	}
}

func hashOf(t *testing.T, m Monitor, gens map[string]string) string {
	t.Helper()
	e, err := MonitorEvaluationSemantics(m, gens)
	if err != nil {
		t.Fatalf("MonitorEvaluationSemantics: %v", err)
	}
	return e.Hash()
}

// The case a narrower snapshot got wrong: a target change bumps execution_revision, and it
// MUST produce an epoch, because it makes two availability numbers incomparable.
func TestTargetChangeChangesTheHash(t *testing.T) {
	before := baseMonitor()
	after := before
	after.Target = "https://checkout.example.com/healthz2"
	if hashOf(t, before, nil) == hashOf(t, after, nil) {
		t.Fatal("a target change produced no new hash — the epoch would never be created")
	}
}

func TestConditionChangeChangesTheHash(t *testing.T) {
	before := baseMonitor()
	after := before
	after.Conditions = []string{"status==200", "body~=ok"}
	if hashOf(t, before, nil) == hashOf(t, after, nil) {
		t.Fatal("a condition change produced no new hash")
	}
}

// Conditions describe an ordered sequence of assertions; two orders are two different
// probes and must not collide.
func TestConditionOrderIsSignificant(t *testing.T) {
	a := baseMonitor()
	a.Conditions = []string{"status==200", "latency<500"}
	b := a
	b.Conditions = []string{"latency<500", "status==200"}
	if hashOf(t, a, nil) == hashOf(t, b, nil) {
		t.Fatal("reordered conditions hashed the same")
	}
}

func TestCredentialRotationChangesTheHash(t *testing.T) {
	m := baseMonitor()
	before := hashOf(t, m, map[string]string{"password_ref": "2026-08-01T00:00:00Z"})
	after := hashOf(t, m, map[string]string{"password_ref": "2026-08-16T09:00:00Z"})
	if before == after {
		t.Fatal("a rotated credential produced no new hash — a probe may now be authorized differently")
	}
}

// Rename, status churn and every other non-evaluation field must leave the hash alone:
// each of them bumps execution_revision under the coarse fence, and creating an epoch for
// each would make the timeline unreadable.
func TestNonEvaluationFieldsDoNotChangeTheHash(t *testing.T) {
	base := baseMonitor()
	want := hashOf(t, base, nil)

	cases := map[string]func(*Monitor){
		"Name":                   func(m *Monitor) { m.Name = "renamed" },
		"Tags":                   func(m *Monitor) { m.Tags = []string{"team-a"} },
		"FailureThreshold":       func(m *Monitor) { m.FailureThreshold = 5 },
		"ConfirmIntervalSeconds": func(m *Monitor) { m.ConfirmIntervalSeconds = 10 },
		"RenotifySeconds":        func(m *Monitor) { m.RenotifySeconds = 900 },
		"AutoIncident":           func(m *Monitor) { m.AutoIncident = true },
		"EscalationPolicyID":     func(m *Monitor) { m.EscalationPolicyID = "policy-1" },
		"DependsOn":              func(m *Monitor) { m.DependsOn = []string{"m2"} },
		"Status":                 func(m *Monitor) { m.Status = StatusDown },
		"ConsecutiveFailures":    func(m *Monitor) { m.ConsecutiveFailures = 3 },
		"ExecutionRevision":      func(m *Monitor) { m.ExecutionRevision = 99 },
		"StateSequence":          func(m *Monitor) { m.StateSequence = 42 },
		"PushToken":              func(m *Monitor) { m.PushToken = "cbxp_secret" },
	}
	for field, mutate := range cases {
		m := base
		mutate(&m)
		if got := hashOf(t, m, nil); got != want {
			t.Errorf("changing %s changed the evaluation-semantics hash; it would create an epoch for a change the evaluator never reads", field)
		}
	}
}

// Every evaluation field must move the hash. Asserting the positive direction matters as
// much as the negative one: a field classified IN but dropped from Canonical() would be
// silently absent, and no rename test would notice.
func TestEveryEvaluationFieldChangesTheHash(t *testing.T) {
	base := baseMonitor()
	want := hashOf(t, base, nil)

	cases := map[string]func(*Monitor){
		"Type":            func(m *Monitor) { m.Type = MonitorTCP },
		"Target":          func(m *Monitor) { m.Target = "https://other/" },
		"Method":          func(m *Monitor) { m.Method = "POST" },
		"Region":          func(m *Monitor) { m.Region = "geo1" },
		"IntervalSeconds": func(m *Monitor) { m.IntervalSeconds = 60 },
		"TimeoutSeconds":  func(m *Monitor) { m.TimeoutSeconds = 9 },
		"Retries":         func(m *Monitor) { m.Retries = 4 },
		"GraceSeconds":    func(m *Monitor) { m.GraceSeconds = 30 },
		"Enabled":         func(m *Monitor) { m.Enabled = false },
		"Conditions":      func(m *Monitor) { m.Conditions = []string{"status==204"} },
		"Config":          func(m *Monitor) { m.Config = map[string]string{"record": "AAAA"} },
	}
	for field, mutate := range cases {
		m := base
		mutate(&m)
		if got := hashOf(t, m, nil); got == want {
			t.Errorf("changing %s left the evaluation-semantics hash unchanged; the epoch would never be created", field)
		}
	}
}

// A monitor's secret VALUE never reaches the projection, and neither does its ciphertext.
// Only identity and generation do.
func TestSecretMaterialNeverEntersTheProjection(t *testing.T) {
	m := baseMonitor()
	m.Type = MonitorRedis
	m.Target = "redis://cache:6379"
	// The inline value is deliberately present: the projection must drop it by
	// construction rather than because callers happen not to pass one.
	m.Config = map[string]string{"password_ref": "cache-password", "password": "hunter2"}

	e, err := MonitorEvaluationSemantics(m, map[string]string{"password_ref": "gen-1"})
	if err != nil {
		t.Fatalf("MonitorEvaluationSemantics: %v", err)
	}
	if e.CredentialRefs["password_ref"] != "cache-password" {
		t.Errorf("the credential ref NAME is identity and belongs in the projection, got %+v", e.CredentialRefs)
	}
	for k, v := range e.Config {
		if k == "password" || v == "hunter2" {
			t.Fatalf("a secret value slot reached the projection: %s=%s", k, v)
		}
	}
	if strings.Contains(string(e.Canonical()), "hunter2") {
		t.Fatal("secret material reached the canonical encoding")
	}
}

// Length prefixing is what stops ("ab","c") and ("a","bc") from encoding identically. A
// concatenating encoder would let two different monitors share an epoch.
func TestCanonicalEncodingIsUnambiguous(t *testing.T) {
	a := baseMonitor()
	a.Target, a.Method = "ab", "c"
	b := baseMonitor()
	b.Target, b.Method = "a", "bc"
	if hashOf(t, a, nil) == hashOf(t, b, nil) {
		t.Fatal("field boundaries are ambiguous: two different monitors hash the same")
	}
}

func TestHashIsStableAcrossMapIteration(t *testing.T) {
	m := baseMonitor()
	m.Config = map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	first := hashOf(t, m, nil)
	for i := 0; i < 50; i++ {
		if got := hashOf(t, m, nil); got != first {
			t.Fatalf("hash is not stable across map iteration order: %s != %s", got, first)
		}
	}
}
