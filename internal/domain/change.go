package domain

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// FR-025 — change intelligence (func-change-intelligence.md).
//
// This file owns the VOCABULARY and the pure decisions of a change: the closed kinds and phases
// of D2, the phase order of D3, the canonical text form of D2 (the domain is the ONLY Unicode
// authority — the transport normalizes, the store validates through here before SQL, the
// database CHECK enforces only length and the ASCII control class), the closed error codes the
// API maps without string matching, the `🚀 Changes:` marker and note of D7, and the
// comparison's withholding words and horizons of D8. Nothing here reads a clock or a database.

// ChangeKind is what kind of change happened (D2): a deploy, a rollback or a flag flip. A
// configuration change is a `deploy` with a `ref` (D-0209, answer 1).
type ChangeKind string

const (
	ChangeKindDeploy   ChangeKind = "deploy"
	ChangeKindRollback ChangeKind = "rollback"
	ChangeKindFlag     ChangeKind = "flag"
)

// ChangeKinds is the closed kind set, in the order the API documents it.
var ChangeKinds = []ChangeKind{ChangeKindDeploy, ChangeKindRollback, ChangeKindFlag}

// ValidChangeKind reports whether k is one of the three kinds.
func ValidChangeKind(k ChangeKind) bool {
	switch k {
	case ChangeKindDeploy, ChangeKindRollback, ChangeKindFlag:
		return true
	}
	return false
}

// ChangePhase is where the change is in its life (D2, D3): `started`, then exactly one of the
// three terminal phases.
type ChangePhase string

const (
	ChangePhaseStarted   ChangePhase = "started"
	ChangePhaseSucceeded ChangePhase = "succeeded"
	ChangePhaseFailed    ChangePhase = "failed"
	ChangePhaseCancelled ChangePhase = "cancelled"
)

// ChangePhases is the closed phase set, in lifecycle order.
var ChangePhases = []ChangePhase{ChangePhaseStarted, ChangePhaseSucceeded, ChangePhaseFailed, ChangePhaseCancelled}

// ValidChangePhase reports whether p is one of the four phases.
func ValidChangePhase(p ChangePhase) bool {
	switch p {
	case ChangePhaseStarted, ChangePhaseSucceeded, ChangePhaseFailed, ChangePhaseCancelled:
		return true
	}
	return false
}

// IsTerminalPhase reports whether p ends the change (D3): succeeded, failed or cancelled.
func IsTerminalPhase(p ChangePhase) bool {
	return p == ChangePhaseSucceeded || p == ChangePhaseFailed || p == ChangePhaseCancelled
}

// ChangeLinkRole is the role a linked change plays for an incident (D7): recorded on the
// incident's own service, or on a service its impact rows mark `probable_root`.
const (
	ChangeLinkRoleOwnService = "own_service"
	ChangeLinkRoleUpstream   = "upstream"
)

// ChangesMarker prefixes the ONE system-authored note that names the changes preceding an
// auto-incident (D7); its presence is the idempotency guard on redelivery, exactly as
// IncidentContextMarker is for the context note.
const ChangesMarker = "🚀 Changes:"

// Bounds of the text fields (D2), in code points.
const (
	ChangeSourceMaxLen     = 64
	ChangeExternalIDMaxLen = 128
	ChangeRefMaxLen        = 128
	ChangeURLMaxLen        = 512
)

