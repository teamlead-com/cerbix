package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// channelBaseColumns are a channel's stored columns: the original seven, then the typed phone
// columns of FR-041a (§4.3), appended at the END so every scan reads them in this order.
const channelBaseColumns = "id, project_id, type, name, config, enabled, created_at, " +
	"recipient, recipient_user_id, number_masked, number_verified_at, disabled_reason"

// channelColumns lists a channel's columns for a row aliased alias, ending with the computed reason
// it is not a route (D31 rule 1, phoneroute.go) — the source of the DTO's `deliverable` — and the
// display name of a `user` channel's recipient (§4.3: a name, never the email; NULL once the user is
// deleted). The number's ciphertext and hash are never read here: every channel read is
// decrypt-free for the number. The name is recipientNameSQL's.
func channelColumns(alias string) string {
	return prefixed(channelBaseColumns, alias) + ", " + phoneRouteReasonSQL(alias) +
		", (SELECT " + recipientNameSQL(alias+"_rn") + " FROM users " + alias + "_rn WHERE " + alias + "_rn.id = " + alias + ".recipient_user_id)"
}

// recipientNameSQL is how every phone surface names a recipient — a `user` channel's
// recipient_display_name and a delivery's (FR-041a §4.3, §4.5) — from the users row aliased u: the
// display name, never an email. A display name can hold one (the OIDC preferred_username fallback,
// or a person who typed their address), so one with an '@' is cut to the part before the first '@',
// and is NULL when that part is empty — as an empty display name (the column's default) is NULL, never
// "" (revision-7 review M-7). The one spelling of the rule, shared by both reads.
func recipientNameSQL(u string) string {
	return "(CASE WHEN strpos(" + u + ".display_name, '@') > 0 THEN NULLIF(split_part(" + u + ".display_name, '@', 1), '')" +
		" ELSE NULLIF(" + u + ".display_name, '') END)"
}

// scanChannel scans a row and decrypts each config value (a no-op for plaintext
// or when encryption is disabled).
func (s *Store) scanChannel(row pgx.Row) (domain.NotificationChannel, error) {
	var (
		ch                                                   domain.NotificationChannel
		config                                               []byte
		recipient, recipientUser, masked, disabled, notRoute *string
		recipientName                                        *string
		verifiedAt                                           *time.Time
	)
	if err := row.Scan(&ch.ID, &ch.ProjectID, &ch.Type, &ch.Name, &config, &ch.Enabled, &ch.CreatedAt,
		&recipient, &recipientUser, &masked, &verifiedAt, &disabled, &notRoute, &recipientName); err != nil {
		return domain.NotificationChannel{}, err
	}
	ch.Config = map[string]string{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &ch.Config); err != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: decode channel config: %w", err)
		}
	}
	for k, v := range ch.Config {
		plain, err := s.cipher.Decrypt(v)
		if err != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: decrypt channel config %q: %w", k, err)
		}
		ch.Config[k] = plain
	}
	if domain.IsPhoneChannelType(ch.Type) {
		ch.Recipient = deref(recipient)
		ch.RecipientUserID = deref(recipientUser)
		ch.DisabledReason = deref(disabled)
		if ch.Recipient == domain.PhoneRecipientNumber {
			ch.NumberMasked = deref(masked)
			verified := verifiedAt != nil
			ch.NumberVerified = &verified
		}
		if ch.Recipient == domain.PhoneRecipientUser {
			ch.RecipientDisplayName = domain.RecipientName{Present: true, Name: recipientName}
		}
		ch.Deliverable = &domain.ChannelDeliverable{Route: notRoute == nil, Reason: deref(notRoute)}
	}
	return ch, nil
}

