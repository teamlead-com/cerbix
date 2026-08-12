// Package store is the infra layer: a Postgres-backed persistence implementation
// (pgx v5) for the cerbix domain entities. It owns technical guarantees
// (connections, transactions, SQL) but not business rules — those live in
// package domain and are re-checked here only where the schema enforces them.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamlead-com/cerbix/internal/secret"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// ErrAlreadyOpen is returned by CreateIncident when a monitor already has an open
// auto-incident (the incidents_one_open_auto partial unique index fired). The
// caller treats it as a benign no-op — the concurrent create won the race.
var ErrAlreadyOpen = errors.New("store: auto-incident already open")

// ErrConflict is returned when a create violates a unique constraint (e.g. a
// duplicate slug), so the API can answer 409 instead of a raw 500.
var ErrConflict = errors.New("store: conflict")

// isUniqueViolation reports whether err is a Postgres unique-constraint (23505) error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Pool sizing. Several long-lived connections are checked out for the whole
// process lifetime — the scheduler's leadership advisory lock plus the LISTEN
// notifiers (confirm-phase, pull) — so pgx's default MaxConns of max(4, numCPU)
// can starve query traffic on a small host (2 CPUs → 4 conns, 3 pinned, 1 left).
// Enforce a floor and set lifetimes/health-check so dead connections are pruned.
//
// The floor alone is NOT enough once Monitoring-as-Code file providers are in play:
// each configured file provider elects its own leader and pins ONE connection for the
// leader's whole life (fencing, see LeaderSession). With up to maxConfiguredFileProviders
// (64) providers, that many pinned conns would exhaust a floor-sized pool and block both
// further leader acquisition AND the reconcile pool queries (FileProviderProjects/Counts)
// — a subsystem deadlock. So Open sizes MaxConns to fit every leader pin plus reconcile
// and app headroom; see RequiredMaxConns.
const (
	poolMinConns          = 2
	poolMaxConnsFloor     = 12
	poolMaxConnLifetime   = time.Hour
	poolMaxConnIdleTime   = 30 * time.Minute
	poolHealthCheckPeriod = time.Minute
	// poolPinnedAppBaseline covers the connections that OTHER subsystems pin for the whole
	// process lifetime, independent of file providers: the scheduler's leadership advisory
	// lock and the Confirm/Config/Pull LISTEN notifiers. Like file-provider leaders these
	// are checked out for life, so they must be reserved on top of the leader pins.
	poolPinnedAppBaseline = 4
	// poolQueryHeadroom is a small extra reserve for ordinary, short-lived pooled query
	// traffic (API reads, ingest, metrics) so it is not starved by the pinned connections
	// and the concurrent reconcile queries. Kept small on purpose.
	poolQueryHeadroom = 2
)

// openConfig holds Open's tunables set via Option.
type openConfig struct {
	fileProviderCount    int
	reconcileConcurrency int
}

// Option customizes Open (pool sizing so far).
type Option func(*openConfig)

// WithFileProviderPool tells Open that up to fileProviders Monitoring-as-Code file
// providers will run in this process, each pinning ONE leader connection for its whole
// life (fencing, see LeaderSession). reconcileConcurrency is the process-wide cap on
// concurrent reconciles, whose pool queries must not be starved by those pinned leaders.
// Open sizes MaxConns to fit all of them plus an app baseline (see requiredMaxConns).
func WithFileProviderPool(fileProviders, reconcileConcurrency int) Option {
	return func(o *openConfig) {
		o.fileProviderCount = fileProviders
		o.reconcileConcurrency = reconcileConcurrency
	}
}

// RequiredMaxConns is the minimum pool MaxConns that lets every file-provider leader pin
// its connection for life while reconcile queries and the rest of the app still acquire
// connections. It sums the connections that are pinned or in-flight concurrently:
//
//	fileProviders        — one pinned leader conn each (fencing, LeaderSession)
//	reconcileConcurrency  — concurrent reconcile pool queries (FileProviderProjects/Counts)
//	poolPinnedAppBaseline — scheduler lock + Confirm/Config/Pull LISTEN pins (pinned for life)
//	poolQueryHeadroom     — small reserve for ordinary short-lived query traffic
//
// The result is never below poolMaxConnsFloor.
func RequiredMaxConns(fileProviders, reconcileConcurrency int) int32 {
	if fileProviders < 0 {
		fileProviders = 0
	}
	if reconcileConcurrency < 0 {
		reconcileConcurrency = 0
	}
	need := fileProviders + reconcileConcurrency + poolPinnedAppBaseline + poolQueryHeadroom
	if need < poolMaxConnsFloor {
		need = poolMaxConnsFloor
	}
	return int32(need)
}

