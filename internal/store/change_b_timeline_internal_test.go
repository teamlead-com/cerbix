package store

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-025 D6 at its edges (func-change-intelligence §7 *Timeline*, invariant 6; iter-0165 task 2,
// Agent B): the cursor's encoding under hostile external ids, forged cursors, and the half-open
// range against groups that sit exactly on `from`, exactly on `to`, or straddle `to`.

// Every printable external_id — the separator, quotes, slashes, spaces, base64 alphabet, a
// string that is itself a well-formed cursor, composed Unicode, 128 CJK code points — survives
// Encode/Decode byte for byte, together with its source and instant.
func TestChangeCursorEncodingIsUnambiguousForEveryPrintableExternalID(t *testing.T) {
	at := time.Date(2026, 8, 30, 12, 34, 56, 789012000, time.UTC)
	ids := []string{
		":", "::", ":::", "a:b", ":leading", "trailing:", "1700000000000000:ci:x", // the separator, and a cursor-shaped id
		`"double"`, "'single'", "`back`", "with spaces inside", "slash/and\\backslash",
		"base64+/=chars", "A-Za-z0-9_-", "12345", "café", "日本語 ✓", "🚀 v4.2.1", "Run-42", "run-42",
		strings.Repeat("日", 128), strings.Repeat("x", 128),
	}
	for _, source := range []string{"a", "github-actions", strings.Repeat("s", 64)} {
		for _, id := range ids {
			c := ChangeCursor{LatestOccurredAt: at, Source: source, ExternalID: id}
			got, err := DecodeChangeCursor(c.Encode())
			if err != nil {
				t.Errorf("%s/%q: decode: %v", source, id, err)
				continue
			}
			if got != c {
				t.Errorf("%s/%q: round-trip gave %+v", source, id, got)
			}
		}
	}
	// What the domain refuses in an external_id is refused in a cursor too — the cursor is not a
	// second text authority: empty, untrimmed, a Cf character, 129 code points, a newline.
	for _, bad := range []string{"", " x", "x ", "x\u200by", strings.Repeat("日", 129), "a\nb"} {
		if _, err := DecodeChangeCursor(ChangeCursor{LatestOccurredAt: at, Source: "ci", ExternalID: bad}.Encode()); err == nil {
			t.Errorf("cursor with external_id %q decoded", bad)
		}
	}
	// A shape Encode never produces — a missing field, a non-numeric or signless-empty instant,
	// a padded (standard) base64 text, an empty text — is cursor_invalid: never a panic, never a
	// partial cursor.
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, enc := range []string{raw("123:ci"), raw("123"), raw("abc:ci:x"), raw(""), raw(":ci:x"), raw("-:ci:x"),
		base64.StdEncoding.EncodeToString([]byte("12:ci:x")), "not base64!"} {
		_, err := DecodeChangeCursor(enc)
		var ce *domain.ChangeError
		if !errors.As(err, &ce) || ce.Code != domain.ChangeErrCursorInvalid {
			t.Errorf("cursor %q: %v, want cursor_invalid", enc, err)
		}
	}
	// A signed instant is a valid int64 and positions at the extremes without error.
	for _, enc := range []string{raw("-5:ci:x"), raw("+5:ci:x"), raw("9223372036854775807:ci:x")} {
		if _, err := DecodeChangeCursor(enc); err != nil {
			t.Errorf("cursor %q: %v, want a decoded cursor", enc, err)
		}
	}
}

// A forged cursor only POSITIONS the traversal: with foreign identity components, another
// service's real identity, or an instant in the future, the page is exactly the uncursored
// listing filtered by the keyset predicate — never a row of another service, never a duplicate,
// never an error. A negative instant positions below everything (an empty page).
func TestChangeCursorWithForeignIdentityOrFutureInstantOnlyPositionsTheTraversal(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	from, to := now.Add(-time.Hour), now.Add(time.Minute)
	same := now.Add(-10 * time.Minute)
	// Five groups on the service: three share one instant (tiebreak by identity), two older.
	for _, ext := range []string{"m1", "m2", "m3"} {
		mustRecord(t, st, ctx, changeInput(f, ext, domain.ChangePhaseSucceeded, same))
	}
	mustRecord(t, st, ctx, changeInput(f, "o1", domain.ChangePhaseSucceeded, now.Add(-20*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "o2", domain.ChangePhaseSucceeded, now.Add(-30*time.Minute)))
	// The upstream service has its own identity at the shared instant: a cursor "from" it must
	// never surface its rows here.
	up := changeInput(f, "m2", domain.ChangePhaseSucceeded, same)
	up.ServiceID = f.upstreamID
	mustRecord(t, st, ctx, up)

	all, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, to, nil, nil, nil, 50)
	if err != nil || len(all) != 5 {
		t.Fatalf("uncursored: %d groups %v", len(all), err)
	}
	// keysetBelow is D6's bound computed in Go over the uncursored listing (ASCII identities keep
	// Go and the database collation in agreement).
	keysetBelow := func(c ChangeCursor) []string {
		var out []string
		for _, g := range all {
			if g.LatestOccurredAt.Before(c.LatestOccurredAt) ||
				(g.LatestOccurredAt.Equal(c.LatestOccurredAt) && (g.Source > c.Source || (g.Source == c.Source && g.ExternalID > c.ExternalID))) {
				out = append(out, g.Source+"/"+g.ExternalID)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		cursor ChangeCursor
	}{
		{"foreign identity at the shared instant", ChangeCursor{LatestOccurredAt: same, Source: "github-actions", ExternalID: "m1x"}},
		{"another service's real identity", ChangeCursor{LatestOccurredAt: same, Source: "github-actions", ExternalID: "m2"}},
		{"a source sorting before every real one", ChangeCursor{LatestOccurredAt: same, Source: "aaa", ExternalID: "zzz"}},
		{"a source sorting after every real one", ChangeCursor{LatestOccurredAt: same, Source: "zzz", ExternalID: "a"}},
		{"an instant in the future", ChangeCursor{LatestOccurredAt: now.Add(365 * 24 * time.Hour), Source: "ci", ExternalID: "x"}},
		{"an instant far in the future", ChangeCursor{LatestOccurredAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), Source: "ci", ExternalID: "x"}},
		{"an instant before the range", ChangeCursor{LatestOccurredAt: from.Add(-time.Hour), Source: "ci", ExternalID: "x"}},
		{"a negative instant", ChangeCursor{LatestOccurredAt: time.UnixMicro(-5).UTC(), Source: "ci", ExternalID: "x"}},
	} {
		// Through the opaque encoding, as a client would hand it back.
		decoded, err := DecodeChangeCursor(tc.cursor.Encode())
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		page, next, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, to, nil, nil, &decoded, 50)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := keysetBelow(tc.cursor)
		if got := groupIDs([][]domain.ChangeGroup{page}); strings.Join(got, ",") != strings.Join(want, ",") || next != nil {
			t.Fatalf("%s: page = %v next=%v, want %v (the keyset predicate over the uncursored listing)", tc.name, got, next, want)
		}
		for _, g := range page {
			for _, p := range g.Phases {
				if p.ServiceID != f.serviceID {
					t.Fatalf("%s: a row of service %s leaked onto the page", tc.name, p.ServiceID)
				}
			}
		}
	}
	// Paging by one through the tie never repeats and never skips, whatever the page size.
	for _, limit := range []int{1, 2, 3} {
		pages := listAllGroups(t, st, ctx, f, from, to, nil, nil, limit)
		if got := groupIDs(pages); strings.Join(got, ",") != strings.Join(groupIDs([][]domain.ChangeGroup{all}), ",") {
			t.Fatalf("limit %d: traversal = %v, want %v", limit, got, groupIDs([][]domain.ChangeGroup{all}))
		}
	}
}

// `[from, to)` on latest_occurred_at: a group whose latest is exactly `from` is in, exactly `to`
// is out, one microsecond below `from` is out, one microsecond below `to` is in; a group whose
// started is inside but whose terminal sits AT `to` is out as a whole; a group whose started
// precedes `from` and whose terminal is inside is in with both phases; `from`/`to` given in
// non-UTC offsets select the same groups.
func TestChangeTimelineRangeIncludesFromExcludesToAndAGroupFollowsItsLatestPhase(t *testing.T) {
	st, ctx := declStore(t)
	f := changeService(t, st, ctx)
	now := gateDBNow(t, st, ctx)
	from, to := now.Add(-30*time.Minute), now.Add(-10*time.Minute)

	mustRecord(t, st, ctx, changeInput(f, "at-from", domain.ChangePhaseSucceeded, from))
	mustRecord(t, st, ctx, changeInput(f, "below-from", domain.ChangePhaseSucceeded, from.Add(-time.Microsecond)))
	mustRecord(t, st, ctx, changeInput(f, "at-to", domain.ChangePhaseSucceeded, to))
	mustRecord(t, st, ctx, changeInput(f, "below-to", domain.ChangePhaseSucceeded, to.Add(-time.Microsecond)))
	mustRecord(t, st, ctx, changeInput(f, "straddle-out", domain.ChangePhaseStarted, to.Add(-5*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "straddle-out", domain.ChangePhaseFailed, to))
	mustRecord(t, st, ctx, changeInput(f, "straddle-in", domain.ChangePhaseStarted, from.Add(-5*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "straddle-in", domain.ChangePhaseSucceeded, from.Add(5*time.Minute)))
	mustRecord(t, st, ctx, changeInput(f, "started-only-in", domain.ChangePhaseStarted, from.Add(time.Minute)))

	want := "github-actions/below-to,github-actions/straddle-in,github-actions/started-only-in,github-actions/at-from"
	for _, loc := range []*time.Location{time.UTC, time.FixedZone("plus5", 5*3600), time.FixedZone("minus3", -3*3600)} {
		pages := listAllGroups(t, st, ctx, f, from.In(loc), to.In(loc), nil, nil, 50)
		if got := groupIDs(pages); strings.Join(got, ",") != want {
			t.Fatalf("%s: groups = %v, want %s", loc, got, want)
		}
		byID := map[string]domain.ChangeGroup{}
		for _, g := range pages[0] {
			byID[g.ExternalID] = g
		}
		if g := byID["straddle-in"]; len(g.Phases) != 2 || !g.Phases[0].OccurredAt.Before(from) || !g.LatestOccurredAt.Equal(from.Add(5*time.Minute)) {
			t.Fatalf("straddle-in = %+v, want both phases and latest = the terminal", g)
		}
		if g := byID["at-from"]; !g.LatestOccurredAt.Equal(from) {
			t.Fatalf("at-from latest = %s, want exactly from %s", g.LatestOccurredAt, from)
		}
		if _, leaked := byID["straddle-out"]; leaked {
			t.Fatal("a group whose terminal sits at `to` was returned: the group follows its latest phase")
		}
	}
	// The straddling-out group is whole on the other side of `to`.
	later, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, to, to.Add(time.Minute), nil, nil, nil, 50)
	if err != nil || len(later) != 2 {
		t.Fatalf("[to, to+1m) = %v %v, want at-to and straddle-out", groupIDs([][]domain.ChangeGroup{later}), err)
	}
	for _, g := range later {
		if g.ExternalID == "straddle-out" && (len(g.Phases) != 2 || g.Phases[0].Phase != domain.ChangePhaseStarted) {
			t.Fatalf("straddle-out on its own side = %+v, want both phases", g.Phases)
		}
	}
	// A one-microsecond range is a valid half-open range holding exactly the group AT its start.
	one, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, from.Add(time.Microsecond), nil, nil, nil, 50)
	if err != nil || len(one) != 1 || one[0].ExternalID != "at-from" {
		t.Fatalf("[from, from+1µs) = %v %v", groupIDs([][]domain.ChangeGroup{one}), err)
	}
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, from.Add(-time.Microsecond), nil, nil, nil, 50); err == nil {
		t.Fatal("to < from accepted")
	}
	// The 92-day bound is exact to the microsecond.
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-ChangeRangeMax), now, nil, nil, nil, 50); err != nil {
		t.Fatalf("exactly 92 days: %v", err)
	}
	var ce *domain.ChangeError
	if _, _, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, now.Add(-ChangeRangeMax-time.Microsecond), now, nil, nil, nil, 50); !errors.As(err, &ce) || ce.Code != domain.ChangeErrRangeTooWide {
		t.Fatalf("92 days + 1µs: %v", err)
	}
	// A kind filter and a source filter that match nothing are an empty, non-nil page with no cursor.
	src := "nobody"
	empty, next, err := st.ListChangeGroups(ctx, f.projectID, f.serviceID, from, to, []domain.ChangeKind{domain.ChangeKindFlag}, &src, nil, 50)
	if err != nil || empty == nil || len(empty) != 0 || next != nil {
		t.Fatalf("no match: %v %v %v", empty, next, err)
	}
}
