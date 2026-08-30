package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// FR-025 §7 — the pure rows of *Phases* and *Bounds and shape* (func-change-intelligence.md).

func changeCode(t *testing.T, err error) *ChangeError {
	t.Helper()
	var ce *ChangeError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v (%T), want a *ChangeError", err, err)
	}
	return ce
}

// D3: the order table, row by row. A terminal alone is accepted; a second terminal and a
// `started` after a terminal are `phase_order` naming the terminal already recorded; a phase
// already present is `phase_exists`.
func TestChangePhaseOrderIsTheDomainsTable(t *testing.T) {
	cases := []struct {
		name     string
		existing []ChangePhase
		next     ChangePhase
		code     string
		mentions string
	}{
		{"started first", nil, ChangePhaseStarted, "", ""},
		{"terminal alone", nil, ChangePhaseSucceeded, "", ""},
		{"started then succeeded", []ChangePhase{ChangePhaseStarted}, ChangePhaseSucceeded, "", ""},
		{"started then failed", []ChangePhase{ChangePhaseStarted}, ChangePhaseFailed, "", ""},
		{"started then cancelled", []ChangePhase{ChangePhaseStarted}, ChangePhaseCancelled, "", ""},
		{"second terminal", []ChangePhase{ChangePhaseStarted, ChangePhaseSucceeded}, ChangePhaseFailed, ChangeErrPhaseOrder, "succeeded already recorded"},
		{"second terminal without start", []ChangePhase{ChangeChangeTerminal(ChangePhaseFailed)}, ChangePhaseCancelled, ChangeErrPhaseOrder, "failed already recorded"},
		{"started after terminal", []ChangePhase{ChangePhaseFailed}, ChangePhaseStarted, ChangeErrPhaseOrder, "failed"},
		{"started twice", []ChangePhase{ChangePhaseStarted}, ChangePhaseStarted, ChangeErrPhaseExists, "started already recorded"},
		{"same terminal twice", []ChangePhase{ChangePhaseSucceeded}, ChangePhaseSucceeded, ChangeErrPhaseExists, "succeeded already recorded"},
		{"unknown phase", nil, ChangePhase("done"), ChangeErrPhaseInvalid, "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ChangePhaseOrder(tc.existing, tc.next)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			ce := changeCode(t, err)
			if ce.Code != tc.code {
				t.Fatalf("code = %q, want %q (%v)", ce.Code, tc.code, err)
			}
			if !strings.Contains(ce.Msg, tc.mentions) {
				t.Fatalf("message %q does not name %q", ce.Msg, tc.mentions)
			}
		})
	}
}

// ChangeChangeTerminal is an identity helper that keeps the table above readable where a
// terminal stands alone in `existing`.
func ChangeChangeTerminal(p ChangePhase) ChangePhase { return p }

func TestChangeTerminalPhasesAreExactlyThree(t *testing.T) {
	var terminals []ChangePhase
	for _, p := range ChangePhases {
		if !ValidChangePhase(p) {
			t.Fatalf("%q listed but not valid", p)
		}
		if IsTerminalPhase(p) {
			terminals = append(terminals, p)
		}
	}
	if len(terminals) != 3 || IsTerminalPhase(ChangePhaseStarted) {
		t.Fatalf("terminals = %v; want succeeded|failed|cancelled and never started", terminals)
	}
	if ValidChangePhase("done") || ValidChangeKind("config") || len(ChangeKinds) != 3 {
		t.Fatal("the enums are closed: no fourth kind (D-0209 answer 1), no fifth phase")
	}
}

