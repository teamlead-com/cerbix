package domain

import "net/mail"

// MaxMailAddressOctets is RFC 5321's 256-octet limit on a path less its angle brackets: no server is
// obliged to accept a longer address, so the rule refuses it where it is written.
const MaxMailAddressOctets = 254

// ValidMailAddress is the ONE address rule of every mail cerbix sends (sec-hardening-3 T-3, S18): the
// message builder, the three write surfaces that store an address (subscriber, email channel, Settings
// → Mail) and the startup check of the YAML mail block all call it, so a value is never accepted in
// one place and refused in another.
//
// An address passes when net/mail parses it, it has no display name, the parsed address equals the
// input byte for byte, and every byte is printable ASCII. That refuses display names, <…>, comments,
// lists, quoted local parts Go rewrites, whitespace and every control byte. Non-ASCII addresses are
// refused on purpose: cerbix does not request SMTPUTF8 (mailer.mailFrom), so such an address cannot
// be delivered through a relay that lacks it, and refusing it at write tells the person now.
//
// An address is at most MaxMailAddressOctets long (code review of sec-hardening-3, P2-2).
//
// The caller decides about surrounding whitespace: write surfaces and the channel sender judge and send
// the trimmed value; the YAML seed is judged as written, because it is sent as written.
func ValidMailAddress(s string) bool {
	if s == "" || len(s) > MaxMailAddressOctets {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s
}
