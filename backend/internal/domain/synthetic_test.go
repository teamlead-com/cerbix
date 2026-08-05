package domain

import "testing"

func TestParseScenarioValidate(t *testing.T) {
	ok := `{"steps":[{"url":"https://x/login","extract":[{"var":"t","from":"json","path":"data.token"}],"assert":[{"that":"status","value":"200"}]}]}`
	if _, err := ParseScenario(map[string]string{SyntheticScenarioKey: ok}); err != nil {
		t.Fatalf("valid scenario rejected: %v", err)
	}

	bad := map[string]string{
		"missing":        ``,
		"not json":       `{oops`,
		"no steps":       `{"steps":[]}`,
		"step no url":    `{"steps":[{"assert":[{"that":"status","value":"200"}]}]}`,
		"bad from":       `{"steps":[{"url":"https://x","extract":[{"var":"t","from":"pigeon"}]}]}`,
		"json no path":   `{"steps":[{"url":"https://x","extract":[{"var":"t","from":"json"}]}]}`,
		"bad that":       `{"steps":[{"url":"https://x","assert":[{"that":"vibes","value":"1"}]}]}`,
		"bad op":         `{"steps":[{"url":"https://x","assert":[{"that":"status","op":"approx","value":"1"}]}]}`,
		"json no path a": `{"steps":[{"url":"https://x","assert":[{"that":"json","value":"1"}]}]}`,
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseScenario(map[string]string{SyntheticScenarioKey: raw}); err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
		})
	}

	// Monitor.Validate rejects a synthetic with no scenario and accepts a valid one.
	if err := (Monitor{Name: "s", ProjectID: "p", Type: MonitorSynthetic, IntervalSeconds: 60, TimeoutSeconds: 5}).Validate(); err == nil {
		t.Fatal("synthetic without scenario should be invalid")
	}
	if err := (Monitor{Name: "s", ProjectID: "p", Type: MonitorSynthetic, IntervalSeconds: 60, TimeoutSeconds: 5, Config: map[string]string{SyntheticScenarioKey: ok}}).Validate(); err != nil {
		t.Fatalf("valid synthetic rejected: %v", err)
	}
}