// The closed error codes of FR-025 (D2, D3, D6, D8, D11, D12). Each maps to exactly one HTTP
// answer in the API layer; the store and the domain return them as *ChangeError.
const (
	ChangeErrPhaseOrder            = "phase_order"
	ChangeErrPhaseExists           = "phase_exists"
	ChangeErrKindMismatch          = "kind_mismatch"
	ChangeErrDecisionUnknown       = "decision_unknown"
	ChangeErrOccurredBeforeStart   = "occurred_at_before_start"
	ChangeErrOccurredOutOfBounds   = "occurred_at_out_of_bounds"
	ChangeErrSourceInvalid         = "source_invalid"
	ChangeErrExternalIDInvalid     = "external_id_invalid"
	ChangeErrRefInvalid            = "ref_invalid"
	ChangeErrURLInvalid            = "url_invalid"
	ChangeErrKindInvalid           = "kind_invalid"
	ChangeErrPhaseInvalid          = "phase_invalid"
	ChangeErrRangeInvalid          = "range_invalid"
	ChangeErrRangeTooWide          = "range_too_wide"
	ChangeErrLimitInvalid          = "limit_invalid"
	ChangeErrCursorInvalid         = "cursor_invalid"
	ChangeErrHorizonInvalid        = "horizon_invalid"
	ChangeErrNoTerminalPhase       = "no_terminal_phase"
	ChangeErrActionUnknown         = "action_unknown"
	ChangeErrNoteMaxInvalid        = "correlation_note_max_invalid"
	ChangeErrRetentionBatchInvalid = "retention_batch_invalid"
)

// ChangeError is a refused change input: Code is one of the closed codes above, Field names
// the offending field in the wire's spelling (empty for a whole-request refusal), and Msg
// states the rule, so a 400/404/409 can carry all three without the transport re-deriving them.
type ChangeError struct {
	Code  string
	Field string
	Msg   string
}

func (e *ChangeError) Error() string {
	if e.Field == "" {
		return e.Code + ": " + e.Msg
	}
	return e.Code + " (" + e.Field + "): " + e.Msg
}

// NewChangeError builds a *ChangeError.
func NewChangeError(code, field, format string, args ...any) *ChangeError {
	return &ChangeError{Code: code, Field: field, Msg: fmt.Sprintf(format, args...)}
}

// ChangePhaseOrder is D3's order rule over the phases an identity ALREADY holds and the phase
// about to be recorded. `started` may be followed by exactly one terminal; a terminal alone is
// accepted (many pipelines can only report the end); a second terminal is `phase_order`
// naming the terminal already recorded; `started` after a terminal is `phase_order`. A `next`
// that is already among `existing` is `phase_exists` — the store decides BEFORE calling this
// whether such a replay is identical (200 with the original row) or differing (409), so this
// answer is only a guard against a caller that skipped that step.
func ChangePhaseOrder(existing []ChangePhase, next ChangePhase) error {
	if !ValidChangePhase(next) {
		return NewChangeError(ChangeErrPhaseInvalid, "phase", "must be one of %s, got %q", joinPhases(ChangePhases), next)
	}
	var terminal ChangePhase
	for _, p := range existing {
		if p == next {
			return NewChangeError(ChangeErrPhaseExists, "phase", "%s already recorded", next)
		}
		if IsTerminalPhase(p) {
			terminal = p
		}
	}
	if terminal == "" {
		return nil
	}
	if IsTerminalPhase(next) {
		return NewChangeError(ChangeErrPhaseOrder, "phase", "%s already recorded", terminal)
	}
	return NewChangeError(ChangeErrPhaseOrder, "phase", "%s cannot follow %s", next, terminal)
}

func joinPhases(ps []ChangePhase) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = string(p)
	}
	return strings.Join(names, "|")
}

// NormalizeChangeText is the TRANSPORT's half of D2: Unicode NFC and trimmed of leading and
// trailing whitespace, so the CLI and any other client may send what they were given. It
// decides nothing — the result still goes through ValidateChangeText.
func NormalizeChangeText(value string) string {
	return strings.TrimSpace(norm.NFC.String(value))
}

