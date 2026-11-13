package metrics

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

// sec-hardening-2 H-1 fix 7: every refusal on the OIDC bearer path is counted by bounded reason.
func TestRegistryExposesOIDCBearerRefused(t *testing.T) {
	r := New(buildinfo.Info{}, "api")
	r.RecordOIDCBearerRefused("client_not_listed")
	r.RecordOIDCBearerRefused("client_not_listed")
	r.RecordOIDCBearerRefused("subject_is_human")
	var out strings.Builder
	r.WritePrometheus(&out)
	got := out.String()
	for _, want := range []string{
		"# TYPE cerbix_oidc_bearer_refused_total counter",
		`cerbix_oidc_bearer_refused_total{reason="client_not_listed"} 2`,
		`cerbix_oidc_bearer_refused_total{reason="subject_is_human"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}
