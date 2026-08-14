package domain

import (
	"strings"
	"testing"
)

func TestCredentialedType(t *testing.T) {
	for _, typ := range []MonitorType{MonitorPostgres, MonitorMySQL, MonitorRedis, MonitorRabbitMQ} {
		if !CredentialedType(typ) {
			t.Fatalf("%s must be credentialed", typ)
		}
	}
	for _, typ := range []MonitorType{MonitorHTTP, MonitorTCP, MonitorPush, MonitorComposite} {
		if CredentialedType(typ) {
			t.Fatalf("%s must not be credentialed", typ)
		}
	}
}

// TestValidateCredentialSettingsMatrix is the §4.2 accept/reject table.
func TestValidateCredentialSettingsMatrix(t *testing.T) {
	pg := func(mut func(map[string]string)) map[string]string {
		m := map[string]string{"username": "monitor_ro", "database": "payments", "password_ref": "payments-db-ro"}
		mut(m)
		return m
	}
	cases := []struct {
		name    string
		typ     MonitorType
		s       map[string]string
		surface CredentialSurface
		wantErr string // "" = ok
	}{
		// postgres
		{"pg file minimal ok", MonitorPostgres, pg(func(m map[string]string) {}), SurfaceFile, ""},
		{"pg full ok", MonitorPostgres, pg(func(m map[string]string) { m["sslmode"] = "verify-full"; m["query"] = "SELECT 1" }), SurfaceFile, ""},
		{"pg bad sslmode", MonitorPostgres, pg(func(m map[string]string) { m["sslmode"] = "prefer" }), SurfaceFile, "sslmode"},
		{"pg missing username", MonitorPostgres, pg(func(m map[string]string) { delete(m, "username") }), SurfaceFile, "`username` is required"},
		{"pg unknown key", MonitorPostgres, pg(func(m map[string]string) { m["bogus"] = "x" }), SurfaceFile, "unknown key"},
		{"pg oversize query", MonitorPostgres, pg(func(m map[string]string) { m["query"] = strings.Repeat("a", 1025) }), SurfaceFile, "exceeds"},
		{"pg inline password in file", MonitorPostgres, pg(func(m map[string]string) { delete(m, "password_ref"); m["password"] = "x" }), SurfaceFile, "forbidden in bundles"},
		{"pg file requires ref", MonitorPostgres, pg(func(m map[string]string) { delete(m, "password_ref") }), SurfaceFile, "password_ref` is required"},
		{"pg bad ref slug", MonitorPostgres, pg(func(m map[string]string) { m["password_ref"] = "Bad_Ref" }), SurfaceFile, "secret name"},
		// API exactly-one-of
		{"pg api value ok", MonitorPostgres, pg(func(m map[string]string) { delete(m, "password_ref"); m["password"] = "x" }), SurfaceAPI, ""},
		{"pg api ref ok", MonitorPostgres, pg(func(m map[string]string) {}), SurfaceAPI, ""},
		{"pg api both", MonitorPostgres, pg(func(m map[string]string) { m["password"] = "x" }), SurfaceAPI, "exactly one"},
		{"pg api neither", MonitorPostgres, pg(func(m map[string]string) { delete(m, "password_ref") }), SurfaceAPI, "exactly one"},
		// mysql
		{"mysql ok tls default-absent", MonitorMySQL, map[string]string{"username": "u", "database": "d", "password_ref": "r1"}, SurfaceFile, ""},
		{"mysql skip-verify needs tls", MonitorMySQL, map[string]string{"username": "u", "database": "d", "password_ref": "r1", "tls": "false", "tls_skip_verify": "true"}, SurfaceFile, "requires tls"},
		{"mysql bad bool", MonitorMySQL, map[string]string{"username": "u", "database": "d", "password_ref": "r1", "tls": "yes"}, SurfaceFile, "must be"},
		// redis
		{"redis minimal ok", MonitorRedis, map[string]string{"password_ref": "r1"}, SurfaceFile, ""},
		{"redis with acl user ok", MonitorRedis, map[string]string{"username": "acl", "password_ref": "r1", "tls": "true"}, SurfaceFile, ""},
		{"redis no database key", MonitorRedis, map[string]string{"password_ref": "r1", "database": "0"}, SurfaceFile, "unknown key"},
		// rabbitmq conditional
		{"rabbit amqp mode only", MonitorRabbitMQ, map[string]string{"mode": "amqp"}, SurfaceFile, ""},
		{"rabbit amqp forbids creds", MonitorRabbitMQ, map[string]string{"mode": "amqp", "password_ref": "r1"}, SurfaceFile, "unknown key"},
		{"rabbit missing mode", MonitorRabbitMQ, map[string]string{"password_ref": "r1"}, SurfaceFile, "requires `mode`"},
		{"rabbit bad mode", MonitorRabbitMQ, map[string]string{"mode": "http"}, SurfaceFile, "amqp|management"},
		{"rabbit management ok", MonitorRabbitMQ, map[string]string{"mode": "management", "username": "u", "password_ref": "r1", "path": "/api/health"}, SurfaceFile, ""},
		{"rabbit management requires username", MonitorRabbitMQ, map[string]string{"mode": "management", "password_ref": "r1"}, SurfaceFile, "`username` is required"},
		{"rabbit oversize path", MonitorRabbitMQ, map[string]string{"mode": "management", "username": "u", "password_ref": "r1", "path": strings.Repeat("p", 513)}, SurfaceFile, "exceeds"},
		// non-credentialed type has no schema here
		{"http rejected", MonitorHTTP, map[string]string{}, SurfaceFile, "no credential settings schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCredentialSettings(tc.typ, tc.s, tc.surface)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidateCredentialSettingsNeverEchoesValues: error text must not leak submitted values.
func TestValidateCredentialSettingsNeverEchoesValues(t *testing.T) {
	secretVal := "SuperSecret123"
	cases := []map[string]string{
		{"username": "u", "database": "d", "password": secretVal, "password_ref": "r1"}, // both → error
		{"username": "u", "database": "d", "password_ref": "r1", "sslmode": secretVal},  // bad sslmode value
	}
	for _, s := range cases {
		err := ValidateCredentialSettings(MonitorPostgres, s, SurfaceAPI)
		if err == nil {
			t.Fatalf("case %v must be rejected (a regression accepting it would silently skip the leak assertion)", s)
		}
		if strings.Contains(err.Error(), secretVal) {
			t.Fatalf("error must not echo submitted values: %v", err)
		}
	}
}

// TestNormalizeCredentialSettings: canonical defaults materialize (§4.2/§4.8), implicit ==
// explicit (same effective map, hence same future hash), the input is never mutated, and
// normalized output validates.
func TestNormalizeCredentialSettings(t *testing.T) {
	assertEq := func(t *testing.T, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("map size %d != %d: got %v want %v", len(got), len(want), got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("key %q = %q, want %q (got %v)", k, got[k], v, got)
			}
		}
	}
	// postgres: implicit == explicit
	in := map[string]string{"username": "u", "database": "d", "password_ref": "r1"}
	got := NormalizeCredentialSettings(MonitorPostgres, in)
	want := map[string]string{"username": "u", "database": "d", "password_ref": "r1", "sslmode": "require", "query": "SELECT 1"}
	assertEq(t, got, want)
	explicit := NormalizeCredentialSettings(MonitorPostgres, want)
	assertEq(t, explicit, want)
	if _, ok := in["sslmode"]; ok {
		t.Fatal("input map must not be mutated")
	}
	if err := ValidateCredentialSettings(MonitorPostgres, got, SurfaceFile); err != nil {
		t.Fatalf("normalized postgres must validate: %v", err)
	}
	// explicit non-default wins
	ov := NormalizeCredentialSettings(MonitorPostgres, map[string]string{"username": "u", "database": "d", "password_ref": "r1", "sslmode": "disable"})
	if ov["sslmode"] != "disable" {
		t.Fatal("explicit value must not be overridden by the default")
	}
	// mysql / redis tls default
	my := NormalizeCredentialSettings(MonitorMySQL, map[string]string{"username": "u", "database": "d", "password_ref": "r1"})
	if my["tls"] != "true" || my["query"] != "SELECT 1" {
		t.Fatalf("mysql defaults missing: %v", my)
	}
	rd := NormalizeCredentialSettings(MonitorRedis, map[string]string{"password_ref": "r1"})
	if rd["tls"] != "true" {
		t.Fatalf("redis tls default missing: %v", rd)
	}
	// skip-verify with absent tls is safe ONLY because normalization sets tls=true first
	sv := NormalizeCredentialSettings(MonitorRedis, map[string]string{"password_ref": "r1", "tls_skip_verify": "true"})
	if sv["tls"] != "true" {
		t.Fatalf("normalization must set tls before skip-verify is judged: %v", sv)
	}
	if err := ValidateCredentialSettings(MonitorRedis, sv, SurfaceFile); err != nil {
		t.Fatalf("normalized skip-verify must validate: %v", err)
	}
	// rabbitmq management: tls + canonical path; amqp untouched
	rm := NormalizeCredentialSettings(MonitorRabbitMQ, map[string]string{"mode": "management", "username": "u", "password_ref": "r1"})
	if rm["tls"] != "true" || rm["path"] != "/api/overview" {
		t.Fatalf("rabbit management defaults missing: %v", rm)
	}
	if err := ValidateCredentialSettings(MonitorRabbitMQ, rm, SurfaceFile); err != nil {
		t.Fatalf("normalized rabbit management must validate: %v", err)
	}
	ra := NormalizeCredentialSettings(MonitorRabbitMQ, map[string]string{"mode": "amqp"})
	assertEq(t, ra, map[string]string{"mode": "amqp"})
}
