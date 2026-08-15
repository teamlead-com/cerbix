package store

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

func TestReencryptProjectSecretsConvergesAcrossBatches(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA).WithSecretsEnabled(true)
	org, _ := st.CreateOrganization(ctx, "batch-org", "Batch org")
	proj, _ := st.CreateProject(ctx, org.ID, "batch-project", "Batch project")

	const total = reencryptInventoryBatch + 1
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		var id string
		if err := st.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ciphertext, err := cA.EncryptBytes([]byte(fmt.Sprintf("value-%d", i)), secret.CanonicalAAD(proj.ID, id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO project_secrets(id, project_id, name, value_encrypted) VALUES($1,$2,$3,$4)`,
			id, proj.ID, fmt.Sprintf("secret-%03d", i), ciphertext); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	if err := st.reencryptProjectSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	cB, _ := secret.New(keyB)
	for _, id := range ids {
		var ciphertext string
		if err := st.pool.QueryRow(ctx, `SELECT value_encrypted FROM project_secrets WHERE id=$1`, id).Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		needs, err := cB.NeedsReencryptBytes(ciphertext, secret.CanonicalAAD(proj.ID, id))
		if err != nil || needs {
			t.Fatalf("row %s did not converge to primary: needs=%v err=%v", id, needs, err)
		}
	}
}

// TestReencryptProjectSecretCASDoesNotOverwriteRotate exercises the exact
// rotate-vs-reencrypt linearization point. Reencrypt prepares a replacement from an old
// ciphertext, a user rotation commits a newer value, and the stale CAS must lose.
func TestReencryptProjectSecretCASDoesNotOverwriteRotate(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)
	cA, _ := secret.New(keyA)
	st.WithCipher(cA).WithSecretsEnabled(true)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	created, err := st.CreateProjectSecret(ctx, testSecretActor, proj.ID, "database-password", "old-value")
	if err != nil {
		t.Fatal(err)
	}
	var oldCiphertext string
	if err := st.pool.QueryRow(ctx,
		`SELECT value_encrypted FROM project_secrets WHERE id=$1 AND project_id=$2`,
		created.ID, proj.ID).Scan(&oldCiphertext); err != nil {
		t.Fatal(err)
	}

	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	aad := secret.CanonicalAAD(proj.ID, created.ID)
	oldPlain, err := rotated.DecryptBytes(oldCiphertext, aad)
	if err != nil {
		t.Fatal(err)
	}
	staleReplacement, err := rotated.EncryptBytes(oldPlain, aad)
	wipe(oldPlain)
	if err != nil {
		t.Fatal(err)
	}

	newValue := "concurrent-value"
	if _, didRotate, _, err := st.UpdateProjectSecret(ctx, testSecretActor, proj.ID, created.Name, nil, &newValue); err != nil || !didRotate {
		t.Fatalf("concurrent rotate: rotated=%v err=%v", didRotate, err)
	}
	applied, err := st.reencryptProjectSecretCAS(ctx, created.ID, proj.ID, oldCiphertext, staleReplacement)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale reencrypt CAS overwrote a concurrent user rotation")
	}

	var current string
	if err := st.pool.QueryRow(ctx,
		`SELECT value_encrypted FROM project_secrets WHERE id=$1 AND project_id=$2`,
		created.ID, proj.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	plain, err := rotated.DecryptBytes(current, aad)
	if err != nil {
		t.Fatal(err)
	}
	defer wipe(plain)
	if string(plain) != newValue {
		t.Fatalf("stored value = %q, want concurrent rotation", plain)
	}
}

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestReencryptRotatesToPrimary encrypts under key A, rotates the store to a
// keyring [B, A], reencrypts, and proves the data is now readable under B alone
// and no longer under A.
func TestReencryptRotatesToPrimary(t *testing.T) {
	st, ctx := outboxTestStore(t)
	keyA, keyB := randKey(t), randKey(t)

	cA, _ := secret.New(keyA)
	st.WithCipher(cA).WithSecretsEnabled(true)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	inventory, err := st.CreateProjectSecret(ctx, testSecretActor, proj.ID, "database-password", "inventory-secret")
	if err != nil {
		t.Fatalf("create inventory secret: %v", err)
	}
	wh, _ := st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, URL: "https://hook", Secret: "whsecret", Enabled: true})
	ch, _ := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelTelegram, Name: "tg", Enabled: true,
		Config: map[string]string{"bot_token": "TOKEN", "chat_id": "1"},
	})
	// A monitor with an encrypted config secret (password) and a user TOTP secret —
	// both were previously MISSED by reencrypt and would break on old-key removal.
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "db", Type: domain.MonitorTCP, Target: "db:5432",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
		Config: map[string]string{"password": "dbpass"},
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	// A push monitor whose token is encrypted at rest + blind-indexed for lookup.
	push, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "cron", Type: domain.MonitorPush,
		IntervalSeconds: 3600, Enabled: true, PushToken: "cbxp_rotate_me",
	})
	if err != nil {
		t.Fatalf("create push monitor: %v", err)
	}
	usr, _ := st.CreateLocalUser(ctx, "u@x", "U", "pwhash", false)
	encTOTP, _ := cA.Encrypt("totpsecret")
	if _, err := st.pool.Exec(ctx, `UPDATE users SET totp_secret = $2 WHERE id = $1`, usr.ID, encTOTP); err != nil {
		t.Fatalf("seed totp: %v", err)
	}

	// Rotate: new primary B, old A kept for reading, then reencrypt.
	rotated, _ := secret.New(keyB, keyA)
	st.WithCipher(rotated)
	nw, nc, err := st.ReencryptSecrets(ctx)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if nw != 1 || nc != 1 {
		t.Fatalf("reencrypted %d webhooks / %d channels, want 1/1", nw, nc)
	}

	// The raw columns are now decryptable with B alone, and not with A alone.
	var rawSecret, rawCfg string
	_ = st.pool.QueryRow(ctx, `SELECT secret FROM webhooks WHERE id = $1`, wh.ID).Scan(&rawSecret)
	_ = st.pool.QueryRow(ctx, `SELECT config->>'bot_token' FROM notification_channels WHERE id = $1`, ch.ID).Scan(&rawCfg)

	cB, _ := secret.New(keyB)
	if got, err := cB.Decrypt(rawSecret); err != nil || got != "whsecret" {
		t.Fatalf("after reencrypt, B should read webhook secret: got %q err=%v", got, err)
	}
	if got, err := cB.Decrypt(rawCfg); err != nil || got != "TOKEN" {
		t.Fatalf("after reencrypt, B should read channel token: got %q err=%v", got, err)
	}
	if _, err := cA.Decrypt(rawSecret); err == nil {
		t.Fatal("after reencrypt, old key A must no longer read the webhook secret")
	}

	// Monitor config secret and TOTP secret must also have rotated to B.
	var rawMonPw, rawTOTP string
	_ = st.pool.QueryRow(ctx, `SELECT config->>'password' FROM monitors WHERE id = $1`, mon.ID).Scan(&rawMonPw)
	_ = st.pool.QueryRow(ctx, `SELECT totp_secret FROM users WHERE id = $1`, usr.ID).Scan(&rawTOTP)
	if got, err := cB.Decrypt(rawMonPw); err != nil || got != "dbpass" {
		t.Fatalf("after reencrypt, B should read monitor password: got %q err=%v", got, err)
	}
	if got, err := cB.Decrypt(rawTOTP); err != nil || got != "totpsecret" {
		t.Fatalf("after reencrypt, B should read TOTP secret: got %q err=%v", got, err)
	}
	if _, err := cA.Decrypt(rawMonPw); err == nil {
		t.Fatal("after reencrypt, old key A must no longer read the monitor password")
	}
	if _, err := cA.Decrypt(rawTOTP); err == nil {
		t.Fatal("after reencrypt, old key A must no longer read the TOTP secret")
	}

	var rawInventory string
	if err := st.pool.QueryRow(ctx, `SELECT value_encrypted FROM project_secrets WHERE id=$1 AND project_id=$2`, inventory.ID, proj.ID).Scan(&rawInventory); err != nil {
		t.Fatal(err)
	}
	aad := secret.CanonicalAAD(proj.ID, inventory.ID)
	if got, err := cB.DecryptBytes(rawInventory, aad); err != nil || string(got) != "inventory-secret" {
		t.Fatalf("after reencrypt, B should read inventory secret: got %q err=%v", got, err)
	}
	if _, err := cA.DecryptBytes(rawInventory, aad); err == nil {
		t.Fatal("after convergence, old key A must no longer read inventory secret")
	}

	// The push token must also have rotated to B, stay looked-up-able by its blind
	// index, and never appear in plaintext at rest.
	var rawPushEnc, rawPushHash string
	_ = st.pool.QueryRow(ctx, `SELECT push_token_enc, push_token_hash FROM monitors WHERE id = $1`, push.ID).Scan(&rawPushEnc, &rawPushHash)
	if got, err := cB.Decrypt(rawPushEnc); err != nil || got != "cbxp_rotate_me" {
		t.Fatalf("after reencrypt, B should read push token: got %q err=%v", got, err)
	}
	if _, err := cA.Decrypt(rawPushEnc); err == nil {
		t.Fatal("after reencrypt, old key A must no longer read the push token")
	}
	if rawPushEnc == "cbxp_rotate_me" {
		t.Fatal("push token stored in plaintext at rest")
	}
	if rawPushHash != HashToken("cbxp_rotate_me") {
		t.Fatalf("push token blind index = %q, want HashToken(token)", rawPushHash)
	}
	if got, _, err := st.GetMonitorByPushToken(ctx, "cbxp_rotate_me"); err != nil || got.ID != push.ID {
		t.Fatalf("lookup by push token after rotation: %+v err=%v", got, err)
	}
}

// TestBackfillPushTokenEnc proves the readiness-gated backfill encrypts a plaintext
// (migration-seeded) push token, is idempotent (already-encrypted rows untouched), and
// leaves lookup working — no plaintext bearer token survives once a key is configured.
func TestBackfillPushTokenEnc(t *testing.T) {
	st, ctx := outboxTestStore(t)
	key := randKey(t)
	c, _ := secret.New(key)
	st.WithCipher(c)
	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "cron", Type: domain.MonitorPush,
		IntervalSeconds: 3600, Enabled: true, PushToken: "cbxp_seed",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate the 00053 seed: force push_token_enc back to plaintext.
	if _, err := st.pool.Exec(ctx, `UPDATE monitors SET push_token_enc = 'cbxp_seed' WHERE id=$1`, mon.ID); err != nil {
		t.Fatalf("seed plaintext: %v", err)
	}

	n, err := st.BackfillPushTokenEnc(ctx)
	if err != nil || n != 1 {
		t.Fatalf("backfill = %d err=%v, want 1", n, err)
	}
	var raw string
	_ = st.pool.QueryRow(ctx, `SELECT push_token_enc FROM monitors WHERE id=$1`, mon.ID).Scan(&raw)
	if !secret.IsEncrypted(raw) {
		t.Fatalf("push token still plaintext after backfill: %q", raw)
	}
	if got, _ := c.Decrypt(raw); got != "cbxp_seed" {
		t.Fatalf("encrypted token decrypts to %q, want cbxp_seed", got)
	}
	// Idempotent: a second run converts nothing.
	if n2, _ := st.BackfillPushTokenEnc(ctx); n2 != 0 {
		t.Fatalf("second backfill = %d, want 0 (idempotent)", n2)
	}
	// Lookup still works (blind index unaffected).
	if got, _, err := st.GetMonitorByPushToken(ctx, "cbxp_seed"); err != nil || got.ID != mon.ID {
		t.Fatalf("lookup after backfill: %+v err=%v", got, err)
	}
}
