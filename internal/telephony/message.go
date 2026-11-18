// Package telephony renders and sends phone alerts (FR-041a).
package telephony

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// AlertKind is the kind of event a phone alert reports (D18).
type AlertKind string

const (
	KindMonitorDown      AlertKind = "monitor_down"
	KindMonitorRecovered AlertKind = "monitor_recovered"
	KindServiceHealth    AlertKind = "service_health" // State: down | degraded | unknown
	KindServiceRecovered AlertKind = "service_recovered"
	KindEscalationStep   AlertKind = "escalation_step" // Step, Repeat; subject a monitor or a service
	KindMonitorBurn      AlertKind = "monitor_burn"    // BurnRate, Window
	KindServiceBurn      AlertKind = "service_burn"    // BurnRate, Window
)

// Encoding is the instance's sms_encoding (§4.1).
type Encoding string

const (
	// EncodingTransliterate converts names to GSM-7 ASCII.
	EncodingTransliterate Encoding = "transliterate"
	// EncodingUnicode sends names as written; a name outside GSM-7 makes the
	// whole message UCS-2.
	EncodingUnicode Encoding = "unicode"
)

// PhoneAlert is what an SMS or a call is rendered from (D16). It carries
// only what a project viewer may already see (§4.4).
type PhoneAlert struct {
	Kind AlertKind
	// State is the service health of a KindServiceHealth alert.
	State string
	// Step and Repeat describe a KindEscalationStep alert.
	Step   int
	Repeat bool
	// BurnRate and Window describe a burn alert.
	BurnRate float64
	Window   string
	// SubjectName is the monitor name or the service slug.
	SubjectName string
	// IsService names the subject "service <slug>". Service kinds imply it
	// and monitor kinds exclude it; it decides only for an escalation step.
	IsService     bool
	ProjectName   string
	IncidentRef   string // may be empty (no_incident_ref)
	PublicBaseURL string // may be empty (no_public_url)
}

// SMS is a rendered SMS and what it costs.
type SMS struct {
	Text string
	// Units are GSM-7 septets, or UTF-16 code units when Unicode is set.
	Units    int
	Segments int
	Unicode  bool
	// Notes carries domain.PhoneNoteLinkOmitted when the link was dropped to
	// fit; the caller adds no_public_url / no_incident_ref.
	Notes []string
}

// Truncation floors (§4.4, R16): the "..." counts inside both.
const (
	nameFloor    = 12
	projectFloor = 8
	ellipsis     = "..."
)

// RenderSMS renders the SMS of a (§4.4, R16, R17) so that it fits maxSegments
// when it can: the name is shortened first (to 12 characters), then the
// project (to 8), then the project is dropped with its brackets. The link is
// dropped first, with link_omitted, when the frame and the link cannot fit
// even with the shortest names. The incident reference is never cut; a text
// that cannot fit even at its shortest is returned as such, and Segments
// reports what it really costs.
func RenderSMS(a PhoneAlert, enc Encoding, maxSegments int) (SMS, error) {
	if enc != EncodingTransliterate && enc != EncodingUnicode {
		return SMS{}, fmt.Errorf("telephony: unknown sms encoding %q", enc)
	}
	if maxSegments < 1 {
		maxSegments = 1
	}
	f, err := smsFrame(a, enc)
	if err != nil {
		return SMS{}, err
	}

	fits := func(s string) bool {
		_, segs, _ := smsCost(s)
		return segs <= maxSegments
	}
	shortestName := min(len(f.name), nameFloor)

	var notes []string
	if f.link != "" && !fits(f.render(shortestName, -1, true)) {
		f.link = ""
		notes = append(notes, domain.PhoneNoteLinkOmitted)
	}
	text := f.fit(fits, shortestName)

	units, segments, ucs2 := smsCost(text)
	return SMS{Text: text, Units: units, Segments: segments, Unicode: ucs2, Notes: notes}, nil
}

// frame is an SMS split into the parts truncation may touch.
type frame struct {
	head    string // "[cerbix] VERB " and "service " for a service
	name    []rune
	project []rune // nil: no project
	tail    string // " - burn rate …"
	ref     string // " - incident REF"
	link    string // " <base>/i/REF"
}

// render builds the text with the name cut to nameLen runes and the project
// to projectLen runes; projectLen < 0 drops the project with its brackets.
func (f frame) render(nameLen, projectLen int, withLink bool) string {
	var b strings.Builder
	b.WriteString(f.head)
	b.WriteString(shorten(f.name, nameLen))
	if f.project != nil && projectLen >= 0 {
		b.WriteString(" (")
		b.WriteString(shorten(f.project, projectLen))
		b.WriteString(")")
	}
	b.WriteString(f.tail)
	b.WriteString(f.ref)
	if withLink {
		b.WriteString(f.link)
	}
	return b.String()
}

// fit returns the longest-names text that fits, in the truncation order.
func (f frame) fit(fits func(string) bool, shortestName int) string {
	withLink := f.link != ""
	fullProject := len(f.project)
	if f.project == nil {
		fullProject = -1
	}
	for n := len(f.name); n >= shortestName; n-- {
		if s := f.render(n, fullProject, withLink); fits(s) {
			return s
		}
	}
	for p := len(f.project) - 1; p >= projectFloor; p-- {
		if s := f.render(shortestName, p, withLink); fits(s) {
			return s
		}
	}
	return f.render(shortestName, -1, withLink)
}

