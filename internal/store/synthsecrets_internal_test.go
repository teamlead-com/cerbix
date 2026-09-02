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
