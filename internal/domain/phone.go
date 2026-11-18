package domain

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Phone channel types (FR-041a §4.3).
const (
	ChannelSMS   ChannelType = "sms"
	ChannelVoice ChannelType = "voice"
)

// IsPhoneChannelType reports whether t is an SMS or voice channel.
func IsPhoneChannelType(t ChannelType) bool { return t == ChannelSMS || t == ChannelVoice }

// Recipient modes of a phone channel.
const (
	PhoneRecipientUser   = "user"
	PhoneRecipientNumber = "number"
)

// Closed set of phone delivery statuses/reasons (§4.5, part a).
const (
	PhoneReasonSending                    = "sending"
	PhoneReasonSent                       = "sent"
	PhoneReasonProviderUnreachable        = "provider_unreachable"
	PhoneReasonProviderRejected           = "provider_rejected"
	PhoneReasonOutcomeUnknown             = "outcome_unknown"
	PhoneReasonNotDeliverable             = "not_deliverable"
	PhoneReasonVoiceUnsupportedByProvider = "voice_unsupported_by_provider"
	PhoneReasonTelephonyDisabled          = "telephony_disabled"
	PhoneReasonStormLimited               = "storm_limited"
)

var phoneReasons = map[string]bool{
	PhoneReasonSending: true, PhoneReasonSent: true, PhoneReasonProviderUnreachable: true,
	PhoneReasonProviderRejected: true, PhoneReasonOutcomeUnknown: true, PhoneReasonNotDeliverable: true,
	PhoneReasonVoiceUnsupportedByProvider: true, PhoneReasonTelephonyDisabled: true, PhoneReasonStormLimited: true,
}

// ValidPhoneReason reports whether r is in the closed reason set.
func ValidPhoneReason(r string) bool { return phoneReasons[r] }

// Notes: informational annotations on a delivery, not reasons.
const (
	PhoneNoteNoIncidentRef        = "no_incident_ref"
	PhoneNoteNoPublicURL          = "no_public_url"
	PhoneNoteLinkOmitted          = "link_omitted"
	PhoneNoteNoVerifiedNumber     = "no_verified_number"
	PhoneNoteRecipientNotEligible = "recipient_not_eligible"
	// PhoneNoteStormPerIncident and PhoneNoteStormPerNumber name the storm limit that refused a
	// `storm_limited` row (§4.5, §6.2) — the per-incident one includes its monitor-counted form. The
	// limit's value is never recorded.
	PhoneNoteStormPerIncident = "storm_per_incident"
	PhoneNoteStormPerNumber   = "storm_per_number"
)

// Reasons of the channel DTO's deliverable.reason (R11).
const (
	DeliverableDisabled                   = "disabled"
	DeliverableTelephonyDisabled          = "telephony_disabled"
	DeliverableVoiceUnsupportedByProvider = "voice_unsupported_by_provider"
	DeliverableNumberUnverified           = "number_unverified"
	DeliverableNoVerifiedNumber           = "no_verified_number"
	DeliverableRecipientNotEligible       = "recipient_not_eligible"
	DeliverableRecipientDeleted           = "recipient_deleted"
)

// ChannelDeliverable is a phone channel's answer to "is this a route" (D31 rule 1), with the reason
// (one of the Deliverable* constants above) when it is not (R11).
type ChannelDeliverable struct {
	Route  bool   `json:"route"`
	Reason string `json:"reason,omitempty"`
}

var (
	e164Strip = strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "")
	e164Re    = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
)

// ErrInvalidPhone is returned for input that is not an E.164 number. It never
// carries the input (P3).
var ErrInvalidPhone = errors.New("invalid phone number")

// NormalizeE164 strips spaces, dashes, dots and parentheses and requires E.164.
func NormalizeE164(input string) (string, error) {
	s := e164Strip.Replace(input)
	if !e164Re.MatchString(s) {
		return "", ErrInvalidPhone
	}
	return s, nil
}

var twoDigitCodes = map[string]bool{}

