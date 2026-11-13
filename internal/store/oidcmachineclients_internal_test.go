package store

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// TestOIDCSettingsMachineClientsRoundTrip: the DB-saved OIDC override carries machine_clients
// (sec-hardening-2 H-1 fix 1), and a row written before the column existed reads an empty list.
func TestOIDCSettingsMachineClientsRoundTrip(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	in := domain.OIDCSettings{Enabled: true, Issuer: "https://idp/x", ClientID: "cerbix", RedirectURL: "https://c/cb",
		MachineClients: []string{"svc", "ci"}}
	if err := st.UpsertOIDCSettings(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetOIDCSettings(ctx)
	if err != nil || strings.Join(got.MachineClients, ",") != "svc,ci" {
		t.Fatalf("read back %v (%v), want [svc ci]", got.MachineClients, err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE oidc_settings SET machine_clients = DEFAULT`); err != nil {
		t.Fatalf("reset to default: %v", err)
	}
	got, err = st.GetOIDCSettings(ctx)
	if err != nil || got.MachineClients == nil || len(got.MachineClients) != 0 {
		t.Fatalf("default column: %#v (%v), want an empty non-nil list", got.MachineClients, err)
	}
}
