package store

// FR-032 invariant 13a, the source-scan half (D-0237 §17.3): no site may bump the D-0142 config
// fence without writing the matching `monitor_execution_revisions` row in the same transaction.
//
// Four sites use `revisionFenceSetSQL` today and only ONE of them is `updateMonitorTxPrepared` — the other
// three are retire, restore and the secret-rotation fence. `updateMonitorTx`'s own comment calls
// itself "the shared config-write contract", which is true for the user and file paths and is NOT
// the same as being the only place a generation is created; the design spent a revision believing
// otherwise. A generation with no timeline row is a window whose configuration cannot be described,
// so the compiler cannot help here and a guard has to.
//
// It parses rather than greps, and the reason is concrete: `monitors.go` mentions
// `revisionFenceSetSQL` inside a raw SQL string, as an SQL comment, and again in its own doc
// comment. A text scan counts those as uses and reports more sites than exist — a guard that
// miscounts its own subject fails on a correct tree and gets deleted by whoever is unblocking CI.
// `TestTheRevisionFenceGuardCountsReferencesNotTextOccurrences` pins that difference.

import (
	"context"
	"crypto/rand"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

const (
	fenceIdent    = "revisionFenceSetSQL"
	timelineIdent = "writeRevisionTimeline"
)

// referencesIn reports whether a function body contains an identifier reference — not a string
// literal, not a comment — to any of the given names.
func referencesIn(fn *ast.FuncDecl, names ...string) bool {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && want[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

// insertsMonitor reports whether a function body contains an `INSERT INTO monitors` statement.
//
// Deliberately the OPPOSITE detection from referencesIn: the SQL lives INSIDE a string literal, so
// this one inspects literals while the fence check ignores them. Two properties, two detections —
// and the reason both exist is that the reviewer found creation missing when the invariant was
// scoped to "every bump" instead of "every generation". Creation initializes generation 1 and never
// touches the fence, so a fence-keyed guard is blind to it by construction.
func insertsMonitor(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && strings.Contains(lit.Value, "INSERT INTO monitors") {
			found = true
		}
		return !found
	})
	return found
}

// fenceSites names every function that applies the revision fence.
func fenceSites(files []*ast.File) []string {
	var got []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if referencesIn(fn, fenceIdent) {
				got = append(got, fn.Name.Name)
			}
		}
	}
	sort.Strings(got)
	return got
}

// unpairedGenerationSites names every function that CREATES a generation — by bumping the fence or
// by inserting a monitor at generation 1 — without recording what that generation IS.
func unpairedGenerationSites(files []*ast.File) []string {
	var got []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			creates := referencesIn(fn, fenceIdent) || insertsMonitor(fn)
			if creates && !referencesIn(fn, timelineIdent) {
				got = append(got, fn.Name.Name)
			}
		}
	}
	sort.Strings(got)
	return got
}

// textOccurrences is the naive implementation, kept ONLY so a test can prove why it is not used.
func textOccurrences(src string) int {
	return strings.Count(src, fenceIdent)
}

func parseStorePackage(t *testing.T) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse internal/store: %v", err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		t.Fatal("parsed no files — the guard would pass vacuously")
	}
	return files
}

func TestEveryRevisionFenceWritesItsTimelineRow(t *testing.T) {
	files := parseStorePackage(t)
	if got := unpairedGenerationSites(files); len(got) != 0 {
		t.Fatalf("these functions create a generation without writing its timeline row: %v — "+
			"a generation with no row is a window whose configuration cannot be described "+
			"(FR-032 invariant 13a). Creation counts: a monitor starts at generation 1", got)
	}
}

// The count is asserted and the sites enumerated, so a FIFTH bump site is a failure rather than a
// silent pass: a new one that happens to pair correctly still deserves a human deciding whether the
// ledger's assumptions survive it.
func TestTheRevisionFenceSitesAreTheFourWeKnowAbout(t *testing.T) {
	// Enumerated from the tree, not from memory: this guard caught its author guessing all four
	// names wrong on its first run. `updateMonitorTxPrepared` is the prepared variant, which is
	// where the fence actually lives.
	want := []string{"ReactivateMonitor", "RetireMonitor", "fenceSecretMonitors", "updateMonitorTxPrepared"}
	got := fenceSites(parseStorePackage(t))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("revision-fence sites changed:\n  got  %v\n  want %v\n"+
			"a new site must write its timeline row AND be added here deliberately", got, want)
	}
}

