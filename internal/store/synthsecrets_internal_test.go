package store

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

const scenarioWithToken = `{"steps":[{"url":"https://api.internal/login","headers":{"authorization":"Bearer s3cr3t-bearer"}}]}`

// FR-028 stage 1. The scenario is ciphertext at rest, a WRITER reads it back, a SAFE reader
// never sees it and never decrypts it, and the execution reader — the one that builds a job —
// gets it so a synthetic probe still runs. The last of those is the case that would have
// broken the product if `scenario` had simply joined the old single secret set.
func TestScenarioIsEncryptedAtRestAndReadPerMode(t *testing.T) {
	st, ctx := outboxTestStore(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.New(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st.WithCipher(cipher)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "checkout journey", Type: domain.MonitorSynthetic,
		IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true,
		Config: map[string]string{domain.SyntheticScenarioKey: scenarioWithToken},
	})
	if err != nil {
		t.Fatalf("create synthetic monitor: %v", err)
	}

	var raw string
	if err := st.pool.QueryRow(ctx, `SELECT config::text FROM monitors WHERE id = $1`, m.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if strings.Contains(raw, "s3cr3t-bearer") {
		t.Fatalf("the scenario is stored in cleartext: %s", raw)
	}
	if !strings.Contains(raw, "enc:v1:") {
		t.Fatalf("the scenario is not encrypted at rest: %s", raw)
	}

	safe, err := st.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatalf("safe read: %v", err)
	}
	if _, ok := safe.Config[domain.SyntheticScenarioKey]; ok {
		t.Fatal("the SAFE reader returned the scenario: a viewer's list is built from this")
	}

	writer, err := st.GetMonitorForWriter(ctx, m.ID)
	if err != nil {
		t.Fatalf("writer read: %v", err)
	}
	if writer.Config[domain.SyntheticScenarioKey] != scenarioWithToken {
		t.Fatalf("the writer must read the scenario back verbatim, got %q", writer.Config[domain.SyntheticScenarioKey])
	}

	// The execution path: what a job carries. Without this the probe would run with no
	// scenario and every synthetic monitor would fail — the trap the review caught.
	execs, err := st.MaterializeExecutionConfigs(ctx, []string{m.ID}, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("materialized %d executions, want 1", len(execs))
	}
	if got := execs[0].Job.Monitor.Config[domain.SyntheticScenarioKey]; got != scenarioWithToken {
		t.Fatalf("the job must carry the scenario, got %q (reason %q)", got, execs[0].Reason)
	}
}

// A partial update that does not carry the scenario keeps the stored one, and it is carried
// as ciphertext — this path never decrypts (FR-028 invariant 6).
func TestScenarioSurvivesAPartialUpdate(t *testing.T) {
	st, ctx := outboxTestStore(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, _ := secret.New(key)
	st.WithCipher(cipher)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "journey", Type: domain.MonitorSynthetic,
		IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true,
		Config: map[string]string{domain.SyntheticScenarioKey: scenarioWithToken},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A rename that submits a config WITHOUT the scenario, exactly as a safe-read client would.
	m.Name = "journey renamed"
	m.Config = map[string]string{}
	if _, err := st.UpdateMonitor(ctx, m); err != nil {
		t.Fatalf("partial update: %v", err)
	}
	back, err := st.GetMonitorForWriter(ctx, m.ID)
	if err != nil {
		t.Fatalf("writer read: %v", err)
	}
	if back.Config[domain.SyntheticScenarioKey] != scenarioWithToken {
		t.Fatalf("the partial update wiped the scenario: %q", back.Config[domain.SyntheticScenarioKey])
	}
}

// §10 rule 2: with no key configured, a scenario write is REFUSED — one monitor's write
// fails and nothing else is touched. Readiness is not involved anywhere in this test, which
// is the point: a service must not go down for an unset variable.
func TestScenarioWriteRefusedWithoutAnAtRestKey(t *testing.T) {
	st, ctx := outboxTestStore(t) // no WithCipher: this is a keyless instance
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	_, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "journey", Type: domain.MonitorSynthetic,
		IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true,
		Config: map[string]string{domain.SyntheticScenarioKey: scenarioWithToken},
	})
	if !errors.Is(err, ErrNoAtRestKey) {
		t.Fatalf("create = %v, want ErrNoAtRestKey", err)
	}

	// Every other monitor type still writes: the refusal is scoped to what it protects.
	if _, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "plain", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("an ordinary monitor must still be writable on a keyless instance: %v", err)
	}
}

