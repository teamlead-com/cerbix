package metrics

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

func TestAuditRetentionMetricsAreBoundedAndDoNotExposeAuditPayload(t *testing.T) {
	r := New(buildinfo.Info{}, "scheduler")
	r.SetAuditRetentionConfigured(3600, 1200)
	if err := r.RecordAuditRetentionPass("deleted", 7); err != nil {
		t.Fatal(err)
	}
	r.SetAuditRetentionGauges(120, 1234)
	var out strings.Builder
	r.WritePrometheus(&out)
	got := out.String()
	for _, want := range []string{"cerbix_audit_retention_purge_interval_seconds 3600.000", "cerbix_audit_retention_configured_timestamp_seconds 1200", "cerbix_audit_retention_rows_deleted_total 7", `cerbix_audit_retention_passes_total{result="deleted"} 1`, "cerbix_audit_retention_oldest_expired_seconds 120.000", "cerbix_audit_retention_last_success_timestamp_seconds 1234"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "target=") || strings.Contains(got, "org_id=") || strings.Contains(got, "action=") {
		t.Fatalf("audit labels leaked into metrics:\n%s", got)
	}
	if err := r.RecordAuditRetentionPass("tenant-42", 0); err == nil {
		t.Fatal("unbounded result label accepted")
	}
}

func TestAuditRetentionConfiguredMetricsCoverEveryValidCadenceExtreme(t *testing.T) {
	for _, seconds := range []float64{300, 3600, 86400} {
		r := New(buildinfo.Info{}, "scheduler")
		r.SetAuditRetentionConfigured(seconds, 100)
		var out strings.Builder
		r.WritePrometheus(&out)
		want := "cerbix_audit_retention_purge_interval_seconds " + fmt.Sprintf("%.3f", seconds)
		if !strings.Contains(out.String(), want) {
			t.Fatalf("cadence %.0f missing from:\n%s", seconds, out.String())
		}
		if strings.Contains(out.String(), "cerbix_audit_retention_last_success_timestamp_seconds") {
			t.Fatalf("configured-only registry fabricated success for cadence %.0f", seconds)
		}
	}
}

func TestAuditRetentionAlertRulesAreCadenceAwareAndAbsenceSafe(t *testing.T) {
	raw, err := os.ReadFile("../../docs/alerts.yaml")
	if err != nil {
		t.Fatalf("read audit retention alerts: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"2 * cerbix_audit_retention_purge_interval_seconds",
		"cerbix_audit_retention_configured_timestamp_seconds",
		"unless cerbix_audit_retention_last_success_timestamp_seconds",
		"unless cerbix_audit_retention_oldest_expired_seconds",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("alert rules missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "> 7200") {
		t.Fatalf("alert rules still hard-code the default cadence:\n%s", text)
	}
}
