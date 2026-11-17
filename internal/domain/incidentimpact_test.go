package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestImpactRankOrdersNoneBelowCritical(t *testing.T) {
	cases := map[IncidentImpact]int{ImpactNone: 0, ImpactMinor: 1, ImpactMajor: 2, ImpactCritical: 3, "": -1, "severe": -1}
	for in, want := range cases {
		if got := in.Rank(); got != want {
			t.Errorf("Rank(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestImpactMappingResolution(t *testing.T) {
	def := ImpactMapping{Degraded: ImpactMinor, Down: ImpactMajor, Unknown: ImpactNone}
	if DefaultImpactMapping() != def {
		t.Fatalf("defaults = %+v", DefaultImpactMapping())
	}
	if !(ImpactMapping{}).IsZero() || def.IsZero() {
		t.Fatal("IsZero wrong")
	}
	if (ImpactMapping{}).Resolved() != def {
		t.Fatal("zero mapping must resolve to the D6 defaults")
	}
	partial := ImpactMapping{Down: ImpactCritical}
	if partial.IsZero() || partial.Resolved() != partial {
		t.Fatal("a partial mapping must be returned unchanged")
	}
	if partial.Validate() == nil {
		t.Fatal("a partial mapping must fail Validate")
	}
	if got := def.String(); got != "degraded=minor,down=major,unknown=none" {
		t.Fatalf("String() = %q", got)
	}
}

func TestImpactMappingValidateRefusesUnknownAndEmptyValues(t *testing.T) {
	if err := DefaultImpactMapping().Validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
	err := ImpactMapping{Degraded: ImpactMinor, Down: "severe", Unknown: ImpactNone}.Validate()
	if err == nil || err.Error() != `impact_by_state.down: unknown impact "severe"` {
		t.Fatalf("err = %v", err)
	}
	for _, m := range []ImpactMapping{
		{Down: ImpactMajor, Unknown: ImpactNone},
		{Degraded: ImpactMinor, Unknown: ImpactNone},
		{Degraded: ImpactMinor, Down: ImpactMajor},
	} {
		if m.Validate() == nil {
			t.Errorf("%+v: empty value accepted", m)
		}
	}
}

func TestImpactMappingForCoversOnlyThePagingStates(t *testing.T) {
	m := DefaultImpactMapping()
	for s, want := range map[ServiceAlertState]IncidentImpact{
		ServiceAlertDegraded: ImpactMinor, ServiceAlertDown: ImpactMajor, ServiceAlertUnknown: ImpactNone,
	} {
		if got, ok := m.For(s); !ok || got != want {
			t.Errorf("For(%s) = %q,%v", s, got, ok)
		}
	}
	for _, s := range []ServiceAlertState{ServiceAlertHealthy, ServiceAlertExcluded} {
		if got, ok := m.For(s); ok || got != "" {
			t.Errorf("For(%s) = %q,%v, want empty,false", s, got, ok)
		}
	}
}

func TestNullAndExplicitDefaultMappingsAreOnePolicy(t *testing.T) {
	zero := ServiceAlertPolicy{}.Impacts()
	if zero != DefaultServiceAlertPolicy().Impacts() {
		t.Fatal("zero policy and default policy differ")
	}
	explicit := ServiceAlertPolicy{ImpactByState: ImpactMapping{Degraded: ImpactMinor, Down: ImpactMajor, Unknown: ImpactNone}}
	if zero != explicit.Impacts() {
		t.Fatal("explicit defaults differ from the unstated mapping")
	}
}

func TestImpactBasisShapesSerializeExactly(t *testing.T) {
	m := DefaultImpactMapping()
	cases := []struct {
		b    ImpactBasis
		want string
	}{
		{ImpactBasis{Source: ImpactBasisManual}, `{"source":"manual"}`},
		{ImpactBasis{Source: ImpactBasisProjectDefault}, `{"source":"project_default"}`},
		{ImpactBasis{Source: ImpactBasisAlertmanager}, `{"source":"alertmanager"}`},
		{ImpactBasis{Source: ImpactBasisServiceMapping, Mapping: &m, State: ServiceAlertDown,
			Service: &ImpactBasisService{Name: "Checkout", Slug: "checkout"}},
			`{"source":"service_mapping","mapping":{"degraded":"minor","down":"major","unknown":"none"},"state":"down","service":{"name":"Checkout","slug":"checkout"}}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.b)
		if err != nil || string(got) != c.want {
			t.Errorf("got %s (%v), want %s", got, err, c.want)
		}
	}
}

func TestImpactBasisValidateMirrorsTheSchema(t *testing.T) {
	m := DefaultImpactMapping()
	bad := ImpactMapping{Degraded: ImpactMinor, Down: "severe", Unknown: ImpactNone}
	svc := &ImpactBasisService{Name: "Checkout", Slug: "checkout"}
	mapping := func(state ServiceAlertState, mp *ImpactMapping, s *ImpactBasisService) ImpactBasis {
		return ImpactBasis{Source: ImpactBasisServiceMapping, Mapping: mp, State: state, Service: s}
	}
	for name, b := range map[string]ImpactBasis{
		"manual":          {Source: ImpactBasisManual},
		"project_default": {Source: ImpactBasisProjectDefault},
		"alertmanager":    {Source: ImpactBasisAlertmanager},
		"mapping down":    mapping(ServiceAlertDown, &m, svc),
		"mapping unknown": mapping(ServiceAlertUnknown, &m, svc),
	} {
		if err := b.Validate(); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, b := range map[string]ImpactBasis{
		"unknown source":      {Source: "weird"},
		"empty source":        {},
		"manual with mapping": {Source: ImpactBasisManual, Mapping: &m},
		"manual with state":   {Source: ImpactBasisManual, State: ServiceAlertDown},
		"manual with service": {Source: ImpactBasisManual, Service: svc},
		"no service":          mapping(ServiceAlertDown, &m, nil),
		"state healthy":       mapping(ServiceAlertHealthy, &m, svc),
		"state empty":         mapping(ServiceAlertState(""), &m, svc),
		"invalid mapping":     mapping(ServiceAlertDown, &bad, svc),
		"no mapping":          mapping(ServiceAlertDown, nil, svc),
	} {
		if b.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestIncidentValidateAllowsAnEmptyImpactOnlyForAMonitorAutoIncident(t *testing.T) {
	base := Incident{Title: "t", ProjectID: "p", Status: IncidentInvestigating}
	mk := func(src IncidentSource, mon, svc string, imp IncidentImpact) Incident {
		i := base
		i.Source, i.MonitorID, i.ServiceID, i.Impact = src, mon, svc, imp
		return i
	}
	if err := mk(SourceAuto, "m", "", "").Validate(); err != nil {
		t.Fatalf("auto+monitor empty impact refused: %v", err)
	}
	for name, i := range map[string]Incident{
		"auto no anchor": mk(SourceAuto, "", "", ""),
		"auto+service":   mk(SourceAuto, "", "s", ""),
		"manual":         mk(SourceManual, "", "", ""),
		"api":            mk(SourceAPI, "", "", ""),
	} {
		err := i.Validate()
		if err == nil || err.Error() != "incident: impact is required" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, i := range map[string]Incident{
		"auto+monitor": mk(SourceAuto, "m", "", "severe"),
		"auto":         mk(SourceAuto, "", "", "severe"),
		"manual":       mk(SourceManual, "", "", "severe"),
		"api":          mk(SourceAPI, "", "", "severe"),
	} {
		err := i.Validate()
		if err == nil || err.Error() != `incident: unknown impact "severe"` {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestImpactRaisedNoteStrings(t *testing.T) {
	for s, tail := range map[ServiceAlertState]string{
		ServiceAlertDegraded: "the service is degraded.",
		ServiceAlertDown:     "the service is down.",
		ServiceAlertUnknown:  "the service's state is unknown.",
	} {
		want := "⬆ Impact raised: minor → major — " + tail
		if got := ImpactRaisedNote(ImpactMinor, ImpactMajor, s); got != want {
			t.Errorf("%s: %q, want %q", s, got, want)
		}
		if !strings.HasPrefix(want, ImpactRaisedMarker) {
			t.Errorf("marker %q is not a prefix", ImpactRaisedMarker)
		}
	}
}

func TestServiceIncidentOpeningNoteNamesTheState(t *testing.T) {
	for s, word := range map[ServiceAlertState]string{
		ServiceAlertDown: "DOWN", ServiceAlertDegraded: "DEGRADED", ServiceAlertUnknown: "UNKNOWN",
	} {
		want := "Opened automatically: service " + word + " confirmed over 2 evaluation(s)."
		if got := ServiceIncidentOpeningNote(s, 2); got != want {
			t.Errorf("%s: %q, want %q", s, got, want)
		}
	}
}

func TestTheIncidentModelsCarryNoImpactBasis(t *testing.T) {
	names := func(v any) map[string]bool {
		out := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			out[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
		}
		return out
	}
	if names(Incident{})["impact_basis"] || names(IncidentEvent{})["impact_basis"] {
		t.Fatal("impact_basis must not be a field of Incident or IncidentEvent (D15)")
	}
	if !names(IncidentEvent{})["impact_raised"] {
		t.Fatal("IncidentEvent must carry impact_raised")
	}
}
