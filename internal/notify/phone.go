package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// Phone delivery (FR-041a D16, D19, D20, D31, D33): an sms or voice channel gets a structured alert,
// rendered per channel, through a row reserved before the provider is called. The text paths never
// see these channels (dispatchStore).

const (
	// phoneCallTimeout bounds one provider request; the delivery's remaining budget bounds it further
	// (D33).
	phoneCallTimeout = 5 * time.Second
	// phoneConcurrency is how many phone channels of one event are delivered at once (D33).
	phoneConcurrency = 4
	// phoneRecordTimeout bounds each write of the delivery record. The record is written with a
	// context of its own — not the budget's — so that an answer the provider gave at the end of the
	// budget is stored rather than left `sending` to be closed as outcome_unknown.
	phoneRecordTimeout = 5 * time.Second
	// phoneProviderNone labels the rows of a delivery that has no provider at all.
	phoneProviderNone = "none"
)

// PhoneStore is the phone delivery record (§4.5, D31, D33).
type PhoneStore interface {
	// PhoneTargets returns the enabled sms and voice channels among channelIDs with their D31 rule-1
	// verdict and their verified number.
	PhoneTargets(ctx context.Context, channelIDs []string) ([]domain.PhoneTarget, error)
	ReservePhoneDelivery(ctx context.Context, in domain.PhoneReserveInput) (domain.PhoneReservation, error)
	// RecordUnsentPhoneDelivery writes the final row of a channel that is no route at send. wrote is
	// false when the event already has a row for the channel; rowReason is that row's reason then.
	RecordUnsentPhoneDelivery(ctx context.Context, in domain.PhoneReserveInput, reason string) (wrote bool, rowReason string, err error)
	// FinishPhoneDelivery stores the provider's answer; an error matching domain.ErrPhoneDeliveryClosed
	// when the row was closed meanwhile.
	FinishPhoneDelivery(ctx context.Context, id string, out domain.PhoneOutcome) error
}

// TelephonySource is the telephony in force: telephony.Service in production.
type TelephonySource interface {
	Settings() domain.TelephonySettings
	Provider() (telephony.Provider, error)
}

// PhoneMetrics counts phone deliveries (§9): metrics.Registry. Optional and nil-safe.
type PhoneMetrics interface {
	RecordPhoneDelivery(kind, provider, reason string)
	RecordPhoneQuotaEvent(scope, kind, event string)
}

type phoneDeps struct {
	store   PhoneStore
	src     TelephonySource
	metrics PhoneMetrics
	logger  *slog.Logger
}

// WithPhone wires phone delivery: the delivery record, the telephony in force, the metrics (nil-safe)
// and the logger (nil: slog.Default). Without it DeliverPhone refuses a set that holds a phone channel
// rather than drop it.
func (d *Dispatcher) WithPhone(store PhoneStore, src TelephonySource, m PhoneMetrics, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d.store.phone = &phoneDeps{store: store, src: src, metrics: m, logger: logger}
	return d
}

// dispatchStore is the Store as the Dispatcher uses it. The text paths read channels through it with
// every sms and voice channel left out — those are DeliverPhone's (D16) — so no text path sends to
// one or counts one as resolved, and a set without a phone channel reads exactly as before (P14).
// Each channel left out is recorded in the context's domain.PhoneChannelsSeen, when it carries one:
// that is how the outbox learns an event has a phone recipient without a read of its own.
// phone is DeliverPhone's wiring, nil until WithPhone.
type dispatchStore struct {
	Store
	phone *phoneDeps
}

func (s *dispatchStore) ListEnabledChannelsForMonitor(ctx context.Context, monitorID string) ([]domain.NotificationChannel, error) {
	chs, err := s.Store.ListEnabledChannelsForMonitor(ctx, monitorID)
	return withoutPhoneChannels(ctx, chs, err), err
}

func (s *dispatchStore) ListEnabledChannelsByProject(ctx context.Context, projectID string) ([]domain.NotificationChannel, error) {
	chs, err := s.Store.ListEnabledChannelsByProject(ctx, projectID)
	return withoutPhoneChannels(ctx, chs, err), err
}

func (s *dispatchStore) EnabledChannelsByIDs(ctx context.Context, ids []string) ([]domain.NotificationChannel, error) {
	chs, err := s.Store.EnabledChannelsByIDs(ctx, ids)
	return withoutPhoneChannels(ctx, chs, err), err
}

