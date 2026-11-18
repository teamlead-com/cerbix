package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// The phone half of a delivery (FR-041a D16, D18, D32, R15). The worker decides suppression per topic
// before any channel is told anything — silence, maintenance, dependency ancestors, delegation,
// escalation flat-down, the staleness gates — and the phone half runs only after all of them have
// passed, beside the text half, so a phone channel gets exactly the suppression a Slack channel gets
// (P2) by construction. Which events phone at all is D18's table (telephony.PhoneKindFor).
//
// Which channels it phones is learnt from the text half's own read: the text half runs with a
// domain.PhoneChannelsSeen in its context, and the dispatcher records there every sms and voice
// channel it leaves out. The phone half starts as soon as that read has answered and runs beside the
// text half's sends, within the same budget (alongside, FR-041a review P2-1). An event without one —
// every event of a project without phone channels — runs no phone half, makes no call, and fails
// exactly as it failed before phone existed (P14). Its one difference is when the text half's read
// did not complete: then the phone-id-only fallback read runs (phoneRecipients), and if that read
// fails too it writes one log line. Neither changes the event's error.

// PhoneDeliverer is NotifyDeliverer's phone half (FR-041a D16): a structured alert to the sms and
// voice channels among channelIDs, which every text method skips. Resolved counts the phone channels
// that are routes at send (D31 rule 1), Delivered those whose provider accepted the alert (D20);
// Requested is the snapshot's length, which a caller that also ran a text method on the same snapshot
// must not add twice. An error — provider_unreachable, a `sending` row too young to judge — asks for
// the event's retry (D19). A voice channel is not handed an alert D18 sends as SMS only.
type PhoneDeliverer interface {
	DeliverPhone(ctx context.Context, channelIDs []string, alert telephony.PhoneAlert, ev domain.PhoneEventRef) (domain.ChannelDelivery, error)
	// PhoneRoutes counts the phone channels among channelIDs that are routes now (D31 rule 1),
	// delivering nothing: what a service alert D18 does not phone adds to its Resolved.
	PhoneRoutes(ctx context.Context, channelIDs []string) (int, error)
}

// PhoneStore is what the phone half reads beside the payload: the incident an alert links to, and the
// names a message carries (§4.4). Only an event with a phone recipient reads it — and, when the text
// half's channel read failed, the phone channels alone (phoneRecipients).
type PhoneStore interface {
	// MonitorPhoneChannelIDs and PhoneChannelIDs are the phone-id-only reads: the enabled sms and
	// voice channels linked to a monitor, and those among ids. They open no channel config.
	MonitorPhoneChannelIDs(ctx context.Context, monitorID string) ([]string, error)
	PhoneChannelIDs(ctx context.Context, ids []string) ([]string, error)
	MonitorPhoneLink(ctx context.Context, monitorID string, resolvedSince time.Time) (store.PhoneLink, error)
	ServicePhoneLink(ctx context.Context, serviceID string, resolvedSince time.Time) (store.PhoneLink, error)
	GetProject(ctx context.Context, id string) (domain.Project, error)
	GetService(ctx context.Context, projectID, id string) (domain.Service, error)
	GetIncident(ctx context.Context, id string) (domain.Incident, error)
	IncidentRef(ctx context.Context, id string) (string, error)
}

// alongside runs an event's text half and its phone half side by side, inside the one delivery budget
// ctx carries (FR-041a review P2-1). The text half runs in its own goroutine with a
// domain.PhoneChannelsSeen in its context; its channel read is the event's ONE read, and the phone
// half starts the moment that read has answered — with the phone channels it left out — rather than
// after every text channel has been sent to. A slow Slack or webhook channel can then no longer spend
// the page's budget: the provider is called while the text is still in flight. When the text half
// returns without a completed read (the read failed), the phone half starts then, from
// phoneRecipients' fallback read, exactly as before. phone is called once, on this goroutine, with the
// event's phone channels and whether they are known (phoneRecipients); alongside returns when both
// halves have, with the text half's error — the caller joins the phone half's (§4.6).
//
// An event without a phone channel — every event of a project without one — reads its channels once,
// as it always did, calls phone with none, makes no further read and no call, and fails exactly as it
// failed before (P14).
func (w *Worker) alongside(ctx context.Context, e domain.OutboxEvent, text func(context.Context) error,
	fallback func(context.Context) ([]string, error), phone func(ids []string, known bool)) error {
	_, err := w.alongsideRead(ctx, e, text, fallback, phone)
	return err
}

