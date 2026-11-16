package appmux

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/store"
)

// TestPatchWithoutConfigKeepsTheMonitorEditable (sec-hardening-3 T-1d, T-5; S20) runs the real
// handler over the real Store. A PATCH without `config` — the SPA's Pause/Resume button sends
// exactly `{"enabled": …}` — must not be refused for what the stored settings hold, and must not
// rewrite them. Before: a synthetic monitor answered 400 (the handler validated a monitor read
// without its writer-only scenario), a postgres row with `prefer` answered 500, and one with no
// `sslmode` was silently written `require`.
func TestPatchWithoutConfigKeepsTheMonitorEditable(t *testing.T) {
	w := newWorld(t)
	// The scenario is encrypted at rest, so the store needs a cipher to hold a synthetic monitor.
	cipher, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	w.st.WithCipher(cipher)
	as := withCookie(w.adminSess)
	db, err := pgx.Connect(w.ctx, probeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(w.ctx) }()

	create := func(body map[string]any) string {
		t.Helper()
		var out struct{ ID string }
		w.must(w.send(http.MethodPost, "/api/v1/projects/"+w.project+"/monitors", body, as), 201, &out, "create "+body["name"].(string))
		return out.ID
	}
	pg := func(name string) map[string]any {
		return map[string]any{
			"name": name, "type": "postgres", "target": "db.example:5432", "region": "core",
			"interval_seconds": 60, "timeout_seconds": 5, "enabled": true,
			"config": map[string]string{"username": "u", "database": "d", "password": "pw"},
		}
	}
	stored := func(id string) string {
		t.Helper()
		var s string
		if err := db.QueryRow(w.ctx, `SELECT config::text FROM monitors WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	synthetic := create(map[string]any{
		"name": "e2e-synthetic", "type": "synthetic", "region": "core", "interval_seconds": 60, "timeout_seconds": 30, "enabled": true,
		"config": map[string]string{"scenario": `{"steps":[{"url":"https://example.test/health"}]}`},
	})
	prefer := create(pg("e2e-pg-prefer"))
	absent := create(pg("e2e-pg-absent"))
	if _, err := db.Exec(w.ctx, `UPDATE monitors SET config = jsonb_set(config, '{sslmode}', '"prefer"') WHERE id = $1`, prefer); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(w.ctx, `UPDATE monitors SET config = config - 'sslmode' WHERE id = $1`, absent); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, id string }{
		{"synthetic", synthetic},
		{"postgres with sslmode prefer", prefer},
		{"postgres with no sslmode", absent},
	} {
		before := stored(tc.id)
		rec := w.send(http.MethodPatch, "/api/v1/monitors/"+tc.id, map[string]any{"enabled": false}, as)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: PATCH {\"enabled\": false} = %d %s, want 200", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if after := stored(tc.id); after != before {
			t.Errorf("%s: the stored config changed:\n before %s\n after  %s", tc.name, before, after)
		}
	}

	// A PATCH that DOES carry a config is validated as before: a legacy value submitted back is refused,
	// and the answer names the allowed values. That is the moment an editor chooses one.
	rec := w.send(http.MethodPatch, "/api/v1/monitors/"+prefer, map[string]any{
		"config": map[string]string{"username": "u", "database": "d", "sslmode": "prefer"},
	}, as)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "disable|require|verify-ca|verify-full") {
		t.Fatalf("PATCH with config sslmode=prefer = %d %s, want 400 naming the allowed values", rec.Code, rec.Body.String())
	}
}

// TestAPartialUpdateHonoursTheSecretsSwitch (code review of sec-hardening-3, P1-1): with
// `secrets.enabled: false`, a PATCH without `config` on a monitor that references an inventory secret
// is refused 404 feature_disabled exactly as on main — the pause included — and changes nothing.
// Without that, the monitor could be enabled and re-targeted while the inventory was switched off, and
// under an enforced dispatch envelope its secret would be sealed for the new target. With the switch on
// the same PATCH succeeds and S20 holds; a monitor with no reference is editable either way.
func TestAPartialUpdateHonoursTheSecretsSwitch(t *testing.T) {
	w := newWorld(t)
	cipher, err := secret.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	w.st.WithCipher(cipher)
	w.st.WithSecretsEnabled(true)
	as := withCookie(w.adminSess)
	db, err := pgx.Connect(w.ctx, probeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(w.ctx) }()

	if _, err := w.st.CreateProjectSecret(w.ctx, store.SecretActor{}, w.project, "db-password", "inventory-value"); err != nil {
		t.Fatal(err)
	}
	actor := store.AuditActor{ViaToken: true, Label: "fixture"}
	withRef, err := w.st.CreateMonitorByPrincipal(w.ctx, domain.Monitor{
		ProjectID: w.project, Name: "e2e-ref", Type: domain.MonitorPostgres, Target: "db.example:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: false,
		Config: map[string]string{"username": "u", "database": "d", "password_ref": "db-password"},
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := w.st.CreateMonitorByPrincipal(w.ctx, domain.Monitor{
		ProjectID: w.project, Name: "e2e-plain", Type: domain.MonitorTCP, Target: "db.example:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: false,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(id string) string {
		t.Helper()
		var s string
		if err := db.QueryRow(w.ctx, `
			SELECT m.target || ' | ' || m.enabled::text || ' | ' || m.config::text || ' | ' || COALESCE((
			         SELECT string_agg(r.setting_key || '=' || r.secret_id::text, ',' ORDER BY r.setting_key)
			           FROM monitor_secret_refs r WHERE r.monitor_id = m.id), '')
			  FROM monitors m WHERE m.id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	retarget := map[string]any{"enabled": true, "target": "elsewhere.example:5432"}

	// (a) the inventory switched off: refused, nothing changes — and a pause is refused the same way.
	w.st.WithSecretsEnabled(false)
	before := snapshot(withRef.ID)
	for _, body := range []map[string]any{retarget, {"enabled": false}} {
		rec := w.send(http.MethodPatch, "/api/v1/monitors/"+withRef.ID, body, as)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "feature_disabled") {
			t.Errorf("secrets off, PATCH %v = %d %s, want 404 feature_disabled", body, rec.Code, rec.Body.String())
		}
	}
	if after := snapshot(withRef.ID); after != before {
		t.Errorf("secrets off: the monitor changed:\n before %s\n after  %s", before, after)
	}

	// (c) a monitor without a reference is editable with the switch off.
	if rec := w.send(http.MethodPatch, "/api/v1/monitors/"+plain.ID, map[string]any{"enabled": true}, as); rec.Code != http.StatusOK {
		t.Errorf("secrets off, monitor without a reference: PATCH = %d %s, want 200", rec.Code, rec.Body.String())
	}

	// (b) the switch on: the same PATCH succeeds, and the config and its references are untouched (S20).
	w.st.WithSecretsEnabled(true)
	configBefore := strings.SplitN(snapshot(withRef.ID), " | ", 3)[2]
	if rec := w.send(http.MethodPatch, "/api/v1/monitors/"+withRef.ID, retarget, as); rec.Code != http.StatusOK {
		t.Fatalf("secrets on: PATCH = %d %s, want 200", rec.Code, rec.Body.String())
	}
	after := snapshot(withRef.ID)
	if !strings.HasPrefix(after, "elsewhere.example:5432 | true | ") {
		t.Errorf("secrets on: the target and enabled flag were not written: %s", after)
	}
	if got := strings.SplitN(after, " | ", 3)[2]; got != configBefore {
		t.Errorf("secrets on: the config or its references changed:\n before %s\n after  %s", configBefore, got)
	}
}
