package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// FR-029 phase B: the typed contract, its refusals, and canonicalization. Every case here is a row
// of the spec's §7 domain matrix, and the ones that look pedantic are the ones a review round asked
// for by name — bounds with numbers, a header name that is not a JSON path, a placeholder that is not
// a whole path segment.

const canaryTimeout = 300

func validMultipartWorkflow() CanaryWorkflow {
	return CanaryWorkflow{
		Kind:    CanaryWorkflowKind,
		Secrets: map[string]string{"upload": "charla-upload-token"},
		Submit: CanarySubmit{
			Kind:           CanarySubmitMultipartFixture,
			Method:         "POST",
			URL:            "https://files.example.com/files/upload",
			SubmitTimeout:  30,
			AcceptedStatus: []int{202},
			Headers: []CanaryHeader{
				{Name: "authorization", SecretRef: "upload"},
				{Name: "x-tenant", Value: "canary"},
			},
			FixtureRef: "small_wav_v1",
			Multipart: &CanaryMultipart{
				FileField: "file",
				Fields:    map[string]CanaryValue{"only_audio": {Kind: CanaryValueBool, Bool: false}},
			},
		},
		Correlate: CanaryCorrelate{Source: CanaryCorrelateResponseJSON, Path: "task_id"},
		Completion: CanaryCompletion{
			Kind:    CanaryCompletionSSE,
			URL:     "https://files.example.com/tasks/{{ correlation_id }}/events",
			Timeout: 240,
			SSE: &CanarySSE{
				SuccessEvent:       "task.completed",
				FailureEvents:      []string{"task.failed"},
				RequiredJSONFields: []string{"s3_path", "byte_size", "media_type"},
			},
		},
		Result: CanaryResult{
			MaxLatency:         240,
			RequiredJSONFields: []string{"s3_path", "byte_size", "media_type"},
			LifecyclePath:      "s3_path",
		},
		Cleanup: CanaryCleanup{Kind: CanaryCleanupLifecyclePrefix, Prefix: "canary/", Acknowledged: true},
	}
}

func validPollWorkflow() CanaryWorkflow {
	w := validMultipartWorkflow()
	w.Submit.Kind = CanarySubmitHTTPJSON
	w.Submit.FixtureRef = ""
	w.Submit.Multipart = nil
	w.Submit.Body = map[string]CanaryValue{
		"tenant":  {Kind: CanaryValueString, Str: "canary"},
		"dry_run": {Kind: CanaryValueBool, Bool: false},
		"token":   {Kind: CanaryValueSecret, SecretRef: "upload"},
	}
	w.Completion = CanaryCompletion{
		Kind:    CanaryCompletionPollJSON,
		URL:     "https://files.example.com/tasks/{{ correlation_id }}",
		Timeout: 240,
		Poll: &CanaryPoll{
			Interval:    5,
			MaxAttempts: 48,
			Success:     CanaryPollMatch{Path: "status", Value: "completed"},
			Failure:     CanaryPollMatch{Path: "status", Values: []string{"failed", "cancelled"}},
		},
	}
	return w
}

func mustValidate(t *testing.T, w CanaryWorkflow) {
	t.Helper()
	if err := ValidateCanaryWorkflow(w, canaryTimeout); err != nil {
		t.Fatalf("a valid workflow was refused: %v", err)
	}
}

