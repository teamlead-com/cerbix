package mailer

import (
	"fmt"
	"mime"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// maxSubjectRunes bounds the subject (sec-hardening-3 §4.2, P2-3): the first line of the text, at most
// this many runes; the full text is always in the body.
const maxSubjectRunes = 200

// maxHeaderLine is RFC 5322's recommended line length, which the folded Subject keeps to wherever
// whitespace allows.
const maxHeaderLine = 78

// BuildMessage is the ONE place a mail message is assembled (sec-hardening-3 T-3, S18). Both senders —
// the channel sender in internal/notify and Mailer.SendContext (subscriber, confirmation and reset
// mail) — call it, and neither concatenates a header again.
//
//   - From and every To element must pass domain.ValidMailAddress; a refused one ends the send before
//     any connection, with an error that names the field and the position, never the value.
//   - Subject is the first non-blank line of the given text, other control bytes replaced by spaces,
//     at most 200 runes, RFC 2047 Q-encoded when not printable ASCII, and folded. It can therefore
//     never carry a line break of its own: a name or title with CR/LF cannot add a header or end the
//     header block.
//   - The header set is fixed: From, To, Subject, MIME-Version, Content-Type, in that order.
//   - The body is the text given; net/smtp dot-stuffs and normalizes its line breaks.
func BuildMessage(from string, to []string, subject, body string) ([]byte, error) {
	if !domain.ValidMailAddress(from) {
		return nil, fmt.Errorf("mail: the from address is not a single bare address")
	}
	if len(to) == 0 {
		return nil, fmt.Errorf("mail: no to address")
	}
	for i, addr := range to {
		if !domain.ValidMailAddress(addr) {
			return nil, fmt.Errorf("mail: to address %d is not a single bare address", i+1)
		}
	}
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	// Folded between addresses, as the Subject is between words: a long recipient list would otherwise
	// be one line far past 78 octets (code review of sec-hardening-3, P2-2). An address holds no space,
	// so a fold never lands inside one.
	b.WriteString(foldHeader("To", strings.Join(to, ", ")) + "\r\n")
	b.WriteString(foldHeader("Subject", mime.QEncoding.Encode("utf-8", subjectLine(subject))) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n" + body + "\r\n")
	return []byte(b.String()), nil
}

// subjectLine is the first line of text that is not blank, with every other control byte replaced by a
// space and at most maxSubjectRunes runes (the first 199 and "…" when longer).
func subjectLine(text string) string {
	first := ""
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.TrimSpace(line) != "" {
			first = line
			break
		}
	}
	first = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, first))
	if runes := []rune(first); len(runes) > maxSubjectRunes {
		first = string(runes[:maxSubjectRunes-1]) + "…"
	}
	return first
}

// foldHeader writes `name: value`, folding at spaces (RFC 5322 §2.2.3) so that no line exceeds 78
// octets wherever a space allows. An encoded subject is a run of encoded-words separated by single
// spaces, each at most 75 octets, so it always folds within the limit; a plain ASCII subject folds at
// its own spaces. Whitespace between adjacent encoded-words is not part of the decoded text (RFC 2047
// §6.2), and a folded plain line unfolds to the space it was folded at.
func foldHeader(name, value string) string {
	var out strings.Builder
	line := name + ":"
	for _, w := range strings.Split(value, " ") {
		// Fold before a word that would overrun the line. A line that holds only the field name may be
		// folded too: `Subject:` CRLF SP word is a valid unstructured field (FWS after the colon).
		if line != "" && len(line)+1+len(w) > maxHeaderLine {
			out.WriteString(line + "\r\n")
			line = ""
		}
		line += " " + w
	}
	out.WriteString(line)
	return out.String()
}
