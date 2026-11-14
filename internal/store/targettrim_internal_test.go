package store

import (
	"testing"
)

// External review of iter-0210 (P2-2): a control byte in a raw target is refused on every write
// surface, and the websocket, ssh, redis and rabbitmq probers re-check the raw target before they
// dial. Before this iteration those probers — and postgres and mysql, which still do — trimmed the
// target, so a row whose target ends in CR/LF or a tab probed fine. Migration 00115 trims that ASCII
// whitespace from the edges of those types' targets, so none flips DOWN on upgrade or answers 400
// at its next PATCH. Interior bytes, other types and clean rows are untouched; the execution
// revision is not bumped, because the probe dials what it dialled before.
func TestTargetEdgeWhitespaceMigration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "trim", "Trim")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "trim", "Trim")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ins := func(name, typ, target string) string {
		t.Helper()
		var id string
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds, config)
			 VALUES ($1, $2, $2, $3, $4, 60, 10, '{}'::jsonb) RETURNING id`,
			proj.ID, name, typ, target).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	cases := []struct {
		id, name, want string
	}{
		{ins("pg", "postgres", "db.internal:5432\r\n"), "postgres CRLF", "db.internal:5432"},
		{ins("my", "mysql", "\tdb.internal"), "mysql tab", "db.internal"},
		{ins("rd", "redis", " cache.internal:6379 \n"), "redis mixed", "cache.internal:6379"},
		{ins("ssh", "ssh", "bastion.internal\n"), "ssh LF", "bastion.internal"},
		{ins("rb", "rabbitmq", "http://rabbit:15672\r\n"), "rabbitmq management URL", "http://rabbit:15672"},
		{ins("ws", "websocket", "wss://chat.internal/ws\n"), "websocket", "wss://chat.internal/ws"},
		{ins("ws00113", "websocket", "ws://\nchat.internal/ws"), "websocket prefixed by 00113", "ws://chat.internal/ws"},
		{ins("in", "redis", "cache\r\n.internal"), "interior bytes", "cache\r\n.internal"},
		{ins("http", "http", "https://app.internal/\n"), "http (its prober never trimmed)", "https://app.internal/\n"},
		{ins("tcp", "tcp", "h.internal:80\n"), "tcp (its prober never trimmed)", "h.internal:80\n"},
		{ins("clean", "postgres", "db.internal"), "clean", "db.internal"},
	}

	rerunMigrationUp(t, st, ctx, "00115_target_edge_whitespace.sql")
	rerunMigrationUp(t, st, ctx, "00115_target_edge_whitespace.sql") // idempotent

	for _, tc := range cases {
		var target string
		var rev int64
		if err := st.pool.QueryRow(ctx, `SELECT target, execution_revision FROM monitors WHERE id = $1`, tc.id).Scan(&target, &rev); err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if target != tc.want {
			t.Errorf("%s: target %q, want %q", tc.name, target, tc.want)
		}
		if rev != 1 {
			t.Errorf("%s: execution_revision %d, want 1 (no bump)", tc.name, rev)
		}
	}
}
