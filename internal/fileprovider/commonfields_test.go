package fileprovider

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// G5 — every declared bundle type carries every field that is not type-specific.
//
// The provider has a builder per type behind a per-field contract, kept in step by hand, and it was
// not: `buildCanaryMonitor` never copied `description`. The raw monitor accepts the key, so strict
// field checking passed it; the canonical hash folds a value that was always empty for that type, so
// declaring a description did nothing and every later edit of it was a permanent no-op the plan
// reported as "no change" (D1).
//
// The test iterates `fileSupportedTypes` rather than naming the types, which is the point: a type
// added without a fixture here fails, and a builder that stops calling `applyCommonMonitorFields`
// fails for every type at once. One more per-type case would have found the description; this finds
// the next field too.
//
// The mutation that must kill this: drop any line from `applyCommonMonitorFields`.

// typeFixtures is the type-SPECIFIC half of a minimal bundle: what each type needs beyond the
// common fields to be valid at all. The set is compared against `fileSupportedTypes` below, so a
// new type stops this test until someone writes its line — which is the intent, not an
// inconvenience.
var typeFixtures = map[domain.MonitorType]string{
	domain.MonitorHTTP:        "    target: https://example.com/health\n",
	domain.MonitorTCP:         "    target: db.example.com:5432\n",
	domain.MonitorICMP:        "    target: 10.0.0.1\n",
	domain.MonitorDNS:         "    target: example.com\n",
	domain.MonitorTLS:         "    target: example.com:443\n",
	domain.MonitorGRPC:        "    target: grpc.example.com:443\n",
	domain.MonitorWebSocket:   "    target: wss://example.com/ws\n",
	domain.MonitorSSH:         "    target: ssh.example.com:22\n",
	domain.MonitorPush:        "",
	domain.MonitorPostgres:    "    target: pg.example.com:5432\n    settings:\n      username: cerbix\n      database: app\n      password_ref: pg\n",
	domain.MonitorMySQL:       "    target: my.example.com:3306\n    settings:\n      username: cerbix\n      database: app\n      password_ref: my\n",
	domain.MonitorRedis:       "    target: redis.example.com:6379\n    settings:\n      password_ref: redis\n",
	domain.MonitorRabbitMQ:    "    target: rabbit.example.com:15672\n    settings:\n      mode: amqp\n",
	domain.MonitorPromQL:      "    target: http://prom.example.com:9090\n    settings:\n      query: up\n",
	domain.MonitorAsyncCanary: "    workflow:\n      kind: async_transaction_v1\n" + canaryPollBody,
}

func TestTheTypeFixtureTableCoversExactlyTheSupportedTypes(t *testing.T) {
	var have, want []string
	for typ := range typeFixtures {
		have = append(have, string(typ))
	}
	for typ, ok := range fileSupportedTypes {
		if ok {
			want = append(want, string(typ))
		}
	}
	sort.Strings(have)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Fatalf("the fixture table covers %v, the provider supports %v — a type with no fixture is "+
			"a type this guard says nothing about", have, want)
	}
}

func TestEveryDeclaredTypeCarriesEveryCommonField(t *testing.T) {
	// Distinctive values, so a field that is silently dropped reads as a zero rather than as the
	// default that happens to equal it.
	const common = "    description: the journey a customer actually makes\n" +
		"    interval: 7m\n" +
		"    timeout: 4m\n" +
		"    confirm_interval: 90s\n" +
		"    renotify: 30m\n" +
		"    grace: 2m\n" +
		"    region: geo-frankfurt\n" +
		"    enabled: false\n" +
		"    auto_incident: false\n" +
		"    tags: [alpha, beta]\n" +
		"    retries: 0\n" +
		"    failure_threshold: 4\n"

	for typ := range typeFixtures {
		t.Run(string(typ), func(t *testing.T) {
			body := "format: 2\norganization: acme\nproject: api\nmonitors:\n  subject:\n" +
				"    name: Subject\n    type: " + string(typ) + "\n" + common + typeFixtures[typ]
			proj, err := Decode([]byte(body), config.ProviderScopeConfig{Type: config.ProviderScopeInstance})
			if err != nil {
				t.Fatalf("the fixture for %s does not decode: %v", typ, err)
			}
			m := proj.Monitors["subject"].Monitor

			if m.Description != "the journey a customer actually makes" {
				t.Errorf("description = %q — the builder for this type does not copy it, so a "+
					"declared description never reaches the monitor and every edit of it is a "+
					"permanent no-op", m.Description)
			}
			if m.IntervalSeconds != 420 {
				t.Errorf("interval_seconds = %d, want 420", m.IntervalSeconds)
			}
			if m.TimeoutSeconds != 240 {
				t.Errorf("timeout_seconds = %d, want 240", m.TimeoutSeconds)
			}
			// A push monitor has no active probing to accelerate, so the domain clears the confirm
			// interval for it. Another domain rule, asserted as one.
			wantConfirm := 90
			if typ == domain.MonitorPush {
				wantConfirm = 0
			}
			if m.ConfirmIntervalSeconds != wantConfirm {
				t.Errorf("confirm_interval_seconds = %d, want %d", m.ConfirmIntervalSeconds, wantConfirm)
			}
			if m.RenotifySeconds != 1800 {
				t.Errorf("renotify_seconds = %d, want 1800", m.RenotifySeconds)
			}
			// `grace` is copied by the shared step and then normalized away for every type but
			// push, which is a DOMAIN rule and not a builder omission — so the assertion follows
			// the rule rather than asserting a value the domain deliberately clears.
			wantGrace := 0
			if typ == domain.MonitorPush {
				wantGrace = 120
			}
			if m.GraceSeconds != wantGrace {
				t.Errorf("grace_seconds = %d, want %d", m.GraceSeconds, wantGrace)
			}
			if m.Region != "geo-frankfurt" {
				t.Errorf("region = %q, want geo-frankfurt", m.Region)
			}
			if m.Enabled {
				t.Error("enabled = true; the bundle said false")
			}
			if m.AutoIncident {
				t.Error("auto_incident = true; the bundle said false")
			}
			if strings.Join(m.Tags, ",") != "alpha,beta" {
				t.Errorf("tags = %v, want [alpha beta]", m.Tags)
			}
			if m.Retries != 0 {
				t.Errorf("retries = %d, want 0", m.Retries)
			}
			// And a push timeout is a single definitive signal, so its threshold is pinned to 1.
			wantThreshold := 4
			if typ == domain.MonitorPush {
				wantThreshold = 1
			}
			if m.FailureThreshold != wantThreshold {
				t.Errorf("failure_threshold = %d, want %d", m.FailureThreshold, wantThreshold)
			}
			if m.Name != "Subject" {
				t.Errorf("name = %q", m.Name)
			}
		})
	}
}

// The list this guard iterates must describe what the shared step actually assigns. A field added to
// `applyCommonMonitorFields` and not to `commonMonitorFields` would be unguarded; one listed and not
// assigned would make the list a claim about nothing.
func TestTheCommonFieldListMatchesWhatTheSharedStepAssigns(t *testing.T) {
	var m domain.Monitor
	if err := applyCommonMonitorFields("uid", rawMonitor{Name: "n"}, &m); err != nil {
		t.Fatalf("the shared step refused a minimal monitor: %v", err)
	}
	rt := reflect.TypeOf(m)
	for _, field := range commonMonitorFields {
		if _, ok := rt.FieldByName(field); !ok {
			t.Errorf("commonMonitorFields names %q, which domain.Monitor does not have", field)
		}
	}
	if len(commonMonitorFields) == 0 {
		t.Fatal("the list is empty, so it guards nothing")
	}
}
