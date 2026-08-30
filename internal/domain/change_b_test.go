package domain

import (
	"strings"
	"testing"
	"unicode"
)

// FR-025 D2 text at the Unicode edges (func-change-intelligence invariant 23; iter-0165 task 2,
// Agent B): length is code points — not bytes, not grapheme clusters; the transport's half
// composes and trims what the domain then accepts; U+00A0 is whitespace at the edges and text
// inside; every Cf character is refused wherever it stands, including the ones a client is likely
// to send by accident (a BOM, a soft hyphen, the ZWJ inside an emoji sequence). Every non-ASCII
// fixture is a Go escape so the file itself carries no format character.

// 128 three-byte and four-byte code points are accepted (384 and 512 bytes); 129 are refused;
// 64 base+combining-mark pairs that have NO precomposed form ("x" + U+0301) are 128 code points
// (accepted) and 65 pairs are 130 (refused) although they are only 65 graphemes.
func TestChangeTextLengthIsCodePointsNotBytesOrGraphemes(t *testing.T) {
	for _, ok := range []string{strings.Repeat("日", 128), strings.Repeat("\U0001F680", 128), strings.Repeat("x\u0301", 64)} {
		if _, err := ValidateChangeText("ref", ok, ChangeRefMaxLen); err != nil {
			t.Errorf("%d code points / %d bytes refused: %v", len([]rune(ok)), len(ok), err)
		}
	}
	for _, bad := range []string{strings.Repeat("日", 129), strings.Repeat("\U0001F680", 129), strings.Repeat("x\u0301", 65)} {
		_, err := ValidateChangeText("ref", bad, ChangeRefMaxLen)
		if err == nil || !strings.Contains(err.Error(), "at most 128") {
			t.Errorf("%d code points accepted or refused for another reason: %v", len([]rune(bad)), err)
		}
	}
	// "x" + U+0301 has no composition: the fixture exercises length, not NFC.
	if NormalizeChangeText("x\u0301") != "x\u0301" {
		t.Fatal("fixture: x + U+0301 composed; the grapheme case would test NFC, not length")
	}
	// external_id has the same upper bound and a lower bound of one code point.
	if _, err := ValidateChangeExternalID(strings.Repeat("日", 128)); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateChangeExternalID("\U0001F680"); err != nil {
		t.Fatal("a single four-byte code point is a valid external_id")
	}
}

// The transport composes and trims: 65 decomposed "e + U+0301" pairs are 130 code points that
// NormalizeChangeText turns into 65 composed U+00E9 — accepted; the same raw text handed
// straight to the validator is refused for NFC before its length is ever counted; 129 pairs
// compose to 129 (refused), 128 to 128 (accepted). U+00A0, U+0085 and the other White_Space are
// trimmed at the edges by the transport and refused at the edges by the domain; inside, the Zs
// are text and NEL is a control. A BOM is not White_Space: the transport leaves it and the
// domain refuses it, so a BOM-prefixed body is a 400, never silently repaired.
func TestChangeNormalizeComposesAndTrimsWhatTheValidatorThenAccepts(t *testing.T) {
	raw := strings.Repeat("e\u0301", 65)
	norm := NormalizeChangeText(raw)
	if norm != strings.Repeat("é", 65) {
		t.Fatalf("NormalizeChangeText composed to %q", norm)
	}
	if _, err := ValidateChangeText("ref", norm, ChangeRefMaxLen); err != nil {
		t.Fatalf("65 composed é refused: %v", err)
	}
	if _, err := ValidateChangeText("ref", raw, ChangeRefMaxLen); err == nil || !strings.Contains(err.Error(), "NFC") {
		t.Fatalf("130 decomposed code points: %v, want the NFC refusal first", err)
	}
	if _, err := ValidateChangeText("ref", NormalizeChangeText(strings.Repeat("e\u0301", 129)), ChangeRefMaxLen); err == nil {
		t.Fatal("129 composed code points accepted")
	}
	if _, err := ValidateChangeText("ref", NormalizeChangeText(strings.Repeat("e\u0301", 128)), ChangeRefMaxLen); err != nil {
		t.Fatalf("128 composed code points refused: %v", err)
	}

	for _, ws := range []struct {
		name string
		r    rune
	}{{"NBSP U+00A0", '\u00a0'}, {"NEL U+0085", '\u0085'}, {"EN SPACE U+2002", '\u2002'}, {"IDEOGRAPHIC SPACE U+3000", '\u3000'}} {
		if !unicode.IsSpace(ws.r) {
			t.Fatalf("%s is not White_Space in Go's tables; the trim rule below does not hold", ws.name)
		}
		if got := NormalizeChangeText("v1" + string(ws.r)); got != "v1" {
			t.Errorf("trailing %s: NormalizeChangeText gave %q, want it trimmed", ws.name, got)
		}
		if got := NormalizeChangeText(string(ws.r) + "v1"); got != "v1" {
			t.Errorf("leading %s: NormalizeChangeText gave %q, want it trimmed", ws.name, got)
		}
		if _, err := ValidateChangeText("ref", "v1"+string(ws.r), ChangeRefMaxLen); err == nil || !strings.Contains(err.Error(), "whitespace") {
			t.Errorf("trailing %s at the domain: %v, want the whitespace refusal", ws.name, err)
		}
	}
	for _, ok := range []string{"release\u00a04", "a\u2002b", "a\u3000b"} {
		if _, err := ValidateChangeText("ref", ok, ChangeRefMaxLen); err != nil {
			t.Errorf("interior space %q refused: %v", ok, err)
		}
	}
	if _, err := ValidateChangeText("ref", "a\u0085b", ChangeRefMaxLen); err == nil || !strings.Contains(err.Error(), "U+0085") {
		t.Errorf("interior NEL: %v, want refused by name", err)
	}
	// A space with a SINGLETON canonical decomposition — U+2000 EN QUAD is NFC-normalized to
	// U+2002 EN SPACE — is not canonical: the transport rewrites it, the domain refuses it as
	// non-NFC (before any whitespace or length rule), even in the interior.
	if got := NormalizeChangeText("a\u2000b"); got != "a\u2002b" {
		t.Fatalf("NormalizeChangeText(EN QUAD) = %q, want the EN SPACE form", got)
	}
	if _, err := ValidateChangeText("ref", "a\u2000b", ChangeRefMaxLen); err == nil || !strings.Contains(err.Error(), "NFC") {
		t.Errorf("interior EN QUAD: %v, want the NFC refusal", err)
	}
	if got := NormalizeChangeText("\ufeffv1"); got != "\ufeffv1" {
		t.Fatalf("NormalizeChangeText altered a leading BOM to %q", got)
	}
	if _, err := ValidateChangeText("ref", "\ufeffv1", ChangeRefMaxLen); err == nil || !strings.Contains(err.Error(), "U+FEFF") {
		t.Fatalf("leading BOM: %v, want refused by name", err)
	}
}