// ValidateChangeText is the ONE Unicode authority of D2 (invariant 23). It validates a value
// that is expected to be CANONICAL already and refuses, naming `<field>_invalid`:
//
//   - invalid UTF-8;
//   - a value that is not NFC-invariant (the transport failed to normalize) or not trimmed;
//   - any code point of Unicode category Cc or Cf, and any of U+000A, U+000D, U+2028, U+2029;
//   - a length in CODE POINTS above max.
//
// Field is the wire spelling (`external_id`, `ref`, `url`). The returned string is the
// validated value, unchanged — nothing is silently repaired here, because the store calls this
// on what it is about to write and a repair at that layer would mean two canonical forms.
func ValidateChangeText(field, value string, max int) (string, error) {
	code := field + "_invalid"
	if !utf8.ValidString(value) {
		return "", NewChangeError(code, field, "must be valid UTF-8")
	}
	if !norm.NFC.IsNormalString(value) {
		return "", NewChangeError(code, field, "must be Unicode NFC")
	}
	if strings.TrimSpace(value) != value {
		return "", NewChangeError(code, field, "must not start or end with whitespace")
	}
	n := 0
	for _, r := range value {
		n++
		if r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' ||
			unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return "", NewChangeError(code, field, "must not contain control or format characters (found U+%04X)", r)
		}
	}
	if n > max {
		return "", NewChangeError(code, field, "must be at most %d characters, got %d", max, n)
	}
	return value, nil
}

var changeSourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ChangeSourcePattern is the slug class of `source` (D2), as the schema CHECK spells it.
func ChangeSourcePattern() string { return changeSourcePattern.String() }

// ValidateChangeSource refuses a source that is not a slug `^[a-z0-9][a-z0-9-]{0,63}$`
// (`source_invalid`). Lower-case by its class, so identity needs no folding.
func ValidateChangeSource(source string) error {
	if !changeSourcePattern.MatchString(source) {
		return NewChangeError(ChangeErrSourceInvalid, "source", "must match %s, got %q", changeSourcePattern, source)
	}
	return nil
}

// ValidateChangeExternalID is ValidateChangeText for `external_id`, plus the 1..128 lower
// bound (`external_id_invalid`). Case-sensitive: `Run-42` and `run-42` are two identities.
func ValidateChangeExternalID(id string) (string, error) {
	v, err := ValidateChangeText("external_id", id, ChangeExternalIDMaxLen)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", NewChangeError(ChangeErrExternalIDInvalid, "external_id", "must not be empty")
	}
	return v, nil
}

// ValidateChangeRef is ValidateChangeText for `ref` (0..128, `ref_invalid`).
func ValidateChangeRef(ref string) (string, error) {
	return ValidateChangeText("ref", ref, ChangeRefMaxLen)
}

// ValidateChangeURL is ValidateChangeText for `url` (0..512) plus D2's scheme rule: an empty
// url is fine; anything else must be `https://` with a host — a plain `http://` is refused
// (`url_invalid`), because the UI renders it as a link and must not become a phishing surface.
func ValidateChangeURL(u string) (string, error) {
	v, err := ValidateChangeText("url", u, ChangeURLMaxLen)
	if err != nil {
		return "", err
	}
	if v == "" {
		return v, nil
	}
	if !strings.HasPrefix(v, "https://") {
		return "", NewChangeError(ChangeErrURLInvalid, "url", "must start with https:// (http:// is refused)")
	}
	parsed, perr := url.Parse(v)
	if perr != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", NewChangeError(ChangeErrURLInvalid, "url", "must be an absolute https:// URL with a host")
	}
	return v, nil
}

// ── The correlation note (D7) ─────────────────────────────────────────────────────────────

// ChangeNoteEntry is one preceding change as the `🚀 Changes:` note names it.
type ChangeNoteEntry struct {
	Kind       ChangeKind
	Ref        string
	Source     string
	LagSeconds int64
}

// RenderChangesNote renders D7's single system note: `🚀 Changes: <n> preceded this incident —
// <kind ref by source, −<lag>>; …`, at most `max` entries named and the rest counted. The
// caller passes the entries in the order it wants them named (nearest the open first) and the
// TOTAL, which may exceed len(entries). It says "preceded", never "caused" (invariant 9). An
// empty batch renders "" — the caller then writes no note.
func RenderChangesNote(entries []ChangeNoteEntry, total, max int) string {
	if total <= 0 || len(entries) == 0 {
		return ""
	}
	if max < 1 {
		max = 1
	}
	named := entries
	if len(named) > max {
		named = named[:max]
	}
	var b strings.Builder
	b.WriteString(ChangesMarker)
	fmt.Fprintf(&b, " %d preceded this incident — ", total)
	for i, e := range named {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(string(e.Kind))
		if e.Ref != "" {
			b.WriteString(" ")
			b.WriteString(e.Ref)
		}
		fmt.Fprintf(&b, " by %s, −%s", e.Source, formatLag(e.LagSeconds))
	}
	if rest := total - len(named); rest > 0 {
		fmt.Fprintf(&b, "; … and %d more", rest)
	}
	b.WriteString(".")
	return b.String()
}

