package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// FR-025 D9: the leader's retention pass repeats the store's batch until a batch selects fewer
// group keys than the bound, hands the store the configured bound and a cutoff of
// retention_days behind now, and stops on an error without failing anything else.
func TestChangeRetentionPassRepeatsUntilABatchIsShort(t *testing.T) {
	fs := &fakeStore{}
	var cutoffs []time.Time
	var bounds []int
	answers := []int{250, 250, 7} // three batches: two full, one short
	fs.changePurgeFn = func(_ context.Context, cutoff time.Time, groupsPerBatch int) (int, int, error) {
		cutoffs = append(cutoffs, cutoff)
		bounds = append(bounds, groupsPerBatch)
		n := answers[len(cutoffs)-1]
		return n, 4 * n, nil
	}
	s := New(fs, nil, testLogger()).WithChangeRetention(400, 250)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	s.purgeChangeGroups(context.Background(), now)

	if got := atomic.LoadInt32(&fs.changePurges); got != 3 {
		t.Fatalf("PurgeChangeGroups called %d times, want 3 (two full batches and the short one)", got)
	}
	for i, b := range bounds {
		if b != 250 {
			t.Fatalf("batch %d bound = %d, want the configured 250", i, b)
		}
	}
	if want := now.Add(-400 * 24 * time.Hour); !cutoffs[0].Equal(want) {
		t.Fatalf("cutoff = %s, want now − 400 d = %s", cutoffs[0], want)
	}

	// A batch exactly at the bound continues; an error stops the pass at that batch.
	fs = &fakeStore{}
	calls := 0
	fs.changePurgeFn = func(context.Context, time.Time, int) (int, int, error) {
		calls++
		if calls == 2 {
			return 0, 0, errors.New("planted")
		}
		return 10, 40, nil
	}
	New(fs, nil, testLogger()).WithChangeRetention(30, 10).purgeChangeGroups(context.Background(), now)
	if calls != 2 {
		t.Fatalf("an error must end the pass: %d calls, want 2", calls)
	}
}

// A zero retention disables the pass; the option is what the leader loop consults.
func TestChangeRetentionIsOffWithoutTheOption(t *testing.T) {
	s := New(&fakeStore{}, nil, testLogger())
	if s.changeRetentionDays != 0 {
		t.Fatal("retention must be off until WithChangeRetention wires it")
	}
	s.WithChangeRetention(400, 250)
	if s.changeRetentionDays != 400 || s.changeGroupsPerBatch != 250 {
		t.Fatalf("option not applied: %d/%d", s.changeRetentionDays, s.changeGroupsPerBatch)
	}
}

// fakeChangeSink records the retained-gauge calls (FR-025 D15).
type fakeChangeSink struct {
	set     []int64
	cleared int
}

func (s *fakeChangeSink) SetChangesRetained(n int64) { s.set = append(s.set, n) }
func (s *fakeChangeSink) ClearChangesRetained()      { s.cleared++ }

// D15: the retention pass samples the rows it left behind into `cerbix_changes_retained` once per
// pass — after the batches, from the store's count — a failed sample leaves the previous value
// standing, and leadership loss clears the gauge. Without the sink nothing is counted.
func TestChangeRetentionPassSetsTheRetainedGaugeAndLeadershipLossClearsIt(t *testing.T) {
	fs := &fakeStore{changeCount: 4321}
	sink := &fakeChangeSink{}
	s := New(fs, nil, testLogger()).WithChangeRetention(400, 250).WithChangeRetentionMetrics(sink)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	s.purgeChangeGroups(context.Background(), now)
	if len(sink.set) != 1 || sink.set[0] != 4321 {
		t.Fatalf("gauge samples = %v, want [4321] after one pass", sink.set)
	}
	fs.changeCountErr = errors.New("planted")
	s.purgeChangeGroups(context.Background(), now)
	if len(sink.set) != 1 {
		t.Fatalf("a failed count must not move the gauge: %v", sink.set)
	}
	s.setLeaderState(true)
	if sink.cleared != 0 {
		t.Fatal("winning leadership must not clear the gauge")
	}
	s.setLeaderState(false)
	if sink.cleared != 1 {
		t.Fatalf("losing leadership must clear the gauge once, cleared %d", sink.cleared)
	}

	// No sink: the pass runs, counts nothing, asks the store for no count.
	fs = &fakeStore{changeCount: 1, changeCountErr: errors.New("must not be asked")}
	New(fs, nil, testLogger()).WithChangeRetention(400, 250).purgeChangeGroups(context.Background(), now)
	if got := atomic.LoadInt32(&fs.changePurges); got != 1 {
		t.Fatalf("purge calls = %d", got)
	}
}
