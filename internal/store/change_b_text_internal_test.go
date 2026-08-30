package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D2 text at the store's edges (func-change-intelligence §7 *Phases*/*Bounds*, invariant
// 23; iter-0165 task 2, Agent B): lengths are code points at the domain and characters at the
// CHECK — never bytes — and the whitespace/format-character rules hold on the write path. Every
// non-ASCII fixture is spelled as a Go escape so the file itself carries no format character.

// A 128-code-point CJK external_id (384 bytes) and a 128-emoji ref (512 bytes) are accepted and
// stored whole — the CHECK's char_length counts characters, proved by direct SQL either side of
// the bound; 129 code points are refused by the domain before SQL; the long identity survives
// the cursor across a page boundary.
func TestChangeStoreCountsLengthInCodePointsAndTheCheckInCharactersNeverBytes(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	cjk128 := strings.Repeat("日", 128) // 日

	row := mustRecord(t, st, ctx, changeInput(f, cjk128, domain.ChangePhaseSucceeded, now.Add(-2*time.Minute)))
	var octets, chars int
	if err := st.pool.QueryRow(ctx, `SELECT octet_length(external_id), char_length(external_id) FROM service_changes WHERE id = $1`, row.ID).Scan(&octets, &chars); err != nil {
		t.Fatal(err)
	}
	if octets != 384 || chars != 128 || row.ExternalID != cjk128 {
		t.Fatalf("stored external_id: %d bytes, %d chars, equal=%v", octets, chars, row.ExternalID == cjk128)
	}
	long := changeInput(f, "emoji-ref", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	long.Ref = strings.Repeat("\U0001F680", 128) // 🚀
	if r := mustRecord(t, st, ctx, long); r.Ref != long.Ref {
		t.Fatal("a 128-emoji ref was altered")
	}
	if err := st.pool.QueryRow(ctx, `SELECT octet_length(ref) FROM service_changes WHERE external_id = 'emoji-ref'`).Scan(&octets); err != nil || octets != 512 {
		t.Fatalf("emoji ref octets = %d %v, want 512", octets, err)
	}

	before := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes`)
	if ce := recordCode(t, st, ctx, changeInput(f, cjk128+"日", domain.ChangePhaseSucceeded, now.Add(-time.Minute))); ce.Code != domain.ChangeErrExternalIDInvalid || !strings.Contains(ce.Msg, "129") {
		t.Fatalf("129 code points: %v", ce)
	}
	over := changeInput(f, "over", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
	over.Ref = strings.Repeat("\U0001F680", 129)
	if ce := recordCode(t, st, ctx, over); ce.Code != domain.ChangeErrRefInvalid {
		t.Fatalf("129-emoji ref: %v", ce)
	}
	if countSQL(t, st, ctx, `SELECT count(*) FROM service_changes`) != before {
		t.Fatal("a refused length reached the table")
	}
	// Direct SQL: 129 CJK characters (387 bytes) hit the CHECK; 128 (384 bytes, far over 128 bytes) pass it.
	id, _ := newChangeID(now)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, occurred_at, actor_label, via_token)
		VALUES ($1, $2, $3, 'direct', repeat(U&'\65E5', 129), 'deploy', 'succeeded', $4, 'sql', true)`, id, f.projectID, f.serviceID, now); pgErrCode(err) != "23514" {
		t.Fatalf("direct 129 CJK: %v, want the CHECK", err)
	}
	id, _ = newChangeID(now)
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO service_changes (id, project_id, service_id, source, external_id, kind, phase, occurred_at, actor_label, via_token)
		VALUES ($1, $2, $3, 'direct', repeat(U&'\65E5', 128), 'deploy', 'succeeded', $4, 'sql', true)`, id, f.projectID, f.serviceID, now); err != nil {
		t.Fatalf("direct 128 CJK (384 bytes): %v — the CHECK must count characters", err)
	}
	// The long identity rides the cursor: pages of one, round-trip asserted by the helper.
	pages := listAllGroups(t, st, ctx, f, now.Add(-time.Hour), now.Add(time.Minute), nil, nil, 1)
	if got := groupIDs(pages); len(got) != 3 || got[2] != "github-actions/"+cjk128 {
		t.Fatalf("pages = %d groups, last %q", len(got), got[len(got)-1])
	}
}

// The store refuses at the edge what the transport would have trimmed — a trailing U+00A0 or a
// leading U+0085 — and every format character (U+FEFF, U+200D, U+00AD, a tag character) anywhere;
// an interior U+00A0 and a variation selector are text. Nothing of a refused value reaches SQL.
func TestChangeStoreRefusesEdgeWhitespaceAndFormatCharactersInEveryTextField(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	before := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes`)
	for _, tc := range []struct {
		name string
		mut  func(*RecordChangeInput)
		code string
	}{
		{"trailing NBSP in external_id", func(i *RecordChangeInput) { i.ExternalID = "run-1\u00a0" }, domain.ChangeErrExternalIDInvalid},
		{"leading NEL in external_id", func(i *RecordChangeInput) { i.ExternalID = "\u0085run-1" }, domain.ChangeErrExternalIDInvalid},
		{"BOM in external_id", func(i *RecordChangeInput) { i.ExternalID = "\ufeffrun-1" }, domain.ChangeErrExternalIDInvalid},
		{"ZWJ in ref", func(i *RecordChangeInput) { i.Ref = "\U0001F468\u200d\U0001F469" }, domain.ChangeErrRefInvalid},
		{"soft hyphen in ref", func(i *RecordChangeInput) { i.Ref = "v\u00ad1" }, domain.ChangeErrRefInvalid},
		{"tag character in url", func(i *RecordChangeInput) { i.URL = "https://x.example/\U000E0041" }, domain.ChangeErrURLInvalid},
		{"NBSP inside url", func(i *RecordChangeInput) { i.URL = "https://x.example/a\u00a0b" }, ""},
		{"NBSP inside ref", func(i *RecordChangeInput) { i.Ref = "release\u00a04" }, ""},
		{"variation selector in ref", func(i *RecordChangeInput) { i.Ref = "v1 ✔\ufe0f" }, ""},
	} {
		in := changeInput(f, "edge-"+strings.ReplaceAll(tc.name, " ", "-"), domain.ChangePhaseSucceeded, now.Add(-time.Minute))
		tc.mut(&in)
		_, _, err := st.RecordChangePhase(ctx, in)
		var ce *domain.ChangeError
		switch {
		case tc.code == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.code != "" && (!errors.As(err, &ce) || ce.Code != tc.code):
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
	}
	if n := countSQL(t, st, ctx, `SELECT count(*) FROM service_changes`); n != before+3 {
		t.Fatalf("%d rows written, want exactly the three accepted", n-before)
	}
}