// The creation half of the guard, which is the one that was missing.
func TestTheGuardFailsOnACreationThatWritesNoTimelineRow(t *testing.T) {
	const violation = `package store
func insertSomething(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, ` + "`INSERT INTO monitors (project_id, name) VALUES ($1,$2)`" + `, p, n)
	return err
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := unpairedGenerationSites([]*ast.File{file})
	if len(got) != 1 || got[0] != "insertSomething" {
		t.Fatalf("the guard did not see the unpaired CREATION it exists to catch: %v", got)
	}
}

func TestTheRevisionFenceGuardFailsOnAFixtureThatViolatesIt(t *testing.T) {
	const violation = `package store
func (s *Store) someNewWriter(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, ` + "`UPDATE monitors SET `" + `+revisionFenceSetSQL+` + "`" + ` WHERE id = $1` + "`" + `, id)
	return err
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", violation, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := unpairedGenerationSites([]*ast.File{file})
	if len(got) != 1 || got[0] != "someNewWriter" {
		t.Fatalf("the guard did not see the unpaired bump it exists to catch: %v", got)
	}
}

// Why the guard parses instead of grepping, proven rather than asserted in a comment.
func TestTheRevisionFenceGuardCountsReferencesNotTextOccurrences(t *testing.T) {
	const src = `package store
// revisionFenceSetSQL is the fence.                       <- doc comment, not a use
const revisionFenceSetSQL = ` + "`x`" + `                  // <- the definition, not a use
func onlyRealUse(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, ` + "`UPDATE monitors SET -- see revisionFenceSetSQL` + revisionFenceSetSQL" + `)
	return err
}`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if got := fenceSites([]*ast.File{file}); len(got) != 1 || got[0] != "onlyRealUse" {
		t.Fatalf("the AST guard should find exactly one site, got %v", got)
	}
	// The same source, counted the naive way, sees four — the doc comment, the definition, the SQL
	// comment inside the string literal, and the one real reference.
	if n := textOccurrences(src); n <= 1 {
		t.Fatalf("the fixture no longer demonstrates the difference: text scan found %d", n)
	}
}

// ── The behavioural half of invariant 13a ─────────────────────────────────────────────────────
//
// One test per revision-fence site, plus the backfill and the transaction-sharing property. These
// need a real Postgres and skip without CERBIX_TEST_DATABASE_DSN, like every other store test here.

func timelineStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run revision-timeline tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st, ctx
}

func seedTimelineProject(t *testing.T, st *Store, ctx context.Context) string {
	t.Helper()
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	proj, err := st.CreateProject(ctx, org.ID, "api", "API")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return proj.ID
}

// timelineRow reads the row for one generation. A missing row is the failure this whole phase exists
// to prevent, so the helper fails rather than returning a zero value.
func timelineRow(t *testing.T, st *Store, ctx context.Context, monitorID string, rev int64) (iv, confirm, timeout, retries int) {
	t.Helper()
	err := st.pool.QueryRow(ctx,
		`SELECT interval_seconds, confirm_interval_seconds, timeout_seconds, retries
		   FROM monitor_execution_revisions WHERE monitor_id = $1 AND execution_revision = $2`,
		monitorID, rev).Scan(&iv, &confirm, &timeout, &retries)
	if err != nil {
		t.Fatalf("no timeline row for monitor %s generation %d: %v", monitorID, rev, err)
	}
	return
}