// D2 text: NFC-invariant, trimmed, no Cc/Cf, no line separators, length in code points. The
// domain VALIDATES a canonical value; NormalizeChangeText is the transport's half.
func TestChangeTextValidatorIsTheUnicodeAuthority(t *testing.T) {
	composed := "café"         // é as one code point
	decomposed := "cafe\u0301" // e + combining acute
	if NormalizeChangeText(decomposed) != composed {
		t.Fatalf("NormalizeChangeText(%q) = %q, want the composed %q", decomposed, NormalizeChangeText(decomposed), composed)
	}
	if NormalizeChangeText("  v1.2.3\t\n") != "v1.2.3" {
		t.Fatal("NormalizeChangeText must trim leading and trailing whitespace")
	}
	if v, err := ValidateChangeText("ref", composed, ChangeRefMaxLen); err != nil || v != composed {
		t.Fatalf("composed é refused or altered: %q %v", v, err)
	}
	refuse := []struct {
		name  string
		value string
		want  string
	}{
		{"decomposed é (not NFC)", decomposed, "NFC"},
		{"leading space", " v1", "whitespace"},
		{"trailing space", "v1 ", "whitespace"},
		{"zero width space U+200B (Cf)", "v1\u200b.2", "U+200B"},
		{"newline", "v1\n2", "U+000A"},
		{"carriage return", "v1\r2", "U+000D"},
		{"tab (Cc)", "v1\t2", "U+0009"},
		{"line separator U+2028", "v1\u20282", "U+2028"},
		{"paragraph separator U+2029", "v1\u20292", "U+2029"},
		{"DEL", "v1\x7f", "U+007F"},
		{"invalid UTF-8", "v1\xff", "UTF-8"},
		{"129 code points", strings.Repeat("é", 129), "at most 128"},
	}
	for _, tc := range refuse {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateChangeText("ref", tc.value, ChangeRefMaxLen)
			ce := changeCode(t, err)
			if ce.Code != ChangeErrRefInvalid || ce.Field != "ref" {
				t.Fatalf("code/field = %q/%q, want ref_invalid/ref", ce.Code, ce.Field)
			}
			if !strings.Contains(ce.Msg, tc.want) {
				t.Fatalf("message %q does not say %q", ce.Msg, tc.want)
			}
		})
	}
	// Length counts CODE POINTS: 128 two-byte characters are accepted, 129 refused (above).
	if _, err := ValidateChangeText("ref", strings.Repeat("é", 128), ChangeRefMaxLen); err != nil {
		t.Fatalf("128 code points refused: %v", err)
	}
	// An interior ordinary space is text, not a control.
	if _, err := ValidateChangeText("ref", "release 4.2", ChangeRefMaxLen); err != nil {
		t.Fatalf("interior space refused: %v", err)
	}
	// The code is `<field>_invalid` for every text field.
	if _, err := ValidateChangeExternalID(""); changeCode(t, err).Code != ChangeErrExternalIDInvalid {
		t.Fatal("an empty external_id must be external_id_invalid")
	}
	if _, err := ValidateChangeExternalID(strings.Repeat("x", 129)); changeCode(t, err).Code != ChangeErrExternalIDInvalid {
		t.Fatal("a 129-character external_id must be external_id_invalid")
	}
	if v, err := ValidateChangeExternalID("Run-42"); err != nil || v != "Run-42" {
		t.Fatalf("external_id is case-sensitive and kept verbatim: %q %v", v, err)
	}
}

