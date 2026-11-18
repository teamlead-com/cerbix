package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-041a pre-review 2, tested as the operator runs it: `cerbix reencrypt` against a database where
// a rolling rotation left one number held by two people under two keys' hashes. The first run finds
// the other row busy and skips the collision: it still re-encrypts the monitor and TOTP secrets that
// come after the phone numbers, and exits 3 — completed, rows skipped, keep the previous key and run
// again — never 0, which would let automation drop the key the skipped number still needs. The
// second run settles it and exits 0; the third has nothing to do. The probe database is unique per
// run, created by this test and dropped by exact name.
func TestReencryptCommandSurvivesAPhoneCollision(t *testing.T) {
	gate := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if gate == "" {
		t.Skip("CERBIX_TEST_DATABASE_DSN not set")
	}
	u, err := url.Parse(gate)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("CERBIX_TEST_DATABASE_DSN is not a postgres:// URL (%q): %v", gate, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, gate)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx) //nolint:errcheck // test teardown
	probeName := fmt.Sprintf("cerbix_test_reencryptcli_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+probeName); err != nil {
		t.Fatalf("create probe database %s (this end-to-end gate requires CREATEDB): %v", probeName, err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+probeName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop probe database %s: %v", probeName, err)
		}
	}()
	u.Path = "/" + probeName
	probeDSN := u.String()

	newKey := func() []byte {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	keyA, keyB := newKey(), newKey()
	cA, _ := secret.New(keyA)
	cB, _ := secret.New(keyB)
	cfg := writeConfig(t, fmt.Sprintf("database:\n  dsn: %q\nsecurity:\n  encryption_key: %q\n  previous_keys: [%q]\n",
		probeDSN, base64.StdEncoding.EncodeToString(keyB), base64.StdEncoding.EncodeToString(keyA)))
	if code := Main([]string{"migrate", "--config", cfg}); code != 0 {
		t.Fatalf("migrate probe database = %d, want 0", code)
	}

	st, err := store.Open(ctx, probeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const number = "+905321116869"
	onNew, err := st.CreateLocalUser(ctx, "on-new@example.com", "N", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	onOld, err := st.CreateLocalUser(ctx, "on-old@example.com", "O", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	// A monitor password under the old key: rewritten AFTER the phone numbers.
	st.WithCipher(cA)
	org, err := st.CreateOrganization(ctx, "rot", "Rot")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "rot", "Rot")
	if err != nil {
		t.Fatal(err)
	}
	mon, err := st.CreateMonitorByPrincipal(ctx, domain.Monitor{
		RecoveryThreshold: 1, ProjectID: proj.ID, Name: "db", Type: domain.MonitorTCP, Target: "db:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true, Config: map[string]string{"password": "dbpass"},
	}, store.AuditActor{ViaToken: true, Label: "apitoken:test"})
	if err != nil {
		t.Fatal(err)
	}
	// The process on B accepts the number for one person, the process still on A for another.
	st.WithCipher(cB)
	if _, _, err := st.SetUserPhone(ctx, onNew.ID, number); err != nil {
		t.Fatal(err)
	}
	st.WithCipher(cA)
	if _, _, err := st.SetUserPhone(ctx, onOld.ID, number); err != nil {
		t.Fatal(err)
	}

	db, err := pgx.Connect(ctx, probeDSN)
	if err != nil {
		t.Fatalf("connect probe: %v", err)
	}
	defer db.Close(ctx) //nolint:errcheck // test teardown
	totp, _ := cA.Encrypt("totpsecret")
	if _, err := db.Exec(ctx, `UPDATE users SET totp_secret = $2 WHERE id = $1`, onNew.ID, totp); err != nil {
		t.Fatal(err)
	}
	// onNew's row verified: it is the one that keeps the number.
	if _, err := db.Exec(ctx, `UPDATE user_phones SET verified_at = now() WHERE user_id = $1`, onNew.ID); err != nil {
		t.Fatal(err)
	}

	// Run 1: the row that keeps the number is held by another transaction, so the collision cannot
	// be settled now. The command still reaches the monitor and TOTP secrets, and exits 3.
	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM user_phones WHERE user_id = $1 FOR KEY SHARE`, onNew.ID); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"reencrypt", "--config", cfg}); code != 3 {
		t.Fatalf("reencrypt with a collision it cannot settle now = %d, want 3 (completed, rows skipped)", code)
	}
	var rawPassword string
	if err := db.QueryRow(ctx, `SELECT config->>'password' FROM monitors WHERE id = $1`, mon.ID).Scan(&rawPassword); err != nil {
		t.Fatal(err)
	}
	if got, err := cB.Decrypt(rawPassword); err != nil || got != "dbpass" {
		t.Fatalf("the monitor password was not re-encrypted past the phone collision: %q %v", got, err)
	}
	var rawTOTP string
	if err := db.QueryRow(ctx, `SELECT totp_secret FROM users WHERE id = $1`, onNew.ID).Scan(&rawTOTP); err != nil {
		t.Fatal(err)
	}
	if got, err := cB.Decrypt(rawTOTP); err != nil || got != "totpsecret" {
		t.Fatalf("the TOTP secret was not re-encrypted past the phone collision: %q %v", got, err)
	}
	var rows int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM user_phones`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("after the skipping run: %d rows (%v), want both left as they were", rows, err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Run 2 settles it; run 3 has nothing left to do.
	for run := 2; run <= 3; run++ {
		if code := Main([]string{"reencrypt", "--config", cfg}); code != 0 {
			t.Fatalf("reencrypt run %d = %d, want 0", run, code)
		}
		var holderID string
		if err := db.QueryRow(ctx, `SELECT user_id::text FROM user_phones`).Scan(&holderID); err != nil || holderID != onNew.ID {
			t.Fatalf("after run %d the number is held by %q (%v), want only %s", run, holderID, err, onNew.ID)
		}
	}
}