// errPhoneChannelWriter refuses an sms or voice channel on the generic writers: their typed columns,
// gates and audit rows belong to CreatePhoneChannel and UpdatePhoneChannel (phone_channels.go).
var errPhoneChannelWriter = errors.New("store: sms and voice channels are written by the phone-channel writers")

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CreateNotificationChannel inserts a channel (validated in domain). Config values
// (credentials, tokens, hook URLs) are encrypted at rest when a cipher is set.
func (s *Store) CreateNotificationChannel(ctx context.Context, ch domain.NotificationChannel) (domain.NotificationChannel, error) {
	if domain.IsPhoneChannelType(ch.Type) {
		return domain.NotificationChannel{}, errPhoneChannelWriter
	}
	if err := ch.Validate(); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: invalid channel: %w", err)
	}
	enc := make(map[string]string, len(ch.Config))
	for k, v := range ch.Config {
		ev, err := s.cipher.Encrypt(v)
		if err != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: encrypt channel config %q: %w", k, err)
		}
		enc[k] = ev
	}
	cfg, err := json.Marshal(enc)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: encode channel config: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO notification_channels AS c (project_id, type, name, config, enabled)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+channelColumns("c"),
		ch.ProjectID, ch.Type, ch.Name, string(cfg), ch.Enabled)
	created, err := s.scanChannel(row)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: create channel: %w", err)
	}
	return created, nil
}

// UpdateNotificationChannel rewrites a channel's name and config (validated in
// domain, config re-encrypted exactly as Create writes it) and returns the stored
// row. Type and project are NOT updated: a channel of another type needs another
// required config, which is a new channel rather than an edit. Enabled has its own
// writer, SetNotificationChannelEnabled, so a pause never rewrites credentials.
func (s *Store) UpdateNotificationChannel(ctx context.Context, ch domain.NotificationChannel) (domain.NotificationChannel, error) {
	if ch.ID == "" {
		return domain.NotificationChannel{}, fmt.Errorf("store: update channel: id is required")
	}
	if domain.IsPhoneChannelType(ch.Type) {
		return domain.NotificationChannel{}, errPhoneChannelWriter
	}
	if err := ch.Validate(); err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: invalid channel: %w", err)
	}
	enc := make(map[string]string, len(ch.Config))
	for k, v := range ch.Config {
		ev, err := s.cipher.Encrypt(v)
		if err != nil {
			return domain.NotificationChannel{}, fmt.Errorf("store: encrypt channel config %q: %w", k, err)
		}
		enc[k] = ev
	}
	cfg, err := json.Marshal(enc)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: encode channel config: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE notification_channels AS c SET name = $2, config = $3 WHERE c.id = $1 RETURNING `+channelColumns("c"),
		ch.ID, ch.Name, string(cfg))
	updated, err := s.scanChannel(row)
	if noRows(err) {
		return domain.NotificationChannel{}, ErrNotFound
	}
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: update channel: %w", err)
	}
	return updated, nil
}

// GetNotificationChannel returns a channel by id, or ErrNotFound — for an id that is not a uuid too,
// which names no channel (FR-041a review P2-9).
func (s *Store) GetNotificationChannel(ctx context.Context, id string) (domain.NotificationChannel, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+channelColumns("c")+` FROM notification_channels c WHERE c.id = $1`, id)
	ch, err := s.scanChannel(row)
	if noRows(err) || isInvalidTextRepresentation(err) {
		return domain.NotificationChannel{}, ErrNotFound
	}
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: get channel: %w", err)
	}
	return ch, nil
}

// ListChannelsByProject lists a project's channels, newest first.
func (s *Store) ListChannelsByProject(ctx context.Context, projectID string) ([]domain.NotificationChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+channelColumns("c")+` FROM notification_channels c WHERE c.project_id = $1 ORDER BY c.created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()
	return s.collectChannels(rows)
}

// ListMonitorChannels lists the channels linked to a monitor.
func (s *Store) ListMonitorChannels(ctx context.Context, monitorID string) ([]domain.NotificationChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+channelColumns("c")+` FROM notification_channels c
		 JOIN monitor_notifications mn ON mn.channel_id = c.id
		 WHERE mn.monitor_id = $1 ORDER BY c.created_at DESC`, monitorID)
	if err != nil {
		return nil, fmt.Errorf("store: list monitor channels: %w", err)
	}
	defer rows.Close()
	return s.collectChannels(rows)
}

// ListEnabledChannelsForMonitor returns the enabled channels linked to a monitor
// (the delivery targets for a transition notification).
func (s *Store) ListEnabledChannelsForMonitor(ctx context.Context, monitorID string) ([]domain.NotificationChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+channelColumns("c")+` FROM notification_channels c
		 JOIN monitor_notifications mn ON mn.channel_id = c.id
		 WHERE mn.monitor_id = $1 AND c.enabled ORDER BY c.created_at DESC`, monitorID)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled monitor channels: %w", err)
	}
	defer rows.Close()
	return s.collectChannels(rows)
}