// dsnSetsMaxConns reports whether the operator EXPLICITLY set a pool_max_conns cap in the
// DSN. Rather than reimplement the libpq keyword/URL grammar (spaces around '=', single-
// quoted values with embedded spaces, backslash escapes, service-file merges) — where a
// hand-rolled scan mis-handles `pool_max_conns = 8`, a quoted password containing the
// literal, or a `://` inside a quoted value — it defers to pgx's own parser, the exact
// first stage pgxpool.ParseConfig runs, and checks RuntimeParams for the pool_max_conns
// key. That is precisely where pgxpool reads the cap (before consuming it), so detection is
// identical to what the pool actually honors, and the literal appearing inside a
// username/password/dbname is never mistaken for a cap. Used so an explicit-but-too-small
// cap fails fast instead of silently deadlocking the file-provider subsystem. A DSN pgx
// cannot parse yields false here; Open's own pgxpool.ParseConfig surfaces the real error.
func dsnSetsMaxConns(dsn string) bool {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return false
	}
	_, ok := cc.RuntimeParams["pool_max_conns"]
	return ok
}

// PoolMaxConns returns the pool's effective MaxConns after Open applied sizing (leader
// pins + reconcile/app headroom, or a larger operator-set cap). Exposed so startup logging
// can report the ACTUAL configured cap rather than only the computed minimum.
func (s *Store) PoolMaxConns() int32 {
	return s.pool.Config().MaxConns
}

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
	// Result-ingest policy (spec func-result-protocol). resultSkew bounds how far ahead of
	// the DB clock a scheduled result's timestamp may be before it is quarantined;
	// resultRetention is the lower bound below which a result is ignored (outside the raw
	// window). Zero → sane fallbacks (skew 5m; retention floor disabled). Set via
	// WithResultPolicy at wiring.
	resultSkew         time.Duration
	resultRetention    time.Duration
	resultRevisionMode string // "enforce" (default) | "observe"
}

// WithResultPolicy sets the timestamp bounds used by RecordScheduledResult. Returns the
// store for chaining, mirroring WithCipher.
func (s *Store) WithResultPolicy(skew, retention time.Duration) *Store {
	s.resultSkew = skew
	s.resultRetention = retention
	return s
}

// WithResultRevisionMode sets the execution_revision gate mode: "observe" tolerates a
// missing revision on a scheduled result (rolling-upgrade window), anything else enforces
// (a missing revision is rejected). A present-but-mismatched revision is ALWAYS rejected.
// Empty/unset behaves as enforce.
func (s *Store) WithResultRevisionMode(mode string) *Store {
	s.resultRevisionMode = mode
	return s
}

// WithCipher enables at-rest encryption of secret columns (webhook secrets,
// notification-channel credentials). A nil cipher leaves them as plaintext.
func (s *Store) WithCipher(c *secret.Cipher) *Store {
	s.cipher = c
	return s
}

// Open creates a Store from a Postgres DSN and verifies connectivity. Pass
// WithFileProviderPool so the pool is sized to fit every file-provider leader pin.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	var oc openConfig
	for _, opt := range opts {
		opt(&oc)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	// Size the pool so every file-provider leader can pin its connection for life (fencing)
	// while reconcile queries and the rest of the app still acquire connections. This is
	// always at least the floor, so it subsumes the old floor-only clamp.
	want := RequiredMaxConns(oc.fileProviderCount, oc.reconcileConcurrency)
	// Honor an operator-set pool_max_conns in the DSN when it still fits the requirement, but
	// fail fast on an explicit cap that is too small: silently keeping it would deadlock the
	// file-provider subsystem (pinned leaders exhaust the pool, reconcile queries never run).
	if dsnSetsMaxConns(dsn) && cfg.MaxConns < want {
		return nil, fmt.Errorf("store: pool_max_conns=%d is too small for %d file provider(s): need at least %d connections (one pinned leader conn per provider plus reconcile/app headroom)", cfg.MaxConns, oc.fileProviderCount, want)
	}
	if cfg.MaxConns < want {
		cfg.MaxConns = want
	}
	if cfg.MinConns < poolMinConns {
		cfg.MinConns = poolMinConns
	}
	cfg.MaxConnLifetime = poolMaxConnLifetime
	cfg.MaxConnIdleTime = poolMaxConnIdleTime
	cfg.HealthCheckPeriod = poolHealthCheckPeriod
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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
	// Safety gate: this is destructive and test/dev-only. Refuse unless the connected
	// database's name marks it as a test database, so a mis-set CERBIX_TEST_DATABASE_DSN
	// (or a stray call) can never wipe a real dev/prod database.
	var dbName string
	if err := s.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return fmt.Errorf("store: truncate all (db check): %w", err)
	}
	if !strings.Contains(strings.ToLower(dbName), "test") {
		return fmt.Errorf("store: refusing TruncateAll on non-test database %q (name must contain \"test\")", dbName)
	}
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