// formatLag renders a lag in the unit an on-call reader thinks in: seconds below a minute,
// whole minutes below an hour, then h+m.
func formatLag(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		h, m := seconds/3600, (seconds%3600)/60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}

// ── The comparison (D8) ──────────────────────────────────────────────────────────────────

// The withholding words of the before/after comparison — the reliability page's own vocabulary
// (D8): a figure is absent with exactly one of these.
const (
	// ChangeCompareWithheldDefinitionChanged: a revision or epoch boundary inside the side.
	ChangeCompareWithheldDefinitionChanged = "definition_changed"
	// ChangeCompareWithheldUndecidable: the page would withhold availability for that range;
	// the page's own reason string rides beside it.
	ChangeCompareWithheldUndecidable = "undecidable"
	// ChangeCompareWithheldNoFacts: no sealed bucket in the range.
	ChangeCompareWithheldNoFacts = "no_facts"
	// ChangeCompareWithheldPending: the side's end exceeds sealed_through (`after` past T + h;
	// `before` too when T itself is past the seal — D-0211); sealed_through is stated and NO partial
	// figure is given.
	ChangeCompareWithheldPending = "pending"
)

// ChangeCompareHorizons is the closed horizon set (D8, D-0209 answer 4): no 7d — a slow burn
// is the burn rules' job.
var ChangeCompareHorizons = []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

// ChangeCompareDefaultHorizon is the request's default `horizon`.
const ChangeCompareDefaultHorizon = time.Hour

// ValidChangeCompareHorizon reports whether h is one of the four horizons.
func ValidChangeCompareHorizon(h time.Duration) bool {
	for _, allowed := range ChangeCompareHorizons {
		if h == allowed {
			return true
		}
	}
	return false
}

// ── The read models the store returns and the API serializes ─────────────────────────────

