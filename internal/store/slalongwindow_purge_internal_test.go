package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type purgeRaceRawQueryKey struct{}
type purgeRaceTracer struct {
	st     *Store
	cutoff time.Time
	fired  bool
	err    error
}

func (tr *purgeRaceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	raw := strings.Contains(d.SQL, "count(*)") && strings.Contains(d.SQL, "FROM heartbeats h")
	return context.WithValue(ctx, purgeRaceRawQueryKey{}, raw)
}
func (tr *purgeRaceTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	raw, _ := ctx.Value(purgeRaceRawQueryKey{}).(bool)
	if !raw || tr.fired || d.Err != nil {
		return
	}
	tr.fired = true
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, tr.err = tr.st.PurgeOldHeartbeats(c, tr.cutoff)
}

func purgeRaceDailyPartitions(t *testing.T, f longWindowFixture, from int) {
	t.Helper()
	if f.st.timescale {
		return
	}
	for d := from; d < 0; d++ {
		day := f.today.AddDate(0, 0, d)
		q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS heartbeats_p%s PARTITION OF heartbeats FOR VALUES FROM ('%s') TO ('%s')`, day.Format("20060102"), day.Format(pgTimestamp), day.AddDate(0, 0, 1).Format(pgTimestamp))
		if _, err := f.st.pool.Exec(f.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// iter-0199 re-review RR1-1 (the reviewer's reproducer, made permanent): pause at a real pgx
// query-completion boundary right after a reader's raw aggregate, run the ordinary purge on a
// separate connection, then let the reader continue. A reader that takes its raw counts and its
// rollup boundary from two statements sees two database states and counts the purged day twice —
// raw before the purge, rollup after it. Every reader must read both in ONE statement (one
// snapshot), so there is no point between them for a purge to land.
func TestLongWindowPurgeBetweenReadsCountsNoDayTwice(t *testing.T) {
	for _, reader := range []string{"monitor", "project", "page"} {
		t.Run(reader, func(t *testing.T) {
			f := newLongWindowFixture(t)
			mon := f.monitor(t, "purge-interleave")
			purgeRaceDailyPartitions(t, f, -6)
			f.beats(t, mon, -6, 0, 1, -3, 1)
			f.purge(t, 3)
			since := f.today.AddDate(0, 0, -90)
			before, err := f.st.MonitorSLI(f.ctx, mon, since)
			if err != nil || before.Total != 6 || before.Up != 5 {
				t.Fatalf("control: %+v %v", before, err)
			}
			tr := &purgeRaceTracer{st: f.st, cutoff: f.today.AddDate(0, 0, -2)}
			cfg, err := pgxpool.ParseConfig(os.Getenv("CERBIX_TEST_DATABASE_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.Tracer = tr
			pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			readStore := &Store{pool: pool, timescale: f.st.timescale}
			var got SLICounts
			switch reader {
			case "monitor":
				got, err = readStore.MonitorSLI(f.ctx, mon, since)
			case "project":
				got, err = readStore.ProjectSLI(f.ctx, f.project, since)
			case "page":
				var page map[string]MonitorPageProjection
				page, err = readStore.MonitorPageProjections(f.ctx, []string{mon}, since, true)
				if err == nil {
					if page[mon].Uptime == nil {
						t.Fatal("nil uptime")
					}
					t.Logf("interleaved page uptime=%.9f want=%.9f", *page[mon].Uptime, 5.0/6*100)
					if delta := *page[mon].Uptime - 5.0/6*100; delta < -1e-9 || delta > 1e-9 {
						t.Errorf("purged day counted twice: uptime=%.9f", *page[mon].Uptime)
					}
				}
			}
			if err != nil || !tr.fired || tr.err != nil {
				t.Fatalf("interleave setup: reader=%v fired=%v purge=%v", err, tr.fired, tr.err)
			}
			if reader != "page" {
				t.Logf("interleaved %s=%d/%d want=5/6", reader, got.Up, got.Total)
				if got.Total != 6 || got.Up != 5 {
					t.Errorf("purged day counted twice: %+v", got)
				}
			}
			after, err := f.st.MonitorSLI(f.ctx, mon, since)
			if err != nil || after.Total != 6 || after.Up != 5 {
				t.Fatalf("post-purge control: %+v %v", after, err)
			}
		})
	}
}