// withoutPhoneChannels drops the sms and voice channels, recording each in ctx's collector, and then
// — when err is nil — records there that the read answered, which starts the outbox's phone half
// beside this text half's sends (FR-041a review P2-1); a list without a phone channel is returned as
// it is.
func withoutPhoneChannels(ctx context.Context, channels []domain.NotificationChannel, err error) []domain.NotificationChannel {
	out := channels
	for i, ch := range channels {
		if domain.IsPhoneChannelType(ch.Type) {
			out = append([]domain.NotificationChannel(nil), channels[:i]...)
			domain.RecordPhoneChannelSeen(ctx, ch.ID)
			for _, c := range channels[i+1:] {
				if domain.IsPhoneChannelType(c.Type) {
					domain.RecordPhoneChannelSeen(ctx, c.ID)
				} else {
					out = append(out, c)
				}
			}
			break
		}
	}
	if err == nil {
		domain.RecordChannelReadCompleted(ctx)
	}
	return out
}

// DeliverPhone delivers alert to the sms and voice channels among channelIDs; every other id is the
// text paths' and is ignored (D16). For each phone channel:
//
//   - not a route at send (D31 rule 1: PhoneTargets' verdict, or no verified number) → its final row
//     with the reason of §4.5 ("when a row is written"), NOT counted in Resolved;
//   - a route → counted in Resolved; rendered (the recipient's language, else the instance's);
//     reserved (D33, the storm guard inside); then — on Proceed — the provider is called with
//     min(5 s, the remaining budget) and the answer is stored. At most four channels at once;
//   - a route whose verified number cannot be opened (PhoneTarget.Unreadable) → counted in Resolved,
//     neither reserved nor recorded, and an error for that channel alone: the event is retried and
//     its other channels are phoned once (D19);
//   - a voice channel and an alert D18 sends as SMS only (a recovery) → not handed the alert: no row
//     and no call, but counted in Resolved when it is a route (rule 1 and nothing else, D31);
//   - a channel deleted after the targets were read (domain.ErrPhoneChannelGone from the record) → no
//     row, not counted, no error, logged (FR-041a review P3-4).
//
// Delivered counts the channels whose provider accepted the alert, on this attempt or an earlier one
// of the same event (D20). Requested is len(channelIDs), the snapshot: a caller that also called a
// text path for the same snapshot must not add it twice (D16).
//
// The error asks the outbox to retry the event (D19): provider_unreachable (a channel the budget did
// not reach included), a `sending` row too young to judge (domain.ErrPhoneDeliveryInFlight), a store
// or render failure. Every final outcome — sent, rejected, storm_limited, outcome_unknown, an unsent
// reason, a row that was already final — is nil. An error returned before the channels were counted
// matches domain.ErrPhoneRoutesUncounted: Resolved is then no count (FR-041a review P2-7).
func (d *Dispatcher) DeliverPhone(
	ctx context.Context, channelIDs []string, alert telephony.PhoneAlert, ev domain.PhoneEventRef,
) (domain.ChannelDelivery, error) {
	out := domain.ChannelDelivery{Requested: len(channelIDs)}
	ph := d.store.phone
	if ph == nil {
		return out, domain.PhoneRoutesUncounted(d.refuseUnwiredPhone(ctx, channelIDs))
	}
	targets, err := ph.store.PhoneTargets(ctx, channelIDs)
	if err != nil {
		return out, domain.PhoneRoutesUncounted(fmt.Errorf("notify: phone channels: %w", err))
	}
	if len(targets) == 0 {
		return out, nil
	}
	if !domain.ValidPhoneAlertKind(string(alert.Kind)) {
		return out, domain.PhoneRoutesUncounted(fmt.Errorf("notify: unknown phone alert kind %q", alert.Kind))
	}
	run := newPhoneRun(ph, alert, ev, targets)

	results := make([]phoneResult, len(targets))
	sem := make(chan struct{}, phoneConcurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		if t.Kind == domain.ChannelVoice && !alert.Kind.Voiced() {
			continue // D18: not this channel's alert
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			results[i] = run.deliver(ctx, t)
		}()
	}
	wg.Wait()

	var errs []error
	for i, r := range results {
		if routeAtSend(targets[i]) && !r.gone {
			out.Resolved++
		}
		if r.delivered {
			out.Delivered++
		}
		if r.err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", targets[i].ChannelID, r.err))
		}
	}
	return out, errors.Join(errs...)
}

