package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

const channelColumns = "id, project_id, type, name, config, enabled, created_at"

// scanChannel scans a row and decrypts each config value (a no-op for plaintext
// or when encryption is disabled).
func (s *Store) scanChannel(row pgx.Row) (domain.NotificationChannel, error) {
	var (
		ch     domain.NotificationChannel
		config []byte
	)
	if err := row.Scan(&ch.ID, &ch.ProjectID, &ch.Type, &ch.Name, &config, &ch.Enabled, &ch.CreatedAt); err != nil {
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
	return ch, nil
}

// CreateNotificationChannel inserts a channel (validated in domain). Config values
// (credentials, tokens, hook URLs) are encrypted at rest when a cipher is set.
func (s *Store) CreateNotificationChannel(ctx context.Context, ch domain.NotificationChannel) (domain.NotificationChannel, error) {
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
		`INSERT INTO notification_channels (project_id, type, name, config, enabled)
		 VALUES ($1,$2,$3,$4,$5) RETURNING `+channelColumns,
		ch.ProjectID, ch.Type, ch.Name, string(cfg), ch.Enabled)
	created, err := s.scanChannel(row)
	if err != nil {
		return domain.NotificationChannel{}, fmt.Errorf("store: create channel: %w", err)
	}
	return created, nil
}

// GetNotificationChannel returns a channel by id, or ErrNotFound.
func (s *Store) GetNotificationChannel(ctx context.Context, id string) (domain.NotificationChannel, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1`, id)
	ch, err := s.scanChannel(row)
	if noRows(err) {
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
		`SELECT `+channelColumns+` FROM notification_channels WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()
	return s.collectChannels(rows)
}

// ListMonitorChannels lists the channels linked to a monitor.
func (s *Store) ListMonitorChannels(ctx context.Context, monitorID string) ([]domain.NotificationChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+prefixed(channelColumns, "c")+` FROM notification_channels c
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
		`SELECT `+prefixed(channelColumns, "c")+` FROM notification_channels c
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
		`SELECT `+prefixed(channelColumns, "c")+` FROM notification_channels c
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

// DeleteNotificationChannel removes a channel (and its monitor links via cascade).
func (s *Store) DeleteNotificationChannel(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LinkMonitorChannel links a monitor to a channel (idempotent).
func (s *Store) LinkMonitorChannel(ctx context.Context, monitorID, channelID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO monitor_notifications (monitor_id, channel_id) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, monitorID, channelID)
	if err != nil {
		return fmt.Errorf("store: link monitor channel: %w", err)
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
