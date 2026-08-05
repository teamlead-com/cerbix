package sla

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUptime(t *testing.T) {
	if Uptime(0, 0) != 0 {
		t.Error("no checks → 0")
	}
	if !almost(Uptime(99, 100), 99) {
		t.Errorf("99/100 = %v", Uptime(99, 100))
	}
	if !almost(Uptime(1, 4), 25) {
		t.Errorf("1/4 = %v", Uptime(1, 4))
	}
}

func TestWindowByName(t *testing.T) {
	if _, ok := WindowByName("30d"); !ok {
		t.Error("30d should exist")
	}
	if _, ok := WindowByName("bogus"); ok {
		t.Error("bogus should not exist")
	}
	if len(StandardWindows) != 4 {
		t.Fatalf("expected 4 standard windows, got %d", len(StandardWindows))
	}
}

func TestErrorBudgetMet(t *testing.T) {
	// Objective 99.0%, observed 99.5% (995/1000) → met, budget partly burned.
	b := ErrorBudget(99.0, 995, 1000)
	if !b.Met {
		t.Fatal("99.5% observed should meet 99% objective")
	}
	if !almost(b.AllowedDowntimeRatio, 0.01) {
		t.Errorf("allowed = %v, want 0.01", b.AllowedDowntimeRatio)
	}
	if !almost(b.ActualDowntimeRatio, 0.005) {
		t.Errorf("actual = %v, want 0.005", b.ActualDowntimeRatio)
	}
	if b.RemainingRatio <= 0 {
		t.Errorf("remaining should be positive, got %v", b.RemainingRatio)
	}
	if !almost(b.BurnedPercent, 50) {
		t.Errorf("burned = %v, want 50", b.BurnedPercent)
	}
}

func TestErrorBudgetBreached(t *testing.T) {
	// Objective 99.9%, observed 99.0% → not met, budget overrun.
	b := ErrorBudget(99.9, 990, 1000)
	if b.Met {
		t.Fatal("99.0% should not meet 99.9% objective")
	}
	if b.RemainingRatio >= 0 {
		t.Errorf("remaining should be negative, got %v", b.RemainingRatio)
	}
	if b.BurnedPercent <= 100 {
		t.Errorf("burned should exceed 100, got %v", b.BurnedPercent)
	}
}

func TestErrorBudgetNoData(t *testing.T) {
	b := ErrorBudget(99.9, 0, 0)
	if b.Met {
		t.Error("no data should not count as met")
	}
	if b.ActualDowntimeRatio != 0 {
		t.Errorf("no data → 0 observed downtime, got %v", b.ActualDowntimeRatio)
	}
}

func TestBurnRate(t *testing.T) {
	// 99% objective → allowed bad = 0.01. 10% bad over the window → rate 10.
	if got := BurnRate(99, 90, 100); got < 9.99 || got > 10.01 {
		t.Fatalf("BurnRate(99,90,100) = %v, want ~10", got)
	}
	// Meeting the objective exactly → burn rate 1 (consumes budget at nominal pace).
	if got := BurnRate(99, 99, 100); got < 0.99 || got > 1.01 {
		t.Fatalf("BurnRate(99,99,100) = %v, want ~1", got)
	}
	// No data or a 100% objective (no allowed budget) → 0, never a divide-by-zero.
	if got := BurnRate(99, 0, 0); got != 0 {
		t.Fatalf("BurnRate with no data = %v, want 0", got)
	}
	if got := BurnRate(100, 0, 100); got != 0 {
		t.Fatalf("BurnRate(100,...) = %v, want 0 (no budget)", got)
	}
}