func refusal(t *testing.T, w CanaryWorkflow, want string) {
	t.Helper()
	err := ValidateCanaryWorkflow(w, canaryTimeout)
	if err == nil {
		t.Fatalf("expected a refusal mentioning %q, got none", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal = %v, want it to mention %q", err, want)
	}
}

func TestBothValidShapesAreAccepted(t *testing.T) {
	mustValidate(t, validMultipartWorkflow())
	mustValidate(t, validPollWorkflow())
}

func TestForbiddenBranchCombinations(t *testing.T) {
	w := validMultipartWorkflow()
	w.Submit.Body = map[string]CanaryValue{"x": {Kind: CanaryValueString, Str: "y"}}
	refusal(t, w, "body belongs to")

	w = validPollWorkflow()
	w.Submit.FixtureRef = "small_wav_v1"
	refusal(t, w, "fixture_ref and multipart belong to")

	w = validMultipartWorkflow()
	w.Completion.Poll = &CanaryPoll{Interval: 5, MaxAttempts: 2}
	refusal(t, w, "poll_json fields belong to")

	w = validPollWorkflow()
	w.Completion.SSE = &CanarySSE{SuccessEvent: "x"}
	refusal(t, w, "sse fields belong to")

	w = validMultipartWorkflow()
	w.Correlate = CanaryCorrelate{Source: CanaryCorrelateResponseJSON, Path: "task_id", HeaderName: "task-id"}
	refusal(t, w, "header_name belongs to")

	w = validMultipartWorkflow()
	w.Kind = "async_transaction_v2"
	refusal(t, w, "unknown kind")
}

func TestHeaderRules(t *testing.T) {
	// A credential-bearing header takes a binding and nothing else — by SCHEMA, never by inspecting
	// the value (D7).
	w := validMultipartWorkflow()
	w.Submit.Headers = []CanaryHeader{{Name: "authorization", Value: "Bearer literal"}}
	err := ValidateCanaryWorkflow(w, canaryTimeout)
	if err == nil || !strings.Contains(err.Error(), "credential-bearing") {
		t.Fatalf("a literal in a credential header must be refused, got %v", err)
	}
	if strings.Contains(err.Error(), "Bearer literal") {
		t.Fatalf("the refusal echoed the value: %v", err)
	}

	// Two names differing only in case are ONE location on the wire.
	w = validMultipartWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "Authorization", SecretRef: "upload"})
	refusal(t, w, "declared twice")

	// The runner owns these, so an author-supplied value would make its own contract ambiguous.
	for _, name := range []string{"idempotency-key", "Host", "content-length", "transfer-encoding"} {
		w = validMultipartWorkflow()
		w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: name, Value: "x"})
		refusal(t, w, "set by the runner")
	}
	// content-type is the runner's only on a multipart submit, where it owns the boundary.
	w = validMultipartWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "content-type", Value: "application/json"})
	refusal(t, w, "set by the runner")
	w = validPollWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "content-type", Value: "application/json"})
	mustValidate(t, w)

	w = validMultipartWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "x-both", Value: "v", SecretRef: "upload"})
	refusal(t, w, "not both")

	w = validMultipartWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "x-empty"})
	refusal(t, w, "has no value")

	w = validMultipartWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "x-long", Value: strings.Repeat("v", CanaryMaxHeaderValueBytes+1)})
	refusal(t, w, "longer than")

	w = validMultipartWorkflow()
	for i := 0; i < CanaryMaxHeadersPerRequest; i++ {
		w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "x-pad-" + string(rune('a'+i)), Value: "v"})
	}
	refusal(t, w, "at most")

	w = validMultipartWorkflow()
	w.Submit.Headers = append(w.Submit.Headers, CanaryHeader{Name: "bad header", Value: "v"})
	refusal(t, w, "not a valid header name")
}

func TestBindingRules(t *testing.T) {
	w := validMultipartWorkflow()
	w.Secrets["unused"] = "another-secret"
	refusal(t, w, "declared and never used")

	w = validMultipartWorkflow()
	w.Submit.Headers = []CanaryHeader{{Name: "authorization", SecretRef: "ghost"}}
	refusal(t, w, "workflow.secrets does not declare")

	w = validMultipartWorkflow()
	w.Secrets = map[string]string{"Upload": "x"}
	refusal(t, w, "not a valid binding name")

	w = validMultipartWorkflow()
	w.Secrets["upload"] = "   "
	refusal(t, w, "names no project secret")

	w = validMultipartWorkflow()
	w.Secrets = map[string]string{}
	for i := 0; i <= CanaryMaxBindings; i++ {
		w.Secrets["b"+string(rune('a'+i))] = "s"
	}
	refusal(t, w, "at most")
}

