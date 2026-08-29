package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// §5a: the one duration key is bounded to the second, not the minute — 4m59s and 24h1s are
// outside, 5m and 24h are inside, and the refusal names the key and both bounds.
func TestGatePurgeEveryIsBoundedToTheSecond(t *testing.T) {
	parse := func(every string) error {
		_, err := Parse([]byte("gate:\n  decision_purge_every: " + every + "\n"))
		return err
	}
	for _, v := range []string{"4m59s", "24h1s", "0s", "1ns"} {
		err := parse(v)
		if err == nil {
			t.Errorf("gate.decision_purge_every = %s accepted; the range is 5m..24h", v)
			continue
		}
		for _, w := range []string{"gate.decision_purge_every", "5m0s", "24h0m0s", "got"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("gate.decision_purge_every = %s: refusal %q does not say %q", v, err, w)
			}
		}
	}
	for _, v := range []string{"5m", "5m0s", "300s", "24h", "1440m", "86400s"} {
		if err := parse(v); err != nil {
			t.Errorf("gate.decision_purge_every = %s (inside 5m..24h under the other defaults) was refused: %v", v, err)
		}
	}
	// A negative duration is refused as out of range, not accepted as "never".
	if err := parse("-1h"); err == nil {
		t.Error("gate.decision_purge_every = -1h accepted")
	}
}

// The defaults validate on their own AND sit strictly inside their ranges, so a default is a
// working configuration rather than a bound that happens to pass.
func TestGateDefaultsValidateAndSitInsideTheirRanges(t *testing.T) {
	cfg := defaults()
	if err := cfg.validateGate(); err != nil {
		t.Fatalf("the default gate block does not validate: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults() does not validate as a whole: %v", err)
	}
	g := cfg.Gate
	for _, tc := range []struct {
		key      string
		v        int
		min, max int
	}{
		{"evaluate_inflight_process", g.EvaluateInflightProcess, 1, 64},
		{"evaluate_inflight_principal", g.EvaluateInflightPrincipal, 1, 16},
		{"evaluate_rate_principal_per_minute", g.EvaluateRatePrincipalPerMinute, 1, 600},
		{"evaluate_rate_process_per_minute", g.EvaluateRateProcessPerMinute, 1, 600},
		{"evaluate_tx_budget_ms", g.EvaluateTxBudgetMs, 500, 30000},
		{"decision_retention_days", g.DecisionRetentionDays, 7, 365},
		{"decision_partition_lead_days", g.DecisionPartitionLeadDays, 2, 30},
		{"decision_partition_create_max", g.DecisionPartitionCreateMax, 1, 8},
		{"decision_purge_max_partitions", g.DecisionPurgeMaxPartitions, 1, 48},
	} {
		if tc.v <= tc.min || tc.v >= tc.max {
			t.Errorf("default gate.%s = %d is not strictly inside %d..%d", tc.key, tc.v, tc.min, tc.max)
		}
	}
	if e := g.DecisionPurgeEvery.Std(); e <= 5*time.Minute || e >= 24*time.Hour {
		t.Errorf("default gate.decision_purge_every = %s is not strictly inside 5m..24h", e)
	}
	// With the default cadence the cross-checks have 24 passes a day to work with.
	if n := g.DecisionPartitionCreateMax * int((24*time.Hour)/g.DecisionPurgeEvery.Std()); n < 2 {
		t.Errorf("defaults give %d partition creations a day, want >= 2", n)
	}
	if n := g.DecisionPurgeMaxPartitions * int((24*time.Hour)/g.DecisionPurgeEvery.Std()); n < 4 {
		t.Errorf("defaults give %d removal stage-ops a day, want >= 4", n)
	}
}

// docker/config.example.yaml documents every gate key with its default: the file loads through
// the real loader (env expansion included), its gate block equals defaults(), and the block
// spells out all ten keys — so an operator copying it sees the whole table, not a subset.
func TestExampleConfigGateBlockLoadsAndEqualsTheDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// The header mentions ${ENV_VAR} in prose and the loader expands the whole file, so the
	// variable must exist for Load to run at all — as it would for a real deployment.
	t.Setenv("ENV_VAR", "example")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	if cfg.Gate != defaults().Gate {
		t.Fatalf("the example's gate block differs from the defaults:\n example  %+v\n defaults %+v", cfg.Gate, defaults().Gate)
	}

	var doc struct {
		Gate map[string]any `yaml:"gate"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	want := []string{
		"evaluate_inflight_process", "evaluate_inflight_principal",
		"evaluate_rate_principal_per_minute", "evaluate_rate_process_per_minute",
		"evaluate_tx_budget_ms", "decision_retention_days", "decision_partition_lead_days",
		"decision_partition_create_max", "decision_purge_every", "decision_purge_max_partitions",
	}
	if len(doc.Gate) != len(want) {
		t.Fatalf("the example's gate block has %d keys, want the ten of §5a: %v", len(doc.Gate), doc.Gate)
	}
	for _, k := range want {
		if _, ok := doc.Gate[k]; !ok {
			t.Errorf("the example's gate block does not spell out %s", k)
		}
	}
}

// Every refusal is a *single* message that names one key (or, for a cross-check, both) — a
// config with several bad keys is refused on the first, so a fix-and-retry loop converges.
func TestGateRefusalNamesTheFirstBadKeyOnly(t *testing.T) {
	_, err := Parse([]byte("gate:\n  evaluate_inflight_process: 0\n  decision_retention_days: 0\n"))
	if err == nil {
		t.Fatal("two out-of-range keys accepted")
	}
	if !strings.Contains(err.Error(), "gate.evaluate_inflight_process") || strings.Contains(err.Error(), "decision_retention_days") {
		t.Fatalf("want a refusal naming only the first key in table order: %v", err)
	}
	// A per-key refusal wins over a cross-check refusal on the same config.
	_, err = Parse([]byte("gate:\n  decision_purge_every: 24h\n  decision_purge_max_partitions: 0\n"))
	if err == nil || !strings.Contains(err.Error(), "between 1 and 48") {
		t.Fatalf("purge_max=0 at 24h: want the range refusal first, got %v", err)
	}
}
