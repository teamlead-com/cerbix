package domain

import (
	"encoding/json"
	"os"
	"testing"
)

// The GO half of the cross-surface seam. The other half is
// `frontend/src/lib/canaryWorkflow.seam.spec.ts`, which enumerates every union variant the typed form
// can produce, keeps only the ones the CLIENT calls valid, and writes them to the fixture this reads.
//
// Why it exists: the first version of this file proved exactly ONE happy vector, and the independent
// reviewer used that to find five categories of shapes the client accepted and this validator refuses
// (party [84]) — a fixture registry key that was free text, a correlation marker that was a fragment
// of a path segment or appeared twice, body keys outside the grammar (blank and duplicate keys
// silently dropped or overwritten), field lists with duplicates and expressions, a half-specified poll
// failure condition, an unconditionally required `lifecycle_path` treated as optional, a multipart
// `content-type`, and an ordinary header with no value. One example proves one example.
//
// Neither half can pass alone, which is the point: a TypeScript test cannot know these rules, and a Go
// test cannot know what the form builds. If the client starts accepting something this refuses, THIS
// fails. If the client changes what it produces, the fixture's diff shows up in review.
func TestEveryClientValidFormVariantIsValidToTheServer(t *testing.T) {
	raw, err := os.ReadFile("testdata/canary_form_variants.json")
	if err != nil {
		t.Fatalf("the seam fixture is missing — run the frontend suite to generate it: %v", err)
	}
	var fixture struct {
		MonitorTimeoutSeconds int `json:"monitor_timeout_seconds"`
		Variants              []struct {
			Name   string            `json:"name"`
			Config map[string]string `json:"config"`
		} `json:"variants"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("the seam fixture does not parse: %v", err)
	}
	// A fixture that quietly shrank to nothing would make this test pass while proving nothing, which
	// is the exact failure mode the reviewer caught in its predecessor.
	if len(fixture.Variants) < 21 {
		t.Fatalf("the fixture carries %d variants; it is meant to cover every union variant of the form",
			len(fixture.Variants))
	}
	if fixture.MonitorTimeoutSeconds <= 0 {
		t.Fatal("the fixture does not state the monitor timeout its bounds were checked against")
	}

	seenKinds := map[string]bool{}
	for _, v := range fixture.Variants {
		t.Run(v.Name, func(t *testing.T) {
			cfg := make(map[string]string, len(v.Config))
			for k, val := range v.Config {
				cfg[k] = val
			}
			m := Monitor{
				ProjectID: "p", Name: "canary", Type: MonitorAsyncCanary, Region: "core",
				IntervalSeconds: fixture.MonitorTimeoutSeconds,
				TimeoutSeconds:  fixture.MonitorTimeoutSeconds,
				Enabled:         true, Config: cfg,
			}
			m.Normalize()
			if err := m.Validate(); err != nil {
				t.Fatalf("the form builds a document this validator refuses: %v", err)
			}
			// It must also survive the round trip the executor makes: a document that validates and
			// cannot be read back is not usable.
			w, err := ParseCanaryConfig(m.Config)
			if err != nil {
				t.Fatalf("the form's document does not parse back: %v", err)
			}
			seenKinds[w.Submit.Kind+"|"+w.Completion.Kind+"|"+w.Cleanup.Kind+"|"+w.Correlate.Source] = true
		})
	}

	// The fixture is meant to COVER the unions, not merely to be long. Every branch of all four must
	// appear somewhere, or a whole arm of the contract is unproven at this seam.
	for _, want := range []string{
		CanarySubmitHTTPJSON, CanarySubmitMultipartFixture,
		CanaryCompletionSSE, CanaryCompletionPollJSON,
		CanaryCleanupLifecyclePrefix, CanaryCleanupNone,
		CanaryCorrelateResponseJSON, CanaryCorrelateResponseHeader,
	} {
		found := false
		for combo := range seenKinds {
			if len(combo) > 0 && containsToken(combo, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no variant exercises %q: that arm of the contract is unproven at this seam", want)
		}
	}
}

func containsToken(combo, token string) bool {
	start := 0
	for i := 0; i <= len(combo); i++ {
		if i == len(combo) || combo[i] == '|' {
			if combo[start:i] == token {
				return true
			}
			start = i + 1
		}
	}
	return false
}
