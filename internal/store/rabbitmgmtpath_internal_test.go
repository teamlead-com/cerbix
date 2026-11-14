package store

import (
	"testing"
)

// External review of iter-0210 (P2-1): a RabbitMQ management `path` starts with a single "/" (H-2
// fix 2). The prober used to prepend the "/" to a relative path silently; it now refuses one, so
// migration 00114 writes the "/" the prober used to add into every existing rabbitmq row whose path
// lacks it. Empty or absent paths, paths already starting with "/", and every other type are
// untouched; the execution revision is not bumped, because the request the probe sends is the same.
func TestRabbitMgmtRelativePathMigration(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "rmqpath", "RMQ path")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "rmqpath", "RMQ path")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ins := func(name, typ, config string) string {
		t.Helper()
		var id string
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds, config)
			 VALUES ($1, $2, $2, $3, 'rabbit', 60, 10, $4::jsonb) RETURNING id`,
			proj.ID, name, typ, config).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	rel := ins("rel", "rabbitmq", `{"mode":"management","path":"api/aliveness-test/%2F"}`)
	relAMQP := ins("rel-amqp", "rabbitmq", `{"path":"api/overview?columns=x"}`)
	abs := ins("abs", "rabbitmq", `{"mode":"management","path":"/api/overview"}`)
	empty := ins("empty", "rabbitmq", `{"mode":"management","path":""}`)
	absent := ins("absent", "rabbitmq", `{"mode":"management"}`)
	other := ins("other", "http", `{"path":"api/overview"}`)

	rerunMigrationUp(t, st, ctx, "00114_rabbitmq_management_path_slash.sql")
	rerunMigrationUp(t, st, ctx, "00114_rabbitmq_management_path_slash.sql") // idempotent

	for _, tc := range []struct {
		id, name, path string
		hasKey         bool
	}{
		{rel, "relative", "/api/aliveness-test/%2F", true},
		{relAMQP, "relative, amqp mode", "/api/overview?columns=x", true},
		{abs, "absolute", "/api/overview", true},
		{empty, "empty", "", true},
		{absent, "absent", "", false},
		{other, "not rabbitmq", "api/overview", true},
	} {
		var path string
		var hasKey bool
		var rev int64
		if err := st.pool.QueryRow(ctx,
			`SELECT coalesce(config->>'path', ''), config ? 'path', execution_revision FROM monitors WHERE id = $1`,
			tc.id).Scan(&path, &hasKey, &rev); err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if path != tc.path || hasKey != tc.hasKey {
			t.Errorf("%s: path %q (key %v), want %q (key %v)", tc.name, path, hasKey, tc.path, tc.hasKey)
		}
		if rev != 1 {
			t.Errorf("%s: execution_revision %d, want 1 (no bump)", tc.name, rev)
		}
	}
}