// The backfill converts a legacy plaintext scenario, is idempotent, and — the property §10
// turns on — reports rather than fails.
func TestBackfillMonitorConfigEncIsIdempotent(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	// Write the legacy shape directly: a monitor created before stage 1 existed.
	var id string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO monitors (project_id, name, slug, type, interval_seconds, timeout_seconds, enabled, config)
		 VALUES ($1,'legacy','legacy','synthetic',60,30,true,$2::jsonb) RETURNING id::text`,
		proj.ID, `{"scenario":`+jsonQuote(scenarioWithToken)+`}`).Scan(&id); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Without a cipher the backfill is a no-op rather than an error.
	if n, err := st.BackfillMonitorConfigEnc(ctx); err != nil || n != 0 {
		t.Fatalf("keyless backfill = (%d, %v), want (0, nil)", n, err)
	}

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, _ := secret.New(key)
	st.WithCipher(cipher)

	n, err := st.BackfillMonitorConfigEnc(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("converted %d rows, want 1", n)
	}
	var raw string
	_ = st.pool.QueryRow(ctx, `SELECT config::text FROM monitors WHERE id = $1`, id).Scan(&raw)
	if strings.Contains(raw, "s3cr3t-bearer") || !strings.Contains(raw, "enc:v1:") {
		t.Fatalf("the legacy scenario was not converted: %s", raw)
	}
	if again, err := st.BackfillMonitorConfigEnc(ctx); err != nil || again != 0 {
		t.Fatalf("second pass = (%d, %v), want (0, nil): the backfill must be idempotent", again, err)
	}
	// And it is still readable through the writer reader after conversion.
	back, err := st.GetMonitorForWriter(ctx, id)
	if err != nil {
		t.Fatalf("writer read after backfill: %v", err)
	}
	if back.Config[domain.SyntheticScenarioKey] != scenarioWithToken {
		t.Fatalf("the converted scenario reads back as %q", back.Config[domain.SyntheticScenarioKey])
	}
}

// jsonQuote renders a Go string as a JSON string literal, so the seeded legacy row carries
// the same bytes a real one would.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// monitorRefSettings is the single source of the ref keys that rename, rotation, deletion
// and materialization all act on. A `scenario_secret_*` key means "scenario binding" and
// only a synthetic monitor has a scenario, so on any other type it must contribute nothing
// — otherwise the key writes a monitor_secret_refs row for a monitor that can never consume
// it, and dispatch then refuses a monitor the write surface accepted.
func TestScenarioRefsAreContributedBySyntheticMonitorsOnly(t *testing.T) {
	cfg := map[string]string{
		domain.ScenarioSecretRefKey("login"): "login-token",
		"password_ref":                       "db-password",
	}
	synthetic := monitorRefSettings(domain.Monitor{Type: domain.MonitorSynthetic, Config: cfg})
	if synthetic[domain.ScenarioSecretRefKey("login")] != "login-token" {
		t.Fatalf("a synthetic monitor must contribute its binding refs, got %v", synthetic)
	}
	http := monitorRefSettings(domain.Monitor{Type: domain.MonitorHTTP, Config: cfg})
	if _, ok := http[domain.ScenarioSecretRefKey("login")]; ok {
		t.Fatalf("an http monitor must contribute no scenario refs, got %v", http)
	}
	// The credentialed half is untouched: postgres still contributes password_ref.
	pg := monitorRefSettings(domain.Monitor{Type: domain.MonitorPostgres, Config: cfg})
	if pg["password_ref"] != "db-password" {
		t.Fatalf("password_ref must still be contributed, got %v", pg)
	}
	if _, ok := pg[domain.ScenarioSecretRefKey("login")]; ok {
		t.Fatalf("postgres must contribute no scenario refs, got %v", pg)
	}
}

// The architectural claim of stage 2, tested rather than argued: because the inventory NAME
// sits in an ordinary flat config key, a scenario binding rides the paths `password_ref`
// already rides — delete counting, rename repointing and rotation — with no scenario-aware
// code anywhere. The review's BLOCKER 1 demanded a scenario-aware repoint for the scoped-key
// design; this test is what says it is not needed, and it would fail the moment the ref
// moved back inside the JSON.
func TestScenarioBindingRidesTheOrdinaryRefPath(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")
	if _, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "login-token", "v1"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	scenario := `{"steps":[{"url":"https://api.internal/login","headers":{"authorization":"{{secret:login}}"}}]}`
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: "checkout journey", Type: domain.MonitorSynthetic,
		IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true,
		Config: map[string]string{
			domain.SyntheticScenarioKey:          scenario,
			domain.ScenarioSecretRefKey("login"): "login-token",
		},
	})
	if err != nil {
		t.Fatalf("create synthetic monitor with a binding: %v", err)
	}

	// Delete counting sees the binding: the same tenant-safe guard that protects password_ref.
	var inUse SecretInUseError
	if err := st.DeleteProjectSecret(ctx, testSecretActor, projID, "login-token"); !errors.As(err, &inUse) || inUse.Count != 1 {
		t.Fatalf("delete of a bound secret = %v, want SecretInUseError{Count: 1}", err)
	}

	// Rename repoints the flat key, and the scenario is not touched at all — the placeholder
	// names the BINDING, which a rename does not change.
	newName := "login-token-renamed"
	_, _, repointed, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, "login-token", &newName, nil)
	if err != nil || repointed != 1 {
		t.Fatalf("rename = (repointed %d, %v), want (1, nil)", repointed, err)
	}
	got, err := st.GetMonitorForWriter(ctx, m.ID)
	if err != nil {
		t.Fatalf("writer read: %v", err)
	}
	if got.Config[domain.ScenarioSecretRefKey("login")] != newName {
		t.Fatalf("binding ref = %q, want re-pointed to %q", got.Config[domain.ScenarioSecretRefKey("login")], newName)
	}
	if got.Config[domain.SyntheticScenarioKey] != scenario {
		t.Fatalf("the rename rewrote the scenario:\n got %s\nwant %s", got.Config[domain.SyntheticScenarioKey], scenario)
	}

	// Rotation of the value needs no monitor edit: the monitor references a NAME.
	value := "v2"
	_, rotated, _, err := st.UpdateProjectSecret(ctx, testSecretActor, projID, newName, nil, &value)
	if err != nil || !rotated {
		t.Fatalf("rotate = (%v, %v), want (true, nil)", rotated, err)
	}
	after, err := st.GetMonitorForWriter(ctx, m.ID)
	if err != nil {
		t.Fatalf("writer read after rotation: %v", err)
	}
	if after.Config[domain.ScenarioSecretRefKey("login")] != newName || after.Config[domain.SyntheticScenarioKey] != scenario {
		t.Fatalf("rotation changed the monitor: %+v", after.Config)
	}
}

// The row that CANNOT exist, seeded anyway (party round 5). No released build ever accepted a
// `scenario_secret_*` key on a non-synthetic monitor — the code that did lives only in unpushed
// commits — so this state is unreachable through any write path. The review was still right that
// calling such a row "inert" was inaccurate: the type gates stop it from reaching MATERIALIZATION,
// and they do not reach backwards into a row that already exists. This test seeds one with raw SQL,
// exactly as a pre-fix write would have left it, and states what actually happens to it, so the
// claim is a description and not a hope.
func TestAPreFixNonSyntheticBindingRowBehavesAsDocumented(t *testing.T) {
	st, ctx := secretsTestStore(t)
	_, projID := secretsFixture(t, st, ctx, "acme", "api")
	sec, err := st.CreateProjectSecret(ctx, testSecretActor, projID, "login-token", "v1")
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: projID, Name: "api", Type: domain.MonitorHTTP, Target: "https://api.internal/health",
		IntervalSeconds: 60, TimeoutSeconds: 10, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create http monitor: %v", err)
	}
	key := domain.ScenarioSecretRefKey("login")
	if _, err := st.pool.Exec(ctx,
		`UPDATE monitors SET config = config || jsonb_build_object($2::text, 'login-token') WHERE id = $1`,
		m.ID, key); err != nil {
		t.Fatalf("seed the config key: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO monitor_secret_refs (monitor_id, project_id, setting_key, secret_id) VALUES ($1, $2, $3, $4)`,
		m.ID, projID, key, sec.ID); err != nil {
		t.Fatalf("seed the ref row: %v", err)
	}

	// 1. The monitor KEEPS PROBING. This is the property that matters: the fix must not turn a
	//    stale row into an outage, which is the whole reason the type gate lives in materialization
	//    as well as at the write boundary.
	execs, err := st.MaterializeExecutionConfigs(ctx, []string{m.ID}, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(execs) != 1 || execs[0].Reason != "" {
		t.Fatalf("a seeded row must not stop the monitor: %+v", execs)
	}
	if execs[0].Job.CredentialEnvelope != nil {
		t.Fatal("the seeded row must not produce a credential envelope")
	}

	// 2. The ref row still COUNTS against deleting the secret. It is a real row; nothing in the
	//    fix rewrites history, and the operator sees the secret as in use.
	var inUse SecretInUseError
	if err := st.DeleteProjectSecret(ctx, testSecretActor, projID, "login-token"); !errors.As(err, &inUse) || inUse.Count != 1 {
		t.Fatalf("delete = %v, want SecretInUseError{Count: 1} — the seeded row is real", err)
	}

	// 3. The store itself does NOT refuse the edit — domain validation is the write BOUNDARY the
	//    API and the file provider call, not something `UpdateMonitor` repeats. The refusal an
	//    operator actually meets is asserted where it lives, in
	//    `api.TestAPreFixNonSyntheticBindingBlocksEditsUntilTheKeyIsDropped`. Written down here
	//    because I assumed the opposite when I first wrote this test and the test said otherwise.
	edit, err := st.GetMonitorForWriter(ctx, m.ID)
	if err != nil {
		t.Fatalf("writer read: %v", err)
	}
	if edit.Config[key] != "login-token" {
		t.Fatalf("the seeded key must survive a writer read: %+v", edit.Config)
	}

	// 4. The repair needs no migration: drop the key on an ordinary write, and the monitor and the
	//    secret are ordinary again. `monitorRefSettings` no longer contributes the key for an http
	//    monitor, so the update clears the ref row with it.
	delete(edit.Config, key)
	if _, err := st.UpdateMonitor(ctx, edit); err != nil {
		t.Fatalf("the repair edit must succeed: %v", err)
	}
	if err := st.DeleteProjectSecret(ctx, testSecretActor, projID, "login-token"); err != nil {
		t.Fatalf("after the repair the secret must be deletable: %v", err)
	}
}
