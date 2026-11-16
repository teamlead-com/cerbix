package domain

import (
	"fmt"
	"time"
)

// RecoverySnapshot is what the transaction that confirmed a down→up recovery knew before it reset
// the run (FR-043 §4.6): Run is the length of the run that completed, Threshold the value it was
// compared against (they differ only when the threshold was lowered during the run, D10), and
// FirstOK the database clock of the run's first success (D6).
type RecoverySnapshot struct {
	Run       int
	Threshold int
	FirstOK   time.Time
}

// Recovery returns the snapshot an event carries, or nil for an event without one.
func (t MonitorTransition) Recovery() *RecoverySnapshot {
	if t.RecoveryRun == 0 || t.RecoveryFirstOK == nil {
		return nil
	}
	return &RecoverySnapshot{Run: t.RecoveryRun, Threshold: t.RecoveryThreshold, FirstOK: *t.RecoveryFirstOK}
}

// Sentence is the one rendering of the snapshot every surface appends after one space (FR-043
// §4.6): the resolving update in the reconciler and the notification text in notify both call it,
// so the two carry the same string character for character (I14). It is empty for a nil snapshot
// and for a run of one, which keeps every text byte for byte what it was before the feature (I1).
// The instant is fixed here — RFC 3339, UTC, whole seconds — rather than left to each surface.
func (r *RecoverySnapshot) Sentence() string {
	if r == nil || r.Run <= 1 {
		return ""
	}
	return fmt.Sprintf("Recovered: %d/%d consecutive checks passed; first OK at %s",
		r.Run, r.Threshold, r.FirstOK.UTC().Format(time.RFC3339))
}