func init() {
	for _, c := range strings.Split("20,27,30,31,32,33,34,36,39,40,41,43,44,45,46,47,48,49,51,52,53,54,55,56,57,58,60,61,62,63,64,65,66,81,82,84,86,90,91,92,93,94,95,98", ",") {
		twoDigitCodes[c] = true
	}
}

// MaskPhone renders "+<calling code>•••••<last 4 digits>" of an E.164 number,
// or "" for input that is not one.
func MaskPhone(e164 string) string {
	if !e164Re.MatchString(e164) {
		return ""
	}
	digits := e164[1:]
	n := 3
	switch {
	case digits[0] == '1' || digits[0] == '7':
		n = 1
	case twoDigitCodes[digits[:2]]:
		n = 2
	}
	return "+" + digits[:n] + "•••••" + digits[len(digits)-4:]
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeIncidentRef upper-cases s, folds the Crockford aliases (I, L -> 1;
// O -> 0) and reports whether the result is exactly 8 characters of the alphabet.
func NormalizeIncidentRef(s string) (string, bool) {
	if len(s) != 8 {
		return "", false
	}
	b := []byte(s)
	for i, ch := range b {
		if ch >= 0x80 {
			return "", false
		}
		if ch >= 'a' && ch <= 'z' {
			ch -= 'a' - 'A'
		}
		switch ch {
		case 'I', 'L':
			ch = '1'
		case 'O':
			ch = '0'
		}
		if strings.IndexByte(crockford, ch) < 0 {
			return "", false
		}
		b[i] = ch
	}
	return string(b), true
}

// UserPhone is a person's own number as every reader sees it (§4.2, D35): the mask, never the
// number — the owner's own `GET /api/v1/me` included. Language nil means the instance's voice
// language.
type UserPhone struct {
	NumberMasked string  `json:"number_masked"`
	Verified     bool    `json:"verified"`
	Language     *string `json:"language"`
}

// Subjects of a verification code: a person's own number, or a `number`-mode channel's (R6).
const (
	PhoneSubjectUser    = "user"
	PhoneSubjectChannel = "channel"
)

// Verification codes (§4.2, R5, R6). The hourly limit is counted on the REQUESTER — the principal
// who asks for the code (a person's user id, an API token's "apitoken:<id>") — across every number
// it verifies: its own and every `number` channel's (FR-041a review P2-3). The daily limit is the
// number's, across every requester.
const (
	PhoneCodeTTL                  = 10 * time.Minute
	PhoneCodeMaxAttempts          = 5
	PhoneCodesPerRequesterPerHour = 3
	PhoneCodesPerNumberPerDay     = 10
)

// Refusals of the phone-number writes. None of them carries the number (P3).
var (
	// ErrPhoneKeyMissing: a number is stored only as ciphertext and a keyed hash, both of which
	// need the at-rest key (D29).
	ErrPhoneKeyMissing = errors.New("phone numbers need security.encryption_key to be configured")
	// ErrPhoneNumberInUse: the number is another user's (one number, one person).
	ErrPhoneNumberInUse = errors.New("this number is registered to another user")
	// ErrPhoneSendLimited: R6 — 3 codes per requester per hour, 10 per number per day.
	ErrPhoneSendLimited = errors.New("too many verification codes were requested; try again later")
	// ErrPhoneCodeIncorrect: the code does not match the active one (the attempt is counted).
	ErrPhoneCodeIncorrect = errors.New("the verification code is incorrect")
	// ErrPhoneCodeExpired: the subject's latest code expired or was used, or there is none. A code
	// replaced by a newer send is NOT this: only the latest code is judged, so an older one is a
	// mismatch against it — ErrPhoneCodeIncorrect, with the attempt counted on the newer code.
	ErrPhoneCodeExpired = errors.New("the verification code has expired; request a new one")
	// ErrPhoneCodeAttemptsExhausted: five incorrect attempts end a code.
	ErrPhoneCodeAttemptsExhausted = errors.New("too many incorrect attempts; request a new code")
	// ErrPhoneNotHuman: machine users have no phone (§4.2).
	ErrPhoneNotHuman = errors.New("only a person can have a phone number")
	// ErrPhoneNoNumber: a new code is sent to the STORED number, and there is none (§4.2).
	ErrPhoneNoNumber = errors.New("no phone number is stored; enter your number first")
	// ErrPhoneAlreadyVerified: the stored number needs no code (§4.2).
	ErrPhoneAlreadyVerified = errors.New("this phone number is already verified")
)

// Refusals of the phone-channel writes (§4.2, §4.3, D3, D27, D28). None of them carries a number.
var (
	// ErrPhoneTelephonyDisabled: no phone channel is created or enabled while telephony is off (D27).
	ErrPhoneTelephonyDisabled = errors.New("phone alerts are turned off on this instance")
	// ErrPhoneVoiceUnsupported: the provider in force cannot place calls (D3).
	ErrPhoneVoiceUnsupported = errors.New("the telephony provider in force sends SMS only; a voice channel cannot be created or enabled")
	// ErrPhoneRecipientNotEligible: a `user` channel phones a person who reaches its project (§4.3).
	ErrPhoneRecipientNotEligible = errors.New("the recipient must be a person with access to the channel's project")
	// ErrPhoneRecipientFixed: a channel's recipient mode is fixed at creation (§4.3).
	ErrPhoneRecipientFixed = errors.New("a phone channel's recipient mode is fixed at creation; create another channel")
	// ErrPhoneNumberUnverified: a `number` channel is enabled only once its number is verified (§4.2).
	ErrPhoneNumberUnverified = errors.New("verify the channel's number before enabling it")
	// ErrPhoneChannelNotNumber: only a `number` channel has a number of its own to verify.
	ErrPhoneChannelNotNumber = errors.New("only a phone channel with its own number is verified this way")
)

// PhoneRecipient is one person a `user` channel of a project may phone (§4.3): never a number, not
// even masked — only whether the person has a verified one.
type PhoneRecipient struct {
	UserID        string `json:"user_id"`
	DisplayName   string `json:"display_name"`
	Email         string `json:"email"`
	PhoneVerified bool   `json:"phone_verified"`
}

var (
	// phoneLanguageRe is the BCP-47-ish shape the voice language of the settings has.
	phoneLanguageRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	phoneCodeRe     = regexp.MustCompile(`^[0-9]{6}$`)
)

// maxPhoneLanguageLen bounds a stored language tag; real tags are far shorter.
const maxPhoneLanguageLen = 35

// ValidPhoneLanguage reports whether s may be a person's alert language.
func ValidPhoneLanguage(s string) bool {
	return len(s) <= maxPhoneLanguageLen && phoneLanguageRe.MatchString(s)
}

// ValidPhoneCode reports whether s has the shape of a verification code: six ASCII digits.
func ValidPhoneCode(s string) bool { return phoneCodeRe.MatchString(s) }

// RecipientName is the display name of a `user` channel's recipient as the channel answers with it
// (§4.3): absent when Present is false (omitzero), null when Present and Name is nil (the user is
// deleted), else the name.
type RecipientName struct {
	Present bool
	Name    *string
}

// IsZero reports an absent name, which `omitzero` leaves out of the JSON.
func (n RecipientName) IsZero() bool { return !n.Present }

// MarshalJSON writes the name, or null.
func (n RecipientName) MarshalJSON() ([]byte, error) {
	if n.Name == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Name)
}

// UnmarshalJSON reads a name or null; either is present.
func (n *RecipientName) UnmarshalJSON(b []byte) error {
	n.Present, n.Name = true, nil
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n.Name = &s
	return nil
}

// OnlyRouteFor is one phone channel's `only_route_for` (§4.3): at most 50 services, ordered by
// project name then slug, and how many there are in all.
type OnlyRouteFor struct {
	Services []ServiceWithoutRoute
	Total    int
}

// SetOnlyRouteFor puts o on the channel: an empty list, never null, for a channel that is nobody's
// only route.
func (c *NotificationChannel) SetOnlyRouteFor(o OnlyRouteFor) {
	services := o.Services
	if services == nil {
		services = []ServiceWithoutRoute{}
	}
	total := o.Total
	c.OnlyRouteFor, c.OnlyRouteForTotal = &services, &total
}
