package store

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Second external review of iter-0210 (P1-1): the 00113 → 00114 → 00115 chain must leave every
// websocket target parseable, with tls_skip_verify on the ones that never verified. The first 00113
// called btrim(target) with one argument, which trims only SPACES: a target with a leading tab, LF
// or CR was taken for a scheme-less one and became ws://\nwss://…, without tls_skip_verify, and
// 00115 then made it ws://wss://… — DOWN. Two starting points are covered: a database that runs the
// fixed chain, and one that already ran the first 00113 (and possibly the first 00115), for which
// only the new 00115 runs.
func TestWebSocketMigrationChain(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "wschain", "WS chain")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "wschain", "WS chain")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ins := func(name, target, config string) string {
		t.Helper()
		var id string
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds, config)
			 VALUES ($1, $2, $2, 'websocket', $3, 60, 10, $4::jsonb) RETURNING id`,
			proj.ID, name, target, config).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	type want struct {
		id, name, target, skip string
		hasKey                 bool
	}
	check := func(t *testing.T, cases []want) {
		t.Helper()
		for _, tc := range cases {
			var target, skip string
			var hasKey bool
			var rev int64
			if err := st.pool.QueryRow(ctx,
				`SELECT target, coalesce(config->>'tls_skip_verify', ''), config ? 'tls_skip_verify', execution_revision
				   FROM monitors WHERE id = $1`, tc.id).Scan(&target, &skip, &hasKey, &rev); err != nil {
				t.Fatalf("read %s: %v", tc.name, err)
			}
			if target != tc.target || skip != tc.skip || hasKey != tc.hasKey {
				t.Errorf("%s: target %q skip %q (key %v), want %q %q (key %v)", tc.name, target, skip, hasKey, tc.target, tc.skip, tc.hasKey)
			}
			if _, err := domain.ParseWebSocketTarget(target); err != nil {
				t.Errorf("%s: %q does not parse: %v", tc.name, target, err)
			}
			if rev != 1 {
				t.Errorf("%s: execution_revision %d, want 1 (no bump)", tc.name, rev)
			}
		}
	}

	t.Run("fixed chain", func(t *testing.T) {
		cases := []want{
			{ins("lf-wss", "\nwss://a.internal/ws", `{}`), "LF + wss", "wss://a.internal/ws", "true", true},
			{ins("tab-ws", "\tws://b.internal/ws", `{}`), "tab + ws", "ws://b.internal/ws", "", false},
			{ins("sp-lf-https", "  \nhttps://c.internal/ws", `{}`), "spaces, LF + https", "https://c.internal/ws", "true", true},
			{ins("cr-https-trail", "\rHTTPS://c2.internal/ws\r\n", `{}`), "CR + HTTPS + CRLF", "HTTPS://c2.internal/ws", "true", true},
			{ins("lf-bare", "\nd.internal/ws", `{}`), "LF + no scheme", "ws://d.internal/ws", "", false},
			// The VT trap: E'\v' in a Postgres string literal is the letter v, not VT.
			{ins("vt-bare-v", "\x0be.internal/dev", `{}`), "VT + no scheme ending in v", "ws://e.internal/dev", "", false},
			{ins("lf-wss-v", "\nwss://e2.internal/v", `{}`), "LF + wss ending in v", "wss://e2.internal/v", "true", true},
		}
		for _, f := range []string{"00113_websocket_tls_skip_verify.sql", "00114_rabbitmq_management_path_slash.sql", "00115_target_edge_whitespace.sql"} {
			rerunMigrationUp(t, st, ctx, f)
		}
		for _, f := range []string{"00113_websocket_tls_skip_verify.sql", "00114_rabbitmq_management_path_slash.sql", "00115_target_edge_whitespace.sql"} {
			rerunMigrationUp(t, st, ctx, f) // idempotent
		}
		check(t, cases)
	})

	t.Run("after the first 00113", func(t *testing.T) {
		// Rows in the state the first 00113 (and the first 00115) left them.
		cases := []want{
			{ins("old-lf-wss", "ws://\nwss://f.internal/ws", `{}`), "ws:// + LF + wss", "wss://f.internal/ws", "true", true},
			{ins("old-wss", "ws://wss://g.internal/ws", `{}`), "ws://wss (after the first 00115)", "wss://g.internal/ws", "true", true},
			{ins("old-https-chosen", "ws://\nhttps://h.internal/ws", `{"tls_skip_verify":"false"}`), "https, the operator chose", "https://h.internal/ws", "false", true},
			{ins("old-tab-ws", "ws://\tws://i.internal/ws", `{}`), "ws:// + tab + ws", "ws://i.internal/ws", "", false},
			{ins("old-http", "ws://HTTP://i2.internal/ws", `{}`), "ws://HTTP", "HTTP://i2.internal/ws", "", false},
			// A wss monitor saved after the first 00113 chose verification by leaving the key out;
			// only the rows that migration damaged get the opt-out.
			{ins("new-wss", "wss://j.internal/ws", `{}`), "wss saved later", "wss://j.internal/ws", "", false},
		}
		rerunMigrationUp(t, st, ctx, "00115_target_edge_whitespace.sql")
		rerunMigrationUp(t, st, ctx, "00115_target_edge_whitespace.sql") // idempotent
		check(t, cases)
	})
}