func TestURLRules(t *testing.T) {
	w := validMultipartWorkflow()
	w.Submit.URL = "http://files.example.com/upload"
	refusal(t, w, "must be https")

	w = validMultipartWorkflow()
	w.Submit.URL = "https://user:pass@files.example.com/upload"
	refusal(t, w, "userinfo")

	// The one substitution is legal in completion.url and nowhere else (D4).
	w = validMultipartWorkflow()
	w.Submit.URL = "https://files.example.com/upload/{{ correlation_id }}"
	refusal(t, w, "only legal in completion.url")

	// It must be a WHOLE path segment: glued to other text, a `../` from the target rewrites the
	// request cerbix is about to make.
	for _, bad := range []string{
		"https://files.example.com/tasks/x{{ correlation_id }}/events",
		"https://files.example.com/tasks/{{ correlation_id }}x/events",
		"https://files.example.com/tasks?id={{ correlation_id }}",
		"https://{{ correlation_id }}.example.com/events",
	} {
		w = validMultipartWorkflow()
		w.Completion.URL = bad
		refusal(t, w, "whole path segment")
	}

	w = validMultipartWorkflow()
	w.Completion.URL = "https://files.example.com/tasks/{{ correlation_id }}/{{ correlation_id }}"
	refusal(t, w, "at most one")

	w = validMultipartWorkflow()
	w.Completion.URL = "https://files.example.com/tasks/{{ task }}/events"
	refusal(t, w, "the only substitution")

	// A completion URL with no placeholder at all is legal: not every API addresses by path.
	w = validMultipartWorkflow()
	w.Completion.URL = "https://files.example.com/tasks/events"
	mustValidate(t, w)
}

func TestACompletionBindingMayNotCrossHosts(t *testing.T) {
	w := validMultipartWorkflow()
	w.Completion.Headers = []CanaryHeader{{Name: "authorization", SecretRef: "upload"}}
	mustValidate(t, w) // same host as submit

	w.Completion.URL = "https://other.example.com/tasks/{{ correlation_id }}/events"
	refusal(t, w, "completion host to equal the submit host")

	// A port change is a host change for this rule.
	w = validMultipartWorkflow()
	w.Completion.Headers = []CanaryHeader{{Name: "authorization", SecretRef: "upload"}}
	w.Completion.URL = "https://files.example.com:8443/tasks/{{ correlation_id }}/events"
	refusal(t, w, "completion host to equal the submit host")

	// Without a binding, a different completion host is fine: nothing is being handed over.
	w = validMultipartWorkflow()
	w.Completion.URL = "https://other.example.com/tasks/{{ correlation_id }}/events"
	mustValidate(t, w)
}

func TestCorrelateGrammarsAreSourceSpecific(t *testing.T) {
	// The case that made the field unusable before revision 4: an ordinary header name.
	w := validMultipartWorkflow()
	w.Correlate = CanaryCorrelate{Source: CanaryCorrelateResponseHeader, HeaderName: "task-id"}
	mustValidate(t, w)

	w.Correlate = CanaryCorrelate{Source: CanaryCorrelateResponseHeader, HeaderName: "task id"}
	refusal(t, w, "not a valid header name")

	w = validMultipartWorkflow()
	w.Correlate = CanaryCorrelate{Source: CanaryCorrelateResponseJSON, Path: "data[0].id"}
	refusal(t, w, "not a valid path")

	w = validMultipartWorkflow()
	w.Correlate = CanaryCorrelate{Source: CanaryCorrelateResponseJSON, Path: "data.0.id"}
	mustValidate(t, w)

	w = validMultipartWorkflow()
	w.Correlate = CanaryCorrelate{Source: "response_body", Path: "id"}
	refusal(t, w, "unknown source")

	w = validMultipartWorkflow()
	w.Correlate = CanaryCorrelate{Source: CanaryCorrelateResponseJSON, Path: strings.Repeat("a.", 8) + "b"}
	refusal(t, w, "deeper than")
}

func TestAcceptedStatusIsA2xxSet(t *testing.T) {
	w := validMultipartWorkflow()
	w.Submit.AcceptedStatus = nil
	refusal(t, w, "at least one status")

	w = validMultipartWorkflow()
	w.Submit.AcceptedStatus = []int{302}
	refusal(t, w, "must be 2xx")

	w = validMultipartWorkflow()
	w.Submit.AcceptedStatus = []int{202, 202}
	refusal(t, w, "twice")

	w = validMultipartWorkflow()
	w.Submit.AcceptedStatus = []int{200, 202}
	mustValidate(t, w)
}