// alongsideRead is alongside that also reports whether the text half's channel read completed. A
// service alert needs it (FR-041a owner answer 3, review C m-4): when that read failed, nobody learnt
// which text recipients are there, so none of them may be counted missing — even when the phone-id
// fallback answered for the phone half.
func (w *Worker) alongsideRead(ctx context.Context, e domain.OutboxEvent, text func(context.Context) error,
	fallback func(context.Context) ([]string, error), phone func(ids []string, known bool)) (textRead bool, err error) {
	textCtx, phones := domain.WithPhoneChannelsSeen(ctx)
	textDone := make(chan struct{})
	var textErr error
	go func() {
		defer close(textDone)
		textErr = text(textCtx)
	}()
	select {
	case <-phones.ReadDone():
		phone(phones.IDs(), true)
	case <-textDone:
		phone(w.phoneRecipients(ctx, e, phones, textErr, fallback))
	}
	<-textDone
	return phones.ReadCompleted(), textErr
}

// phoneRecipients is the event's phone channels: those the text half's read left out — or, when that
// read did not complete, a phone-id-only read of the same recipients (Task 12 re-review N-1). One
// channel whose config cannot be opened fails the text read of a whole list, and the page must not
// fail with it: the phone is the backup channel. The text half has failed then, so the event is
// retried for it whatever this answers, and D19 keeps every phone channel at one call. A failure of
// the fallback is logged and costs the phone half this attempt only; it never changes the event's
// error, so a project without phone channels fails exactly as it did. known is false then: nobody
// knows whether the event has phone recipients, and a service alert does not count them missing
// (FR-041a review P2-7).
func (w *Worker) phoneRecipients(ctx context.Context, e domain.OutboxEvent, phones *domain.PhoneChannelsSeen, textErr error,
	read func(context.Context) ([]string, error)) (ids []string, known bool) {
	if textErr == nil || phones.ReadCompleted() {
		return phones.IDs(), true
	}
	ids, err := read(ctx)
	if err != nil {
		w.logger.Warn("phone_recipients_read_failed", "id", e.ID, "topic", e.Topic, "error", err.Error())
		return nil, false
	}
	return ids, true
}

// monitorPhoneIDs and phoneIDsAmong are phoneRecipients' fallback reads for a monitor's linked
// channels and for a snapshot's or a step's ids.
func (w *Worker) monitorPhoneIDs(monitorID string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) { return w.store.MonitorPhoneChannelIDs(ctx, monitorID) }
}

func (w *Worker) phoneIDsAmong(ids []string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) { return w.store.PhoneChannelIDs(ctx, ids) }
}

// phoneConfig is the phone half's wiring: where links point, and D32's wait for a DOWN's incident.
type phoneConfig struct {
	// telephonyBaseURL and mailBaseURL are the two public base URLs, read at each send: the telephony
	// settings' own, else mail's (§4.1). Either may be nil.
	telephonyBaseURL, mailBaseURL func() string
	// refTries lookups after the first, refInterval apart: D32's "up to 1 s, four tries, 250 ms
	// apart". A variable only so tests need not sleep a second.
	refTries    int
	refInterval time.Duration
}

func defaultPhoneConfig() phoneConfig {
	return phoneConfig{refTries: 4, refInterval: 250 * time.Millisecond}
}

// WithPhoneLinks sets where a phone message's incident link points (§4.4): the telephony settings'
// `public_base_url`, else `mail.public_base_url`, each read at send so a settings save is in force for
// the next message. Without either the message keeps the incident reference, drops the link, and its
// row says `no_public_url`.
func (w *Worker) WithPhoneLinks(telephonyBaseURL, mailBaseURL func() string) *Worker {
	w.phone.telephonyBaseURL, w.phone.mailBaseURL = telephonyBaseURL, mailBaseURL
	return w
}

// publicBaseURL is the first of the two bases that can carry a link, trimmed. A base that cannot
// (telephony.LinkBaseRenders) counts as none: mail's is not validated at save, and handing it over
// would make the SMS a render error, retried until the event dead-letters — the page lost for a link
// (final review M-5).
func (w *Worker) publicBaseURL() string {
	for _, f := range []func() string{w.phone.telephonyBaseURL, w.phone.mailBaseURL} {
		if f == nil {
			continue
		}
		if u := strings.TrimSpace(f()); u != "" && telephony.LinkBaseRenders(u) {
			return u
		}
	}
	return ""
}

// linkRule is how a phone alert finds its incident (D32, R15).
type linkRule int

const (
	linkNone           linkRule = iota // a burn: no incident (§6.2, mock D-22)
	linkMonitorDown                    // the monitor's open auto-incident, waited for (D32)
	linkMonitorRecover                 // open, else resolved since the event was enqueued (D32)
	linkService                        // open, else resolved since the event was enqueued (R15)
	linkIncident                       // the incident the event names (an escalation step)
)

