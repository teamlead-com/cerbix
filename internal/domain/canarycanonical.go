package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Canonicalization, the semantic hash and the flat config projection for FR-029 (§5.2, §5.3, D3f).
//
// Three rules govern everything here, and each exists because a document that hashes differently
// after a reformat is a document that reschedules a monitor for nothing:
//
//   - map order is insignificant — `encoding/json` emits object keys sorted, and every map is
//     encoded as an object;
//   - a list that is semantically a SET is sorted before encoding, and duplicates are refused at
//     validation rather than deduplicated here;
//   - durations are whole seconds, header names are lower-case, and the `secrets` map is NOT part of
//     the canonical document at all (D3f).

// CanaryWorkflowKey is the single flat config key that holds the canonical document. `monitors.config`
// is a `map[string]string` and always was — `repointSecretRefs` decodes it as one — so a nested object
// there would fail that decode and therefore fail the whole rename for every monitor in the project.
// The contract is typed; the STORAGE is a canonical serialization of it (D3e).
const CanaryWorkflowKey = "workflow"

const (
	canarySecretPrefix = "canary_secret_"
	canarySecretSuffix = "_ref"
)

// CanarySecretRefKey renders the flat key that holds one binding's project-secret NAME. Same shape as
// `scenario_secret_<binding>_ref` and `password_ref`, which is what keeps rename, delete-counting and
// rotation on the path they already run on, with no canary-aware code (D3b).
func CanarySecretRefKey(binding string) string {
	return canarySecretPrefix + binding + canarySecretSuffix
}

// CanaryBindingFromRefKey is the inverse, and it refuses a key that looks like one and does not
// parse: a typo silently ignored is a binding the operator believes they declared.
func CanaryBindingFromRefKey(key string) (string, bool) {
	if !strings.HasPrefix(key, canarySecretPrefix) || !strings.HasSuffix(key, canarySecretSuffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, canarySecretPrefix), canarySecretSuffix)
	if !canaryBindingName.MatchString(name) {
		return "", false
	}
	return name, true
}

