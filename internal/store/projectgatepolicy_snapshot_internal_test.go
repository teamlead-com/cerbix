package store

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/domain"
)

type effectivePolicyBarrierTracer struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (tr *effectivePolicyBarrierTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM service_gate_policies") {
		tr.once.Do(func() {
			close(tr.reached)
			<-tr.release
		})
	}
	return ctx
}

func (*effectivePolicyBarrierTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestEffectiveGatePolicyUsesOneRepeatableReadSnapshot(t *testing.T) {
	st, ctx := gateStore(t)
	org, err := st.CreateOrganization(ctx, "snapshot-org", "Snapshot Org")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	project, err := st.CreateProject(ctx, org.ID, "snapshot-project", "Snapshot Project")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	service, err := st.CreateService(ctx, domain.Service{ProjectID: project.ID, Slug: "snapshot-service", Name: "Snapshot Service"})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	clauses := `{"budget_exhausted":"block","budget_consumed":"warn","page_burn_firing":"block","ticket_burn_firing":"warn","service_incident_open":"warn"}`
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO project_gate_policies
		  (project_id, window_name, window_mode, schema_version, clauses, budget_consumed_percent,
		   max_seal_lag_seconds, unknown_behavior, revision, updated_at, updated_by)
		VALUES ($1, '24h', 'one', 1, $2::jsonb, 90, 900, 'warn', 1, statement_timestamp(), 'token:ci')`,
		project.ID, clauses); err != nil {
		t.Fatalf("project policy: %v", err)
	}

	tracer := &effectivePolicyBarrierTracer{reached: make(chan struct{}), release: make(chan struct{})}
	cfg, err := pgxpool.ParseConfig(gateTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	oldPool := st.pool
	st.pool = traced
	t.Cleanup(func() {
		st.pool = oldPool
		traced.Close()
	})

	type result struct {
		policy domain.EffectiveGatePolicy
		err    error
	}
	done := make(chan result, 1)
	go func() {
		policy, err := st.EffectiveGatePolicy(ctx, project.ID, service.ID)
		done <- result{policy: policy, err: err}
	}()
	<-tracer.reached
	if _, err := oldPool.Exec(ctx, `
		INSERT INTO service_gate_policies
		  (service_id, project_id, window_name, window_mode, schema_version, clauses,
		   budget_consumed_percent, max_seal_lag_seconds, unknown_behavior, revision, updated_by)
		VALUES ($1, $2, '24h', 'one', 1, $3::jsonb, 90, 900, 'warn', 1, 'token:ci')`,
		service.ID, project.ID, clauses); err != nil {
		t.Fatalf("concurrent service policy: %v", err)
	}
	close(tracer.release)
	first := <-done
	if first.err != nil {
		t.Fatalf("effective policy: %v", first.err)
	}
	if first.policy.Source != domain.GatePolicySourceProject {
		t.Fatalf("crossed snapshot source = %q, want project", first.policy.Source)
	}
	second, err := st.EffectiveGatePolicy(ctx, project.ID, service.ID)
	if err != nil {
		t.Fatalf("second effective policy: %v", err)
	}
	if second.Source != domain.GatePolicySourceService {
		t.Fatalf("fresh snapshot source = %q, want service", second.Source)
	}
}