// ChangePhaseRow is one row of `service_changes` (§5): a phase of one change.
type ChangePhaseRow struct {
	ID         string      `json:"id"`
	ProjectID  string      `json:"project_id"`
	ServiceID  string      `json:"service_id"`
	Source     string      `json:"source"`
	ExternalID string      `json:"external_id"`
	Kind       ChangeKind  `json:"kind"`
	Phase      ChangePhase `json:"phase"`
	Ref        string      `json:"ref"`
	URL        string      `json:"url"`
	OccurredAt time.Time   `json:"occurred_at"`
	DecisionID *string     `json:"decision_id,omitempty"`
	// The actor triple (D5): the immutable label plus the typed pair.
	ActorLabel  string    `json:"actor_label"`
	ActorUserID *string   `json:"actor_user_id,omitempty"`
	ViaToken    bool      `json:"via_token"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// ChangeDecisionLink is a group's gate decision as the timeline reads it back by id (D11):
// state and action from the ledger while the row exists, `aged_out` once it is gone.
type ChangeDecisionLink struct {
	ID         string      `json:"id"`
	State      *GateState  `json:"state,omitempty"`
	Action     *GateAction `json:"action,omitempty"`
	Overridden bool        `json:"overridden,omitempty"`
	AgedOut    bool        `json:"aged_out"`
}

// ChangeIncidentLink is one incident a change PRECEDED (D7, read from `incident_changes` on the
// change side): the incident, when it opened, the role and the lag, and which phase row the
// link anchors.
type ChangeIncidentLink struct {
	IncidentID string    `json:"incident_id"`
	OpenedAt   time.Time `json:"opened_at"`
	Role       string    `json:"role"`
	LagSeconds int64     `json:"lag_seconds"`
	ChangeID   string    `json:"change_id"`
}

// ChangeGroup is one external identity's change on the timeline (D6): the identity, the
// group's kind, `latest_occurred_at` (the max over its phases — the group key's instant), ALL
// its phases nested and ordered, the decision link resolved, and the incidents it preceded.
type ChangeGroup struct {
	Source           string               `json:"source"`
	ExternalID       string               `json:"external_id"`
	Kind             ChangeKind           `json:"kind"`
	LatestOccurredAt time.Time            `json:"latest_occurred_at"`
	Phases           []ChangePhaseRow     `json:"phases"`
	Decision         *ChangeDecisionLink  `json:"decision,omitempty"`
	Incidents        []ChangeIncidentLink `json:"incidents"`
}

// TerminalPhase returns the group's terminal phase row, or nil when it has only `started`.
func (g ChangeGroup) TerminalPhase() *ChangePhaseRow {
	for i := range g.Phases {
		if IsTerminalPhase(g.Phases[i].Phase) {
			return &g.Phases[i]
		}
	}
	return nil
}

// IncidentChangeLink is one change that preceded an incident, read from the incident side
// (D7, `GET /incidents/{id}/changes`): the anchored phase row with the copied instant and
// lag, and the group's CURRENT phases read live beside it.
type IncidentChangeLink struct {
	Change     ChangePhaseRow   `json:"change"`
	Role       string           `json:"role"`
	OccurredAt time.Time        `json:"occurred_at"`
	LagSeconds int64            `json:"lag_seconds"`
	ComputedAt time.Time        `json:"computed_at"`
	Phases     []ChangePhaseRow `json:"phases"`
}

// ChangeCompareFigure is one side of the comparison when it can be stated (D8). Durations
// carries the exact integer-µs sums the seconds are derived from — the same axes the
// reliability page's segments expose — so parity with the series is checkable to the µs.
type ChangeCompareFigure struct {
	Availability    float64              `json:"availability"`
	GoodSeconds     float64              `json:"good_seconds"`
	BadSeconds      float64              `json:"bad_seconds"`
	UnknownSeconds  float64              `json:"unknown_seconds"`
	ExcludedSeconds float64              `json:"excluded_seconds"`
	Buckets         int64                `json:"buckets"`
	Durations       ReliabilityDurations `json:"durations"`
}

// ChangeCompareWithheld is one side of the comparison when it is NOT stated (D8): one reason
// from the closed vocabulary; Detail carries the reliability page's own reason string when the
// reason is `undecidable`; SealedThrough is stated for `pending`.
type ChangeCompareWithheld struct {
	Reason        string     `json:"reason"`
	Detail        string     `json:"detail,omitempty"`
	SealedThrough *time.Time `json:"sealed_through,omitempty"`
}

// ChangeCompareSide is exactly one of a figure or a withholding.
type ChangeCompareSide struct {
	From     time.Time              `json:"from"`
	To       time.Time              `json:"to"`
	Figure   *ChangeCompareFigure   `json:"figure,omitempty"`
	Withheld *ChangeCompareWithheld `json:"withheld,omitempty"`
}

// ChangeComparison is the D8 response: the identity, the terminal phase row it rests on, T (the
// terminal's instant floored to the canonical bucket), the horizon, both sides, and `delta`
// (after − before, availability points) ONLY when both sides are figures. It is a READ over
// facts other owners sealed: nothing is stored, nothing is cached.
type ChangeComparison struct {
	Source        string            `json:"source"`
	ExternalID    string            `json:"external_id"`
	Change        ChangePhaseRow    `json:"change"`
	T             time.Time         `json:"t"`
	Horizon       string            `json:"horizon"`
	Before        ChangeCompareSide `json:"before"`
	After         ChangeCompareSide `json:"after"`
	Delta         *float64          `json:"delta,omitempty"`
	SealedThrough *time.Time        `json:"sealed_through,omitempty"`
	AsOf          time.Time         `json:"as_of"`
}
