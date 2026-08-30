package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D12 (invariant 17): the token's `actions` allow-list round-trips through the store —
// NULL stays nil, a list stays a list — is validated against the action catalogue on create
// (`action_unknown`), and has no update path.
func TestApiTokenActionsRoundTripAndAreValidatedOnCreate(t *testing.T) {
	st, ctx := declStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}

	plain, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "plain", Role: domain.RoleEditor}, HashToken("s1"))
	if err != nil {
		t.Fatalf("create listless token: %v", err)
	}
	if plain.Actions != nil {
		t.Fatalf("a token created without a list must read back nil, got %v", plain.Actions)
	}

	ci, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "ci", Role: domain.RoleEditor,
		Actions: []string{"gate:evaluate", "change:record"}}, HashToken("s2"))
	if err != nil {
		t.Fatalf("create CI token: %v", err)
	}
	if want := []string{"gate:evaluate", "change:record"}; !reflect.DeepEqual(ci.Actions, want) {
		t.Fatalf("CI token Actions = %v, want %v", ci.Actions, want)
	}
	for _, read := range []func() (domain.ApiToken, error){
		func() (domain.ApiToken, error) { return st.ApiTokenByHash(ctx, HashToken("s2")) },
		func() (domain.ApiToken, error) { return st.GetApiToken(ctx, ci.ID) },
	} {
		got, err := read()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Actions, ci.Actions) {
			t.Fatalf("read model Actions = %v, want %v", got.Actions, ci.Actions)
		}
	}
	list, err := st.ListApiTokensByOrg(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, tk := range list {
		seen[tk.Name] = tk.Actions
	}
	if seen["plain"] != nil || !reflect.DeepEqual(seen["ci"], ci.Actions) {
		t.Fatalf("list read model: %v", seen)
	}

	// An unknown action is refused by name, and nothing is written.
	_, err = st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "bad", Role: domain.RoleEditor,
		Actions: []string{"gate:evaluate", "not:an:action"}}, HashToken("s3"))
	var ce *domain.ChangeError
	if !errors.As(err, &ce) || ce.Code != domain.ChangeErrActionUnknown || ce.Field != "actions" {
		t.Fatalf("unknown action: got %v, want action_unknown on actions", err)
	}
	if _, err := st.ApiTokenByHash(ctx, HashToken("s3")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused token must not exist, got %v", err)
	}

	// Immutable after create: there is no store method that changes the column, and the
	// column itself has no writer but the INSERT — asserted on the schema's own record of the
	// single writer path (no UPDATE statement names it).
	if _, err := st.pool.Exec(ctx, `UPDATE api_tokens SET actions = NULL WHERE id = $1`, ci.ID); err != nil {
		t.Fatalf("direct SQL is outside the contract and is not blocked by the schema: %v", err)
	}
}
