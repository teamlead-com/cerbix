package domain

import (
	"strings"
	"testing"
)

func TestClassifyProbeError(t *testing.T) {
	cases := []struct {
		msg  string
		code int
		want string
	}{
		{`Get "https://x": context deadline exceeded`, 0, ErrClassTimeout},
		{"read tcp 1.2.3.4: i/o timeout", 0, ErrClassTimeout},
		{"dial tcp: lookup db.internal: no such host", 0, ErrClassDNS},
		{"lookup cerbix on 127.0.0.11:53: server misbehaving", 0, ErrClassDNS},
		{"dial tcp 10.0.0.5:5432: connect: connection refused", 0, ErrClassRefused},
		{"read: connection reset by peer", 0, ErrClassRefused},
		{"tls: failed to verify certificate: x509: certificate has expired", 0, ErrClassTLS},
		{"body assert failed: expected 'ok'", 200, ErrClassAssertBody},
		{"", 503, ErrClassHTTP5xx},
		{"", 404, ErrClassHTTP4xx},
		{"composite has no child monitors", 0, ErrClassOther},
	}
	for _, c := range cases {
		if got := ClassifyProbeError(c.msg, c.code); got != c.want {
			t.Errorf("ClassifyProbeError(%q, %d) = %q, want %q", c.msg, c.code, got, c.want)
		}
	}
}

func TestIncidentContextRender(t *testing.T) {
	full := IncidentContext{
		CoFailures: []string{"api", "cache"}, CoFailureTotal: 7,
		DominantClass: ErrClassRefused, Region: "geo1", WindowMinutes: 5,
	}
	s := full.Render()
	if !strings.HasPrefix(s, IncidentContextMarker) {
		t.Fatalf("render must start with the marker: %q", s)
	}
	for _, want := range []string{"7 other monitor(s)", "api, cache, …", "connection refused", `region "geo1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("render missing %q in %q", want, s)
		}
	}

	solo := IncidentContext{DominantClass: ErrClassTimeout, WindowMinutes: 5}
	if s := solo.Render(); !strings.Contains(s, "no other monitors") || !strings.Contains(s, "timeout") {
		t.Errorf("solo render = %q", s)
	}
	if (IncidentContext{}).Empty() != true || full.Empty() {
		t.Error("Empty() misreports")
	}
}
