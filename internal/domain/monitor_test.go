package domain

import "testing"

func TestMonitorTypeValidAndActive(t *testing.T) {
	if !MonitorHTTP.Valid() || !MonitorTCP.Valid() || !MonitorPush.Valid() {
		t.Error("known types should be valid")
	}
	if MonitorType("gopher").Valid() {
		t.Error("unknown type should be invalid")
	}
	if !MonitorHTTP.Active() || !MonitorTCP.Active() {
		t.Error("http/tcp are active")
	}
	if MonitorPush.Active() {
		t.Error("push is passive")
	}
}

func TestMonitorValidate(t *testing.T) {
	ok := Monitor{Name: "api", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 10}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid monitor rejected: %v", err)
	}
	// Push needs no target, but its interval is the expected heartbeat window.
	push := Monitor{Name: "cron", ProjectID: "p", Type: MonitorPush, IntervalSeconds: 300}
	if err := push.Validate(); err != nil {
		t.Fatalf("valid push rejected: %v", err)
	}

	bad := []Monitor{
		{ProjectID: "p", Type: MonitorHTTP, Target: "x", IntervalSeconds: 1, TimeoutSeconds: 1},             // no name
		{Name: "n", Type: MonitorHTTP, Target: "x", IntervalSeconds: 1, TimeoutSeconds: 1},                  // no project
		{Name: "n", ProjectID: "p", Type: MonitorType("bad"), IntervalSeconds: 1, TimeoutSeconds: 1},        // bad type
		{Name: "n", ProjectID: "p", Type: MonitorHTTP, IntervalSeconds: 1, TimeoutSeconds: 1},               // no target
		{Name: "n", ProjectID: "p", Type: MonitorTCP, Target: "x:1", IntervalSeconds: 0, TimeoutSeconds: 1}, // interval 0
		{Name: "n", ProjectID: "p", Type: MonitorTCP, Target: "x:1", IntervalSeconds: 1, TimeoutSeconds: 0}, // timeout 0
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestStatusFor(t *testing.T) {
	if StatusFor(true) != StatusUp || StatusFor(false) != StatusDown {
		t.Fatal("StatusFor mapping wrong")
	}
}

func TestPushMonitorRequiresInterval(t *testing.T) {
	valid := Monitor{ProjectID: "p1", Name: "cron", Type: MonitorPush, IntervalSeconds: 60}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid push monitor rejected: %v", err)
	}
	noInterval := Monitor{ProjectID: "p1", Name: "cron", Type: MonitorPush}
	if err := noInterval.Validate(); err == nil {
		t.Fatal("push monitor without interval should fail validation")
	}
}

func TestICMPMonitorValidateAndActive(t *testing.T) {
	if !MonitorICMP.Active() {
		t.Fatal("icmp should be an active (probed) type")
	}
	ok := Monitor{Name: "ping", ProjectID: "p", Type: MonitorICMP, Target: "10.0.0.1", IntervalSeconds: 30, TimeoutSeconds: 5}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid icmp monitor rejected: %v", err)
	}
	noTarget := Monitor{Name: "ping", ProjectID: "p", Type: MonitorICMP, IntervalSeconds: 30, TimeoutSeconds: 5}
	if err := noTarget.Validate(); err == nil {
		t.Fatal("icmp monitor without target should fail")
	}
}

func TestMonitorMethodAndGrace(t *testing.T) {
	base := Monitor{Name: "x", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5}

	m := base
	m.Method = "TRACE"
	if err := m.Validate(); err == nil {
		t.Fatal("TRACE should be rejected")
	}
	m = base
	m.Method = "POST"
	if err := m.Validate(); err != nil {
		t.Fatalf("POST should be valid: %v", err)
	}
	// Normalize: http upper-cases and defaults to GET.
	m = base
	m.Method = "post"
	m.Normalize()
	if m.Method != "POST" {
		t.Fatalf("normalize post = %q, want POST", m.Method)
	}
	m = base
	m.Normalize()
	if m.Method != "GET" {
		t.Fatalf("normalize empty = %q, want GET", m.Method)
	}
	// Non-http clears method and grace.
	tcp := Monitor{Name: "x", ProjectID: "p", Type: MonitorTCP, Target: "h:1", IntervalSeconds: 60, TimeoutSeconds: 5, Method: "POST", GraceSeconds: 30}
	tcp.Normalize()
	if tcp.Method != "" || tcp.GraceSeconds != 0 {
		t.Fatalf("non-http normalize = %+v, want cleared", tcp)
	}
	// A push monitor's failure_threshold is pinned to 1: a dead-man timeout is a
	// single definitive signal, not confirmation-gated over N missed intervals.
	pushT := Monitor{Name: "x", ProjectID: "p", Type: MonitorPush, IntervalSeconds: 60, FailureThreshold: 5}
	pushT.Normalize()
	if pushT.FailureThreshold != 1 {
		t.Fatalf("push failure_threshold = %d, want pinned to 1", pushT.FailureThreshold)
	}
	// Negative grace is rejected.
	push := Monitor{Name: "x", ProjectID: "p", Type: MonitorPush, IntervalSeconds: 60, GraceSeconds: -1}
	if err := push.Validate(); err == nil {
		t.Fatal("negative grace should be rejected")
	}
}

