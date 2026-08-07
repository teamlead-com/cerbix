package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending schema migrations against the given DSN. It uses
// goose as a library with embedded SQL, so no external goose CLI is required.
// migrateLockKey is the advisory-lock key that serializes concurrent migrators. Every role
// runs Migrate at startup, so in a distributed deploy several processes race goose — and a
// loser fails with "relation already exists". Holding this session lock on a dedicated
// connection makes the first process migrate while the rest block, then run a no-op.
const migrateLockKey int64 = 0x6365726269780002 // "cerbix" + slot 2 (distinct from the scheduler lock)

func Migrate(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("store: open migrate db: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Pin one connection for the session advisory lock (lock scope = connection), so it is
	// held for this process's entire goose run and released when the connection is returned.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("store: acquire migrate lock: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateLockKey) }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("store: migrate up: %w", err)
	}
	return nil
}
