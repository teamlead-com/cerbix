package store

import (
	"crypto/rand"
	"testing"

	"git.example.com/monitoring/cerbix/internal/domain"
	"git.example.com/monitoring/cerbix/internal/secret"
)

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
	st.WithCipher(cA)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	wh, _ := st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, URL: "https://hook", Secret: "whsecret", Enabled: true})
	ch, _ := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelTelegram, Name: "tg", Enabled: true,
		Config: map[string]string{"bot_token": "TOKEN", "chat_id": "1"},
	})

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
}
