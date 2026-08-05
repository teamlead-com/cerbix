package prober

import (
	"context"
	"fmt"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// compositeProber derives a monitor's status from its child monitors' current
// statuses (no network probe). Mode "all" (default) is up only when every child
// is up; mode "any" is up when at least one child is up. A missing child (e.g.
// deleted) counts as not-up.
type compositeProber struct{ r *Runner }

func (p compositeProber) Probe(ctx context.Context, m domain.Monitor) Result {
	if p.r.childStatus == nil {
		return Result{Connected: false, Msg: "composite evaluation unavailable"}
	}
	ids := m.ChildIDs()
	if len(ids) == 0 {
		return Result{Connected: false, Msg: "composite has no child monitors"}
	}
	statuses, err := p.r.childStatus(ctx, ids)
	if err != nil {
		return Result{Connected: false, Msg: err.Error()}
	}

	upCount := 0
	for _, id := range ids {
		if statuses[id] == domain.StatusUp {
			upCount++
		}
	}
	var up bool
	switch m.CompositeMode() {
	case "any":
		up = upCount > 0
	case "quorum":
		// Multi-region set (D-0101): down only when at least M children vote
		// down. A pending/missing child counts as a down vote — consistent with
		// mode "all", where anything not-up breaks the composite.
		downVotes := len(ids) - upCount
		up = downVotes < m.CompositeQuorum()
		if !up {
			return Result{Connected: false, Msg: fmt.Sprintf("%d/%d children down (quorum %d)", downVotes, len(ids), m.CompositeQuorum())}
		}
	default:
		up = upCount == len(ids)
	}
	if up {
		return Result{Connected: true}
	}
	return Result{Connected: false, Msg: fmt.Sprintf("%d/%d child monitors up (mode %s)", upCount, len(ids), m.CompositeMode())}
}
