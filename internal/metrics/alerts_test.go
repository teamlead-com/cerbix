package metrics

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMonitoringAsCodeAlertRules validates the shipped Prometheus alert rules
// (docker/alerts/monitoring-as-code.rules.yml): they are structurally well-formed and every
// expr references a real cerbix_file_provider_* metric this package emits — so the rules can't
// silently drift from the metric names. (promtool check rules is run separately in CI/dev; this
// is the runnable in-repo guard.)
func TestMonitoringAsCodeAlertRules(t *testing.T) {
	const path = "../../docker/alerts/monitoring-as-code.rules.yml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read alert rules: %v", err)
	}
	var doc struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse alert rules: %v", err)
	}
	if len(doc.Groups) == 0 {
		t.Fatal("no rule groups")
	}

	// The metric names this package actually emits for the file provider.
	knownMetrics := []string{
		"cerbix_file_provider_leader",
		"cerbix_file_provider_reconcile_total",
		"cerbix_file_provider_reconcile_duration_seconds",
		"cerbix_file_provider_last_success_timestamp_seconds",
		"cerbix_file_provider_managed_monitors",
		"cerbix_file_provider_orphaned_monitors",
		"cerbix_file_provider_bundle_errors",
	}
	isKnown := func(expr string) bool {
		for _, m := range knownMetrics {
			if strings.Contains(expr, m) {
				return true
			}
		}
		return false
	}

	alerts := 0
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" {
				continue
			}
			alerts++
			if !isKnown(r.Expr) {
				t.Fatalf("alert %q expr references no known cerbix_file_provider_ metric: %q", r.Alert, r.Expr)
			}
			if r.For == "" {
				t.Fatalf("alert %q must set a `for:` window", r.Alert)
			}
			if r.Labels["severity"] == "" {
				t.Fatalf("alert %q must carry a severity label", r.Alert)
			}
			if r.Annotations["summary"] == "" {
				t.Fatalf("alert %q must carry a summary annotation", r.Alert)
			}
		}
	}
	if alerts < 3 {
		t.Fatalf("expected at least 3 file-provider alerts, got %d", alerts)
	}
}

func TestSecretInventoryAlertRules(t *testing.T) {
	const path = "../../docker/alerts/secret-inventory.rules.yml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read alert rules: %v", err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse alert rules: %v", err)
	}
	known := []string{"cerbix_secret_resolution_failed_total", "cerbix_executor_probe_error_total", "cerbix_dispatch_shared_trust"}
	alerts := 0
	for _, group := range doc.Groups {
		for _, rule := range group.Rules {
			alerts++
			found := false
			for _, metric := range known {
				found = found || strings.Contains(rule.Expr, metric)
			}
			if rule.Alert == "" || !found || rule.For == "" || rule.Labels["severity"] == "" || rule.Annotations["summary"] == "" {
				t.Fatalf("incomplete or drifting secret-inventory alert: %+v", rule)
			}
		}
	}
	if alerts != 3 {
		t.Fatalf("secret-inventory alerts = %d, want 3", alerts)
	}
}
