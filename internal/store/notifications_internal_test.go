package store

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

// TestUpdateNotificationChannelRewritesNameAndConfig proves the edit path writes
// what Create writes: the new config is encrypted at rest and comes back decrypted,
// the row keeps its id, type and project, and a channel that no longer validates is
// refused before any statement runs.
func TestUpdateNotificationChannelRewritesNameAndConfig(t *testing.T) {
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
	ch, err := st.CreateNotificationChannel(ctx, domain.NotificationChannel{
		ProjectID: proj.ID, Type: domain.ChannelTelegram, Name: "tg", Enabled: true,
		Config: map[string]string{"bot_token": "OLD-TOKEN", "chat_id": "42"},
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	updated, err := st.UpdateNotificationChannel(ctx, domain.NotificationChannel{
		ID: ch.ID, ProjectID: proj.ID, Type: domain.ChannelTelegram, Name: "tg-renamed",
		Config: map[string]string{"bot_token": "NEW-TOKEN", "chat_id": "77"},
	})
	if err != nil {
		t.Fatalf("update channel: %v", err)
	}
	if updated.ID != ch.ID || updated.ProjectID != proj.ID || updated.Type != domain.ChannelTelegram {
		t.Fatalf("update changed identity: %+v", updated)
	}
	if updated.Name != "tg-renamed" || updated.Config["chat_id"] != "77" || updated.Config["bot_token"] != "NEW-TOKEN" {
		t.Fatalf("update returned %+v", updated)
	}
	if !updated.Enabled {
		t.Fatal("update must not touch enabled")
	}

	// Encrypted at rest, exactly as Create leaves it.
	var rawCfg string
	if err := st.pool.QueryRow(ctx, `SELECT config::text FROM notification_channels WHERE id = $1`, ch.ID).Scan(&rawCfg); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if strings.Contains(rawCfg, "NEW-TOKEN") {
		t.Fatalf("updated bot_token stored in plaintext: %s", rawCfg)
	}
	if !strings.Contains(rawCfg, "enc:v1:") {
		t.Fatalf("updated config not encrypted: %s", rawCfg)
	}
	got, err := st.GetNotificationChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if got.Config["bot_token"] != "NEW-TOKEN" {
		t.Fatalf("stored bot_token = %q, want the new one decrypted", got.Config["bot_token"])
	}

	// A merged config that no longer satisfies the type is refused, and the stored
	// row is untouched.
	if _, err := st.UpdateNotificationChannel(ctx, domain.NotificationChannel{
		ID: ch.ID, ProjectID: proj.ID, Type: domain.ChannelTelegram, Name: "tg-renamed",
		Config: map[string]string{"bot_token": "NEW-TOKEN"},
	}); err == nil {
		t.Fatal("update without chat_id must fail")
	}
	if again, _ := st.GetNotificationChannel(ctx, ch.ID); again.Config["chat_id"] != "77" {
		t.Fatalf("refused update mutated the row: %+v", again)
	}

	// An unknown id is ErrNotFound, not a silent no-op.
	if _, err := st.UpdateNotificationChannel(ctx, domain.NotificationChannel{
		ID: "00000000-0000-0000-0000-000000000000", ProjectID: proj.ID, Type: domain.ChannelTelegram,
		Name: "ghost", Config: map[string]string{"bot_token": "t", "chat_id": "1"},
	}); err != ErrNotFound {
		t.Fatalf("update unknown id = %v, want ErrNotFound", err)
	}
}