// Every Cf code point is refused by name wherever it stands — the BOM, the soft hyphen, the word
// joiner, the bidi marks, the Mongolian vowel separator, the ZWJ that glues an emoji family, a
// tag character — and so is every Cc including DEL and NEL; marks (a variation selector), private
// use and unassigned code points are NOT in D2's rule and pass — the letter, stated.
func TestChangeTextRefusesEveryFormatCharacterAndStatesWhatItDoesNotRefuse(t *testing.T) {
	refused := []struct {
		name string
		r    rune
	}{
		{"BOM U+FEFF", '\ufeff'}, {"soft hyphen U+00AD", '\u00ad'}, {"word joiner U+2060", '\u2060'},
		{"LRM U+200E", '\u200e'}, {"RLM U+200F", '\u200f'}, {"ALM U+061C", '\u061c'},
		{"Mongolian vowel separator U+180E", '\u180e'}, {"ZWJ U+200D", '\u200d'}, {"ZWNJ U+200C", '\u200c'},
		{"tag latin A U+E0041", '\U000E0041'}, {"interlinear anchor U+FFF9", '\ufff9'},
		{"DEL U+007F", '\x7f'}, {"NUL U+0000", '\x00'}, {"ESC U+001B", '\x1b'}, {"NEL U+0085", '\u0085'},
	}
	for _, tc := range refused {
		if !unicode.Is(unicode.Cf, tc.r) && !unicode.Is(unicode.Cc, tc.r) {
			t.Fatalf("%s is neither Cf nor Cc in Go's tables; the fixture is wrong", tc.name)
		}
		for _, field := range []string{"external_id", "ref", "url"} {
			value := "a" + string(tc.r) + "b"
			if field == "url" {
				value = "https://x.example/" + value
			}
			_, err := ValidateChangeText(field, value, 512)
			if err == nil || !strings.Contains(err.Error(), field+"_invalid") || !strings.Contains(err.Error(), "U+") {
				t.Errorf("%s in %s: %v, want <field>_invalid naming the code point", tc.name, field, err)
			}
		}
	}
	// An emoji ZWJ sequence is therefore refused as a whole — the letter of D2 (Cf), worth knowing
	// before a pipeline puts a family in a ref.
	if _, err := ValidateChangeRef("\U0001F468\u200d\U0001F469\u200d\U0001F467"); err == nil {
		t.Fatal("an emoji ZWJ sequence passed; D2 refuses every Cf")
	}
	accepted := []struct {
		name string
		s    string
	}{
		{"variation selector U+FE0F (Mn)", "✔\ufe0f"}, {"combining acute U+0301 (Mn) on x", "x\u0301"},
		{"private use U+E000 (Co)", "\ue000"}, {"unassigned U+0378 (Cn)", "\u0378"},
		{"emoji U+1F680 (So)", "\U0001F680"}, {"CJK", "日本語"}, {"interior ordinary space", "release 4.2"},
	}
	for _, tc := range accepted {
		if _, err := ValidateChangeRef(tc.s); err != nil {
			t.Errorf("%s refused: %v", tc.name, err)
		}
	}
}