// PhoneRoutes counts the sms and voice channels among channelIDs that are routes now (D31 rule 1),
// handing nobody anything: no row, no provider. It is what a service alert that D18 does not phone (a
// burn CLEAR, a close other than `recovered`) adds to its text path's Resolved, so that a phone
// recipient which is there is not reported missing (D16). Without WithPhone: 0 for a set without a
// phone channel, an error for one with.
func (d *Dispatcher) PhoneRoutes(ctx context.Context, channelIDs []string) (int, error) {
	ph := d.store.phone
	if ph == nil {
		return 0, d.refuseUnwiredPhone(ctx, channelIDs)
	}
	targets, err := ph.store.PhoneTargets(ctx, channelIDs)
	if err != nil {
		return 0, fmt.Errorf("notify: phone channels: %w", err)
	}
	n := 0
	for _, t := range targets {
		if routeAtSend(t) {
			n++
		}
	}
	return n, nil
}

// routeAtSend is D31 rule 1 at send: the channel is a route by configuration, whether or not its
// number could be opened this time (an unreadable one fails and is retried, it does not stop being a
// route).
func routeAtSend(t domain.PhoneTarget) bool { return t.Route() || t.Unreadable() }

// refuseUnwiredPhone is DeliverPhone without WithPhone: nothing to do for a set without a phone
// channel, an error — never a silent drop — for one with.
func (d *Dispatcher) refuseUnwiredPhone(ctx context.Context, channelIDs []string) error {
	if len(channelIDs) == 0 {
		return nil
	}
	chs, err := d.store.Store.EnabledChannelsByIDs(ctx, channelIDs)
	if err != nil {
		return fmt.Errorf("notify: channels by ids: %w", err)
	}
	for _, ch := range chs {
		if domain.IsPhoneChannelType(ch.Type) {
			return errors.New("notify: phone delivery is not configured")
		}
	}
	return nil
}

// phoneRun is one DeliverPhone call: what every channel of it shares.
type phoneRun struct {
	ph           *phoneDeps
	alert        telephony.PhoneAlert
	ev           domain.PhoneEventRef
	settings     domain.TelephonySettings
	provider     telephony.Provider // nil when no channel is a route: nothing to call
	providerName string
}

type phoneResult struct {
	delivered bool
	err       error
	// gone: the channel (or its project) was deleted after the targets were read — no row, not
	// counted, no error (FR-041a review P3-4).
	gone bool
}

// newPhoneRun reads the settings in force and builds the provider — only when some channel is a
// route: with telephony off every channel is `telephony_disabled` and no provider is built. A provider
// that cannot be built (its secret unreadable, a snapshot not yet refreshed) answers every call as
// unreachable, so each route gets its row and the event is retried.
func newPhoneRun(ph *phoneDeps, alert telephony.PhoneAlert, ev domain.PhoneEventRef, targets []domain.PhoneTarget) *phoneRun {
	r := &phoneRun{ph: ph, alert: alert, ev: ev, settings: ph.src.Settings()}
	r.providerName = r.settings.Provider
	if r.providerName == "" {
		r.providerName = phoneProviderNone
	}
	for _, t := range targets {
		if !t.Route() {
			continue
		}
		p, err := ph.src.Provider()
		if err != nil {
			ph.logger.Warn("phone provider unavailable; the event's phone channels are retried",
				"provider", r.providerName, "err", err)
			p = unavailableProvider{}
		} else {
			r.providerName = p.Name()
		}
		r.provider = p
		break
	}
	return r
}