func TestBoundsAtTheEdgeAndOnePastIt(t *testing.T) {
	cases := []struct {
		name string
		edge func(w *CanaryWorkflow, past bool)
		want string
	}{
		{"submit_timeout", func(w *CanaryWorkflow, past bool) {
			w.Submit.SubmitTimeout = CanaryMaxSubmitTimeout
			if past {
				w.Submit.SubmitTimeout++
			}
		}, "submit_timeout must be between"},
		{"poll interval", func(w *CanaryWorkflow, past bool) {
			w.Completion.Poll.Interval = CanaryMaxPollInterval
			w.Completion.Poll.MaxAttempts = 1
			if past {
				w.Completion.Poll.Interval++
			}
		}, "poll interval must be between"},
		{"poll attempts", func(w *CanaryWorkflow, past bool) {
			w.Completion.Poll.Interval = 1
			w.Completion.Poll.MaxAttempts = 240
			if past {
				w.Completion.Poll.MaxAttempts = CanaryMaxPollAttempts + 1
			}
		}, "max_attempts must be between"},
		{"required fields", func(w *CanaryWorkflow, past bool) {
			w.Result.RequiredJSONFields = nil
			for i := 0; i < CanaryMaxRequiredFields; i++ {
				w.Result.RequiredJSONFields = append(w.Result.RequiredJSONFields, "f"+string(rune('a'+i)))
			}
			if past {
				w.Result.RequiredJSONFields = append(w.Result.RequiredJSONFields, "extra")
			}
		}, "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := validPollWorkflow()
			tc.edge(&w, false)
			if err := ValidateCanaryWorkflow(w, canaryTimeout); err != nil {
				t.Fatalf("the edge itself must be legal: %v", err)
			}
			w = validPollWorkflow()
			tc.edge(&w, true)
			refusal(t, w, tc.want)
		})
	}
}

func TestPollBudgetMustFitTheWindowItPollsIn(t *testing.T) {
	w := validPollWorkflow()
	w.Completion.Poll.Interval = 5
	w.Completion.Poll.MaxAttempts = 49 // 245s > the 240s completion timeout
	refusal(t, w, "must not exceed the completion timeout")
}

func TestResultPromiseAndTimeoutAreDifferentThings(t *testing.T) {
	w := validMultipartWorkflow()
	w.Result.MaxLatency = canaryTimeout + 1
	refusal(t, w, "max_latency must not exceed")

	w = validMultipartWorkflow()
	w.Result.MaxLatency = canaryTimeout // equal is legal: the promise IS the limit
	mustValidate(t, w)

	w = validMultipartWorkflow()
	w.Result.RequiredJSONFields = nil
	refusal(t, w, "at least one field")

	w = validMultipartWorkflow()
	w.Result.LifecyclePath = "s3 path"
	refusal(t, w, "not a valid path")
}

func TestCleanupAcknowledgement(t *testing.T) {
	w := validMultipartWorkflow()
	w.Cleanup = CanaryCleanup{Kind: CanaryCleanupNone}
	refusal(t, w, "requires acknowledged: true")

	w.Cleanup = CanaryCleanup{Kind: CanaryCleanupNone, Acknowledged: true}
	mustValidate(t, w)

	w.Cleanup = CanaryCleanup{Kind: CanaryCleanupNone, Prefix: "canary/", Acknowledged: true}
	refusal(t, w, "prefix belongs to")

	w.Cleanup = CanaryCleanup{Kind: CanaryCleanupLifecyclePrefix}
	refusal(t, w, "needs a prefix")
}

func TestFixtureIsARegistryKeyAndNothingElse(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "https://example.com/f.wav", "small_wav_v2", ""} {
		w := validMultipartWorkflow()
		w.Submit.FixtureRef = bad
		refusal(t, w, "not a registry key")
	}
	if !CanaryFixtureExists("small_wav_v1") {
		t.Fatal("the v1 registry must carry small_wav_v1")
	}
	f, _ := CanaryFixtureByRef("small_wav_v1")
	if f.MaxBytes > CanaryFixtureRegistryMax {
		t.Fatalf("fixture %s declares %d bytes, past the registry ceiling", f.Ref, f.MaxBytes)
	}
}

