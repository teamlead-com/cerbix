package store

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The constraint is the LAST place a new monitor type has to be told, and the easiest to forget:
// every other layer is Go, and a unit test with a fake store never meets it. FR-029 shipped a type
// the database refused, and the 23514 reached an operator as a 500 with no reason in it.
//
// This test walks the domain's OWN list of valid types and creates one of each against the real
// table, so the next type to be added cannot pass CI while the database still refuses it.
func TestEveryValidMonitorTypeIsAcceptedByTheDatabase(t *testing.T) {
	st, ctx := outboxTestStore(t)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	types := []domain.MonitorType{
		domain.MonitorHTTP, domain.MonitorTCP, domain.MonitorICMP, domain.MonitorDNS, domain.MonitorTLS,
		domain.MonitorGRPC, domain.MonitorComposite, domain.MonitorPostgres, domain.MonitorMySQL,
		domain.MonitorRedis, domain.MonitorPromQL, domain.MonitorRabbitMQ, domain.MonitorWebSocket,
		domain.MonitorSSH, domain.MonitorSynthetic, domain.MonitorAsyncCanary, domain.MonitorPush,
	}
	for _, typ := range types {
		if !typ.Valid() {
			t.Fatalf("%s is in this list and not Valid() — one of the two is wrong", typ)
		}
		// The row is written directly: this test is about the CONSTRAINT, not about the domain
		// rules each type carries, and going through CreateMonitor would fail on a missing scenario
		// or workflow long before the database is asked anything.
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO monitors (project_id, name, slug, type, target, interval_seconds, timeout_seconds)
			 VALUES ($1, $2, $2, $3, 'x', 60, 10)`, proj.ID, "t-"+strings.ReplaceAll(string(typ), "_", "-"), string(typ)); err != nil {
			t.Fatalf("the database refuses monitor type %q: %v", typ, err)
		}
	}
}