// deliver is one channel's delivery.
func (r *phoneRun) deliver(ctx context.Context, t domain.PhoneTarget) phoneResult {
	in := domain.PhoneReserveInput{
		ProjectID: t.ProjectID, ChannelID: t.ChannelID, OutboxEventID: r.ev.OutboxEventID, Kind: t.Kind,
		AlertKind: string(r.alert.Kind), IncidentID: r.ev.IncidentID, MonitorID: r.ev.MonitorID,
		MonitorSeq: r.ev.MonitorSeq, ServiceID: r.ev.ServiceID, UserID: t.UserID, Number: t.Number,
		ServiceState: r.ev.ServiceState, EscalationStep: r.ev.EscalationStep,
		Notes: append([]string(nil), r.ev.Notes...),
	}
	if t.Unreadable() {
		return phoneResult{err: t.NumberErr} // no row: nothing was decided, the retry decides
	}
	if !t.Route() {
		return r.unsent(ctx, t, in)
	}

	var body string
	var segments int
	if t.Kind == domain.ChannelSMS {
		sms, err := telephony.RenderSMS(r.alert, r.encoding(), r.maxSegments())
		if err != nil {
			return phoneResult{err: fmt.Errorf("render: %w", err)}
		}
		body, segments = sms.Text, sms.Segments
		in.Notes = append(in.Notes, sms.Notes...)
	} else {
		body = telephony.RenderVoice(r.alert, r.language(t))
	}

	rec, cancel := r.recordContext(ctx)
	res, err := r.ph.store.ReservePhoneDelivery(rec, in)
	cancel()
	if errors.Is(err, domain.ErrPhoneChannelGone) {
		return r.gone(t)
	}
	if err != nil {
		return phoneResult{err: err} // domain.ErrPhoneDeliveryInFlight among them: retried (D33)
	}
	switch res.Status {
	case domain.PhoneReservationProceed:
	case domain.PhoneReservationStormLimited:
		r.count(t, domain.PhoneReasonStormLimited)
		if m := r.ph.metrics; m != nil {
			m.RecordPhoneQuotaEvent("storm", string(t.Kind), "exceeded")
		}
		r.ph.logger.Warn("phone delivery storm-limited", "delivery_id", res.ID, "channel_id", t.ChannelID,
			"user_id", t.UserID, "kind", string(t.Kind))
		return phoneResult{}
	case domain.PhoneReservationClosedStale:
		r.count(t, domain.PhoneReasonOutcomeUnknown)
		r.ph.logger.Warn("phone delivery closed as outcome_unknown: its call may or may not have been placed",
			"delivery_id", res.ID, "channel_id", t.ChannelID, "user_id", t.UserID)
		return phoneResult{}
	case domain.PhoneReservationExisting: // an earlier attempt of this event finished it
		return phoneResult{delivered: res.Reason == domain.PhoneReasonSent}
	default:
		return phoneResult{err: fmt.Errorf("unknown phone reservation status %q", res.Status)}
	}

	answer := r.call(ctx, t, body)
	outcome := domain.PhoneOutcome{Reason: string(answer.Outcome), HTTPStatus: answer.HTTPStatus,
		Code: answer.Code, Ref: answer.Ref}
	if answer.Outcome == telephony.Sent {
		outcome.Segments = segments
	}
	rec, cancel = r.recordContext(ctx)
	err = r.ph.store.FinishPhoneDelivery(rec, res.ID, outcome)
	cancel()
	if errors.Is(err, domain.ErrPhoneDeliveryClosed) {
		// A retry or the leader's pass closed the row as outcome_unknown meanwhile; that is final and
		// is what the record says. Not an event failure, and not delivered (D20: when in doubt, page).
		r.ph.logger.Warn("phone delivery answered after its row was closed", "delivery_id", res.ID,
			"channel_id", t.ChannelID, "user_id", t.UserID, "outcome", outcome.Reason)
		return phoneResult{}
	}
	if err != nil {
		return phoneResult{err: err}
	}
	r.count(t, outcome.Reason)

	switch answer.Outcome {
	case telephony.Sent:
		return phoneResult{delivered: true}
	case telephony.Rejected:
		r.ph.logger.Warn("phone delivery rejected by the provider", "delivery_id", res.ID,
			"channel_id", t.ChannelID, "user_id", t.UserID, "status", answer.HTTPStatus, "code", answer.Code)
		return phoneResult{}
	default:
		return phoneResult{err: unreachableError(answer)}
	}
}

// call places the request, bounded by min(phoneCallTimeout, what is left of the budget). A channel
// whose turn comes after the budget ended is not called at all: provider_unreachable (D33).
func (r *phoneRun) call(ctx context.Context, t domain.PhoneTarget, body string) telephony.Result {
	if err := ctx.Err(); err != nil {
		return telephony.Result{Outcome: telephony.Unreachable}
	}
	if t.Kind == domain.ChannelVoice && !r.provider.SupportsVoice() {
		// The database says this voice channel is a route — its provider has voice — but this
		// replica's settings snapshot still builds one without. The database is the authority;
		// the snapshot catches up within its refresh, so the event is retried rather than the call
		// refused for good. No request is made.
		return telephony.Result{Outcome: telephony.Unreachable}
	}
	callCtx, cancel := context.WithTimeout(ctx, phoneCallTimeout)
	defer cancel()
	if t.Kind == domain.ChannelVoice {
		return r.provider.PlaceCall(callCtx, t.Number, body)
	}
	return r.provider.SendSMS(callCtx, t.Number, body)
}