func timelineCount(t *testing.T, st *Store, ctx context.Context, monitorID string) (n int) {
	t.Helper()
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM monitor_execution_revisions WHERE monitor_id = $1`, monitorID).Scan(&n); err != nil {
		t.Fatalf("count timeline rows: %v", err)
	}
	return
}

// The four cadence fields are asserted INDIVIDUALLY on purpose: a test that checked only
// interval_seconds would survive dropping confirm_interval_seconds from the write.
func TestUpdateMonitorRecordsItsGenerationWithAllFourCadenceFields(t *testing.T) {
	st, ctx := timelineStore(t)
	proj := seedTimelineProject(t, st, ctx)
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "m", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, ConfirmIntervalSeconds: 10, Retries: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mon.IntervalSeconds, mon.TimeoutSeconds, mon.ConfirmIntervalSeconds, mon.Retries = 120, 9, 30, 3
	updated, err := st.UpdateMonitor(ctx, mon)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ExecutionRevision <= mon.ExecutionRevision {
		t.Fatalf("the fence did not bump: %d then %d", mon.ExecutionRevision, updated.ExecutionRevision)
	}
	iv, confirm, timeout, retries := timelineRow(t, st, ctx, mon.ID, updated.ExecutionRevision)
	if iv != 120 {
		t.Errorf("interval_seconds = %d, want 120", iv)
	}
	if confirm != 30 {
		t.Errorf("confirm_interval_seconds = %d, want 30", confirm)
	}
	if timeout != 9 {
		t.Errorf("timeout_seconds = %d, want 9", timeout)
	}
	if retries != 3 {
		t.Errorf("retries = %d, want 3", retries)
	}
}

func TestRetireAndReactivateEachRecordTheirGeneration(t *testing.T) {
	st, ctx := timelineStore(t)
	proj := seedTimelineProject(t, st, ctx)
	// Retire and reactivate apply to COMPOSITE monitors only — the store says so, and the first
	// version of this test used an http monitor and was told off by name.
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "m", Type: domain.MonitorComposite,
		IntervalSeconds: 60, Region: domain.DefaultRegion, Enabled: true,
		Config: map[string]string{"children": "", "quorum": "1"}})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}
	before := timelineCount(t, st, ctx, mon.ID)

	retired, err := st.RetireMonitor(ctx, proj, mon.ID, GraphActor{Label: "t"})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	timelineRow(t, st, ctx, mon.ID, retired.ExecutionRevision)

	restored, err := st.ReactivateMonitor(ctx, proj, mon.ID, GraphActor{Label: "t"})
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	timelineRow(t, st, ctx, mon.ID, restored.ExecutionRevision)

	// Retire and restore are two generations, so two more rows — not one, and not zero.
	if got := timelineCount(t, st, ctx, mon.ID); got != before+2 {
		t.Fatalf("timeline rows = %d, want %d: retire and restore are two generations", got, before+2)
	}
}

// The BULK case. Three monitors on purpose: a rotation touching ONE monitor would pass against an
// implementation that writes a single row regardless of how many generations it created.
func TestASecretRotationRecordsAGenerationForEveryAffectedMonitor(t *testing.T) {
	st, ctx := timelineStore(t)
	// The secret inventory refuses to work without a configured encryption key, which is the
	// point of FR-020: a secret at rest is never plaintext. The test supplies one.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.New(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st.WithCipher(cipher)
	// FR-020's persistence-boundary gate: `secrets.enabled: false` means nothing else changes,
	// so the inventory refuses even to be read. The rotation fence lives behind it.
	st.WithSecretsEnabled(true)
	proj := seedTimelineProject(t, st, ctx)
	actor := SecretActor{ActorUserID: ""}
	if _, err := st.CreateProjectSecret(ctx, actor, proj, "db-password", "s3cret"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	var ids []string
	var revBefore []int64
	for i, name := range []string{"a", "b", "c"} {
		mon, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj, Name: name, Type: domain.MonitorPostgres, Target: "db" + name + ":5432",
			IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true,
			Config: map[string]string{"password_ref": "db-password", "database": "d", "username": "u"}})
		if err != nil {
			t.Fatalf("create monitor %d: %v", i, err)
		}
		ids = append(ids, mon.ID)
		revBefore = append(revBefore, mon.ExecutionRevision)
	}

	newValue := "rotated"
	if _, rotated, _, err := st.UpdateProjectSecret(ctx, actor, proj, "db-password", nil, &newValue); err != nil {
		t.Fatalf("rotate: %v", err)
	} else if !rotated {
		t.Fatal("the secret was not rotated, so the fence never ran and this test proves nothing")
	}

	for i, id := range ids {
		var rev int64
		if err := st.pool.QueryRow(ctx, `SELECT execution_revision FROM monitors WHERE id = $1`, id).Scan(&rev); err != nil {
			t.Fatalf("read revision %d: %v", i, err)
		}
		if rev <= revBefore[i] {
			t.Fatalf("monitor %d was not fenced by the rotation: %d then %d", i, revBefore[i], rev)
		}
		// Each of the three owes its OWN row for its OWN new generation.
		timelineRow(t, st, ctx, id, rev)
	}
}

// The bump and its row share ONE transaction, so a rollback leaves neither. If the helper opened its
// own connection instead of using the caller's tx, the row would survive this rollback.
func TestTheTimelineRowAndTheBumpShareOneTransaction(t *testing.T) {
	st, ctx := timelineStore(t)
	proj := seedTimelineProject(t, st, ctx)
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "m", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := timelineCount(t, st, ctx, mon.ID)

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE monitors SET `+revisionFenceSetSQL+` WHERE id = $1`, mon.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("fence: %v", err)
	}
	if err := writeRevisionTimeline(ctx, tx, proj, mon.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("timeline: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var rev int64
	if err := st.pool.QueryRow(ctx, `SELECT execution_revision FROM monitors WHERE id = $1`, mon.ID).Scan(&rev); err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if rev != mon.ExecutionRevision {
		t.Errorf("the bump survived the rollback: %d then %d", mon.ExecutionRevision, rev)
	}
	if got := timelineCount(t, st, ctx, mon.ID); got != before {
		t.Errorf("the timeline row survived the rollback: %d rows, want %d", got, before)
	}
}

// The backfill runs inside migration 00100, so by the time a test can seed monitors it has already
// happened. To test it rather than a copy of it, this reads the SHIPPED statement out of the embedded
// migration and runs that — a hand-written duplicate here would drift from the migration silently,
// which is the failure mode this whole phase is about.
func backfillStatementFromMigration(t *testing.T) string {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/00100_monitor_execution_revisions.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	up := string(raw)
	if i := strings.Index(up, "-- +goose Down"); i >= 0 {
		up = up[:i]
	}
	i := strings.Index(up, "INSERT INTO monitor_execution_revisions")
	if i < 0 {
		t.Fatal("migration 00100 no longer contains a backfill INSERT — the test cannot check what is not there")
	}
	stmt := up[i:]
	if j := strings.Index(stmt, ";"); j >= 0 {
		stmt = stmt[:j+1]
	}
	return stmt
}

// Every monitor gets a row — including a DISABLED one, because a disabled monitor's history is still
// history, and a monitor whose generation has no row is a window whose configuration cannot be
// described the moment it is re-enabled.
func TestTheBackfillCoversEveryMonitorIncludingDisabledOnes(t *testing.T) {
	st, ctx := timelineStore(t)
	proj := seedTimelineProject(t, st, ctx)
	for _, tc := range []struct {
		name    string
		enabled bool
	}{{"live", true}, {"paused", false}} {
		if _, err := st.CreateMonitor(ctx, domain.Monitor{
			ProjectID: proj, Name: tc.name, Type: domain.MonitorHTTP, Target: "https://" + tc.name,
			IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: tc.enabled}); err != nil {
			t.Fatalf("create %s: %v", tc.name, err)
		}
	}
	// Clear what CreateMonitor's own path may have written, so the backfill is what is under test.
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitor_execution_revisions`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := st.pool.Exec(ctx, backfillStatementFromMigration(t)); err != nil {
		t.Fatalf("run the migration's backfill: %v", err)
	}

	var monitors, rows int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM monitors`).Scan(&monitors); err != nil {
		t.Fatalf("count monitors: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM monitor_execution_revisions`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if monitors != 2 {
		t.Fatalf("expected two monitors, got %d — the fixture is wrong, not the code", monitors)
	}
	if rows != monitors {
		t.Fatalf("backfill wrote %d rows for %d monitors: every monitor owes exactly one", rows, monitors)
	}
}

// A monitor is CREATED at generation 1, and that generation needs its row as much as a bumped one
// does. The reviewer found this missing: the invariant had been scoped to "every bump", and creation
// initializes rather than bumps, so both the fence-keyed guard and every test were blind to it.
func TestCreatingAMonitorRecordsItsFirstGeneration(t *testing.T) {
	st, ctx := timelineStore(t)
	proj := seedTimelineProject(t, st, ctx)
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: proj, Name: "fresh", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 45, TimeoutSeconds: 7, ConfirmIntervalSeconds: 15, Retries: 2, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if mon.ExecutionRevision != 1 {
		t.Fatalf("a new monitor should start at generation 1, got %d", mon.ExecutionRevision)
	}
	// Before any update at all, generation 1 must already be describable.
	iv, confirm, timeout, retries := timelineRow(t, st, ctx, mon.ID, 1)
	if iv != 45 || confirm != 15 || timeout != 7 || retries != 2 {
		t.Errorf("generation 1 reads %d/%d/%d/%d, want 45/15/7/2", iv, confirm, timeout, retries)
	}
}

// The FILE-managed create path, reached the way the apply loop reaches it. Both paths call
// insertMonitorTx, so this proves the shared contract rather than a second implementation — and if
// someone ever gives file apply its own INSERT, the guard's creation half fails on it.
func TestTheFileApplyCreatePathRecordsItsFirstGenerationToo(t *testing.T) {
	st, ctx := timelineStore(t)
	proj := seedTimelineProject(t, st, ctx)

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	created, err := insertMonitorTx(ctx, tx, st, domain.Monitor{
		ProjectID: proj, Name: "from-file", Type: domain.MonitorHTTP, Target: "https://f",
		IntervalSeconds: 90, TimeoutSeconds: 8, ConfirmIntervalSeconds: 20, Retries: 1, Enabled: true})
	if err != nil {
		t.Fatalf("insertMonitorTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	iv, confirm, timeout, retries := timelineRow(t, st, ctx, created.ID, created.ExecutionRevision)
	if iv != 90 || confirm != 20 || timeout != 8 || retries != 1 {
		t.Errorf("file-created generation reads %d/%d/%d/%d, want 90/20/8/1", iv, confirm, timeout, retries)
	}
}

// The project predicate must REFUSE, not merely be present. Adding `AND m.project_id = $2` and
// writing no test for it would leave a predicate that could be deleted with nothing failing — the
// same defect as a rule with no killing mutation, one layer down. Reviewer P1.
func TestTheTimelineWriteRefusesAMonitorFromAnotherProject(t *testing.T) {
	st, ctx := timelineStore(t)
	org, err := st.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	mineProj, err := st.CreateProject(ctx, org.ID, "mine", "Mine")
	if err != nil {
		t.Fatalf("project mine: %v", err)
	}
	theirsProj, err := st.CreateProject(ctx, org.ID, "theirs", "Theirs")
	if err != nil {
		t.Fatalf("project theirs: %v", err)
	}
	mine, theirs := mineProj.ID, theirsProj.ID
	mon, err := st.CreateMonitor(ctx, domain.Monitor{
		ProjectID: mine, Name: "m", Type: domain.MonitorHTTP, Target: "https://x",
		IntervalSeconds: 60, TimeoutSeconds: 5, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Creation already wrote generation 1; clear it so this test observes only its own write.
	if _, err := st.pool.Exec(ctx, `DELETE FROM monitor_execution_revisions WHERE monitor_id = $1`, mon.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	// The monitor is real and the project is real; they simply do not belong together.
	if err := writeRevisionTimeline(ctx, tx, theirs, mon.ID); err != nil {
		t.Fatalf("the write should be a silent no-op, not an error: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := timelineCount(t, st, ctx, mon.ID); got != 0 {
		t.Fatalf("a cross-project pair wrote %d rows; the project predicate refuses nothing", got)
	}

	// And the same call with the RIGHT project writes exactly one, so the test is not passing
	// because the write is broken for every input.
	tx2, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	defer tx2.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := writeRevisionTimeline(ctx, tx2, mine, mon.ID); err != nil {
		t.Fatalf("same-project write: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	if got := timelineCount(t, st, ctx, mon.ID); got != 1 {
		t.Fatalf("same-project write produced %d rows, want 1", got)
	}
}
