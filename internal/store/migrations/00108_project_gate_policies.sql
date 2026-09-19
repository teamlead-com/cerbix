-- +goose Up
+-- FR-034/NFR-028: project policies are versioned source rows.  Inherited service policy
+-- copies are deliberately impossible: resolution joins this table at evaluation time.
+CREATE TABLE project_gate_policies (
+    project_id uuid PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
+    window_name text NOT NULL,
+    schema_version integer NOT NULL,
+    clauses jsonb NOT NULL,
+    budget_consumed_percent integer NOT NULL,
+    max_seal_lag_seconds integer NOT NULL,
+    unknown_behavior text NOT NULL,
+    revision bigint NOT NULL,
+    deleted_at timestamptz,
+    updated_at timestamptz NOT NULL,
+    updated_by text NOT NULL,
+    CONSTRAINT project_gate_policies_revision_chk CHECK (revision > 0),
+    CONSTRAINT project_gate_policies_clauses_chk CHECK (jsonb_typeof(clauses) = 'object'),
+    CONSTRAINT project_gate_policies_unknown_behavior_chk CHECK (unknown_behavior IN ('warn', 'block'))
+);
+
+ALTER TABLE service_gate_overrides
+    ADD COLUMN policy_source text,
+    ADD COLUMN policy_owner_id uuid;
+UPDATE service_gate_overrides
+   SET policy_source = 'service', policy_owner_id = service_id
+ WHERE policy_source IS NULL;
+ALTER TABLE service_gate_overrides
+    ALTER COLUMN policy_source SET NOT NULL,
+    ALTER COLUMN policy_owner_id SET NOT NULL,
+    ADD CONSTRAINT service_gate_overrides_policy_source_chk CHECK (policy_source IN ('service', 'project'));
+CREATE INDEX service_gate_overrides_project_source_open_idx
+    ON service_gate_overrides (project_id, policy_source)
+ WHERE revoked_at IS NULL;
+
+ALTER TABLE service_gate_decisions
+    ADD COLUMN policy_source text,
+    ADD COLUMN policy_owner_id uuid,
+    ADD CONSTRAINT service_gate_decisions_policy_source_chk
+        CHECK ((state = 'NOT_CONFIGURED') = (policy_source IS NULL)
+           AND (state = 'NOT_CONFIGURED') = (policy_owner_id IS NULL)
+           AND (policy_source IS NULL OR policy_source IN ('service', 'project')));
+
+-- +goose Down
+ALTER TABLE service_gate_decisions DROP CONSTRAINT IF EXISTS service_gate_decisions_policy_source_chk;
+ALTER TABLE service_gate_decisions DROP COLUMN IF EXISTS policy_owner_id;
+ALTER TABLE service_gate_decisions DROP COLUMN IF EXISTS policy_source;
+DROP INDEX IF EXISTS service_gate_overrides_project_source_open_idx;
+ALTER TABLE service_gate_overrides DROP CONSTRAINT IF EXISTS service_gate_overrides_policy_source_chk;
+ALTER TABLE service_gate_overrides DROP COLUMN IF EXISTS policy_owner_id;
+ALTER TABLE service_gate_overrides DROP COLUMN IF EXISTS policy_source;
+DROP TABLE IF EXISTS project_gate_policies;
