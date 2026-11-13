package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-2 H-5 (invariant S5, amends D-0139 item 3): after a reset every session created
// before it is gone; after a change every session but the caller's is, atomically with the new
// hash. API tokens are not touched by either.

func localUserWithSessions(t *testing.T, st *Store, ctx context.Context, email string, tokens ...string) domain.User {
	t.Helper()
	u, err := st.CreateLocalUser(ctx, email, email, "old-hash", false)
	if err != nil {
		t.Fatalf("local user: %v", err)
	}
	for _, tok := range tokens {
		if _, err := st.CreateSession(ctx, u.ID, tok, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("session %s: %v", tok, err)
		}
	}
	return u
}

// failSessionDeletesFor makes every DELETE of userID's sessions raise — the forced failure
// "between hash and delete" — until the returned cleanup runs (also registered with t.Cleanup).
func failSessionDeletesFor(t *testing.T, st *Store, ctx context.Context, userID string) func() {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	name := "test_fail_session_delete_" + hex.EncodeToString(b)
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'forced session delete failure'; END $$`); err != nil {
		t.Fatalf("create function: %v", err)
	}
	// userID comes from the database (a uuid), so inlining it is safe; a trigger WHEN cannot bind.
	if _, err := st.pool.Exec(ctx, `CREATE TRIGGER `+name+` BEFORE DELETE ON sessions FOR EACH ROW
		WHEN (OLD.user_id = '`+userID+`'::uuid) EXECUTE FUNCTION `+name+`()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	done := false
	drop := func() {
		if done {
			return
		}
		done = true
		_, _ = st.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON sessions`)
		_, _ = st.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
	}
	t.Cleanup(drop)
	return drop
}

func passwordHash(t *testing.T, st *Store, ctx context.Context, id string) string {
	t.Helper()
	h, err := st.PasswordHashByID(ctx, id)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

func TestResetRevokesEverySession(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, _ := st.CreateOrganization(ctx, "reset-org", "Reset")
	u := localUserWithSessions(t, st, ctx, "r@x.com", "s1", "s2")
	if _, err := st.CreateApiToken(ctx, domain.ApiToken{OrgID: org.ID, Name: "ci", Role: domain.RoleEditor}, HashToken("api-1")); err != nil {
		t.Fatalf("api token: %v", err)
	}
	if err := st.CreatePasswordResetToken(ctx, u.ID, HashToken("rt"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reset token: %v", err)
	}
	if _, err := st.ResetPasswordWithToken(ctx, HashToken("rt"), "new-hash"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, tok := range []string{"s1", "s2"} {
		if _, err := st.SessionByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %s survived the reset: %v", tok, err)
		}
	}
	if _, err := st.ApiTokenByHash(ctx, HashToken("api-1")); err != nil {
		t.Fatalf("an API token was revoked by the reset: %v", err)
	}
}

func TestResetRollsBackWhenSessionDeleteFails(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	u := localUserWithSessions(t, st, ctx, "rb@x.com", "s1")
	if err := st.CreatePasswordResetToken(ctx, u.ID, HashToken("rt"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reset token: %v", err)
	}
	drop := failSessionDeletesFor(t, st, ctx, u.ID)
	if _, err := st.ResetPasswordWithToken(ctx, HashToken("rt"), "new-hash"); err == nil {
		t.Fatal("reset succeeded although its session delete failed")
	}
	if got := passwordHash(t, st, ctx, u.ID); got != "old-hash" {
		t.Fatalf("hash changed to %q by a failed reset", got)
	}
	if _, err := st.SessionByToken(ctx, "s1"); err != nil {
		t.Fatalf("session gone after a failed reset: %v", err)
	}
	drop()
	if _, err := st.ResetPasswordWithToken(ctx, HashToken("rt"), "new-hash"); err != nil {
		t.Fatalf("the failed reset burned the token: %v", err)
	}
}

func TestChangePasswordKeepsOnlyCallerSession(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	u := localUserWithSessions(t, st, ctx, "c@x.com", "s1", "s2", "s3")
	localUserWithSessions(t, st, ctx, "o@x.com", "o1")
	n, err := st.ChangePassword(ctx, u.ID, "new-hash", "s1")
	if err != nil || n != 2 {
		t.Fatalf("change: n=%d err=%v, want 2 sessions removed", n, err)
	}
	if _, err := st.SessionByToken(ctx, "s1"); err != nil {
		t.Fatalf("the caller's session was revoked: %v", err)
	}
	for _, tok := range []string{"s2", "s3"} {
		if _, err := st.SessionByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %s survived the change: %v", tok, err)
		}
	}
	if _, err := st.SessionByToken(ctx, "o1"); err != nil {
		t.Fatalf("another user's session was revoked: %v", err)
	}
	if got := passwordHash(t, st, ctx, u.ID); got != "new-hash" {
		t.Fatalf("hash = %q, want new-hash", got)
	}
}

func TestChangePasswordAtomicWhenSessionDeleteFails(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	u := localUserWithSessions(t, st, ctx, "a@x.com", "s1", "s2")
	failSessionDeletesFor(t, st, ctx, u.ID)
	if _, err := st.ChangePassword(ctx, u.ID, "new-hash", "s1"); err == nil {
		t.Fatal("change succeeded although its session delete failed")
	}
	if got := passwordHash(t, st, ctx, u.ID); got != "old-hash" {
		t.Fatalf("hash changed to %q by a failed change", got)
	}
}

func TestChangePasswordNotLocalAccount(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	u, _, err := st.UpsertHumanUserByOIDCIdentity(ctx, "https://idp.test", "sso", "sso@x.com", "SSO")
	if err != nil {
		t.Fatalf("sso user: %v", err)
	}
	if _, err := st.ChangePassword(ctx, u.ID, "new-hash", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("change on an SSO account: err %v, want ErrNotFound", err)
	}
	if _, err := st.PasswordHashByID(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an SSO account gained a password: %v", err)
	}
}