// The service id is accepted in every spelling PostgreSQL parses (upper-case, braces) and stored
// canonical; a text that is not a uuid is ErrNotFound — the lock statement now casts it, and the
// answer must stay the one serviceExistsOn gives.
func TestChangeRecordAcceptsEveryUUIDSpellingAndAMalformedServiceIDIsNotFound(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	for _, bad := range []string{"not-a-uuid", "", "0199a0c0-0000-7000-8000-00000000000", "'; DROP TABLE service_changes; --"} {
		in := changeInput(f, "spell", domain.ChangePhaseSucceeded, now.Add(-time.Minute))
		in.ServiceID = bad
		if _, _, err := st.RecordChangePhase(ctx, in); !errors.Is(err, ErrNotFound) {
			t.Fatalf("service id %q: %v, want ErrNotFound", bad, err)
		}
	}
	braces := changeInput(f, "spell", domain.ChangePhaseStarted, now.Add(-2*time.Minute))
	braces.ServiceID = "{" + strings.ToUpper(f.serviceID) + "}"
	row := mustRecord(t, st, ctx, braces)
	if row.ServiceID != f.serviceID {
		t.Fatalf("stored service_id %q, want the canonical %q", row.ServiceID, f.serviceID)
	}
	// The same identity under the canonical spelling sees the started row: one group, ordered.
	done := mustRecord(t, st, ctx, changeInput(f, "spell", domain.ChangePhaseSucceeded, now.Add(-time.Minute)))
	rows, err := changeIdentityRowsTx(ctx, st.pool, f.projectID, f.serviceID, "github-actions", "spell")
	if err != nil || len(rows) != 2 || rows[0].ID != row.ID || rows[1].ID != done.ID {
		t.Fatalf("rows = %+v %v", rows, err)
	}
}