// unsent writes the final row of a channel that is no route at send (§4.5).
func (r *phoneRun) unsent(ctx context.Context, t domain.PhoneTarget, in domain.PhoneReserveInput) phoneResult {
	reason, note := unsentReason(t)
	if note != "" {
		in.Notes = append(in.Notes, note)
	}
	rec, cancel := r.recordContext(ctx)
	defer cancel()
	wrote, rowReason, err := r.ph.store.RecordUnsentPhoneDelivery(rec, in, reason)
	if errors.Is(err, domain.ErrPhoneChannelGone) {
		return r.gone(t)
	}
	if err != nil {
		return phoneResult{err: err}
	}
	if wrote {
		r.count(t, reason)
	}
	// A retry whose earlier attempt reached the person (the row is `sent`) reports it, as the route
	// path does for an Existing row: the channel is no route now, but the alert was delivered (D20).
	return phoneResult{delivered: !wrote && rowReason == domain.PhoneReasonSent}
}

// gone ends the delivery to a channel deleted after the targets were read (FR-041a review P3-4): there
// is nobody to tell and no row to write, so it is not counted and does not fail the event — a retry
// would find the channel gone too. Logged, by channel id only.
func (r *phoneRun) gone(t domain.PhoneTarget) phoneResult {
	r.ph.logger.Info("phone channel deleted during its delivery; nothing recorded", "channel_id", t.ChannelID,
		"kind", string(t.Kind), "outbox_event_id", r.ev.OutboxEventID)
	return phoneResult{gone: true}
}

// unsentReason maps a non-route's verdict (R11) to the row's reason and, for not_deliverable, the
// cause in its notes (§4.5). A route without a verified number is no route either (P1).
func unsentReason(t domain.PhoneTarget) (reason, note string) {
	switch t.NotRouteReason {
	case domain.DeliverableTelephonyDisabled:
		return domain.PhoneReasonTelephonyDisabled, ""
	case domain.DeliverableVoiceUnsupportedByProvider:
		return domain.PhoneReasonVoiceUnsupportedByProvider, ""
	case domain.DeliverableRecipientNotEligible, domain.DeliverableRecipientDeleted:
		return domain.PhoneReasonNotDeliverable, domain.PhoneNoteRecipientNotEligible
	default: // no_verified_number, number_unverified, or a route with no number
		return domain.PhoneReasonNotDeliverable, domain.PhoneNoteNoVerifiedNumber
	}
}

// recordContext is the context of one write of the record: detached from the budget, bounded on its
// own (phoneRecordTimeout).
func (r *phoneRun) recordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), phoneRecordTimeout)
}

func (r *phoneRun) count(t domain.PhoneTarget, reason string) {
	if m := r.ph.metrics; m != nil {
		m.RecordPhoneDelivery(string(t.Kind), r.providerName, reason)
	}
}

func (r *phoneRun) encoding() telephony.Encoding {
	if r.settings.SMSEncoding == domain.TelephonySMSUnicode {
		return telephony.EncodingUnicode
	}
	return telephony.EncodingTransliterate
}

func (r *phoneRun) maxSegments() int {
	if r.settings.SMSMaxSegments < 1 {
		return 2 // §4.1's default
	}
	return r.settings.SMSMaxSegments
}

// language is the recipient's own voice language, else the instance's, else §4.1's default.
func (r *phoneRun) language(t domain.PhoneTarget) string {
	switch {
	case t.Language != "":
		return t.Language
	case r.settings.VoiceLanguage != "":
		return r.settings.VoiceLanguage
	default:
		return "en-US"
	}
}

// unreachableError is the retryable error of a provider_unreachable answer: the status or the
// transport class, never response text (D34) and never the number (P3).
func unreachableError(res telephony.Result) error {
	switch {
	case res.HTTPStatus != 0:
		return errors.New("provider unreachable: status " + strconv.Itoa(res.HTTPStatus))
	case res.TransportClass != "":
		return errors.New("provider unreachable: post failed: " + res.TransportClass)
	default:
		return errors.New("provider unreachable: no request made or no answer within the delivery budget")
	}
}

// unavailableProvider stands in for a provider that cannot be built: nothing is sent, every request is
// unreachable, and the outbox retries.
type unavailableProvider struct{}

func (unavailableProvider) SendSMS(context.Context, string, string) telephony.Result {
	return telephony.Result{Outcome: telephony.Unreachable}
}
func (unavailableProvider) PlaceCall(context.Context, string, string) telephony.Result {
	return telephony.Result{Outcome: telephony.Unreachable}
}
func (unavailableProvider) SupportsVoice() bool { return true }
func (unavailableProvider) Name() string        { return phoneProviderNone }
