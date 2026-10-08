package api

import (
	"strings"
	"testing"
	"time"
)

// The offered months are the UTC months that intersect the window (since, now], newest first.
// Review R1-P3: a 90-day window touches up to FIVE months, not four — on 1 May the window starts on
// 31 January — and the function must not be capped.
func TestOfferedMonthsAreEveryUTCMonthTheWindowTouches(t *testing.T) {
	cases := []struct {
		now  string
		want string
	}{
		{"2026-05-01T00:00:00Z", "2026-05,2026-04,2026-03,2026-02,2026-01"},
		{"2026-10-08T12:00:00Z", "2026-10,2026-09,2026-08,2026-07"},
		{"2026-03-31T23:59:59Z", "2026-03,2026-02,2026-01,2025-12"},
	}
	for _, c := range cases {
		now, _ := time.Parse(time.RFC3339, c.now)
		got := strings.Join(historyMonthsOffered(now.Add(-90*24*time.Hour), now), ",")
		if got != c.want {
			t.Errorf("now %s: offered %s, want %s", c.now, got, c.want)
		}
	}
}
