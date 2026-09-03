package domain

import "testing"

// The phase-F form builds a config in TypeScript and the server validates it in Go. Those are two
// implementations of one contract, and the way they drift is that the client accepts something the
// server refuses — an operator then meets a 400 the form promised would not happen.
//
// This pins the seam with the EXACT document `frontend/src/lib/canaryWorkflow.spec.ts` builds from its
// `validForm()` fixture. If the client's idea of valid stops matching the server's, this fails here
// rather than in a browser.
func TestTheFormsValidDocumentIsValidToTheServer(t *testing.T) {
	const fromTheForm = `{"kind":"async_transaction_v1","submit":{"kind":"http_json","method":"POST",` +
		`"url":"https://files.example.com/files/upload","submit_timeout":30,"accepted_status":[202],` +
		`"headers":[{"name":"authorization","secret_ref":"upload"},{"name":"x-tenant","value":"canary"}],` +
		`"body":{"tenant":"canary"}},` +
		`"correlate":{"source":"response_json","path":"task_id"},` +
		`"completion":{"kind":"poll_json","url":"https://files.example.com/tasks/{{ correlation_id }}",` +
		`"timeout":240,"headers":[],"poll":{"interval":5,"max_attempts":48,"success_path":"status",` +
		`"success_value":"completed","failure_path":"","failure_values":[]}},` +
		`"result":{"max_latency":240,"required_json_fields":["s3_path","byte_size","media_type"],` +
		`"lifecycle_path":"s3_path"},` +
		`"cleanup":{"kind":"lifecycle_prefix","prefix":"canary/","acknowledged":false}}`

	m := Monitor{
		ProjectID: "p", Name: "canary", Type: MonitorAsyncCanary, Region: "core",
		IntervalSeconds: 300, TimeoutSeconds: 300, Enabled: true,
		Config: map[string]string{
			CanaryWorkflowKey:            fromTheForm,
			CanarySecretRefKey("upload"): "upload-token",
		},
	}
	m.Normalize()
	if err := m.Validate(); err != nil {
		t.Fatalf("the form builds a document the server refuses: %v", err)
	}
	// And it survives the round trip the executor makes, so the form is not producing something that
	// validates and then cannot be read back.
	if _, err := ParseCanaryConfig(m.Config); err != nil {
		t.Fatalf("the form's document does not parse back: %v", err)
	}
}
