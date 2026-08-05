package domain

import "testing"

func TestWebhookValidate(t *testing.T) {
	if err := (Webhook{OrgID: "o1", URL: "https://hook.example/x"}).Validate(); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}
	if err := (Webhook{OrgID: "o1", URL: "http://internal:9000/h"}).Validate(); err != nil {
		t.Fatalf("http webhook rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		hook Webhook
	}{
		{"no org", Webhook{URL: "https://x"}},
		{"no url", Webhook{OrgID: "o1"}},
		{"bad scheme", Webhook{OrgID: "o1", URL: "ftp://x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.hook.Validate(); err == nil {
				t.Errorf("expected %s to fail", tc.name)
			}
		})
	}
}
