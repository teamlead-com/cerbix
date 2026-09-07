package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"

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

// minServerVersionNum is the oldest PostgreSQL cerbix's SCHEMA can be created on, expressed the way
// the server reports it: 150000 is 15.0. It is a schema fact, not a preference — see the check in
// Migrate for which migrations depend on it.
const minServerVersionNum = 150000

// pg15Migrations names every migration that uses PostgreSQL 15's column-list
// `ON DELETE SET NULL (col)` form, and it is the ONE list (D5).
//
// The comment above the version check and the operator-facing refusal below it both used to state
// it by hand, and they disagreed: the comment was corrected to six when 00093 arrived, the message
// was not, and the person reading the refusal was told five. One variable cannot half-land.
//
// It is a literal and not a scan of the embedded files, because the refusal must be produceable
// with no database and no I/O — and because a list this short is checked against the tree by
// `make docs-check`, which derives the true set from the migration bodies and fails on any site
// that states a different one, whatever words it uses around it.
var pg15Migrations = []string{"00070", "00080", "00081", "00082", "00084", "00093"}

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
	// The server VERSION is checked before the lock is even taken, and the reason is a
	// production failure rather than a precaution: six migrations use PostgreSQL 15's
	// column-list `ON DELETE SET NULL (col)` form (00070, 00080, 00081, 00082, 00084, 00093),
	// which is
	// a SYNTAX error on 14 and below.
	//
	// The list below and the one in the refusal message are the SAME list, from `pg15Migrations`
	// (D5). They were two hand-written lists and they had already drifted: this comment was
	// corrected to six when 00093 arrived and the operator-facing message beside it still said
	// five, so the person who most needed the list — the one whose upgrade just refused — was
	// handed the shorter one. Half a patch landed, and the guard could not see it because it
	// matched the comment's phrasing and the message is worded differently (F3). Without this check the upgrade dies mid-run on 00070 with
	// a parser error naming a file, leaving an operator to work out from a truncated log that
	// the problem is their server and not their data. The requirement is not new — every
	// document and the CI matrix have said Postgres 16 — it was simply never enforced anywhere
	// a person would see it.
	//
	// It runs BEFORE pg_advisory_lock, and that ordering is the fix for a bug in this check's
	// first version: placed after the lock but before the unlock `defer`, its early returns left the
	// session lock held and relied on `db.Close()` to tear the backend down. Reading a GUC needs no
	// lock, so the question disappears instead of being handled.
	//
	// The check reads `server_version_num` (an integer like 140012), because parsing
	// `version()` text is how people end up comparing "15beta1" with "9.6" as strings.
	var serverNum int
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverNum); err != nil {
		return fmt.Errorf("store: read server version: %w", err)
	}
	if serverNum < minServerVersionNum {
		return fmt.Errorf("store: PostgreSQL %d.%d is too old: cerbix needs %d or newer, because its "+
			"schema uses the column-list ON DELETE SET NULL form introduced in PostgreSQL 15 "+
			"(migrations %s). Nothing has been applied",
			serverNum/10000, serverNum%100, minServerVersionNum/10000, strings.Join(pg15Migrations, ", "))
	}
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
