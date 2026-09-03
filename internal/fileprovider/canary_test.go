package fileprovider

import (
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 in a bundle. This is the type that made the nested typed schema necessary, and every case
// here is a row of the spec's §7 file-provider matrix.

const canaryBundleHead = `format: 2
organization: acme
project: api
monitors:
  charla-upload:
    name: Charla upload journey
    type: async_canary
    interval: 5m
    timeout: 5m
    workflow:
      kind: async_transaction_v1
`

func canaryDecode(t *testing.T, body string) (*DesiredProject, error) {
	t.Helper()
	return Decode([]byte(body), config.ProviderScopeConfig{Type: config.ProviderScopeInstance})
}

const canaryPollBody = `      secrets:
        upload: charla-upload-token
      submit:
        kind: http_json
        method: POST
        url: https://files.example.com/files/upload
        submit_timeout: 30s
        accepted_status: [202]
        headers:
          - name: authorization
            secret_ref: upload
          - name: x-tenant
            value: canary
        body:
          tenant: canary
          attempts: 1
          dry_run: false
          token:
            secret_ref: upload
      correlate:
        source: response_json
        path: task_id
      completion:
        kind: poll_json
        url: https://files.example.com/tasks/{{ correlation_id }}
        timeout: 4m
        poll_json:
          interval: 5s
          max_attempts: 48
          success:
            path: status
            value: completed
          failure:
            path: status
            values: [failed, cancelled]
      result:
        max_latency: 4m
        required_json_fields: [s3_path, byte_size]
        lifecycle_path: s3_path
      cleanup:
        kind: lifecycle_prefix
        prefix: canary/
        acknowledged: true
`

const canarySSEBody = `      secrets:
        upload: charla-upload-token
      submit:
        kind: multipart_fixture
        method: POST
        url: https://files.example.com/files/upload
        submit_timeout: 30s
        accepted_status: [202]
        headers:
          - name: authorization
            secret_ref: upload
        fixture_ref: small_wav_v1
        multipart:
          file_field: file
          fields:
            only_audio: false
      correlate:
        source: response_header
        header_name: task-id
      completion:
        kind: sse
        url: https://files.example.com/tasks/{{ correlation_id }}/events
        timeout: 4m
        sse:
          success_event: task.completed
          failure_events: [task.failed]
          required_json_fields: [s3_path]
      result:
        max_latency: 4m
        required_json_fields: [s3_path, byte_size, media_type]
        lifecycle_path: s3_path
      cleanup:
        kind: none
        acknowledged: true
`

func TestCanaryBundleAcceptsBothValidShapes(t *testing.T) {
	for name, body := range map[string]string{"http_json+poll": canaryPollBody, "multipart+sse": canarySSEBody} {
		t.Run(name, func(t *testing.T) {
			proj, err := canaryDecode(t, canaryBundleHead+body)
			if err != nil {
				t.Fatalf("a valid bundle was refused: %v", err)
			}
			dm, ok := proj.Monitors["charla-upload"]
			if !ok {
				t.Fatal("the monitor is missing from the decoded project")
			}
			if dm.Monitor.Type != domain.MonitorAsyncCanary {
				t.Fatalf("type = %s", dm.Monitor.Type)
			}
			// The persisted form: one canonical document plus one flat ref key per binding, and no
			// project-secret name inside the document (D3f).
			doc := dm.Monitor.Config[domain.CanaryWorkflowKey]
			if doc == "" {
				t.Fatal("the monitor carries no canonical workflow")
			}
			if strings.Contains(doc, "charla-upload-token") {
				t.Fatalf("the persisted document carries a project-secret name:\n%s", doc)
			}
			if dm.Monitor.Config[domain.CanarySecretRefKey("upload")] != "charla-upload-token" {
				t.Fatal("the flat ref key must carry the project secret name")
			}
			if dm.Hash == "" {
				t.Fatal("a decoded monitor must carry its semantic hash")
			}
		})
	}
}

func TestCanaryBundleRefusals(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		reason Reason
		want   string
	}{
		{"inline token in a credential header",
			strings.Replace(canaryPollBody, "          - name: authorization\n            secret_ref: upload",
				"          - name: authorization\n            value: Bearer literal-token", 1),
			ReasonInlineSecret, "literal"},
		{"inline secret nested in a body",
			strings.Replace(canaryPollBody, "          token:\n            secret_ref: upload",
				"          password: hunter2", 1),
			ReasonInlineSecret, "secret inline"},
		{"an arbitrary fixture path",
			strings.Replace(canarySSEBody, "        fixture_ref: small_wav_v1", "        fixture_ref: /etc/passwd", 1),
			ReasonDomainInvalid, "registry key"},
		{"an unknown field inside the workflow",
			strings.Replace(canaryPollBody, "        path: task_id", "        path: task_id\n        surprise: 1", 1),
			ReasonUnknownField, ""},
		{"settings beside a workflow",
			canaryPollBody + "    settings:\n      query: up\n",
			ReasonUnsupportedField, "not `settings`"},
		{"a target on a canary",
			strings.Replace(canaryBundleHead+canaryPollBody, "    type: async_canary",
				"    type: async_canary\n    target: https://files.example.com", 1),
			ReasonUnsupportedField, "no `target`"},
		{"a secret_ref node with a second key",
			strings.Replace(canaryPollBody, "          token:\n            secret_ref: upload",
				"          token:\n            secret_ref: upload\n            extra: 1", 1),
			ReasonUnsupportedField, "exactly one string binding name"},
		{"poll budget past the completion window",
			strings.Replace(canaryPollBody, "          max_attempts: 48", "          max_attempts: 600", 1),
			ReasonDomainInvalid, "completion timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if !strings.HasPrefix(body, "format:") {
				body = canaryBundleHead + body
			}
			_, err := canaryDecode(t, body)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			be, ok := err.(*BundleError)
			if !ok {
				t.Fatalf("err = %v, want a *BundleError", err)
			}
			if be.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s (%v)", be.Reason, tc.reason, be.Msg)
			}
			if tc.want != "" && !strings.Contains(be.Msg, tc.want) {
				t.Fatalf("msg = %q, want it to mention %q", be.Msg, tc.want)
			}
			// A refusal never echoes a credential.
			if strings.Contains(be.Msg, "hunter2") || strings.Contains(be.Msg, "literal-token") {
				t.Fatalf("the refusal echoed the secret: %s", be.Msg)
			}
		})
	}
}

