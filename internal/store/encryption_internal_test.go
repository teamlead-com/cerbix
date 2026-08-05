package store

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

func TestSecretsEncryptedAtRest(t *testing.T) {
	st, ctx := outboxTestStore(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.New(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st.WithCipher(cipher)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")

	// Webhook signing secret: plaintext to callers, ciphertext in the column.
	wh, err := st.CreateWebhook(ctx, domain.Webhook{OrgID: org.ID, URL: "https://hook.example", Secret: "topsecret", Enabled: true})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if wh.Secret != "topsecret" {
		t.Fatalf("create returned secret %q, want plaintext", wh.Secret)
	}
	var rawSecret string
	if err := st.pool.QueryRow(ctx, `SELECT secret FROM webhooks WHERE id = $1`, wh.ID).Scan(&rawSecret); err != nil {
		t.Fatalf("read raw secret: %v", err)
	}
	if rawSecret == "topsecret" || !strings.HasPrefix(rawSecret, "enc:v1:") {
		t.Fatalf("webhook secret not encrypted at rest: %q", rawSecret)
	}
	if got, _ := st.GetWebhook(ctx, wh.ID); got.Secret != "topsecret" {
		t.Fatalf("GetWebhook secret = %q, want decrypted plaintext", got.Secret)
	}

	// Notification-channel credentials: encrypted per value.
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelTelegram, Name: "tg", Enabled: true,
		Config: map[string]string{"bot_token": "SECRETTOKEN", "chat_id": "42"},
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if ch.Config["bot_token"] != "SECRETTOKEN" {
		t.Fatalf("create returned bot_token %q, want plaintext", ch.Config["bot_token"])
	}
	var rawCfg string
	if err := st.pool.QueryRow(ctx, `SELECT config::text FROM notification_channels WHERE id = $1`, ch.ID).Scan(&rawCfg); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if strings.Contains(rawCfg, "SECRETTOKEN") || !strings.Contains(rawCfg, "enc:v1:") {
		t.Fatalf("channel config not encrypted at rest: %q", rawCfg)
	}
	if got, _ := st.GetNotificationChannel(ctx, ch.ID); got.Config["bot_token"] != "SECRETTOKEN" {
		t.Fatalf("GetNotificationChannel bot_token = %q, want decrypted", got.Config["bot_token"])
	}

	// A legacy plaintext row (written before encryption was enabled) still reads.
	var legacyID string
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO webhooks (org_id, url, secret, enabled) VALUES ($1, 'https://legacy', 'plainlegacy', true) RETURNING id`,
		org.ID).Scan(&legacyID); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if got, _ := st.GetWebhook(ctx, legacyID); got.Secret != "plainlegacy" {
		t.Fatalf("legacy plaintext secret = %q, want passthrough", got.Secret)
	}
}

func TestMonitorConfigPasswordEncrypted(t *testing.T) {
	st, ctx := outboxTestStore(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, _ := secret.New(key)
	st.WithCipher(cipher)

	org, _ := st.CreateOrganization(ctx, "acme", "Acme")
	proj, _ := st.CreateProject(ctx, org.ID, "api", "API")
	m, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj.ID, Name: "db", Type: domain.MonitorPostgres, Target: "db:5432", IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"username": "cerbix", "password": "topsecret", "database": "app"},
	})
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	// Returned to the caller as plaintext (the prober needs it).
	if m.Config["password"] != "topsecret" {
		t.Fatalf("create returned password %q, want plaintext", m.Config["password"])
	}
	// Encrypted in the column; username stays plaintext (not a secret key).
	var raw string
	if err := st.pool.QueryRow(ctx, `SELECT config->>'password' FROM monitors WHERE id = $1`, m.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if raw == "topsecret" || !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatalf("password not encrypted at rest: %q", raw)
	}
	var user string
	_ = st.pool.QueryRow(ctx, `SELECT config->>'username' FROM monitors WHERE id = $1`, m.ID).Scan(&user)
	if user != "cerbix" {
		t.Fatalf("username should be plaintext, got %q", user)
	}
	// GetMonitor round-trips to plaintext.
	got, _ := st.GetMonitor(ctx, m.ID)
	if got.Config["password"] != "topsecret" {
		t.Fatalf("get returned %q, want decrypted", got.Config["password"])
	}
}
