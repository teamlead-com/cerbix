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

// TestPrepareCredentialSettingsMatrix is the §4.2 accept/reject table through
// the only exported prepare gate (normalization and validation are inseparable).
func TestPrepareCredentialSettingsMatrix(t *testing.T) {
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
			_, err := PrepareCredentialSettings(tc.typ, tc.s, tc.surface)
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

// TestPrepareCredentialSettingsNeverEchoesValues: error text must not leak submitted values.
func TestPrepareCredentialSettingsNeverEchoesValues(t *testing.T) {
	secretVal := "SuperSecret123"
	cases := []map[string]string{
		{"username": "u", "database": "d", "password": secretVal, "password_ref": "r1"}, // both → error
		{"username": "u", "database": "d", "password_ref": "r1", "sslmode": secretVal},  // bad sslmode value
	}
	for _, s := range cases {
		_, err := PrepareCredentialSettings(MonitorPostgres, s, SurfaceAPI)
		if err == nil {
			t.Fatalf("case %v must be rejected (a regression accepting it would silently skip the leak assertion)", s)
		}
		if strings.Contains(err.Error(), secretVal) {
			t.Fatalf("error must not echo submitted values: %v", err)
		}
	}
}

// TestPrepareCredentialSettings: canonical defaults materialize (§4.2/§4.8), implicit ==
// explicit (same effective map, hence same future hash), the input is never mutated, and
// empty runtime-default values canonicalize at the same boundary.
func TestPrepareCredentialSettings(t *testing.T) {
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
	got, err := PrepareCredentialSettings(MonitorPostgres, in, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare postgres: %v", err)
	}
	want := map[string]string{"username": "u", "database": "d", "password_ref": "r1", "sslmode": "require", "query": "SELECT 1"}
	assertEq(t, got, want)
	explicit, err := PrepareCredentialSettings(MonitorPostgres, want, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare explicit postgres: %v", err)
	}
	assertEq(t, explicit, want)
	if _, ok := in["sslmode"]; ok {
		t.Fatal("input map must not be mutated")
	}
	// explicit non-default wins
	ov, err := PrepareCredentialSettings(MonitorPostgres, map[string]string{"username": "u", "database": "d", "password_ref": "r1", "sslmode": "disable"}, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare postgres override: %v", err)
	}
	if ov["sslmode"] != "disable" {
		t.Fatal("explicit value must not be overridden by the default")
	}
	// mysql / redis tls default
	my, err := PrepareCredentialSettings(MonitorMySQL, map[string]string{"username": "u", "database": "d", "password_ref": "r1"}, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare mysql: %v", err)
	}
	if my["tls"] != "true" || my["query"] != "SELECT 1" {
		t.Fatalf("mysql defaults missing: %v", my)
	}
	rd, err := PrepareCredentialSettings(MonitorRedis, map[string]string{"password_ref": "r1"}, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare redis: %v", err)
	}
	if rd["tls"] != "true" {
		t.Fatalf("redis tls default missing: %v", rd)
	}
	// skip-verify with absent tls is safe ONLY because normalization sets tls=true first
	sv, err := PrepareCredentialSettings(MonitorRedis, map[string]string{"password_ref": "r1", "tls_skip_verify": "true"}, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare redis skip verify: %v", err)
	}
	if sv["tls"] != "true" {
		t.Fatalf("normalization must set tls before skip-verify is judged: %v", sv)
	}
	// rabbitmq management: tls + canonical path; amqp untouched
	rm, err := PrepareCredentialSettings(MonitorRabbitMQ, map[string]string{"mode": "management", "username": "u", "password_ref": "r1"}, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare rabbitmq management: %v", err)
	}
	if rm["tls"] != "true" || rm["path"] != "/api/overview" {
		t.Fatalf("rabbit management defaults missing: %v", rm)
	}
	ra, err := PrepareCredentialSettings(MonitorRabbitMQ, map[string]string{"mode": "amqp"}, SurfaceFile)
	if err != nil {
		t.Fatalf("prepare rabbitmq amqp: %v", err)
	}
	assertEq(t, ra, map[string]string{"mode": "amqp"})

	// Empty values that the historical probers default at runtime are canonicalized
	// here, before hashing/persistence, so a writer cannot preserve a second spelling.
	pgBlank, err := PrepareCredentialSettings(MonitorPostgres, map[string]string{
		"username": "u", "database": "d", "password_ref": "r1", "query": " \t ",
	}, SurfaceFile)
	if err != nil || pgBlank["query"] != "SELECT 1" {
		t.Fatalf("blank postgres query = %q, err %v", pgBlank["query"], err)
	}
	rmBlank, err := PrepareCredentialSettings(MonitorRabbitMQ, map[string]string{
		"mode": "management", "username": "u", "password_ref": "r1", "path": "  ",
	}, SurfaceFile)
	if err != nil || rmBlank["path"] != "/api/overview" {
		t.Fatalf("blank rabbitmq path = %q, err %v", rmBlank["path"], err)
	}
}

// TestCredentialRegistryClassifiesEveryField is the growth guard required by D-0160: the
// registry is the single source for validation, normalization, the expected credential
// field set and the execution-binding keys, so a key added without a binding class would
// silently land outside `body_digest` — the exact way a digest lags a new prober setting
// and reopens the hole it exists to close. The zero value is unclassified and fails here.
func TestCredentialRegistryClassifiesEveryField(t *testing.T) {
	for typ, schema := range credentialSchemas {
		for variantKey, variant := range schema.variants {
			if len(variant.fields) == 0 {
				t.Errorf("%s/%q: variant declares no fields", typ, variantKey)
			}
			if variant.requirement == CredentialInvalid {
				t.Errorf("%s/%q: variant declares no credential requirement", typ, variantKey)
			}
			seen := map[string]bool{}
			for _, f := range variant.fields {
				if f.binding == bindingUnclassified {
					t.Errorf("%s/%q: field %q has no binding class — classify it as execution, secret value or secret ref", typ, variantKey, f.key)
				}
				if f.key == "" {
					t.Errorf("%s/%q: field with an empty key", typ, variantKey)
				}
				if seen[f.key] {
					t.Errorf("%s/%q: field %q declared twice", typ, variantKey, f.key)
				}
				seen[f.key] = true
				if f.enum != nil && f.enumMsg == "" {
					t.Errorf("%s/%q: field %q has an enum but no error message", typ, variantKey, f.key)
				}
			}
			// A schema that requires a credential must declare exactly where it goes.
			hasSecret := false
			for _, f := range variant.fields {
				if f.binding == bindingSecretValue {
					hasSecret = true
				}
			}
			if variant.requirement == CredentialRequired && !hasSecret {
				t.Errorf("%s/%q: requires a credential but declares no secret value slot", typ, variantKey)
			}
			if variant.requirement == CredentialForbidden && hasSecret {
				t.Errorf("%s/%q: forbids credentials but declares a secret value slot", typ, variantKey)
			}
		}
	}
}

// The tri-state of §4.7 is resolved from the EFFECTIVE schema, never from a payload.
func TestResolveCredentialRequirement(t *testing.T) {
	cases := []struct {
		name string
		typ  MonitorType
		s    map[string]string
		want CredentialRequirement
	}{
		{"postgres requires", MonitorPostgres, nil, CredentialRequired},
		{"mysql requires", MonitorMySQL, nil, CredentialRequired},
		{"redis requires", MonitorRedis, nil, CredentialRequired},
		{"rabbit management requires", MonitorRabbitMQ, map[string]string{"mode": "management"}, CredentialRequired},
		{"rabbit amqp forbids", MonitorRabbitMQ, map[string]string{"mode": "amqp"}, CredentialForbidden},
		{"rabbit without mode is invalid", MonitorRabbitMQ, nil, CredentialInvalid},
		{"rabbit with bogus mode is invalid", MonitorRabbitMQ, map[string]string{"mode": "http"}, CredentialInvalid},
		{"non-credentialed type is invalid", MonitorHTTP, nil, CredentialInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCredentialRequirement(tc.typ, tc.s)
			if got != tc.want {
				t.Fatalf("requirement = %v, want %v (err=%v)", got, tc.want, err)
			}
			if tc.want == CredentialInvalid && err == nil {
				t.Fatal("invalid requirement returned without a reason")
			}
			if tc.want != CredentialInvalid && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// The expected envelope field set is exact, and empty where credentials are forbidden.
func TestExpectedCredentialFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  MonitorType
		s    map[string]string
		want []string
	}{
		{"postgres", MonitorPostgres, nil, []string{"password"}},
		{"mysql", MonitorMySQL, nil, []string{"password"}},
		{"redis", MonitorRedis, nil, []string{"password"}},
		{"rabbit management", MonitorRabbitMQ, map[string]string{"mode": "management"}, []string{"password"}},
		{"rabbit amqp", MonitorRabbitMQ, map[string]string{"mode": "amqp"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpectedCredentialFields(tc.typ, tc.s)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("expected fields = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := ExpectedCredentialFields(MonitorHTTP, nil); err == nil {
		t.Fatal("non-credentialed type returned a field set without an error")
	}
}

// The execution-binding keys are the non-secret half of the schema: they carry the
// credential's destination and transport, and they exclude both the value slot and the
// ref NAME (renaming a ref in an already-sealed job changes no remote behaviour).
func TestExecutionBindingKeysCoverNonSecretsOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  MonitorType
		s    map[string]string
		want []string
	}{
		{"postgres", MonitorPostgres, nil, []string{"database", "query", "sslmode", "username"}},
		{"mysql", MonitorMySQL, nil, []string{"database", "query", "tls", "tls_skip_verify", "username"}},
		{"redis", MonitorRedis, nil, []string{"tls", "tls_skip_verify", "username"}},
		{"rabbit management", MonitorRabbitMQ, map[string]string{"mode": "management"},
			[]string{"mode", "path", "tls", "tls_skip_verify", "username"}},
		{"rabbit amqp", MonitorRabbitMQ, map[string]string{"mode": "amqp"}, []string{"mode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExecutionBindingKeys(tc.typ, tc.s)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("binding keys = %v, want %v", got, tc.want)
			}
			for _, k := range got {
				if k == "password" || k == "password_ref" {
					t.Fatalf("binding keys must exclude the credential slot, got %v", got)
				}
			}
		})
	}
}

// Every key a type's validator accepts is classified by exactly one of the three roles,
// so the union of "expected credential fields" and "execution binding keys" plus the ref
// name accounts for the whole allowlist — no key can be validated yet unbound.
func TestEveryAllowedKeyIsAccountedForByABinding(t *testing.T) {
	for typ, schema := range credentialSchemas {
		for variantKey, variant := range schema.variants {
			settings := map[string]string{}
			if schema.discriminator != "" {
				settings[schema.discriminator] = variantKey
			}
			binding, err := ExecutionBindingKeys(typ, settings)
			if err != nil {
				t.Fatalf("%s/%q: %v", typ, variantKey, err)
			}
			secrets, err := ExpectedCredentialFields(typ, settings)
			if err != nil {
				t.Fatalf("%s/%q: %v", typ, variantKey, err)
			}
			accounted := map[string]bool{}
			for _, k := range append(append([]string{}, binding...), secrets...) {
				accounted[k] = true
			}
			for _, f := range variant.fields {
				if f.binding == bindingSecretRef {
					accounted[f.key] = true // deliberately excluded from the digest
				}
				if !accounted[f.key] {
					t.Errorf("%s/%q: key %q is validated but reachable through no binding", typ, variantKey, f.key)
				}
			}
		}
	}
}

// TestCredentialSlotContractIsSingleAndPaired makes the registry's actual contract explicit
// rather than letting the "adding a setting adds it everywhere by construction" claim run
// ahead of the code. `credentialSlot` validates exactly one slot named password/password_ref,
// so a future multi-field schema would pass the classification guard while validation and
// materialization stayed single-slot. This test states the limit and fails the moment a
// schema outgrows it — at which point the slot policy is what has to change, not this test.
func TestCredentialSlotContractIsSingleAndPaired(t *testing.T) {
	for typ, schema := range credentialSchemas {
		for variantKey, variant := range schema.variants {
			var values, refs []string
			for _, f := range variant.fields {
				switch f.binding {
				case bindingSecretValue:
					values = append(values, f.key)
				case bindingSecretRef:
					refs = append(refs, f.key)
				}
			}
			if variant.requirement != CredentialRequired {
				if len(values) != 0 || len(refs) != 0 {
					t.Errorf("%s/%q: a variant that requires no credential declares slots %v/%v", typ, variantKey, values, refs)
				}
				continue
			}
			if len(values) != 1 || len(refs) != 1 {
				t.Fatalf("%s/%q: registry declares %d value and %d ref slots; the credential-slot policy "+
					"(exactly-one-of, D-0152) is written for exactly one of each — extend that policy first",
					typ, variantKey, len(values), len(refs))
			}
			if refs[0] != values[0]+"_ref" {
				t.Errorf("%s/%q: slot pair is %q/%q, want %q/%q — the ref name must be derivable from the value name",
					typ, variantKey, values[0], refs[0], values[0], values[0]+"_ref")
			}
			// The slot policy is hard-coded to this pair; keep the two in step.
			if values[0] != "password" {
				t.Errorf("%s/%q: credentialSlot validates password/password_ref, but the registry declares %q",
					typ, variantKey, values[0])
			}
		}
	}
}