func TestDNSAndTLSMonitorTypesActive(t *testing.T) {
	for _, tp := range []MonitorType{MonitorDNS, MonitorTLS} {
		if !tp.Valid() || !tp.Active() {
			t.Fatalf("%s should be a valid active type", tp)
		}
		m := Monitor{Name: "x", ProjectID: "p", Type: tp, Target: "example.com", IntervalSeconds: 60, TimeoutSeconds: 5}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s validate: %v", tp, err)
		}
		// Active types require a target.
		m.Target = ""
		if err := m.Validate(); err == nil {
			t.Fatalf("%s without target should be invalid", tp)
		}
	}
}

// TestCompositeQuorumValidation covers the quorum-mode invariants: M must be a
// number within [1, len(children)]; the mode whitelist includes "quorum".
func TestCompositeQuorumValidation(t *testing.T) {
	mk := func(mode, quorum string) Monitor {
		return Monitor{
			Name: "q", ProjectID: "p", Type: MonitorComposite, IntervalSeconds: 60,
			Config: map[string]string{"children": "a,b,c", "mode": mode, "quorum": quorum},
		}
	}
	for _, ok := range []Monitor{mk("quorum", "1"), mk("quorum", "2"), mk("quorum", "3")} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("valid quorum rejected: %v (%+v)", err, ok.Config)
		}
	}
	for _, bad := range []Monitor{mk("quorum", "0"), mk("quorum", "4"), mk("quorum", ""), mk("quorum", "x"), mk("majority", "2")} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid quorum accepted: %+v", bad.Config)
		}
	}
	if q := mk("quorum", "2").CompositeQuorum(); q != 2 {
		t.Fatalf("CompositeQuorum = %d, want 2", q)
	}
	if mode := mk("quorum", "2").CompositeMode(); mode != "quorum" {
		t.Fatalf("CompositeMode = %q", mode)
	}
}

func TestCompositeMonitorValidation(t *testing.T) {
	base := Monitor{Name: "c", ProjectID: "p", Type: MonitorComposite, IntervalSeconds: 60}
	// Needs children.
	if err := base.Validate(); err == nil {
		t.Fatal("composite without children should be invalid")
	}
	// No target required.
	ok := base
	ok.Config = map[string]string{"children": "a,b", "mode": "all"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid composite rejected: %v", err)
	}
	if ok.Type.NeedsTarget() {
		t.Fatal("composite should not need a target")
	}
	if !ok.Type.Active() {
		t.Fatal("composite must be Active (scheduled)")
	}
	// Bad mode.
	bad := ok
	bad.Config = map[string]string{"children": "a", "mode": "most"}
	if err := bad.Validate(); err == nil {
		t.Fatal("composite bad mode should be invalid")
	}
	// Helpers.
	if got := ok.ChildIDs(); len(got) != 2 || got[0] != "a" {
		t.Fatalf("ChildIDs = %v", got)
	}
	if ok.CompositeMode() != "all" {
		t.Fatalf("mode = %q", ok.CompositeMode())
	}
}