// phoneSend is the phone half of one event, prepared once the text half has shown it has a phone
// recipient.
type phoneSend struct {
	channelIDs []string
	alert      telephony.PhoneAlert
	ev         domain.PhoneEventRef
	link       linkRule
	subject    string    // the monitor, the service or the incident the link is looked up by
	since      time.Time // the event's created_at
	// noteMissingRef: no link found is worth a `no_incident_ref` on the row — a monitor with
	// auto_incident on (D32), a service's health or recovery alert, an escalation step, whose
	// incident should exist.
	noteMissingRef bool
	wait           bool // D32's wait for a DOWN's incident
}

// monitorPhone prepares a monitor transition's or a monitor burn alert's phone half: ids are the
// phone channels linked to the monitor that the text half's read left out (D18's monitor rows). nil
// when D18 does not phone the event or there is none.
func (w *Worker) monitorPhone(ctx context.Context, e domain.OutboxEvent, monitor domain.Monitor, ids []string) (*phoneSend, error) {
	kind, ok := telephony.PhoneKindFor(e.Topic, e.Payload)
	if !ok || len(ids) == 0 {
		return nil, nil
	}
	project, err := w.projectName(ctx, monitor.ProjectID)
	if err != nil {
		return nil, err
	}
	ps := &phoneSend{
		channelIDs: ids,
		alert:      telephony.PhoneAlert{Kind: kind, SubjectName: monitor.Name, ProjectName: project},
		ev:         domain.PhoneEventRef{OutboxEventID: e.ID, MonitorID: monitor.ID},
		subject:    monitor.ID, since: e.CreatedAt, noteMissingRef: monitor.AutoIncident,
	}
	switch kind {
	case telephony.KindMonitorDown:
		var mt domain.MonitorTransition
		if err := json.Unmarshal(e.Payload, &mt); err != nil {
			return nil, err
		}
		// The DOWN's row finds its incident under D32's lock in the store, and is attached when the
		// incident opens later: the seq is what that attachment matches.
		ps.ev.MonitorSeq = mt.Seq
		ps.link, ps.wait = linkMonitorDown, monitor.AutoIncident
	case telephony.KindMonitorRecovered:
		ps.link = linkMonitorRecover
	case telephony.KindMonitorBurn:
		var a domain.SLOBurnAlert
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			return nil, err
		}
		ps.alert.BurnRate, ps.alert.Window = a.BurnRate, burnWindow(a.WindowSeconds, a.Window)
		ps.link, ps.noteMissingRef = linkNone, false
	}
	return ps, nil
}

// servicePhone prepares a service alert's phone half: ids are the snapshot's phone recipients the text
// half's read left out; the service is named by its slug (R17) and linked per R15. nil when D18 does
// not phone the alert or there is none.
func (w *Worker) servicePhone(ctx context.Context, e domain.OutboxEvent, a domain.ServiceAlert, ids []string) (*phoneSend, error) {
	kind, ok := telephony.PhoneKindFor(e.Topic, e.Payload)
	if !ok || len(ids) == 0 {
		return nil, nil
	}
	project, err := w.projectName(ctx, a.ProjectID)
	if err != nil {
		return nil, err
	}
	subject := a.ServiceSlug
	if subject == "" {
		subject = a.ServiceName
	}
	// A service incident opens in the evaluator's transaction with its alert — there is no D32 race —
	// so a health or recovery alert that finds none says so on its row, as a monitor's with
	// auto_incident does (FR-041a review P2-7). A burn has no incident at all.
	ps := &phoneSend{
		channelIDs: ids,
		alert:      telephony.PhoneAlert{Kind: kind, SubjectName: subject, IsService: true, ProjectName: project},
		ev:         domain.PhoneEventRef{OutboxEventID: e.ID, ServiceID: a.ServiceID},
		link:       linkService, subject: a.ServiceID, since: e.CreatedAt, noteMissingRef: true,
	}
	switch kind {
	case telephony.KindServiceHealth:
		ps.alert.State = string(a.State)
		// The paging state the onset announced, for the row (§4.5); the store keeps NULL for a state
		// outside the vocabulary.
		ps.ev.ServiceState = string(a.State)
	case telephony.KindServiceBurn:
		ps.alert.BurnRate, ps.alert.Window = a.BurnRate, burnWindow(a.WindowSeconds, a.Window)
		ps.link = linkNone
	}
	return ps, nil
}

