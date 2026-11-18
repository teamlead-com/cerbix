package telephony

import (
	"strings"
	"unicode"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// Segment capacities (§4.4): GSM-7 septets and UTF-16 code units, for a
// single-part message and per part of a concatenated one.
const (
	gsmSingle  = 160
	gsmPart    = 153
	ucs2Single = 70
	ucs2Part   = 67
)

// gsmBasic is the GSM 03.38 default alphabet (one septet each), without
// ESC and the two line breaks: a normalised name never carries them and the
// frame does not use them.
const gsmBasic = "@£$¥èéùìòÇØøÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

// gsmExtension is the extension table: each character costs two septets.
const gsmExtension = "^{}\\[~]|€"

var gsmSeptets = func() map[rune]int {
	m := make(map[rune]int, len(gsmBasic)+len(gsmExtension))
	for _, r := range gsmBasic {
		m[r] = 1
	}
	for _, r := range gsmExtension {
		m[r] = 2
	}
	return m
}()

// smsCost reports the length of s in its encoding's units, the number of
// segments it takes, and whether it must be sent as UCS-2 (some rune is not
// in GSM-7).
func smsCost(s string) (units, segments int, ucs2 bool) {
	septets := 0
	for _, r := range s {
		n, ok := gsmSeptets[r]
		if !ok {
			ucs2 = true
			break
		}
		septets += n
	}
	single, part := gsmSingle, gsmPart
	units = septets
	if ucs2 {
		single, part = ucs2Single, ucs2Part
		units = len(utf16.Encode([]rune(s)))
	}
	switch {
	case units <= single:
		segments = 1
	default:
		segments = (units + part - 1) / part
	}
	return units, segments, ucs2
}

// normaliseName makes user-chosen text safe to render (§4.4): every line
// break becomes a space, control and format characters (bidi overrides,
// zero-width characters) are removed, runs of white space collapse to one
// space, and the ends are trimmed. The result may be empty; displayName
// guarantees a non-empty name.
func normaliseName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r): // includes \t \n \v \f \r, U+0085, U+2028, U+2029
			pendingSpace = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// transliterate converts s to printable ASCII that is also GSM-7: a table
// for Cyrillic, Greek and a few Latin letters NFKD does not decompose, then
// NFKD with the combining marks dropped. A rune with no transliteration
// becomes "?", so a non-empty name is never emptied; a name that would
// still come out empty (only hard and soft signs) becomes "?".
func transliterate(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	// NFC first, so that a letter the table knows precomposed (й, ё) is
	// looked up whole rather than as a base letter and a mark.
	for _, r := range norm.NFC.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue // a mark that composes with nothing
		}
		if t, ok := transliterateRune(r); ok {
			b.WriteString(t)
		} else {
			b.WriteByte('?')
		}
	}
	if b.Len() == 0 && s != "" {
		return "?"
	}
	return b.String()
}

func transliterateRune(r rune) (string, bool) {
	if isGSMASCII(r) {
		return string(r), true
	}
	if t, ok := translitTable[r]; ok {
		return t, true
	}
	var d strings.Builder
	for _, dr := range norm.NFKD.String(string(r)) {
		switch {
		case unicode.Is(unicode.Mn, dr):
			// combining mark: dropped
		case isGSMASCII(dr):
			d.WriteRune(dr)
		default:
			t, ok := translitTable[dr]
			if !ok {
				return "", false
			}
			d.WriteString(t)
		}
	}
	return d.String(), d.Len() > 0
}

// isGSMASCII reports whether r is printable ASCII and in GSM-7 (the
// backtick is the one printable ASCII character GSM-7 lacks).
func isGSMASCII(r rune) bool {
	return r >= 0x20 && r <= 0x7e && r != '`'
}