// A `workflow` block on any other type is refused: two ways to configure one thing is how one of
// them stops being validated.
func TestWorkflowBelongsOnlyToACanary(t *testing.T) {
	body := `format: 2
organization: acme
project: api
monitors:
  web:
    name: web
    type: http
    target: https://example.com
    workflow:
      kind: async_transaction_v1
`
	_, err := canaryDecode(t, body)
	be, ok := err.(*BundleError)
	if !ok || be.Reason != ReasonUnsupportedField {
		t.Fatalf("err = %v, want unsupported_field", err)
	}
}

// The semantic hash is what decides create / update / no-op, so it must move for a semantic change
// and stay put for a reformat.
func TestCanaryBundleHashSemantics(t *testing.T) {
	base, err := canaryDecode(t, canaryBundleHead+canaryPollBody)
	if err != nil {
		t.Fatal(err)
	}
	h := base.Monitors["charla-upload"].Hash

	// Reordering a set-like list and re-casing a header name is the SAME document.
	reordered := strings.Replace(canaryPollBody,
		"        required_json_fields: [s3_path, byte_size]", "        required_json_fields: [byte_size, s3_path]", 1)
	reordered = strings.Replace(reordered, "          - name: authorization", "          - name: Authorization", 1)
	same, err := canaryDecode(t, canaryBundleHead+reordered)
	if err != nil {
		t.Fatal(err)
	}
	if same.Monitors["charla-upload"].Hash != h {
		t.Fatal("reordering a set and re-casing a header name must not move the hash")
	}

	// Pointing the binding at a DIFFERENT project secret is a semantic change.
	remapped := strings.Replace(canaryPollBody, "        upload: charla-upload-token", "        upload: another-secret", 1)
	moved, err := canaryDecode(t, canaryBundleHead+remapped)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Monitors["charla-upload"].Hash == h {
		t.Fatal("re-pointing a binding at another secret must move the hash")
	}

	// So is a changed promise.
	slower := strings.Replace(canaryPollBody, "        max_latency: 4m", "        max_latency: 3m", 1)
	changed, err := canaryDecode(t, canaryBundleHead+slower)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Monitors["charla-upload"].Hash == h {
		t.Fatal("a changed promise must move the hash")
	}
}