func TestBodyAlgebra(t *testing.T) {
	deep := CanaryValue{Kind: CanaryValueString, Str: "leaf"}
	for i := 0; i < CanaryMaxBodyDepth; i++ {
		deep = CanaryValue{Kind: CanaryValueObject, Obj: map[string]CanaryValue{"n": deep}}
	}
	w := validPollWorkflow()
	w.Submit.Body = map[string]CanaryValue{"deep": deep}
	w.Secrets = map[string]string{}
	w.Submit.Headers = []CanaryHeader{{Name: "x-tenant", Value: "canary"}}
	refusal(t, w, "nested deeper than")

	w = validPollWorkflow()
	w.Submit.Body["bad key"] = CanaryValue{Kind: CanaryValueString, Str: "x"}
	refusal(t, w, "not a valid key")

	w = validPollWorkflow()
	w.Submit.Body["huge"] = CanaryValue{Kind: CanaryValueString, Str: strings.Repeat("x", CanaryMaxStringLeafBytes+1)}
	refusal(t, w, "longer than")

	w = validPollWorkflow()
	list := make([]CanaryValue, CanaryMaxBodyListElements+1)
	for i := range list {
		list[i] = CanaryValue{Kind: CanaryValueNumber, Num: json.Number("1")}
	}
	w.Submit.Body["many"] = CanaryValue{Kind: CanaryValueList, List: list}
	refusal(t, w, "at most 32 list elements")

	w = validPollWorkflow()
	w.Submit.Body["ghost"] = CanaryValue{Kind: CanaryValueSecret, SecretRef: "nope"}
	refusal(t, w, "workflow.secrets does not declare")

	w = validPollWorkflow()
	w.Submit.Body["broken"] = CanaryValue{Kind: "wat"}
	refusal(t, w, "unsupported value shape")
}

// The D3a residual, asserted as a NON-guarantee so a later reader does not mistake silence for
// coverage: a credential pasted as an ordinary string leaf is ACCEPTED, because it is undecidable.
func TestALiteralInAnOrdinaryBodyLeafIsNotDetectable(t *testing.T) {
	w := validPollWorkflow()
	w.Submit.Body["api_token"] = CanaryValue{Kind: CanaryValueString, Str: "EXAMPLE-looks-exactly-like-a-token"}
	mustValidate(t, w)
}

func TestCanonicalisationIgnoresOrderAndSpelling(t *testing.T) {
	a := validMultipartWorkflow()
	b := validMultipartWorkflow()
	// Same document, different author habits: header order, set order, header case.
	b.Submit.Headers = []CanaryHeader{{Name: "X-Tenant", Value: "canary"}, {Name: "Authorization", SecretRef: "upload"}}
	b.Completion.SSE.RequiredJSONFields = []string{"media_type", "s3_path", "byte_size"}
	b.Result.RequiredJSONFields = []string{"media_type", "byte_size", "s3_path"}

	ja, err := CanaryCanonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := CanaryCanonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if ja != jb {
		t.Fatalf("canonical forms differ:\n%s\n%s", ja, jb)
	}
}

func TestTheCanonicalDocumentCarriesNoProjectSecretName(t *testing.T) {
	cfg, err := CanaryConfig(validMultipartWorkflow())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg[CanaryWorkflowKey], "charla-upload-token") {
		t.Fatalf("the persisted document carries a project-secret name:\n%s", cfg[CanaryWorkflowKey])
	}
	if cfg[CanarySecretRefKey("upload")] != "charla-upload-token" {
		t.Fatalf("the flat ref key must hold the project secret name, got %q", cfg[CanarySecretRefKey("upload")])
	}
	if !strings.Contains(cfg[CanaryWorkflowKey], `"secret_ref":"upload"`) {
		t.Fatalf("the document must carry the binding marker:\n%s", cfg[CanaryWorkflowKey])
	}
}

