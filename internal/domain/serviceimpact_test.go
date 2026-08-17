package domain

import (
	"fmt"
	"strings"
	"testing"
)

// The 🕸 note is bounded prose over a complete relation (invariant 57): at most
// 8 names per role, then "+N more"; roots carry their root-first path verbatim;
// ordering is by path length then slug — the UI's presentation ranking.
func TestRenderImpactNoteBoundedAndOrdered(t *testing.T) {
	var links []ServiceImpactLink
	for i := 0; i < 11; i++ {
		links = append(links, ServiceImpactLink{
			Slug: fmt.Sprintf("svc%02d", i), Role: ImpactProbableRoot,
			Path: []string{fmt.Sprintf("svc%02d", i), "own"},
		})
	}
	links = append(links, ServiceImpactLink{Slug: "down1", Role: ImpactAffected, Path: []string{"own", "down1"}})

	note := RenderImpactNote(links)
	if !strings.HasPrefix(note, ImpactMarker) {
		t.Fatalf("note %q lacks the marker prefix", note)
	}
	if !strings.Contains(note, "+3 more") {
		t.Fatalf("note %q must truncate 11 roots to 8 and name the remainder", note)
	}
	if strings.Contains(note, "svc08") || strings.Contains(note, "svc09") || strings.Contains(note, "svc10") {
		t.Fatalf("note %q lists names beyond the cap", note)
	}
	if !strings.Contains(note, "svc00 (via svc00 → own)") {
		t.Fatalf("note %q must render the stored root-first path verbatim", note)
	}
	if !strings.Contains(note, "affected — down1") {
		t.Fatalf("note %q misses the affected role", note)
	}

	// Nearer roots rank first: a direct edge (2-slug path) beats a 3-slug one.
	ordered := RenderImpactNote([]ServiceImpactLink{
		{Slug: "far", Role: ImpactProbableRoot, Path: []string{"far", "mid", "own"}},
		{Slug: "near", Role: ImpactProbableRoot, Path: []string{"near", "own"}},
	})
	if strings.Index(ordered, "near") > strings.Index(ordered, "far") {
		t.Fatalf("note %q ranks the farther root first", ordered)
	}

	if got := RenderImpactNote(nil); got != "" {
		t.Fatalf("empty batch rendered %q, want empty string", got)
	}
}
