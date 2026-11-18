package telephony

import (
	"strings"
	"testing"
)

func TestVerificationCodeText(t *testing.T) {
	const want = "[cerbix] Your verification code is 482193. It expires in 10 minutes."
	if got := VerificationCodeText("482193"); got != want {
		t.Fatalf("VerificationCodeText = %q, want %q", got, want)
	}
	if _, segments, ucs2 := smsCost(VerificationCodeText("000000")); segments != 1 || ucs2 {
		t.Fatalf("the code SMS is %d segments (ucs2 %v), want one GSM-7 segment", segments, ucs2)
	}
}

func TestTestMessageText(t *testing.T) {
	const want = "[cerbix] Test message: phone alerts are working."
	if TestMessageText != want {
		t.Fatalf("TestMessageText = %q, want %q", TestMessageText, want)
	}
}

// FR-041a revision 9, §4.2 "The code by a call", §11 "Verification by a call": the spoken code is
// one sentence saying what it is, then the six digits one by one, twice. Nothing is asked of the
// listener and no key is read (P9): no <Gather>. The language is the caller's choice, escaped as an
// attribute value, never interpreted.
func TestVerificationCodeVoice(t *testing.T) {
	const want = `<Response>` +
		`<Say language="tr-TR">This is your cerbix verification code.</Say>` +
		`<Say language="tr-TR">4, 8, 2, 1, 9, 3.</Say>` +
		`<Say language="tr-TR">4, 8, 2, 1, 9, 3.</Say>` +
		`</Response>`
	got := VerificationCodeVoice("482193", "tr-TR")
	if got != want {
		t.Fatalf("VerificationCodeVoice =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "<Gather") || strings.Contains(got, "482193") {
		t.Fatalf("the spoken code asks for a key or reads the code as one number: %s", got)
	}
	if n := strings.Count(got, "4, 8, 2, 1, 9, 3."); n != 2 {
		t.Fatalf("the digits are spoken %d times, want twice: %s", n, got)
	}
	if n := strings.Count(got, "verification code"); n != 1 {
		t.Fatalf("the introductory sentence is spoken %d times, want once: %s", n, got)
	}
	// A leading zero is a digit like any other.
	if got := VerificationCodeVoice("000017", "en-US"); !strings.Contains(got, ">0, 0, 0, 0, 1, 7.</Say>") {
		t.Fatalf("code 000017 = %s", got)
	}
	// The language is an attribute value: markup in it is escaped, never interpreted.
	if got := VerificationCodeVoice("482193", `en"><Gather/>`); strings.Contains(got, "<Gather") || strings.Contains(got, `en">`) {
		t.Fatalf("the language was not escaped: %s", got)
	}
}
