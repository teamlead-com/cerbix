package telephony

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
)

var (
	// ErrTelephonyDisabled: the instance's telephony is off or was never saved (D27, D36).
	ErrTelephonyDisabled = errors.New("telephony is disabled")
	// ErrSecretsUnreadable: the secret of the provider in force is stored but cannot be opened with the
	// configured keys.
	ErrSecretsUnreadable = errors.New("the telephony provider secret cannot be decrypted with the configured keys")
)

// SettingsSource is what the service reads the telephony group from — settings.Service in production,
// a fixed value in tests: the snapshot (Telephony), and the group as it is stored NOW
// (CurrentTelephony), which CurrentProvider builds from. Both are required: a source that could not
// read the current group would have CurrentProvider send with a snapshot up to a refresh old, the
// staleness FR-041a review P2-8 removed, and a wrapper must not be able to bring that back silently
// (Group-A review M-5).
type SettingsSource interface {
	Telephony() domain.TelephonySettings
	CurrentTelephony(ctx context.Context) (domain.TelephonySettings, error)
}

// Service builds the Provider the current settings describe. It builds on each use rather than
// caching, so a save is in force for the next send on every replica that has refreshed its snapshot
// (Provider), or at once (CurrentProvider).
type Service struct {
	settings SettingsSource
	client   *http.Client
}

// NewService wires the settings source and the HTTP client every provider gets — the notification
// egress guard's client in production; nil gets the providers' redirect-refusing fallback.
func NewService(settings SettingsSource, client *http.Client) *Service {
	return &Service{settings: settings, client: client}
}

// Settings returns the telephony group in force.
func (s *Service) Settings() domain.TelephonySettings { return s.settings.Telephony() }

// Provider builds the provider of the settings in force. ErrTelephonyDisabled while telephony is off;
// ErrSecretsUnreadable when the secret THAT provider needs is stored but cannot be opened — the other
// provider's secret does not matter.
func (s *Service) Provider() (Provider, error) {
	return s.build(s.settings.Telephony())
}

// CurrentProvider is Provider built from the settings as they are stored now (FR-041a review P2-8):
// the request paths an admin or a person waits on — a verification code, the test SMS — must not send
// with a snapshot up to a refresh old on another replica than the one that saved. The current read is
// its only path; a failed read is an error, never the snapshot's answer.
func (s *Service) CurrentProvider(ctx context.Context) (Provider, error) {
	ts, err := s.settings.CurrentTelephony(ctx)
	if err != nil {
		return nil, fmt.Errorf("telephony: read the current settings: %w", err)
	}
	return s.build(ts)
}

// build makes the provider ts describes.
func (s *Service) build(ts domain.TelephonySettings) (Provider, error) {
	if !ts.Enabled {
		return nil, ErrTelephonyDisabled
	}
	switch ts.Provider {
	case domain.TelephonyProviderTwilio:
		if ts.AuthTokenUnreadable {
			return nil, ErrSecretsUnreadable
		}
		return NewTwilio(TwilioConfig{
			AccountSID: ts.Twilio.AccountSID, AuthToken: ts.Twilio.AuthToken, From: ts.Twilio.From,
		}, s.client), nil
	case domain.TelephonyProviderGenericHTTP:
		// Without its stored headers a gateway would get an unauthenticated request; refuse instead.
		if ts.HeadersUnreadable {
			return nil, ErrSecretsUnreadable
		}
		g := ts.GenericHTTP
		return NewGenericHTTP(GenericHTTPConfig{
			Method: g.Method, URL: g.URL, Headers: g.Headers, ContentType: g.ContentType,
			BodyTemplate: g.BodyTemplate, From: g.From, ResponseCodeField: g.ResponseCodeField,
		}, s.client)
	default:
		return nil, fmt.Errorf("telephony: unknown provider %q", ts.Provider)
	}
}

// ValidateSettings is the whole save-time check of a telephony group: the domain rules, then what the
// provider package owns — the body template's grammar (§4.4, D3) and the header names and values.
// Neither error quotes the template or a header: both may carry a credential.
//
// The template is judged only when generic_http is BOTH enabled AND the selected provider (FR-041a
// review P3-1, then owner answer 2 to review M-6): a disabled group, or another provider, keeps it as a
// draft that does not block the save. Nothing is lost by waiting — every save sends the template
// again, so the save that enables generic_http judges it, and a provider is built only while enabled.
// The domain's field formats and the headers are checked whatever the provider: the headers are a
// secret a later blank write keeps, so they are judged when they are sent.
func ValidateSettings(ts domain.TelephonySettings) error {
	if err := ts.Validate(); err != nil {
		return err
	}
	g := ts.GenericHTTP
	inForce := ts.Enabled && ts.Provider == domain.TelephonyProviderGenericHTTP
	if inForce && strings.TrimSpace(g.BodyTemplate) != "" {
		if err := ValidateTemplate(g.ContentType, g.BodyTemplate); err != nil {
			return fmt.Errorf("generic_http.body_template: %w", err)
		}
	}
	for k, v := range g.Headers {
		if !validHeaderName(k) || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("%w: generic_http.headers must have token names and single-line values",
				domain.ErrInvalidTelephonySettings)
		}
	}
	return nil
}
