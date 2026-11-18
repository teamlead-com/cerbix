package telephony

// The fixed texts cerbix sends outside an alert. None names anything a user chose, so none goes
// through the renderer; the two SMS texts are one GSM-7 segment each.

import "strings"

// VerificationCodeText is the SMS that carries a verification code — the only text cerbix ever
// sends to a number that is not verified (P1).
func VerificationCodeText(code string) string {
	return "[cerbix] Your verification code is " + code + ". It expires in 10 minutes."
}

// VerificationCodeVoice is the TwiML of a call that speaks a verification code (FR-041a §4.2, "The
// code by a call") — the call's counterpart of VerificationCodeText, and like it the only thing
// cerbix delivers to a number that is not verified (P1). One sentence says what it is; then the
// digits, one by one, twice. Nothing is asked of the listener and no key is read (P9): no <Gather>.
// The number is never part of it. language is the <Say> language, escaped as RenderVoice escapes it.
func VerificationCodeVoice(code, language string) string {
	digits := make([]string, 0, len(code))
	for _, r := range code {
		if r >= '0' && r <= '9' {
			digits = append(digits, string(r))
		}
	}
	open := `<Say language="` + xmlEscape(language) + `">`
	spoken := open + strings.Join(digits, ", ") + ".</Say>"
	return "<Response>" + open + "This is your cerbix verification code.</Say>" + spoken + spoken + "</Response>"
}

// TestMessageText is the settings' "Send test SMS" (§4.1), sent to the calling admin's own verified
// number. It is not an alert and leaves no delivery row (R12).
const TestMessageText = "[cerbix] Test message: phone alerts are working."