// shorten cuts r to n runes, the last three of them "...".
func shorten(r []rune, n int) string {
	if n >= len(r) {
		return string(r)
	}
	return string(r[:n-len(ellipsis)]) + ellipsis
}

func smsFrame(a PhoneAlert, enc Encoding) (frame, error) {
	verb, isService, err := smsVerb(a)
	if err != nil {
		return frame{}, err
	}
	f := frame{head: "[cerbix] " + verb}
	if isService {
		f.head += "service "
	}
	f.name = []rune(displayName(a.SubjectName, enc))
	if a.ProjectName != "" {
		f.project = []rune(displayName(a.ProjectName, enc))
	}

	if a.Kind == KindMonitorBurn || a.Kind == KindServiceBurn {
		// A burn has no incident: no reference and no link (mock D-22).
		f.tail = fmt.Sprintf(" - burn rate %gx over %s", a.BurnRate, renderText(a.Window, enc))
		return f, nil
	}
	if a.IncidentRef == "" {
		return f, nil
	}
	ref, ok := domain.NormalizeIncidentRef(a.IncidentRef)
	if !ok {
		return frame{}, errors.New("telephony: malformed incident reference")
	}
	f.ref = " - incident " + ref
	if base := strings.TrimRight(a.PublicBaseURL, "/"); base != "" {
		if !LinkBaseRenders(base) {
			return frame{}, errors.New("telephony: public base URL contains white space")
		}
		f.link = " " + base + "/i/" + ref
	}
	return f, nil
}

// LinkBaseRenders reports whether base can carry an incident link: it holds no Unicode space or
// control character. RenderSMS refuses any other base with an error. The caller that picks the base
// tests it first and treats a base that fails as no base at all — the link is dropped and the row
// says no_public_url — because mail.public_base_url, the fallback, is not validated at save, and a
// render error would cost the page, not just the link (final review M-5).
func LinkBaseRenders(base string) bool {
	return strings.IndexFunc(base, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// smsVerb returns the verb part of the head (R17) and whether the subject is
// a service.
func smsVerb(a PhoneAlert) (string, bool, error) {
	switch a.Kind {
	case KindMonitorDown:
		return "DOWN ", false, nil
	case KindMonitorRecovered:
		return "RECOVERED ", false, nil
	case KindServiceHealth:
		switch a.State {
		case "down", "degraded", "unknown":
			return strings.ToUpper(a.State) + " ", true, nil
		}
		return "", false, fmt.Errorf("telephony: unknown service state %q", a.State)
	case KindServiceRecovered:
		return "RECOVERED ", true, nil
	case KindEscalationStep:
		if a.Step < 1 {
			return "", false, fmt.Errorf("telephony: escalation step %d", a.Step)
		}
		if a.Repeat {
			return fmt.Sprintf("ESCALATION step %d (repeat): ", a.Step), a.IsService, nil
		}
		return fmt.Sprintf("ESCALATION step %d: ", a.Step), a.IsService, nil
	case KindMonitorBurn:
		return "BUDGET BURN ", false, nil
	case KindServiceBurn:
		return "BUDGET BURN ", true, nil
	}
	return "", false, fmt.Errorf("telephony: unknown alert kind %q", a.Kind)
}

// renderText normalises user text and, with transliterate, converts it.
func renderText(s string, enc Encoding) string {
	s = normaliseName(s)
	if enc == EncodingTransliterate {
		// Transliteration can leave a double space (a ъ between two words).
		s = normaliseName(transliterate(s))
	}
	return s
}

// displayName is renderText for a name, which is never empty: a name that
// normalises or transliterates to nothing is rendered "?".
func displayName(s string, enc Encoding) string {
	if s = renderText(s, enc); s == "" {
		return "?"
	}
	return s
}

// RenderVoice renders the TwiML of a call: the facts in words, said twice
// (§4.4). Every user-chosen value is normalised and XML-escaped, so markup in
// a name is spoken, never interpreted. Part (a) offers no keypress.
func RenderVoice(a PhoneAlert, language string) string {
	isService := a.IsService
	switch a.Kind {
	case KindServiceHealth, KindServiceRecovered, KindServiceBurn:
		isService = true
	case KindMonitorDown, KindMonitorRecovered, KindMonitorBurn:
		isService = false
	}
	subject := displayName(a.SubjectName, EncodingUnicode)
	if isService {
		subject = "service " + subject
	}
	if p := normaliseName(a.ProjectName); p != "" {
		subject += " in " + p
	}

	var facts string
	switch a.Kind {
	case KindMonitorDown:
		facts = subject + " is down."
	case KindMonitorRecovered, KindServiceRecovered:
		facts = subject + " has recovered."
	case KindServiceHealth:
		switch a.State {
		case "unknown":
			facts = subject + " is in an unknown state."
		default:
			facts = subject + " is " + normaliseName(a.State) + "."
		}
	case KindEscalationStep:
		if a.Repeat {
			facts = fmt.Sprintf("escalation step %d, repeated, for %s.", a.Step, subject)
		} else {
			facts = fmt.Sprintf("escalation step %d for %s.", a.Step, subject)
		}
	case KindMonitorBurn, KindServiceBurn:
		facts = fmt.Sprintf("budget burn on %s. burn rate %g times over %s.", subject, a.BurnRate, normaliseName(a.Window))
	default:
		facts = subject + "."
	}

	say := `<Say language="` + xmlEscape(language) + `">` + xmlEscape("cerbix alert. "+facts) + `</Say>`
	return "<Response>" + say + say + "</Response>"
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails a write
	return b.String()
}
