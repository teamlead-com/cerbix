package store

import "testing"

func TestAgentTokenLifecycle(t *testing.T) {
	st, ctx := outboxTestStore(t)
	tok, err := st.CreateAgentToken(ctx, "geo3-agent", "geo3", HashToken("secret-abc"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tok.Region != "geo3" || tok.ID == "" {
		t.Fatalf("created = %+v", tok)
	}
	// Resolve a live token → its region.
	region, ok, err := st.ResolveAgentTokenRegion(ctx, HashToken("secret-abc"))
	if err != nil || !ok || region != "geo3" {
		t.Fatalf("resolve = %q ok=%v err=%v", region, ok, err)
	}
	// An unknown token does not resolve.
	if _, ok, _ := st.ResolveAgentTokenRegion(ctx, HashToken("nope")); ok {
		t.Fatal("unknown token resolved")
	}
	if list, _ := st.ListAgentTokens(ctx); len(list) != 1 {
		t.Fatalf("list = %d, want 1", len(list))
	}
	// Revoke → no longer resolves; second revoke is idempotent-ok on an existing row.
	if err := st.RevokeAgentToken(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok, _ := st.ResolveAgentTokenRegion(ctx, HashToken("secret-abc")); ok {
		t.Fatal("revoked token still resolves")
	}
	if err := st.RevokeAgentToken(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("revoke missing = %v, want ErrNotFound", err)
	}
}
