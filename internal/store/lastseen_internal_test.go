package store

import (
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// iter-0205 (func-admin-users.md "Last active"): "last active" on the user and member lists is
// users.last_seen_at. It is stamped by sign-in and survives every way a session disappears —
// logout, a password-change sweep, expiry and the expired-session purge — which the old
// max(sessions.created_at) did not: those users read "never".
func TestLastActiveSurvivesTheSessionsThatStampedIt(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	mk := func(email string) domain.User {
		t.Helper()
		u, err := st.CreateLocalUser(ctx, email, email, "hash", false)
		if err != nil {
			t.Fatalf("user: %v", err)
		}
		if _, err := st.CreateMembership(ctx, domain.Membership{UserID: u.ID, OrgID: org.ID, Role: domain.RoleViewer}); err != nil {
			t.Fatalf("membership: %v", err)
		}
		return u
	}
	loggedOut, swept, expired, never := mk("out@x.com"), mk("swept@x.com"), mk("expired@x.com"), mk("never@x.com")

	before := time.Now().Add(-time.Second)
	if _, err := st.CreateSession(ctx, loggedOut.ID, "tok-out", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := st.CreateSession(ctx, swept.ID, "tok-swept", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("session: %v", err)
	}
	// Already past its expiry when the purge runs.
	if _, err := st.CreateSession(ctx, expired.ID, "tok-expired", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("session: %v", err)
	}
	after := time.Now().Add(time.Second)

	if err := st.DeleteSession(ctx, "tok-out"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := st.DeleteSessionsByUser(ctx, swept.ID, ""); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n, err := st.DeleteExpiredSessions(ctx); err != nil || n != 1 {
		t.Fatalf("purge removed %d (%v), want 1", n, err)
	}
	var left int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d sessions left (%v): this test needs every session gone", left, err)
	}

	users, err := st.ListAllUsers(ctx, "")
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	members, err := st.ListOrgMembers(ctx, org.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	userSeen := map[string]*time.Time{}
	for _, u := range users {
		userSeen[u.ID] = u.LastActiveAt
	}
	memberSeen := map[string]*time.Time{}
	for _, m := range members {
		memberSeen[m.UserID] = m.LastActiveAt
	}
	for _, c := range []struct {
		name string
		id   string
	}{{"logged out", loggedOut.ID}, {"sessions swept", swept.ID}, {"session expired and purged", expired.ID}} {
		for list, got := range map[string]*time.Time{"users": userSeen[c.id], "members": memberSeen[c.id]} {
			if got == nil {
				t.Errorf("%s: %s list reads never, want the sign-in time", c.name, list)
				continue
			}
			if got.Before(before) || got.After(after) {
				t.Errorf("%s: %s list reads %v, want the sign-in time in [%v, %v]", c.name, list, *got, before, after)
			}
		}
	}
	if userSeen[never.ID] != nil || memberSeen[never.ID] != nil {
		t.Errorf("a user who never signed in reads %v / %v, want never", userSeen[never.ID], memberSeen[never.ID])
	}
}

// The refresh moves last_seen_at only when it is absent or older than five minutes (checked at
// 4m59s and 5m01s), whichever API process asks, and never touches updated_at.
func TestTouchUserSeenRefreshesOnlyAStaleStamp(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	u, err := st.CreateLocalUser(ctx, "touch@x.com", "Touch", "hash", false)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	read := func() (seen *time.Time, updated time.Time) {
		t.Helper()
		if err := st.pool.QueryRow(ctx, `SELECT last_seen_at, updated_at FROM users WHERE id = $1`, u.ID).Scan(&seen, &updated); err != nil {
			t.Fatalf("read: %v", err)
		}
		return seen, updated
	}
	set := func(expr string) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, `UPDATE users SET last_seen_at = `+expr+` WHERE id = $1`, u.ID); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	_, updated0 := read()

	// Absent → stamped.
	if err := st.TouchUserSeen(ctx, u.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if seen, _ := read(); seen == nil || time.Since(*seen) > time.Minute {
		t.Fatalf("an absent stamp was not set to now: %v", seen)
	}

	// Younger than five minutes → left alone.
	set(`now() - interval '4 minutes 59 seconds'`)
	held, _ := read()
	if err := st.TouchUserSeen(ctx, u.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if seen, _ := read(); !seen.Equal(*held) {
		t.Fatalf("a stamp 4m59s old moved from %v to %v", *held, *seen)
	}

	// Older than five minutes → moved to now.
	set(`now() - interval '5 minutes 1 second'`)
	if err := st.TouchUserSeen(ctx, u.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if seen, _ := read(); time.Since(*seen) > time.Minute {
		t.Fatalf("a stamp 5m01s old was not refreshed: %v", *seen)
	}

	if _, updated := read(); !updated.Equal(updated0) {
		t.Fatalf("updated_at moved from %v to %v: last_seen_at is not an edit of the user", updated0, updated)
	}
}

// Migration 00111 backfills last_seen_at from the newest remaining session of each user and does
// not overwrite a stamp that is already there. Its Up section is re-run here over data — the test
// helper migrated an empty database.
func TestLastSeenMigrationBackfillsFromTheNewestSession(t *testing.T) {
	st, ctx := serviceSchemaStore(t)
	mk := func(email string) string {
		t.Helper()
		u, err := st.CreateLocalUser(ctx, email, email, "hash", false)
		if err != nil {
			t.Fatalf("user: %v", err)
		}
		return u.ID
	}
	two, one, none, stamped := mk("two@x.com"), mk("one@x.com"), mk("none@x.com"), mk("stamped@x.com")
	ins := func(user, token, created string) {
		t.Helper()
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO sessions (token_hash, user_id, expires_at, created_at)
			 VALUES ($1, $2, now() + interval '1 hour', `+created+`)`, HashToken(token), user); err != nil {
			t.Fatalf("session: %v", err)
		}
	}
	ins(two, "a", `'2026-10-01T10:00:00Z'`)
	ins(two, "b", `'2026-10-03T10:00:00Z'`)
	ins(one, "c", `'2026-10-02T10:00:00Z'`)
	ins(stamped, "d", `'2026-10-02T10:00:00Z'`)
	if _, err := st.pool.Exec(ctx, `UPDATE users SET last_seen_at = NULL`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE users SET last_seen_at = '2026-10-05T10:00:00Z' WHERE id = $1`, stamped); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	raw, err := migrationsFS.ReadFile("migrations/00111_users_last_seen.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	up := string(raw)
	i, j := strings.Index(up, "-- +goose Up"), strings.Index(up, "-- +goose Down")
	if i < 0 || j < i {
		t.Fatalf("migration has no Up/Down sections")
	}
	if _, err := st.pool.Exec(ctx, up[i:j]); err != nil {
		t.Fatalf("re-run Up: %v", err)
	}

	want := map[string]string{two: "2026-10-03T10:00:00Z", one: "2026-10-02T10:00:00Z", stamped: "2026-10-05T10:00:00Z"}
	for id, w := range want {
		var got *time.Time
		if err := st.pool.QueryRow(ctx, `SELECT last_seen_at FROM users WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read: %v", err)
		}
		wt, _ := time.Parse(time.RFC3339, w)
		if got == nil || !got.Equal(wt) {
			t.Errorf("user %s: last_seen_at %v, want %s", id, got, w)
		}
	}
	var got *time.Time
	if err := st.pool.QueryRow(ctx, `SELECT last_seen_at FROM users WHERE id = $1`, none).Scan(&got); err != nil || got != nil {
		t.Errorf("a user without sessions got %v (%v), want NULL", got, err)
	}
}
