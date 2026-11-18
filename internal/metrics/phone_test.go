package metrics

import (
	"bytes"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// FR-041a §9: the phone delivery families. Closed label sets — no project, channel or number label,
// ever (P3) — exact series, and nothing exported until observed.

func phoneLines(t *testing.T, reg *Registry) string {
	t.Helper()
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	var kept []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "cerbix_phone_") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestPhoneFamiliesAbsentUntilObserved(t *testing.T) {
	if got := phoneLines(t, New(buildinfo.Info{}, "api")); got != "" {
		t.Fatalf("a fresh registry exports phone series:\n%s", got)
	}
}

func TestPhoneDeliveriesAndStormCounters(t *testing.T) {
	reg := New(buildinfo.Info{}, "all")
	reg.RecordPhoneDelivery("sms", "twilio", "sent")
	reg.RecordPhoneDelivery("sms", "twilio", "sent")
	reg.RecordPhoneDelivery("voice", "generic_http", "voice_unsupported_by_provider")
	reg.RecordPhoneDelivery("sms", "none", "telephony_disabled")
	reg.RecordPhoneDelivery("sms", "twilio", "storm_limited")
	reg.RecordPhoneQuotaEvent("storm", "sms", "exceeded")
	// Outside the closed sets: dropped, never a label.
	reg.RecordPhoneDelivery("sms", "twilio", "sending") // not a finished row
	reg.RecordPhoneDelivery("fax", "twilio", "sent")
	reg.RecordPhoneDelivery("sms", "+905551236869", "sent")
	reg.RecordPhoneDelivery("sms", "twilio", "quota_exceeded") // part (c)
	reg.RecordPhoneQuotaEvent("project-123", "sms", "exceeded")
	reg.RecordPhoneQuotaEvent("storm", "sms", "warn_81")

	want := strings.Join([]string{
		"# HELP cerbix_phone_deliveries_total Finished phone deliveries by channel kind, provider and reason.",
		"# TYPE cerbix_phone_deliveries_total counter",
		`cerbix_phone_deliveries_total{kind="sms",provider="none",reason="telephony_disabled"} 1`,
		`cerbix_phone_deliveries_total{kind="sms",provider="twilio",reason="sent"} 2`,
		`cerbix_phone_deliveries_total{kind="sms",provider="twilio",reason="storm_limited"} 1`,
		`cerbix_phone_deliveries_total{kind="voice",provider="generic_http",reason="voice_unsupported_by_provider"} 1`,
		"# HELP cerbix_phone_quota_events_total Phone quota and storm-guard events by scope, channel kind and event.",
		"# TYPE cerbix_phone_quota_events_total counter",
		`cerbix_phone_quota_events_total{scope="storm",kind="sms",event="exceeded"} 1`,
	}, "\n")
	if got := phoneLines(t, reg); got != want {
		t.Fatalf("phone series:\n%s\nwant:\n%s", got, want)
	}
}
