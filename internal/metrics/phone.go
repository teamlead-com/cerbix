package metrics

import (
	"maps"
	"sort"
)

// The FR-041a phone delivery families (docs/specs/func-phone-alerts.md §9). Every label has a CLOSED
// set and none follows its input — no project, channel, user or number, ever (P3) — so a value outside
// its set is dropped, never recorded under the offending value. The recorders are the delivery path's
// (notify) and return nothing, like the outbox-facing change recorders.

var (
	phoneKinds     = map[string]bool{"sms": true, "voice": true}
	phoneProviders = map[string]bool{"twilio": true, "generic_http": true, "none": true}
	// phoneDeliveryReasons are the FINAL reasons of part (a) (§4.5): a finished row. `sending` is not
	// one; the reasons of parts (b) and (c) join with them.
	phoneDeliveryReasons = map[string]bool{
		"sent": true, "provider_unreachable": true, "provider_rejected": true, "outcome_unknown": true,
		"not_deliverable": true, "voice_unsupported_by_provider": true, "telephony_disabled": true,
		"storm_limited": true,
	}
	phoneQuotaScopes = map[string]bool{"instance": true, "project": true, "storm": true}
	phoneQuotaEvents = map[string]bool{"warn_80": true, "exceeded": true}
)

type phoneDeliveryKey struct{ kind, provider, reason string }

type phoneQuotaKey struct{ scope, kind, event string }

// phoneMetrics is the phone surface as held inside Registry, guarded by Registry.mu.
type phoneMetrics struct {
	deliveries map[phoneDeliveryKey]uint64
	quota      map[phoneQuotaKey]uint64
}

// RecordPhoneDelivery counts one finished phone delivery row:
// cerbix_phone_deliveries_total{kind,provider,reason}.
func (r *Registry) RecordPhoneDelivery(kind, provider, reason string) {
	if !phoneKinds[kind] || !phoneProviders[provider] || !phoneDeliveryReasons[reason] {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phone.deliveries == nil {
		r.phone.deliveries = map[phoneDeliveryKey]uint64{}
	}
	r.phone.deliveries[phoneDeliveryKey{kind, provider, reason}]++
}

// RecordPhoneQuotaEvent counts one quota or storm-guard event:
// cerbix_phone_quota_events_total{scope,kind,event}. Part (a) records the storm guard's `exceeded`.
func (r *Registry) RecordPhoneQuotaEvent(scope, kind, event string) {
	if !phoneQuotaScopes[scope] || !phoneKinds[kind] || !phoneQuotaEvents[event] {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phone.quota == nil {
		r.phone.quota = map[phoneQuotaKey]uint64{}
	}
	r.phone.quota[phoneQuotaKey{scope, kind, event}]++
}

func (p phoneMetrics) snapshot() phoneMetrics {
	return phoneMetrics{deliveries: maps.Clone(p.deliveries), quota: maps.Clone(p.quota)}
}

func (p phoneMetrics) write(w *prometheusWriter) {
	if len(p.deliveries) > 0 {
		w.println("# HELP cerbix_phone_deliveries_total Finished phone deliveries by channel kind, provider and reason.")
		w.println("# TYPE cerbix_phone_deliveries_total counter")
		for _, k := range sortedKeysBy(p.deliveries, func(k phoneDeliveryKey) string { return k.kind + "\x00" + k.provider + "\x00" + k.reason }) {
			w.printf("cerbix_phone_deliveries_total{kind=%q,provider=%q,reason=%q} %d\n", k.kind, k.provider, k.reason, p.deliveries[k])
		}
	}
	if len(p.quota) > 0 {
		w.println("# HELP cerbix_phone_quota_events_total Phone quota and storm-guard events by scope, channel kind and event.")
		w.println("# TYPE cerbix_phone_quota_events_total counter")
		for _, k := range sortedKeysBy(p.quota, func(k phoneQuotaKey) string { return k.scope + "\x00" + k.kind + "\x00" + k.event }) {
			w.printf("cerbix_phone_quota_events_total{scope=%q,kind=%q,event=%q} %d\n", k.scope, k.kind, k.event, p.quota[k])
		}
	}
}

// sortedKeysBy returns m's keys ordered by their sort text (deterministic /metrics output).
func sortedKeysBy[K comparable, V any](m map[K]V, text func(K) string) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return text(ks[i]) < text(ks[j]) })
	return ks
}
