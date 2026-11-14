package store

import (
	"testing"
)

// sec-hardening-2 H-2 fix 4 + owner ruling 2026-11-14: existing WebSocket monitors keep the behaviour
// they had. A wss/https target skipped verification unconditionally, so it gets tls_skip_verify
// "true"; a scheme-less target was dialled as ws://, so it gets ws://. Plaintext targets, rows that
// already chose, and every other type are untouched; the execution revision is not bumped, because
// nothing about how the probe behaves changes.
func TestWebSocketTLSMigration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "wstls", "WS TLS")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "wstls", "WS TLS")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ins := func(name, typ, target, config string) string {
		t.Helper()
		var id string
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds, config)
			 VALUES ($1, $2, $2, $3, $4, 60, 10, $5::jsonb) RETURNING id`,
			proj.ID, name, typ, target, config).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	a := ins("a", "websocket", "wss://a.internal/ws", `{}`)
	b := ins("b", "websocket", " HTTPS://b.internal/ws", `{}`)
	c := ins("c", "websocket", "ws://c.internal/ws", `{}`)
	d := ins("d", "websocket", "d.internal:80/x", `{}`)
	e := ins("e", "websocket", "wss://e.internal/ws", `{"tls_skip_verify":"false"}`)
	r := ins("r", "redis", "wss://not-a-websocket", `{}`)

	rerunMigrationUp(t, st, ctx, "00113_websocket_tls_skip_verify.sql")
	rerunMigrationUp(t, st, ctx, "00113_websocket_tls_skip_verify.sql") // idempotent

	read := func(id string) (target, skip string, hasKey bool, rev int64) {
		t.Helper()
		if err := st.pool.QueryRow(ctx,
			`SELECT target, coalesce(config->>'tls_skip_verify', ''), config ? 'tls_skip_verify', execution_revision
			   FROM monitors WHERE id = $1`, id).Scan(&target, &skip, &hasKey, &rev); err != nil {
			t.Fatalf("read: %v", err)
		}
		return
	}
	for _, tc := range []struct {
		id, name, target, skip string
		hasKey                 bool
	}{
		{a, "wss", "wss://a.internal/ws", "true", true},
		{b, "https (case, padding)", " HTTPS://b.internal/ws", "true", true},
		{c, "ws", "ws://c.internal/ws", "", false},
		{d, "no scheme", "ws://d.internal:80/x", "", false},
		{e, "already chose", "wss://e.internal/ws", "false", true},
		{r, "not a websocket", "wss://not-a-websocket", "", false},
	} {
		target, skip, hasKey, rev := read(tc.id)
		if target != tc.target || skip != tc.skip || hasKey != tc.hasKey {
			t.Errorf("%s: target %q skip %q (key %v), want %q %q (key %v)", tc.name, target, skip, hasKey, tc.target, tc.skip, tc.hasKey)
		}
		if rev != 1 {
			t.Errorf("%s: execution_revision %d, want 1 (no bump)", tc.name, rev)
		}
	}
}
