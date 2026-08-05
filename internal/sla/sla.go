// Package sla computes availability indicators from heartbeat counts. It is
// pure (no I/O): the store supplies up/total counts over a window, and this
// package turns them into uptime percentages, SLO error budgets, and the
// SLI/SLO/SLA vocabulary.
//
//   - SLI: the measured indicator (uptime % = up / total).
//   - SLO: the internal objective (e.g. 99.9% over 30d).
//   - Error budget: 1 − SLO; how much unavailability is permitted.
package sla

import "time"

// Window is a named rolling time window.
type Window struct {
	Name     string
	Duration time.Duration
}

// StandardWindows are the default rolling windows reported for every monitor.
var StandardWindows = []Window{
	{Name: "24h", Duration: 24 * time.Hour},
	{Name: "7d", Duration: 7 * 24 * time.Hour},
	{Name: "30d", Duration: 30 * 24 * time.Hour},
	{Name: "90d", Duration: 90 * 24 * time.Hour},
}

// WindowByName returns the standard window with the given name, or false.
func WindowByName(name string) (Window, bool) {
	for _, w := range StandardWindows {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

// Uptime returns the uptime percentage (0..100) for up successes out of total
// checks. With no checks it returns 0 (unknown treated as no availability data).
func Uptime(up, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(up) / float64(total) * 100
}

// BurnRate returns the SLO error-budget burn rate for objective given up/total
// heartbeats measured over a (short) window: the observed bad fraction divided by
// the objective's allowed bad fraction. A rate of 1 consumes the whole budget in
// exactly the objective's window; 14.4 exhausts a 30d budget in ~2 days. Returns
// 0 when there is no data or the objective permits no downtime (allowed == 0).
func BurnRate(objective float64, up, total int64) float64 {
	allowed := 1 - objective/100
	if allowed <= 0 || total <= 0 {
		return 0
	}
	bad := 1 - float64(up)/float64(total)
	return bad / allowed
}

// Budget describes an SLO error budget for a window.
type Budget struct {
	// Objective is the target uptime percentage (e.g. 99.9).
	Objective float64 `json:"objective"`
	// AllowedDowntimeRatio is the permitted bad fraction (1 − objective/100).
	AllowedDowntimeRatio float64 `json:"allowed_downtime_ratio"`
	// ActualDowntimeRatio is the observed bad fraction (1 − uptime/100).
	ActualDowntimeRatio float64 `json:"actual_downtime_ratio"`
	// RemainingRatio is AllowedDowntimeRatio − ActualDowntimeRatio (may be negative).
	RemainingRatio float64 `json:"remaining_ratio"`
	// BurnedPercent is how much of the budget is consumed (0..100+, may exceed 100).
	BurnedPercent float64 `json:"burned_percent"`
	// Met reports whether the objective is currently satisfied.
	Met bool `json:"met"`
}

// ErrorBudget computes the SLO error budget for an objective given up/total.
func ErrorBudget(objective float64, up, total int64) Budget {
	allowed := 1 - objective/100
	uptime := Uptime(up, total)
	actual := 1 - uptime/100
	if total <= 0 {
		actual = 0 // no data → no observed downtime
	}
	b := Budget{
		Objective:            objective,
		AllowedDowntimeRatio: allowed,
		ActualDowntimeRatio:  actual,
		RemainingRatio:       allowed - actual,
		Met:                  total > 0 && uptime >= objective,
	}
	if allowed > 0 {
		b.BurnedPercent = actual / allowed * 100
	}
	return b
}