// CanarySecretRefKeys returns the ref keys a config carries, sorted — the store's single source for
// normalizing `monitor_secret_refs` without knowing anything about workflows.
func CanarySecretRefKeys(config map[string]string) []string {
	var out []string
	for k := range config {
		if _, ok := CanaryBindingFromRefKey(k); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ── The canonical document ─────────────────────────────────────────────────────────────────────

type canonicalHeader struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
}

type canonicalSubmit struct {
	Kind           string            `json:"kind"`
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	SubmitTimeout  int               `json:"submit_timeout"`
	AcceptedStatus []int             `json:"accepted_status"`
	Headers        []canonicalHeader `json:"headers,omitempty"`
	FixtureRef     string            `json:"fixture_ref,omitempty"`
	FileField      string            `json:"file_field,omitempty"`
	Fields         map[string]any    `json:"fields,omitempty"`
	Body           map[string]any    `json:"body,omitempty"`
}

type canonicalCorrelate struct {
	Source     string `json:"source"`
	Path       string `json:"path,omitempty"`
	HeaderName string `json:"header_name,omitempty"`
}

type canonicalSSE struct {
	SuccessEvent       string   `json:"success_event"`
	FailureEvents      []string `json:"failure_events,omitempty"`
	RequiredJSONFields []string `json:"required_json_fields,omitempty"`
}

type canonicalPoll struct {
	Interval      int      `json:"interval"`
	MaxAttempts   int      `json:"max_attempts"`
	SuccessPath   string   `json:"success_path"`
	SuccessValue  string   `json:"success_value"`
	FailurePath   string   `json:"failure_path,omitempty"`
	FailureValues []string `json:"failure_values,omitempty"`
}

type canonicalCompletion struct {
	Kind    string            `json:"kind"`
	URL     string            `json:"url"`
	Timeout int               `json:"timeout"`
	Headers []canonicalHeader `json:"headers,omitempty"`
	SSE     *canonicalSSE     `json:"sse,omitempty"`
	Poll    *canonicalPoll    `json:"poll,omitempty"`
}

type canonicalResult struct {
	MaxLatency         int      `json:"max_latency"`
	RequiredJSONFields []string `json:"required_json_fields"`
	LifecyclePath      string   `json:"lifecycle_path"`
}

type canonicalCleanup struct {
	Kind         string `json:"kind"`
	Prefix       string `json:"prefix,omitempty"`
	Acknowledged bool   `json:"acknowledged"`
}

// canonicalWorkflow deliberately has NO secrets field: `workflow.secrets` is input only, and the
// persisted document carries binding MARKERS at their positions. That is what makes "the canonical
// string is byte-identical across a rename" a consequence rather than a hope (D3f).
type canonicalWorkflow struct {
	Kind       string              `json:"kind"`
	Submit     canonicalSubmit     `json:"submit"`
	Correlate  canonicalCorrelate  `json:"correlate"`
	Completion canonicalCompletion `json:"completion"`
	Result     canonicalResult     `json:"result"`
	Cleanup    canonicalCleanup    `json:"cleanup"`
}

func canonicalHeaders(hs []CanaryHeader) []canonicalHeader {
	if len(hs) == 0 {
		return nil
	}
	out := make([]canonicalHeader, 0, len(hs))
	for _, h := range hs {
		out = append(out, canonicalHeader{
			Name:      strings.ToLower(strings.TrimSpace(h.Name)),
			Value:     h.Value,
			SecretRef: h.SecretRef,
		})
	}
	// A header list is a SET: two documents that differ only in the order an author typed the
	// headers are the same document. Duplicates are refused at validation, so a plain sort by name
	// is total.
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// canonicalBodyValue projects one algebra node onto plain JSON-able values. A secret node becomes
// `{"secret_ref": "<binding>"}` — a marker, never a name and never a value.
func canonicalBodyValue(v CanaryValue) any {
	switch v.Kind {
	case CanaryValueString:
		return v.Str
	case CanaryValueNumber:
		return json.RawMessage(v.Num.String())
	case CanaryValueBool:
		return v.Bool
	case CanaryValueSecret:
		return map[string]any{"secret_ref": v.SecretRef}
	case CanaryValueList:
		// A list in a BODY is ordered: it is the request the target receives, and reordering it is
		// a different request. Unlike the set-like lists elsewhere, it is never sorted.
		out := make([]any, 0, len(v.List))
		for _, item := range v.List {
			out = append(out, canonicalBodyValue(item))
		}
		return out
	case CanaryValueObject:
		out := make(map[string]any, len(v.Obj))
		for k, item := range v.Obj {
			out[k] = canonicalBodyValue(item)
		}
		return out
	default:
		return nil
	}
}

func canonicalBodyMap(m map[string]CanaryValue) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = canonicalBodyValue(v)
	}
	return out
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedInts(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

// CanaryCanonicalJSON renders the workflow in its canonical, persisted form.
func CanaryCanonicalJSON(w CanaryWorkflow) (string, error) {
	cw := canonicalWorkflow{
		Kind: w.Kind,
		Submit: canonicalSubmit{
			Kind:           w.Submit.Kind,
			Method:         w.Submit.Method,
			URL:            w.Submit.URL,
			SubmitTimeout:  w.Submit.SubmitTimeout,
			AcceptedStatus: sortedInts(w.Submit.AcceptedStatus),
			Headers:        canonicalHeaders(w.Submit.Headers),
			FixtureRef:     w.Submit.FixtureRef,
			Body:           canonicalBodyMap(w.Submit.Body),
		},
		Correlate: canonicalCorrelate{
			Source:     w.Correlate.Source,
			Path:       w.Correlate.Path,
			HeaderName: strings.ToLower(w.Correlate.HeaderName),
		},
		Completion: canonicalCompletion{
			Kind:    w.Completion.Kind,
			URL:     w.Completion.URL,
			Timeout: w.Completion.Timeout,
			Headers: canonicalHeaders(w.Completion.Headers),
		},
		Result: canonicalResult{
			MaxLatency:         w.Result.MaxLatency,
			RequiredJSONFields: sortedCopy(w.Result.RequiredJSONFields),
			LifecyclePath:      w.Result.LifecyclePath,
		},
		Cleanup: canonicalCleanup{
			Kind:         w.Cleanup.Kind,
			Prefix:       w.Cleanup.Prefix,
			Acknowledged: w.Cleanup.Acknowledged,
		},
	}
	if w.Submit.Multipart != nil {
		cw.Submit.FileField = w.Submit.Multipart.FileField
		cw.Submit.Fields = canonicalBodyMap(w.Submit.Multipart.Fields)
	}
	if w.Completion.SSE != nil {
		cw.Completion.SSE = &canonicalSSE{
			SuccessEvent:       w.Completion.SSE.SuccessEvent,
			FailureEvents:      sortedCopy(w.Completion.SSE.FailureEvents),
			RequiredJSONFields: sortedCopy(w.Completion.SSE.RequiredJSONFields),
		}
	}
	if w.Completion.Poll != nil {
		cw.Completion.Poll = &canonicalPoll{
			Interval:      w.Completion.Poll.Interval,
			MaxAttempts:   w.Completion.Poll.MaxAttempts,
			SuccessPath:   w.Completion.Poll.Success.Path,
			SuccessValue:  w.Completion.Poll.Success.Value,
			FailurePath:   w.Completion.Poll.Failure.Path,
			FailureValues: sortedCopy(w.Completion.Poll.Failure.Values),
		}
	}
	b, err := json.Marshal(cw)
	if err != nil {
		return "", fmt.Errorf("workflow: cannot be encoded: %w", err)
	}
	return string(b), nil
}

// CanaryConfig projects a validated workflow onto the flat monitor config: the canonical document in
// one string key, and one flat ref key per binding. Nothing else, and no project-secret name inside
// the document.
func CanaryConfig(w CanaryWorkflow) (map[string]string, error) {
	doc, err := CanaryCanonicalJSON(w)
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{CanaryWorkflowKey: doc}
	for binding, secret := range w.Secrets {
		cfg[CanarySecretRefKey(binding)] = strings.TrimSpace(secret)
	}
	return cfg, nil
}

// CanarySemanticHash is what the file provider compares to decide create / update / no-op. It covers
// the canonical document AND the flat refs, so the two halves cannot disagree about identity:
// pointing a binding at a DIFFERENT project secret moves it, and rotating that secret's VALUE does
// not. Renaming a secret referenced by a file-managed monitor is refused outright by the store, which
// is why the name being part of this hash cannot leave it stale (D3g).
func CanarySemanticHash(config map[string]string) string {
	h := sha256.New()
	h.Write([]byte(config[CanaryWorkflowKey]))
	for _, k := range CanarySecretRefKeys(config) {
		h.Write([]byte("\x00" + k + "\x00" + config[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ── Reading the persisted form back ────────────────────────────────────────────────────────────

// ParseCanaryConfig reconstructs a workflow from a stored monitor config: the canonical document plus
// the flat ref keys, which is where the binding → project-secret mapping lives. It is the inverse of
// CanaryConfig for every field that survives canonicalization, and it is what the executor, the API
// and the UI all read — none of them re-derives a rule from the document.
func ParseCanaryConfig(config map[string]string) (CanaryWorkflow, error) {
	raw := config[CanaryWorkflowKey]
	if strings.TrimSpace(raw) == "" {
		return CanaryWorkflow{}, fmt.Errorf("workflow: monitor carries no %q", CanaryWorkflowKey)
	}
	var cw canonicalWorkflow
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cw); err != nil {
		return CanaryWorkflow{}, fmt.Errorf("workflow: stored document does not parse: %w", err)
	}

	w := CanaryWorkflow{
		Kind:    cw.Kind,
		Secrets: map[string]string{},
		Submit: CanarySubmit{
			Kind:           cw.Submit.Kind,
			Method:         cw.Submit.Method,
			URL:            cw.Submit.URL,
			SubmitTimeout:  cw.Submit.SubmitTimeout,
			AcceptedStatus: cw.Submit.AcceptedStatus,
			Headers:        parseCanonicalHeaders(cw.Submit.Headers),
			FixtureRef:     cw.Submit.FixtureRef,
			Body:           parseCanonicalBodyMap(cw.Submit.Body),
		},
		Correlate: CanaryCorrelate{
			Source:     cw.Correlate.Source,
			Path:       cw.Correlate.Path,
			HeaderName: cw.Correlate.HeaderName,
		},
		Completion: CanaryCompletion{
			Kind:    cw.Completion.Kind,
			URL:     cw.Completion.URL,
			Timeout: cw.Completion.Timeout,
			Headers: parseCanonicalHeaders(cw.Completion.Headers),
		},
		Result: CanaryResult{
			MaxLatency:         cw.Result.MaxLatency,
			RequiredJSONFields: cw.Result.RequiredJSONFields,
			LifecyclePath:      cw.Result.LifecyclePath,
		},
		Cleanup: CanaryCleanup{
			Kind:         cw.Cleanup.Kind,
			Prefix:       cw.Cleanup.Prefix,
			Acknowledged: cw.Cleanup.Acknowledged,
		},
	}
	if cw.Submit.FileField != "" || len(cw.Submit.Fields) > 0 {
		w.Submit.Multipart = &CanaryMultipart{
			FileField: cw.Submit.FileField,
			Fields:    parseCanonicalBodyMap(cw.Submit.Fields),
		}
	}
	if cw.Completion.SSE != nil {
		w.Completion.SSE = &CanarySSE{
			SuccessEvent:       cw.Completion.SSE.SuccessEvent,
			FailureEvents:      cw.Completion.SSE.FailureEvents,
			RequiredJSONFields: cw.Completion.SSE.RequiredJSONFields,
		}
	}
	if cw.Completion.Poll != nil {
		w.Completion.Poll = &CanaryPoll{
			Interval:    cw.Completion.Poll.Interval,
			MaxAttempts: cw.Completion.Poll.MaxAttempts,
			Success:     CanaryPollMatch{Path: cw.Completion.Poll.SuccessPath, Value: cw.Completion.Poll.SuccessValue},
			Failure:     CanaryPollMatch{Path: cw.Completion.Poll.FailurePath, Values: cw.Completion.Poll.FailureValues},
		}
	}
	// The binding → project-secret mapping lives ONLY in the flat keys (D3f).
	for _, key := range CanarySecretRefKeys(config) {
		binding, _ := CanaryBindingFromRefKey(key)
		w.Secrets[binding] = config[key]
	}
	return w, nil
}

func parseCanonicalHeaders(hs []canonicalHeader) []CanaryHeader {
	if len(hs) == 0 {
		return nil
	}
	out := make([]CanaryHeader, 0, len(hs))
	for _, h := range hs {
		out = append(out, CanaryHeader{Name: h.Name, Value: h.Value, SecretRef: h.SecretRef})
	}
	return out
}

func parseCanonicalBodyMap(m map[string]any) map[string]CanaryValue {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]CanaryValue, len(m))
	for k, v := range m {
		out[k] = parseCanonicalBodyValue(v)
	}
	return out
}

func parseCanonicalBodyValue(v any) CanaryValue {
	switch t := v.(type) {
	case string:
		return CanaryValue{Kind: CanaryValueString, Str: t}
	case bool:
		return CanaryValue{Kind: CanaryValueBool, Bool: t}
	case json.Number:
		return CanaryValue{Kind: CanaryValueNumber, Num: t}
	case float64:
		return CanaryValue{Kind: CanaryValueNumber, Num: json.Number(trimFloat(t))}
	case []any:
		list := make([]CanaryValue, 0, len(t))
		for _, item := range t {
			list = append(list, parseCanonicalBodyValue(item))
		}
		return CanaryValue{Kind: CanaryValueList, List: list}
	case map[string]any:
		if ref, ok := t["secret_ref"].(string); ok && len(t) == 1 {
			return CanaryValue{Kind: CanaryValueSecret, SecretRef: ref}
		}
		obj := make(map[string]CanaryValue, len(t))
		for k, item := range t {
			obj[k] = parseCanonicalBodyValue(item)
		}
		return CanaryValue{Kind: CanaryValueObject, Obj: obj}
	default:
		return CanaryValue{}
	}
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%v", f)
	return s
}

// ── The fixture registry (D11) ─────────────────────────────────────────────────────────────────

// CanaryFixture is one entry of the closed registry a `multipart_fixture` submit may upload. A key,
// never a path, never a URL, never an inline blob: an operator cannot make the canary upload a file
// of their choosing, because that is an exfiltration primitive. Rotating a fixture is a RELEASE.
type CanaryFixture struct {
	Ref       string
	MediaType string
	FileName  string
	MaxBytes  int
	SHA256    string // filled when the asset is embedded (phase E); empty means "not yet carried"
}

// CanaryFixtureRegistryMax is the hard ceiling for any registry entry, checked when the asset is
// embedded as well as here, so a future fixture cannot quietly become a large upload.
const CanaryFixtureRegistryMax = 1 << 20 // 1 MiB

var canaryFixtures = map[string]CanaryFixture{
	"small_wav_v1": {
		Ref:       "small_wav_v1",
		MediaType: "audio/wav",
		FileName:  "small_wav_v1.wav",
		MaxBytes:  128 * 1024,
	},
}

// CanaryFixtureExists reports whether a key names a registry entry. Validation uses it so a bundle
// that names a fixture the binary does not carry is refused at the write boundary rather than at the
// first probe.
func CanaryFixtureExists(ref string) bool {
	_, ok := canaryFixtures[ref]
	return ok
}

// CanaryFixtureByRef returns the entry, for the executor and for the UI.
func CanaryFixtureByRef(ref string) (CanaryFixture, bool) {
	f, ok := canaryFixtures[ref]
	return f, ok
}

// CanaryFixtureRefs lists the registry, sorted — the UI shows these and nothing else.
func CanaryFixtureRefs() []string {
	out := make([]string, 0, len(canaryFixtures))
	for k := range canaryFixtures {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
