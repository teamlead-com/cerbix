package metrics

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
)

func TestAuditRetentionMetricsAreBoundedAndDoNotExposeAuditPayload(t *testing.T) {
	r := New(buildinfo.Info{}, "scheduler")
	if err := r.RecordAuditRetentionPass("deleted", 7); err != nil {
		t.Fatal(err)
	}
	r.SetAuditRetentionGauges(120, 1234)
	var out strings.Builder
	r.WritePrometheus(&out)
	got := out.String()
	for _, want := range []string{"cerbix_audit_retention_rows_deleted_total 7", `cerbix_audit_retention_passes_total{result="deleted"} 1`, "cerbix_audit_retention_oldest_expired_seconds 120.000", "cerbix_audit_retention_last_success_timestamp_seconds 1234"} {
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