func TestPostgresTypeAndRedaction(t *testing.T) {
	if !MonitorPostgres.Valid() || !MonitorPostgres.Active() || !MonitorPostgres.NeedsTarget() {
		t.Fatal("postgres should be a valid, active, target-needing type")
	}
	m := Monitor{Name: "db", ProjectID: "p", Type: MonitorPostgres, Target: "db.internal:5432", IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"username": "cerbix", "password": "s3cr3t", "database": "app"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("valid postgres rejected: %v", err)
	}
	red := m.Redacted()
	if red.Config["password"] != "" {
		t.Fatalf("password not redacted: %q", red.Config["password"])
	}
	if red.Config["username"] != "cerbix" || red.Config["database"] != "app" {
		t.Fatalf("non-secret config should survive: %+v", red.Config)
	}
	// The original is untouched (Redacted copies).
	if m.Config["password"] != "s3cr3t" {
		t.Fatal("Redacted mutated the original")
	}
}

func TestRabbitMQWebSocketSSHTypes(t *testing.T) {
	for _, tp := range []MonitorType{MonitorRabbitMQ, MonitorWebSocket, MonitorSSH} {
		if !tp.Valid() || !tp.Active() || !tp.NeedsTarget() {
			t.Fatalf("%s should be valid, active, and target-needing", tp)
		}
		m := Monitor{Name: "x", ProjectID: "p", Type: tp, Target: "host:1", IntervalSeconds: 60, TimeoutSeconds: 5}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s validate: %v", tp, err)
		}
		m.Target = ""
		if err := m.Validate(); err == nil {
			t.Fatalf("%s without target should be invalid", tp)
		}
	}
}

func TestMySQLRedisPromQLTypes(t *testing.T) {
	for _, tp := range []MonitorType{MonitorMySQL, MonitorRedis, MonitorPromQL} {
		if !tp.Valid() || !tp.Active() || !tp.NeedsTarget() {
			t.Fatalf("%s should be valid, active, and target-needing", tp)
		}
		m := Monitor{Name: "x", ProjectID: "p", Type: tp, Target: "host:1", IntervalSeconds: 60, TimeoutSeconds: 5}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s validate: %v", tp, err)
		}
		m.Target = ""
		if err := m.Validate(); err == nil {
			t.Fatalf("%s without target should be invalid", tp)
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	m := Monitor{Name: "x", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5,
		Tags: []string{"  env:prod ", "env:prod", "ENV:PROD", "", "team:pay", "   "}}
	m.Normalize()
	// trimmed, case-insensitive dedupe (keeps first spelling), empties dropped.
	if len(m.Tags) != 2 || m.Tags[0] != "env:prod" || m.Tags[1] != "team:pay" {
		t.Fatalf("normalized tags = %#v, want [env:prod team:pay]", m.Tags)
	}
	// nil → empty slice (never nil, for a stable JSON [] and NOT NULL column).
	empty := Monitor{Name: "x", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5}
	empty.Normalize()
	if empty.Tags == nil || len(empty.Tags) != 0 {
		t.Fatalf("nil tags should normalize to [], got %#v", empty.Tags)
	}
}

func TestMonitorRegionNormalizeAndValidate(t *testing.T) {
	// Normalize: trim/lowercase; empty → core.
	m := Monitor{Name: "h", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5, Region: "  GEO1 "}
	m.Normalize()
	if m.Region != "geo1" {
		t.Fatalf("normalize region = %q, want geo1", m.Region)
	}
	var empty Monitor = Monitor{Name: "h", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5}
	empty.Normalize()
	if empty.Region != DefaultRegion {
		t.Fatalf("empty region default = %q, want %q", empty.Region, DefaultRegion)
	}
	// Validate: good slug ok, bad slug rejected.
	if err := (Monitor{Name: "h", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5, Region: "geo1"}).Validate(); err != nil {
		t.Fatalf("valid region rejected: %v", err)
	}
	if err := (Monitor{Name: "h", ProjectID: "p", Type: MonitorHTTP, Target: "https://x", IntervalSeconds: 60, TimeoutSeconds: 5, Region: "Geo 1!"}).Validate(); err == nil {
		t.Fatal("invalid region slug should be rejected")
	}
	// Composite pinned to core: non-core rejected by Validate, forced by Normalize.
	comp := Monitor{Name: "c", ProjectID: "p", Type: MonitorComposite, IntervalSeconds: 60, Config: map[string]string{"children": "m1"}, Region: "geo1"}
	if err := comp.Validate(); err == nil {
		t.Fatal("composite in a non-core region should be rejected")
	}
	comp.Normalize()
	if comp.Region != DefaultRegion {
		t.Fatalf("composite normalize should pin to core, got %q", comp.Region)
	}
}