// ListEnabledChannelsByProject returns every enabled channel in a project (the
// delivery targets for a project-level notification such as a weekly SLA report).
func (s *Store) ListEnabledChannelsByProject(ctx context.Context, projectID string) ([]domain.NotificationChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+channelColumns("c")+` FROM notification_channels c
		 WHERE c.project_id = $1 AND c.enabled ORDER BY c.created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled project channels: %w", err)
	}
	defer rows.Close()
	return s.collectChannels(rows)
}

func (s *Store) collectChannels(rows pgx.Rows) ([]domain.NotificationChannel, error) {
	var out []domain.NotificationChannel
	for rows.Next() {
		ch, err := s.scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan channel: %w", err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channels: %w", err)
	}
	return out, nil
}

// DeleteNotificationChannel removes a channel (and its monitor links via cascade). An sms or voice
// channel is refused (errPhoneChannelWriter): DeletePhoneChannel ends its codes and audits it.
func (s *Store) DeleteNotificationChannel(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1 AND type NOT IN ('sms', 'voice')`, id)
	if err != nil {
		return fmt.Errorf("store: delete channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.phoneChannelOrNotFound(ctx, id)
	}
	return nil
}

// phoneChannelOrNotFound explains a generic write that matched no row: errPhoneChannelWriter when id
// is an sms or voice channel, ErrNotFound otherwise.
func (s *Store) phoneChannelOrNotFound(ctx context.Context, id string) error {
	var phone bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notification_channels
		 WHERE id = $1 AND type IN ('sms', 'voice'))`, id).Scan(&phone); err != nil {
		return fmt.Errorf("store: read channel type: %w", err)
	}
	if phone {
		return errPhoneChannelWriter
	}
	return ErrNotFound
}

// LinkMonitorChannel links a monitor to a channel (idempotent).
type monitorChannelReference struct {
	MonitorID string `tenantref:"monitor-notification-channel"`
	ChannelID string `tenantref:"monitor-notification-channel"`
}

func (s *Store) LinkMonitorChannel(ctx context.Context, monitorID, channelID string) error {
	reference := monitorChannelReference{MonitorID: monitorID, ChannelID: channelID}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin link monitor channel: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var projectID string
	err = tx.QueryRow(ctx,
		`SELECT m.project_id::text
		   FROM monitors m
		   JOIN notification_channels c ON c.id = $2 AND c.project_id = m.project_id
		  WHERE m.id = $1
		  FOR KEY SHARE OF m, c`, reference.MonitorID, reference.ChannelID).Scan(&projectID)
	if noRows(err) {
		return ErrRoutingReferenceNotInProject
	}
	if err != nil {
		return fmt.Errorf("store: resolve monitor channel tenancy: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO monitor_notifications (monitor_id, channel_id, project_id) VALUES ($1,$2,$3)
		 ON CONFLICT DO NOTHING`, reference.MonitorID, reference.ChannelID, projectID)
	if err != nil {
		return fmt.Errorf("store: link monitor channel: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit link monitor channel: %w", err)
	}
	return nil
}

// UnlinkMonitorChannel removes a monitor↔channel link.
func (s *Store) UnlinkMonitorChannel(ctx context.Context, monitorID, channelID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM monitor_notifications WHERE monitor_id = $1 AND channel_id = $2`, monitorID, channelID)
	if err != nil {
		return fmt.Errorf("store: unlink monitor channel: %w", err)
	}
	return nil
}

// prefixed rewrites a comma-separated column list to alias.column form (for joins).
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}

// SetNotificationChannelEnabled pauses or resumes a channel; delivery listing
// filters on the flag already. An sms or voice channel is refused (errPhoneChannelWriter): switching
// one on has gates, which UpdatePhoneChannel applies.
func (s *Store) SetNotificationChannelEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE notification_channels SET enabled = $2
		 WHERE id = $1 AND type NOT IN ('sms', 'voice')`, id, enabled)
	if err != nil {
		return fmt.Errorf("store: set channel enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.phoneChannelOrNotFound(ctx, id)
	}
	return nil
}
