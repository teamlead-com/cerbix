package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// func-reliability-gate §5a: the ten gate.* keys, their defaults, and the rule that a value
// outside its range refuses to start naming the key and the range.
func TestGateDefaultsLoadAndMatchTheSpec(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	g := cfg.Gate
	for _, tc := range []struct {
		key  string
		got  int
		want int
	}{
		{"evaluate_inflight_process", g.EvaluateInflightProcess, 8},
		{"evaluate_inflight_principal", g.EvaluateInflightPrincipal, 2},
		{"evaluate_rate_principal_per_minute", g.EvaluateRatePrincipalPerMinute, 10},
		{"evaluate_rate_process_per_minute", g.EvaluateRateProcessPerMinute, 60},
		{"evaluate_tx_budget_ms", g.EvaluateTxBudgetMs, 5000},
		{"decision_retention_days", g.DecisionRetentionDays, 90},
		{"decision_partition_lead_days", g.DecisionPartitionLeadDays, 7},
		{"decision_partition_create_max", g.DecisionPartitionCreateMax, 3},
		{"decision_purge_max_partitions", g.DecisionPurgeMaxPartitions, 8},
	} {
		if tc.got != tc.want {
			t.Errorf("default gate.%s = %d, want %d", tc.key, tc.got, tc.want)
		}
	}
	if g.DecisionPurgeEvery.Std() != time.Hour {
		t.Errorf("default gate.decision_purge_every = %s, want 1h", g.DecisionPurgeEvery.Std())
	}
	// A partial block keeps the other defaults (the result: block's lesson).
	cfg, err = Parse([]byte("gate:\n  evaluate_inflight_process: 16\n"))
	if err != nil || cfg.Gate.EvaluateInflightProcess != 16 || cfg.Gate.EvaluateRateProcessPerMinute != 60 {
		t.Fatalf("partial gate block: %+v err=%v", cfg.Gate, err)
	}
	// An unknown gate key is refused like any other (strict decoding).
	if _, err := Parse([]byte("gate:\n  evaluate_inflight_processes: 8\n")); err == nil {
		t.Fatal("an unknown gate key was accepted")
	}
}

// Each of the ten keys refuses one below its minimum and one above its maximum, naming the
// key; the bounds themselves are accepted, so the rule is a bound and not a ban.
func TestGateEveryKeyRefusesOutsideItsRangeByName(t *testing.T) {
	type key struct {
		name     string
		min, max int
		fmtv     func(int) string
		range_   string
	}
	plain := func(v int) string { return fmt.Sprintf("%d", v) }
	minutes := func(v int) string { return fmt.Sprintf("%dm", v) }
	keys := []key{
		{"evaluate_inflight_process", 1, 64, plain, "between 1 and 64"},
		{"evaluate_inflight_principal", 1, 16, plain, "between 1 and 16"},
		{"evaluate_rate_principal_per_minute", 1, 600, plain, "between 1 and 600"},
		{"evaluate_rate_process_per_minute", 1, 600, plain, "between 1 and 600"},
		{"evaluate_tx_budget_ms", 500, 30000, plain, "between 500 and 30000"},
		{"decision_retention_days", 7, 365, plain, "between 7 and 365"},
		{"decision_partition_lead_days", 2, 30, plain, "between 2 and 30"},
		{"decision_partition_create_max", 1, 8, plain, "between 1 and 8"},
		// 5m..24h expressed in minutes so ±1 is one minute either side of the bound.
		{"decision_purge_every", 5, 24 * 60, minutes, "between 5m0s and 24h0m0s"},
		{"decision_purge_max_partitions", 1, 48, plain, "between 1 and 48"},
	}
	if len(keys) != 10 {
		t.Fatalf("§5a names ten keys; the table has %d", len(keys))
	}
	for _, k := range keys {
		yamlFor := func(v int) []byte {
			return []byte("gate:\n  " + k.name + ": " + k.fmtv(v) + "\n")
		}
		for _, v := range []int{k.min - 1, k.max + 1} {
			_, err := Parse(yamlFor(v))
			if err == nil {
				t.Errorf("gate.%s = %s accepted; outside %s", k.name, k.fmtv(v), k.range_)
				continue
			}
			if !strings.Contains(err.Error(), "gate."+k.name) {
				t.Errorf("gate.%s = %s: refusal does not name the key: %v", k.name, k.fmtv(v), err)
			}
			if !strings.Contains(err.Error(), k.range_) {
				t.Errorf("gate.%s = %s: refusal does not state the range %q: %v", k.name, k.fmtv(v), k.range_, err)
			}
		}
		for _, v := range []int{k.min, k.max} {
			if _, err := Parse(yamlFor(v)); err != nil {
				t.Errorf("gate.%s = %s (a bound, under the other defaults) was refused: %v", k.name, k.fmtv(v), err)
			}
		}
	}
}

// The two cross-checks of §5a: at a 24h cadence there is one pass a day, so the per-pass
// budgets must carry the whole day. Each refusal names BOTH keys.
func TestGateCrossChecksNameBothKeys(t *testing.T) {
	parse := func(every string, createMax, purgeMax int) error {
		_, err := Parse([]byte(fmt.Sprintf(
			"gate:\n  decision_purge_every: %s\n  decision_partition_create_max: %d\n  decision_purge_max_partitions: %d\n",
			every, createMax, purgeMax)))
		return err
	}
	// purge_max × floor(86400 / every) < 4 refuses: 24h with 1, 2 and 3; 4 is accepted.
	for _, pm := range []int{1, 2, 3} {
		err := parse("24h", 3, pm)
		if err == nil {
			t.Errorf("purge_every=24h purge_max=%d accepted: %d stage-ops a day cannot exceed steady state", pm, pm)
			continue
		}
		for _, w := range []string{"gate.decision_purge_max_partitions", "gate.decision_purge_every", "at least 4"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("purge_max=%d: refusal %q does not say %q", pm, err, w)
			}
		}
	}
	if err := parse("24h", 3, 4); err != nil {
		t.Errorf("purge_every=24h purge_max=4 (exactly the floor) was refused: %v", err)
	}
	// create_max × floor(86400 / every) < 2 refuses: create_max=1 at 24h; 2 is accepted.
	err := parse("24h", 1, 8)
	if err == nil {
		t.Fatal("purge_every=24h create_max=1 accepted: one day's partition a day leaves no catch-up")
	}
	for _, w := range []string{"gate.decision_partition_create_max", "gate.decision_purge_every", "at least 2"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("create_max=1: refusal %q does not say %q", err, w)
		}
	}
	if err := parse("24h", 2, 8); err != nil {
		t.Errorf("purge_every=24h create_max=2 was refused: %v", err)
	}
	// floor, not round: 13h gives ONE pass a day (86400/46800 = 1.85), so purge_max=3 is 3 < 4.
	if err := parse("13h", 3, 3); err == nil || !strings.Contains(err.Error(), "3 × 1 = 3") {
		t.Errorf("purge_every=13h purge_max=3: want a floor-based refusal stating 3 × 1 = 3, got %v", err)
	}
	// At the default cadence every single-key bound is comfortably inside both checks.
	if err := parse("1h", 1, 1); err != nil {
		t.Errorf("1h with create_max=1 purge_max=1 (24 passes a day) was refused: %v", err)
	}
}
