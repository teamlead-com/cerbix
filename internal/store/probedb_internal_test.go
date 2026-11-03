package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// probeDropTimeout bounds a probe database's cleanup. DROP DATABASE waits for an immediate
// checkpoint, and on the shared test containers a checkpoint has been measured at 47 s
// (incidentrepair), 56 s and 55 s (the iter-0207 review's sampler, TimescaleDB and plain) — so a
// test's own 30–60 s context, already partly spent, failed tests whose logic had passed
// (TestAPreBarrierDeadRowIsFencedByTheMigration, plain PostgreSQL, iter-0207 round 2). Three
// minutes, as statuspageprojection and incidentrepair already use, is about three times the
// longest drop seen: a margin, not a measured ceiling.
const probeDropTimeout = 3 * time.Minute

type probeExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// dropProbeDatabase drops a probe database on a fresh context of its own, never the test's: the
// cleanup is mandatory (a leaked probe is a real defect, so it still fails loudly), but how long
// a checkpoint takes says nothing about the code under test.
func dropProbeDatabase(t *testing.T, db probeExecer, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), probeDropTimeout)
	defer cancel()
	if _, err := db.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
		t.Errorf("drop probe database %s: %v", name, err)
	}
}