// D2 source: the slug class. `Deploy_Bot` is refused (case and underscore); the pattern is the
// schema's spelling.
func TestChangeSourceIsASlug(t *testing.T) {
	for _, ok := range []string{"github-actions", "gitlab", "a", "0", strings.Repeat("a", 64), "ci-2"} {
		if err := ValidateChangeSource(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"Deploy_Bot", "", "-x", "GitHub", "ci.bot", "ci bot", strings.Repeat("a", 65), "é"} {
		err := ValidateChangeSource(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		if changeCode(t, err).Code != ChangeErrSourceInvalid {
			t.Errorf("%q: code %q, want source_invalid", bad, changeCode(t, err).Code)
		}
	}
	if ChangeSourcePattern() != "^[a-z0-9][a-z0-9-]{0,63}$" {
		t.Fatalf("pattern %q is not the schema's", ChangeSourcePattern())
	}
}

// D2 url: https:// only, 0..512, the text rules apply.
func TestChangeURLIsHTTPSOnly(t *testing.T) {
	for _, ok := range []string{"", "https://ci.example.com/runs/42", "https://x"} {
		if _, err := ValidateChangeURL(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []struct{ u, says string }{
		{"http://ci.example.com/runs/42", "https://"},
		{"HTTPS://ci.example.com", "https://"},
		{"https://", "host"},
		{"ftp://x", "https://"},
		{"https://x\u200b", "U+200B"},
		{"https://" + strings.Repeat("a", 505), "at most 512"},
	} {
		_, err := ValidateChangeURL(bad.u)
		if err == nil {
			t.Errorf("%q accepted", bad.u)
			continue
		}
		ce := changeCode(t, err)
		if ce.Code != ChangeErrURLInvalid || !strings.Contains(ce.Msg, bad.says) {
			t.Errorf("%q: %q/%q, want url_invalid saying %q", bad.u, ce.Code, ce.Msg, bad.says)
		}
	}
}

// D7: the note names at most `max` entries and counts the rest, says "preceded" and never
// "caused", carries the marker as its prefix, and is empty for an empty batch.
func TestRenderChangesNoteNamesAtMostMaxAndCountsTheRest(t *testing.T) {
	if RenderChangesNote(nil, 0, 5) != "" {
		t.Fatal("an empty batch must render nothing")
	}
	entries := []ChangeNoteEntry{
		{Kind: ChangeKindDeploy, Ref: "v4.2.1", Source: "github-actions", LagSeconds: 720},
		{Kind: ChangeKindFlag, Source: "launchdarkly", LagSeconds: 2400},
		{Kind: ChangeKindRollback, Ref: "v4.2.0", Source: "argo", LagSeconds: 3660},
	}
	got := RenderChangesNote(entries, 3, 5)
	want := "🚀 Changes: 3 preceded this incident — deploy v4.2.1 by github-actions, −12m; flag by launchdarkly, −40m; rollback v4.2.0 by argo, −1h01m."
	if got != want {
		t.Fatalf("note =\n%q\nwant\n%q", got, want)
	}
	if !strings.HasPrefix(got, ChangesMarker) || strings.Contains(strings.ToLower(got), "caus") {
		t.Fatal("the note must start with the marker and never say caused")
	}
	capped := RenderChangesNote(entries, 7, 2)
	if !strings.HasSuffix(capped, "flag by launchdarkly, −40m; … and 5 more.") || strings.Contains(capped, "argo") {
		t.Fatalf("capped note = %q; want two named and 5 counted", capped)
	}
	if s := RenderChangesNote(entries[:1], 1, 5); !strings.Contains(s, "1 preceded this incident — deploy v4.2.1 by github-actions, −12m.") {
		t.Fatalf("single = %q", s)
	}
	if strings.Contains(RenderChangesNote([]ChangeNoteEntry{{Kind: ChangeKindDeploy, Source: "s", LagSeconds: 59}}, 1, 5), "−59s.") == false {
		t.Fatal("a lag under a minute is stated in seconds")
	}
}

// D8: the four horizons and nothing else.
func TestChangeCompareHorizonsAreTheFour(t *testing.T) {
	for _, h := range []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour} {
		if !ValidChangeCompareHorizon(h) {
			t.Errorf("%s refused", h)
		}
	}
	for _, h := range []time.Duration{2 * time.Hour, 7 * 24 * time.Hour, 0, 30 * time.Minute} {
		if ValidChangeCompareHorizon(h) {
			t.Errorf("%s accepted", h)
		}
	}
	if ChangeCompareDefaultHorizon != time.Hour {
		t.Fatal("the default horizon is 1h")
	}
}

// The group's terminal phase, if any.
func TestChangeGroupTerminalPhase(t *testing.T) {
	g := ChangeGroup{Phases: []ChangePhaseRow{{Phase: ChangePhaseStarted}}}
	if g.TerminalPhase() != nil {
		t.Fatal("started-only group has no terminal")
	}
	g.Phases = append(g.Phases, ChangePhaseRow{Phase: ChangePhaseFailed, ID: "x"})
	if tp := g.TerminalPhase(); tp == nil || tp.ID != "x" {
		t.Fatalf("terminal = %+v", tp)
	}
}
