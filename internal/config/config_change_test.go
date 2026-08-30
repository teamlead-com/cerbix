package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// func-change-intelligence §5a: the ten change.* keys, their defaults, and the rule that a
// value outside its range refuses to start naming the key and the range (invariant 21).
func TestChangeDefaultsLoadAndMatchTheSpec(t *testing.T) {
	cfg, err := Parse([]byte("log:\n  level: info\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	ch := cfg.Change
	for _, tc := range []struct {
		key  string
		got  int
		want int
	}{
		{"record_rate_process_per_minute", ch.RecordRateProcessPerMinute, 300},
		{"record_rate_principal_per_minute", ch.RecordRatePrincipalPerMinute, 30},
		{"record_inflight_process", ch.RecordInflightProcess, 32},
		{"read_inflight_process", ch.ReadInflightProcess, 64},
		{"correlation_note_max", ch.CorrelationNoteMax, 5},
		{"retention_days", ch.RetentionDays, 400},
		{"retention_groups_per_batch", ch.RetentionGroupsPerBatch, 250},
	} {
		if tc.got != tc.want {
			t.Errorf("default change.%s = %d, want %d", tc.key, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		key  string
		got  time.Duration
		want time.Duration
	}{
		{"max_past", ch.MaxPast.Std(), 24 * time.Hour},
		{"max_future", ch.MaxFuture.Std(), 5 * time.Minute},
		{"correlation_window", ch.CorrelationWindow.Std(), 60 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("default change.%s = %s, want %s", tc.key, tc.got, tc.want)
		}
	}
	// A partial block keeps the other defaults.
	cfg, err = Parse([]byte("change:\n  record_inflight_process: 16\n"))
	if err != nil || cfg.Change.RecordInflightProcess != 16 || cfg.Change.RetentionDays != 400 {
		t.Fatalf("partial change block: %+v err=%v", cfg.Change, err)
	}
	// An unknown change key is refused like any other (strict decoding).
	if _, err := Parse([]byte("change:\n  retention_day: 400\n")); err == nil {
		t.Fatal("an unknown change key was accepted")
	}
}

// Each of the ten keys refuses one below its minimum and one above its maximum, naming the
// key and the range; the bounds themselves are accepted.
func TestChangeEveryKeyRefusesOutsideItsRangeByName(t *testing.T) {
	type key struct {
		name     string
		min, max int
		fmtv     func(int) string
		range_   string
	}
	plain := func(v int) string { return fmt.Sprintf("%d", v) }
	minutes := func(v int) string { return fmt.Sprintf("%dm", v) }
	seconds := func(v int) string { return fmt.Sprintf("%ds", v) }
	keys := []key{
		{"record_rate_process_per_minute", 10, 3000, plain, "between 10 and 3000"},
		{"record_rate_principal_per_minute", 1, 600, plain, "between 1 and 600"},
		{"record_inflight_process", 1, 256, plain, "between 1 and 256"},
		{"read_inflight_process", 1, 512, plain, "between 1 and 512"},
		// 1h..168h in minutes so ±1 is one minute either side of the bound.
		{"max_past", 60, 168 * 60, minutes, "between 1h0m0s and 168h0m0s"},
		// 0s..1h in seconds: -1s is below the floor, 3601s above the ceiling.
		{"max_future", 0, 3600, seconds, "between 0s and 1h0m0s"},
		{"correlation_window", 5, 24 * 60, minutes, "between 5m0s and 24h0m0s"},
		{"correlation_note_max", 1, 20, plain, "between 1 and 20"},
		{"retention_days", 30, 1460, plain, "between 30 and 1460"},
		{"retention_groups_per_batch", 10, 2500, plain, "between 10 and 2500"},
	}
	if len(keys) != 10 {
		t.Fatalf("§5a names ten keys; the table has %d", len(keys))
	}
	for _, k := range keys {
		yamlFor := func(v int) []byte {
			return []byte("change:\n  " + k.name + ": " + k.fmtv(v) + "\n")
		}
		for _, v := range []int{k.min - 1, k.max + 1} {
			_, err := Parse(yamlFor(v))
			if err == nil {
				t.Errorf("change.%s = %s accepted; outside %s", k.name, k.fmtv(v), k.range_)
				continue
			}
			if !strings.Contains(err.Error(), "change."+k.name) {
				t.Errorf("change.%s = %s: refusal does not name the key: %v", k.name, k.fmtv(v), err)
			}
			if !strings.Contains(err.Error(), k.range_) {
				t.Errorf("change.%s = %s: refusal does not state the range %q: %v", k.name, k.fmtv(v), k.range_, err)
			}
		}
		for _, v := range []int{k.min, k.max} {
			if _, err := Parse(yamlFor(v)); err != nil {
				t.Errorf("change.%s = %s (a bound, under the other defaults) was refused: %v", k.name, k.fmtv(v), err)
			}
		}
	}
}

// A refusal names ITS key and no other: the gate's and the change's tables are separate loops
// and a change refusal must not be attributed to a gate key.
func TestChangeRefusalNamesOnlyItsOwnKey(t *testing.T) {
	_, err := Parse([]byte("change:\n  retention_days: 29\n"))
	if err == nil {
		t.Fatal("retention_days=29 accepted")
	}
	if !strings.Contains(err.Error(), "change.retention_days") || strings.Contains(err.Error(), "gate.") {
		t.Fatalf("refusal %q must name change.retention_days and no gate key", err)
	}
}
