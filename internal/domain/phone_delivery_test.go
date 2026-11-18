package domain_test

import (
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/telephony"
)

// The store's alert kinds are the renderer's (D18): one spelling for both packages.
func TestPhoneAlertKindsMatchTelephony(t *testing.T) {
	pairs := map[string]telephony.AlertKind{
		domain.PhoneAlertMonitorDown:      telephony.KindMonitorDown,
		domain.PhoneAlertMonitorRecovered: telephony.KindMonitorRecovered,
		domain.PhoneAlertServiceHealth:    telephony.KindServiceHealth,
		domain.PhoneAlertServiceRecovered: telephony.KindServiceRecovered,
		domain.PhoneAlertEscalationStep:   telephony.KindEscalationStep,
		domain.PhoneAlertMonitorBurn:      telephony.KindMonitorBurn,
		domain.PhoneAlertServiceBurn:      telephony.KindServiceBurn,
	}
	for d, k := range pairs {
		if d != string(k) {
			t.Errorf("domain %q != telephony %q", d, k)
		}
		if !domain.ValidPhoneAlertKind(d) {
			t.Errorf("%q not valid", d)
		}
	}
	if domain.ValidPhoneAlertKind("monitor_up") {
		t.Error("an unknown kind is valid")
	}
	if !domain.IsPhoneBurnAlert(domain.PhoneAlertMonitorBurn) || !domain.IsPhoneBurnAlert(domain.PhoneAlertServiceBurn) ||
		domain.IsPhoneBurnAlert(domain.PhoneAlertMonitorDown) {
		t.Error("burn kinds")
	}
}

func TestTelephonyStormWithDefaults(t *testing.T) {
	got := domain.TelephonyStorm{}.WithDefaults()
	if got.PerIncidentPerHour != 5 || got.PerNumberPerHour != 20 {
		t.Fatalf("defaults %+v; want 5 and 20 (§6.2)", got)
	}
	got = domain.TelephonyStorm{PerIncidentPerHour: 2, PerNumberPerHour: 9}.WithDefaults()
	if got.PerIncidentPerHour != 2 || got.PerNumberPerHour != 9 {
		t.Fatalf("set bounds changed: %+v", got)
	}
}
