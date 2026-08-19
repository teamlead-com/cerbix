package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A production upgrade to v0.1.5-beta.1 died on 00070 with `syntax error at or near "("` because the
// server was PostgreSQL 14 and five migrations use the column-list `ON DELETE SET NULL (col)` form
// that arrived in 15. The schema requirement was real all along; nothing enforced it where an
// operator would see it, and the failure surfaced as a parser error naming a file.
//
// This test pins the guard against the DATABASE IT IS GIVEN, rather than requiring a PG14 in CI:
//   - on a server older than 15 it must refuse, name the version, and leave the database untouched;
//   - on 15 or newer it must not refuse (a guard that blocks supported servers is worse than none).
//
// Reproduced by hand on postgres:14-alpine (14.24): without the guard, goose applies 00061…00069 and
// then fails inside 00070; with it, `goose_db_version` is never even created.
func TestMigrateRefusesAServerTooOldForItsOwnSchema(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run migration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The server's own answer decides which half of the property this run proves.
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	var serverNum int
	if err := st.pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverNum); err != nil {
		t.Fatalf("read server version: %v", err)
	}

	err = Migrate(ctx, dsn)
	if serverNum >= minServerVersionNum {
		if err != nil {
			t.Fatalf("a supported server (%d) was refused or failed: %v", serverNum, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("PostgreSQL %d was accepted: the schema cannot be created there, and 00070 would "+
			"fail mid-run with a parser error instead of this refusal", serverNum)
	}
	for _, want := range []string{"too old", "15 or newer", "00070", "Nothing has been applied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q — an operator reading one truncated log line needs "+
				"the version, the requirement and the fact that nothing was applied: %v", want, err)
		}
	}
}
