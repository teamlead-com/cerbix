// Package store is the infra layer: a Postgres-backed persistence implementation
// (pgx v5) for the cerbix domain entities. It owns technical guarantees
// (connections, transactions, SQL) but not business rules — those live in
// package domain and are re-checked here only where the schema enforces them.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.example.com/monitoring/cerbix/internal/secret"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// Store wraps a pgx connection pool.
type Store struct {
	pool   *pgxpool.Pool
	cipher *secret.Cipher // nil = secrets stored/read as plaintext
	// timescale reports that heartbeats is a TimescaleDB hypertable (migration
	// 00043 converts it when the extension is installed). Partition maintenance
	// branches on it: hypertables auto-create chunks and are purged with
	// drop_chunks; without the extension the declarative daily partitions are
	// managed by hand. Detected once at Open.
	timescale bool
}

// WithCipher enables at-rest encryption of secret columns (webhook secrets,
// notification-channel credentials). A nil cipher leaves them as plaintext.
func (s *Store) WithCipher(c *secret.Cipher) *Store {
	s.cipher = c
	return s
}

// Open creates a Store from a Postgres DSN and verifies connectivity.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.detectTimescale(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// detectTimescale records whether heartbeats runs as a TimescaleDB hypertable.
// Two steps because timescaledb_information only exists with the extension.
func (s *Store) detectTimescale(ctx context.Context) error {
	var ext bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`,
	).Scan(&ext); err != nil {
		return fmt.Errorf("store: detect timescaledb extension: %w", err)
	}
	if !ext {
		return nil
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM timescaledb_information.hypertables WHERE hypertable_name = 'heartbeats')`,
	).Scan(&s.timescale); err != nil {
		return fmt.Errorf("store: detect heartbeats hypertable: %w", err)
	}
	return nil
}

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close releases the pool.
func (s *Store) Close() {
	s.pool.Close()
}

// TruncateAll clears all domain tables. Intended for tests and local dev only;
// it cascades and resets the tenant data set.
func (s *Store) TruncateAll(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`TRUNCATE instance_settings, oidc_settings, region_worker_alerts, escalation_policies, oncall_schedules, oncall_overrides, pull_jobs, pull_tests, agent_heartbeats, agent_tokens, outbox_events, heartbeats_daily, monitor_notifications, notification_channels, webhooks, api_tokens, components, status_pages, postmortems, incident_updates, incidents, sla_targets, maintenance_windows, heartbeats, monitors, sessions, auth_flows, memberships, projects, users, organizations RESTART IDENTITY CASCADE`)
	if err != nil {
		return fmt.Errorf("store: truncate all: %w", err)
	}
	// TRUNCATE empties rows but leaves the daily heartbeat partitions that are
	// created at runtime (EnsureHeartbeatPartitions / tests). Drop them so the
	// table returns to its migration baseline (parent + heartbeats_default),
	// otherwise a re-run against a persistent test DB collides on partition
	// creation. Names are catalog-sourced and regex-validated (see
	// heartbeatPartitionNames), safe to interpolate.
	names, err := s.heartbeatPartitionNames(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
			return fmt.Errorf("store: drop partition %s: %w", name, err)
		}
	}
	return nil
}

func noRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