func TestSemanticHashMovesOnIdentityAndNotOnRotation(t *testing.T) {
	base, err := CanaryConfig(validMultipartWorkflow())
	if err != nil {
		t.Fatal(err)
	}
	h := CanarySemanticHash(base)

	// Re-deriving the same workflow is a no-op: nothing reschedules.
	again, _ := CanaryConfig(validMultipartWorkflow())
	if CanarySemanticHash(again) != h {
		t.Fatal("an unchanged workflow must hash the same")
	}

	// Rotating a secret's VALUE never touches the config, so it cannot move the hash: the value is
	// not in the document and not in the ref key.
	if CanarySemanticHash(base) != h {
		t.Fatal("rotation must not move the hash")
	}

	// Pointing the binding at a DIFFERENT project secret is a semantic change.
	remapped := map[string]string{}
	for k, v := range base {
		remapped[k] = v
	}
	remapped[CanarySecretRefKey("upload")] = "another-secret"
	if CanarySemanticHash(remapped) == h {
		t.Fatal("re-pointing a binding at another secret must move the hash")
	}

	// So is any semantic edit of the document itself.
	w := validMultipartWorkflow()
	w.Result.MaxLatency = 120
	edited, _ := CanaryConfig(w)
	if CanarySemanticHash(edited) == h {
		t.Fatal("a changed promise must move the hash")
	}
}

func TestParseCanaryConfigRoundTrips(t *testing.T) {
	for _, w := range []CanaryWorkflow{validMultipartWorkflow(), validPollWorkflow()} {
		cfg, err := CanaryConfig(w)
		if err != nil {
			t.Fatal(err)
		}
		back, err := ParseCanaryConfig(cfg)
		if err != nil {
			t.Fatalf("stored config does not parse: %v", err)
		}
		if err := ValidateCanaryWorkflow(back, canaryTimeout); err != nil {
			t.Fatalf("a round-tripped workflow must still validate: %v", err)
		}
		again, err := CanaryConfig(back)
		if err != nil {
			t.Fatal(err)
		}
		if again[CanaryWorkflowKey] != cfg[CanaryWorkflowKey] {
			t.Fatalf("round trip is not stable:\n%s\n%s", cfg[CanaryWorkflowKey], again[CanaryWorkflowKey])
		}
		if back.Secrets["upload"] != "charla-upload-token" {
			t.Fatalf("the binding mapping must come back from the flat key, got %q", back.Secrets["upload"])
		}
	}
}

func TestParseCanaryConfigRefusesAnUnknownField(t *testing.T) {
	cfg, _ := CanaryConfig(validMultipartWorkflow())
	cfg[CanaryWorkflowKey] = strings.Replace(cfg[CanaryWorkflowKey], `{"kind"`, `{"surprise":1,"kind"`, 1)
	if _, err := ParseCanaryConfig(cfg); err == nil {
		t.Fatal("a stored document with an unknown field must not parse")
	}
	if _, err := ParseCanaryConfig(map[string]string{}); err == nil {
		t.Fatal("a monitor with no workflow key must not parse")
	}
}

func TestRefKeyRoundTripAndMalformedKeys(t *testing.T) {
	if k := CanarySecretRefKey("upload"); k != "canary_secret_upload_ref" {
		t.Fatalf("ref key = %q", k)
	}
	if b, ok := CanaryBindingFromRefKey("canary_secret_upload_ref"); !ok || b != "upload" {
		t.Fatalf("round trip = %q, %v", b, ok)
	}
	if _, ok := CanaryBindingFromRefKey("canary_secret_Upload_ref"); ok {
		t.Fatal("a name outside the grammar must not parse as a binding")
	}
	keys := CanarySecretRefKeys(map[string]string{
		"canary_secret_b_ref": "x", "canary_secret_a_ref": "y", "workflow": "{}",
	})
	if len(keys) != 2 || keys[0] != "canary_secret_a_ref" {
		t.Fatalf("ref keys = %v, want both, sorted", keys)
	}
}

func TestDurationSpellings(t *testing.T) {
	for in, want := range map[string]int{"30s": 30, "5m": 300, "1h": 3600, "600": 600, " 45s ": 45} {
		got, err := CanaryDurationSeconds(in)
		if err != nil || got != want {
			t.Fatalf("CanaryDurationSeconds(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "500ms", "-1", "soon", "5x"} {
		if _, err := CanaryDurationSeconds(bad); err == nil {
			t.Fatalf("CanaryDurationSeconds(%q) must fail", bad)
		}
	}
}