// stepPhone prepares an escalation step's phone half: ids are the step's phone channels the text
// half's read left out; the link is the step's incident, the subject named — a service by its slug
// (R17). nil when there is none.
func (w *Worker) stepPhone(ctx context.Context, e domain.OutboxEvent, a domain.EscalationStepAlert, ids []string) (*phoneSend, error) {
	kind, ok := telephony.PhoneKindFor(e.Topic, e.Payload)
	if !ok || len(ids) == 0 {
		return nil, nil
	}
	ps := &phoneSend{
		channelIDs: ids,
		alert: telephony.PhoneAlert{Kind: kind, Step: a.Step + 1, Repeat: a.Repeat, SubjectName: a.Subject(),
			IsService: a.ServiceID != ""},
		ev: domain.PhoneEventRef{OutboxEventID: e.ID, IncidentID: a.IncidentID, MonitorID: a.MonitorID,
			ServiceID: a.ServiceID, EscalationStep: a.Step + 1},
		link: linkIncident, subject: a.IncidentID, noteMissingRef: true,
	}
	inc, err := w.store.GetIncident(ctx, a.IncidentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The incident is gone: the page still goes, named from the payload, and its row names no
		// incident (it would not satisfy the row's foreign key).
		ps.ev.IncidentID = ""
		return ps, nil
	case err != nil:
		return nil, err
	}
	if ps.alert.ProjectName, err = w.projectName(ctx, inc.ProjectID); err != nil {
		return nil, err
	}
	if a.ServiceID != "" {
		svc, err := w.store.GetService(ctx, inc.ProjectID, a.ServiceID)
		switch {
		case err == nil && svc.Slug != "":
			ps.alert.SubjectName = svc.Slug
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	return ps, nil
}

// projectName is the project's name for the message; "" (the renderer drops it) when the project is
// gone.
func (w *Worker) projectName(ctx context.Context, id string) (string, error) {
	p, err := w.store.GetProject(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

// sendPhone resolves the link and hands the alert over; nothing for a nil half, and a preparation's
// error as it is. ctx is the delivery's: it carries the lease budget as its deadline, which bounds
// D32's wait and every provider call (D33). It runs beside the text half (alongside), so neither half
// waits for the other's time.
func (w *Worker) sendPhone(ctx context.Context, ps *phoneSend, prepErr error) (domain.ChannelDelivery, error) {
	if prepErr != nil || ps == nil {
		return domain.ChannelDelivery{}, prepErr
	}
	link := w.phoneLink(ctx, ps)
	base := w.publicBaseURL()
	ps.alert.PublicBaseURL = base
	if link.Ref != "" {
		ps.alert.IncidentRef = link.Ref
		if ps.link != linkMonitorDown && ps.link != linkIncident {
			ps.ev.IncidentID = link.IncidentID
		}
		if base == "" {
			ps.ev.Notes = append(ps.ev.Notes, domain.PhoneNoteNoPublicURL)
		}
	} else if ps.link != linkNone && ps.noteMissingRef {
		ps.ev.Notes = append(ps.ev.Notes, domain.PhoneNoteNoIncidentRef)
	}
	return w.notifs.DeliverPhone(ctx, ps.channelIDs, ps.alert, ps.ev)
}

// phoneLink finds the incident the alert links to. A failed lookup is logged and costs the link, never
// the page: the message goes without it and its row says so.
func (w *Worker) phoneLink(ctx context.Context, ps *phoneSend) store.PhoneLink {
	lookup := func() (store.PhoneLink, error) {
		switch ps.link {
		case linkMonitorDown:
			return w.store.MonitorPhoneLink(ctx, ps.subject, time.Time{})
		case linkMonitorRecover:
			return w.store.MonitorPhoneLink(ctx, ps.subject, ps.since)
		case linkService:
			return w.store.ServicePhoneLink(ctx, ps.subject, ps.since)
		case linkIncident:
			ref, err := w.store.IncidentRef(ctx, ps.subject)
			if errors.Is(err, store.ErrNotFound) {
				return store.PhoneLink{}, nil
			}
			return store.PhoneLink{IncidentID: ps.subject, Ref: ref}, err
		}
		return store.PhoneLink{}, nil
	}
	if ps.link == linkNone {
		return store.PhoneLink{}
	}
	link, err := lookup()
	// D32: the reconciler opens a monitor's auto-incident after the transition commits, so a DOWN may
	// be delivered before it exists. Look again, refTries times refInterval apart, inside the budget.
wait:
	for try := 0; ps.wait && link.Ref == "" && try < w.phone.refTries; try++ {
		select {
		case <-ctx.Done():
			break wait
		case <-time.After(w.phone.refInterval):
		}
		link, err = lookup()
	}
	if err != nil {
		w.logger.Warn("phone_incident_link_failed", "subject", ps.subject, "error", err.Error())
		return store.PhoneLink{}
	}
	return link
}

// burnWindow is the window a burn message names: the rule's long window, as the mock draws it ("burn
// rate 14.4x over 1h"); the target's window name for a payload that carries no seconds.
func burnWindow(seconds int, name string) string {
	if seconds > 0 {
		return domain.HumanWindow(seconds)
	}
	return name
}

// joinPhone is the event's outcome when it has a phone half: both errors, joined (§4.6); the text
// error alone, untouched, when the phone half had none.
func joinPhone(textErr, phoneErr error) error {
	if phoneErr == nil {
		return textErr
	}
	return errors.Join(textErr, phoneErr)
}
