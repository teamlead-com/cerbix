package scheduler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-041a D33, R6, §8: the leader's three phone passes. Stale `sending` rows are closed every 15 s
// and each closed row is counted as outcome_unknown (P7); verification codes older than 24 h are
// purged hourly; delivery rows older than 90 days daily.

func TestPhonePassesRunAtTheirCadences(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs, nil, testLogger())
	var last phonePasses
	t0 := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

	type counts struct{ stale, verifications, deliveries int32 }
	steps := []struct {
		at   time.Duration
		want counts
	}{
		{0, counts{1, 1, 1}},                          // the first tick runs all three
		{5 * time.Second, counts{1, 1, 1}},            // nothing is due
		{15 * time.Second, counts{2, 1, 1}},           // the stale sweep, every 15 s
		{29 * time.Second, counts{2, 1, 1}},           // 14 s after the last sweep: not yet
		{30 * time.Second, counts{3, 1, 1}},           //
		{time.Hour, counts{4, 2, 1}},                  // the verification purge, hourly
		{time.Hour + 59*time.Minute, counts{5, 2, 1}}, //
		{2 * time.Hour, counts{6, 3, 1}},              //
		{24 * time.Hour, counts{7, 4, 2}},             // the delivery purge, daily
	}
	for _, st := range steps {
		s.runPhonePasses(context.Background(), t0.Add(st.at), &last)
		got := counts{atomic.LoadInt32(&fs.phoneStaleCloses), atomic.LoadInt32(&fs.phoneVerificationPurges),
			atomic.LoadInt32(&fs.phoneDeliveryPurges)}
		if got != st.want {
			t.Fatalf("after +%s: calls (stale, verifications, deliveries) = %+v; want %+v", st.at, got, st.want)
		}
	}
	// The ages handed to the purges: R6's 24 hours, §8's 90 days.
	if fs.phoneVerificationAge != 24*time.Hour {
		t.Errorf("verification purge age %s; want 24h", fs.phoneVerificationAge)
	}
	if fs.phoneDeliveryAge != 90*24*time.Hour {
		t.Errorf("delivery purge age %s; want 90 days", fs.phoneDeliveryAge)
	}
}

// P7: every row the leader closes as outcome_unknown is counted, by its channel kind and the provider
// in force (`none` when there is none), and exported with those labels only — no project, channel or
// number.
func TestStalePhoneSweepCountsEveryClosedRow(t *testing.T) {
	fs := &fakeStore{phoneStale: []store.ClosedPhoneDelivery{
		{Kind: domain.ChannelSMS, Provider: domain.TelephonyProviderTwilio},
		{Kind: domain.ChannelVoice, Provider: domain.TelephonyProviderTwilio},
		{Kind: domain.ChannelSMS, Provider: domain.TelephonyProviderTwilio},
		{Kind: domain.ChannelSMS, Provider: ""},
	}}
	reg := metrics.New(buildinfo.Info{}, "scheduler")
	s := New(fs, nil, testLogger()).WithPhoneMetrics(reg)
	var last phonePasses
	s.runPhonePasses(context.Background(), time.Now(), &last)

	var out bytes.Buffer
	reg.WritePrometheus(&out)
	var got []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "cerbix_phone_deliveries_total{") {
			got = append(got, line)
		}
	}
	want := []string{
		`cerbix_phone_deliveries_total{kind="sms",provider="none",reason="outcome_unknown"} 1`,
		`cerbix_phone_deliveries_total{kind="sms",provider="twilio",reason="outcome_unknown"} 2`,
		`cerbix_phone_deliveries_total{kind="voice",provider="twilio",reason="outcome_unknown"} 1`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exported:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A failed sweep counts nothing and does not stop the purges.
	fs = &fakeStore{phoneStale: []store.ClosedPhoneDelivery{{Kind: domain.ChannelSMS}}, phoneStaleErr: errors.New("planted")}
	sink := &countingPhoneSink{}
	last = phonePasses{}
	New(fs, nil, testLogger()).WithPhoneMetrics(sink).runPhonePasses(context.Background(), time.Now(), &last)
	if sink.n != 0 {
		t.Errorf("a failed sweep counted %d rows", sink.n)
	}
	if atomic.LoadInt32(&fs.phoneVerificationPurges) != 1 || atomic.LoadInt32(&fs.phoneDeliveryPurges) != 1 {
		t.Error("a failed sweep stopped the purges")
	}

	// Without a sink the sweep still closes the rows.
	fs = &fakeStore{phoneStale: []store.ClosedPhoneDelivery{{Kind: domain.ChannelSMS}}}
	last = phonePasses{}
	New(fs, nil, testLogger()).runPhonePasses(context.Background(), time.Now(), &last)
	if atomic.LoadInt32(&fs.phoneStaleCloses) != 1 {
		t.Error("the sweep did not run without a metrics sink")
	}
}

type countingPhoneSink struct{ n int }

func (c *countingPhoneSink) RecordPhoneDelivery(string, string, string) { c.n++ }

// The leader loop runs the three passes; a standby runs none.
func TestLeaderLoopRunsThePhonePasses(t *testing.T) {
	fs := &fakeStore{leader: true}
	s := New(fs, dispatch.NewInProc(8), testLogger())
	s.tick = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	deadline := time.After(3 * time.Second)
	for atomic.LoadInt32(&fs.phoneStaleCloses) < 1 || atomic.LoadInt32(&fs.phoneVerificationPurges) < 1 ||
		atomic.LoadInt32(&fs.phoneDeliveryPurges) < 1 {
		select {
		case <-deadline:
			t.Fatalf("leader did not run the phone passes: stale=%d verifications=%d deliveries=%d",
				atomic.LoadInt32(&fs.phoneStaleCloses), atomic.LoadInt32(&fs.phoneVerificationPurges),
				atomic.LoadInt32(&fs.phoneDeliveryPurges))
		case <-time.After(20 * time.Millisecond):
		}
	}

	standby := &fakeStore{leader: false}
	s = New(standby, dispatch.NewInProc(8), testLogger())
	s.tick, s.retry = 15*time.Millisecond, 15*time.Millisecond
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	s.Run(ctx2)
	if n := atomic.LoadInt32(&standby.phoneStaleCloses) + atomic.LoadInt32(&standby.phoneVerificationPurges) +
		atomic.LoadInt32(&standby.phoneDeliveryPurges); n != 0 {
		t.Fatalf("a standby ran %d phone passes", n)
	}
}
