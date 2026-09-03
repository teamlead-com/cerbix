package fileprovider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// FR-029 in Monitoring-as-Code. The bundle carries the workflow as NESTED TYPED YAML — not a JSON
// string, and not an opaque `settings` map.
//
// That is the whole reason this monitor type exists as a type. `synthetic` cannot live in a bundle
// (FR-028 D9) because a flat `settings map[string]string` cannot carry a nested scenario, and
// admitting one as a JSON STRING would put an unvalidated document inside a validated bundle — the
// file provider would be guessing about its contents exactly the way D7 had to guess about a
// credential. A closed schema does not guess: the decoder refuses an unknown field, the domain
// refuses an invalid combination, and the secret guard walks every position.

// rawCanaryWorkflow and its children mirror the contract of func-async-canary.md §5.1. Their yaml
// tags ARE the contract's spelling; the decoder runs with KnownFields(true), so a field absent from
// these structs rejects the bundle rather than being ignored.
type rawCanaryWorkflow struct {
	Kind       string              `yaml:"kind"`
	Secrets    map[string]string   `yaml:"secrets"`
	Submit     rawCanarySubmit     `yaml:"submit"`
	Correlate  rawCanaryCorrelate  `yaml:"correlate"`
	Completion rawCanaryCompletion `yaml:"completion"`
	Result     rawCanaryResult     `yaml:"result"`
	Cleanup    rawCanaryCleanup    `yaml:"cleanup"`
}

type rawCanaryHeader struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	SecretRef string `yaml:"secret_ref"`
}

type rawCanaryMultipart struct {
	FileField string         `yaml:"file_field"`
	Fields    map[string]any `yaml:"fields"`
}

type rawCanarySubmit struct {
	Kind           string              `yaml:"kind"`
	Method         string              `yaml:"method"`
	URL            string              `yaml:"url"`
	SubmitTimeout  string              `yaml:"submit_timeout"`
	AcceptedStatus []int               `yaml:"accepted_status"`
	Headers        []rawCanaryHeader   `yaml:"headers"`
	FixtureRef     string              `yaml:"fixture_ref"`
	Multipart      *rawCanaryMultipart `yaml:"multipart"`
	Body           map[string]any      `yaml:"body"`
}

type rawCanaryCorrelate struct {
	Source     string `yaml:"source"`
	Path       string `yaml:"path"`
	HeaderName string `yaml:"header_name"`
}

type rawCanarySSE struct {
	SuccessEvent       string   `yaml:"success_event"`
	FailureEvents      []string `yaml:"failure_events"`
	RequiredJSONFields []string `yaml:"required_json_fields"`
}

type rawCanaryPollMatch struct {
	Path   string   `yaml:"path"`
	Value  string   `yaml:"value"`
	Values []string `yaml:"values"`
}

type rawCanaryPoll struct {
	Interval    string             `yaml:"interval"`
	MaxAttempts int                `yaml:"max_attempts"`
	Success     rawCanaryPollMatch `yaml:"success"`
	Failure     rawCanaryPollMatch `yaml:"failure"`
}

type rawCanaryCompletion struct {
	Kind    string            `yaml:"kind"`
	URL     string            `yaml:"url"`
	Timeout string            `yaml:"timeout"`
	Headers []rawCanaryHeader `yaml:"headers"`
	SSE     *rawCanarySSE     `yaml:"sse"`
	Poll    *rawCanaryPoll    `yaml:"poll_json"`
}

type rawCanaryResult struct {
	MaxLatency         string   `yaml:"max_latency"`
	RequiredJSONFields []string `yaml:"required_json_fields"`
	LifecyclePath      string   `yaml:"lifecycle_path"`
}

type rawCanaryCleanup struct {
	Kind         string `yaml:"kind"`
	Prefix       string `yaml:"prefix"`
	Acknowledged bool   `yaml:"acknowledged"`
}

// canaryWorkflowFromRaw converts the YAML shape into the domain contract. It does no validation of
// its own beyond what a conversion must decide (durations, the value algebra): the RULES live in
// `domain.ValidateCanaryWorkflow`, so a bundle and an API write are refused by one implementation
// and cannot drift into disagreeing about what is legal.
func canaryWorkflowFromRaw(uid string, raw *rawCanaryWorkflow) (domain.CanaryWorkflow, error) {
	if raw == nil {
		return domain.CanaryWorkflow{}, rejectf(ReasonDomainInvalid, uid, "an async_canary monitor needs a workflow block")
	}
	w := domain.CanaryWorkflow{
		Kind:    raw.Kind,
		Secrets: map[string]string{},
	}
	for k, v := range raw.Secrets {
		w.Secrets[k] = strings.TrimSpace(v)
	}

	submitTimeout, err := canaryDuration(uid, "workflow.submit.submit_timeout", raw.Submit.SubmitTimeout)
	if err != nil {
		return domain.CanaryWorkflow{}, err
	}
	body, err := canaryValueMap(uid, "workflow.submit.body", raw.Submit.Body)
	if err != nil {
		return domain.CanaryWorkflow{}, err
	}
	w.Submit = domain.CanarySubmit{
		Kind:           raw.Submit.Kind,
		Method:         strings.ToUpper(strings.TrimSpace(raw.Submit.Method)),
		URL:            strings.TrimSpace(raw.Submit.URL),
		SubmitTimeout:  submitTimeout,
		AcceptedStatus: raw.Submit.AcceptedStatus,
		Headers:        canaryHeaders(raw.Submit.Headers),
		FixtureRef:     strings.TrimSpace(raw.Submit.FixtureRef),
		Body:           body,
	}
	if raw.Submit.Multipart != nil {
		fields, err := canaryValueMap(uid, "workflow.submit.multipart.fields", raw.Submit.Multipart.Fields)
		if err != nil {
			return domain.CanaryWorkflow{}, err
		}
		w.Submit.Multipart = &domain.CanaryMultipart{
			FileField: strings.TrimSpace(raw.Submit.Multipart.FileField),
			Fields:    fields,
		}
	}

	w.Correlate = domain.CanaryCorrelate{
		Source:     raw.Correlate.Source,
		Path:       strings.TrimSpace(raw.Correlate.Path),
		HeaderName: strings.ToLower(strings.TrimSpace(raw.Correlate.HeaderName)),
	}

	completionTimeout, err := canaryDuration(uid, "workflow.completion.timeout", raw.Completion.Timeout)
	if err != nil {
		return domain.CanaryWorkflow{}, err
	}
	w.Completion = domain.CanaryCompletion{
		Kind:    raw.Completion.Kind,
		URL:     strings.TrimSpace(raw.Completion.URL),
		Timeout: completionTimeout,
		Headers: canaryHeaders(raw.Completion.Headers),
	}
	if raw.Completion.SSE != nil {
		w.Completion.SSE = &domain.CanarySSE{
			SuccessEvent:       strings.TrimSpace(raw.Completion.SSE.SuccessEvent),
			FailureEvents:      raw.Completion.SSE.FailureEvents,
			RequiredJSONFields: raw.Completion.SSE.RequiredJSONFields,
		}
	}
	if raw.Completion.Poll != nil {
		pollInterval, err := canaryDuration(uid, "workflow.completion.poll_json.interval", raw.Completion.Poll.Interval)
		if err != nil {
			return domain.CanaryWorkflow{}, err
		}
		w.Completion.Poll = &domain.CanaryPoll{
			Interval:    pollInterval,
			MaxAttempts: raw.Completion.Poll.MaxAttempts,
			Success: domain.CanaryPollMatch{
				Path:   strings.TrimSpace(raw.Completion.Poll.Success.Path),
				Value:  raw.Completion.Poll.Success.Value,
				Values: raw.Completion.Poll.Success.Values,
			},
			Failure: domain.CanaryPollMatch{
				Path:   strings.TrimSpace(raw.Completion.Poll.Failure.Path),
				Value:  raw.Completion.Poll.Failure.Value,
				Values: raw.Completion.Poll.Failure.Values,
			},
		}
	}

	maxLatency, err := canaryDuration(uid, "workflow.result.max_latency", raw.Result.MaxLatency)
	if err != nil {
		return domain.CanaryWorkflow{}, err
	}
	w.Result = domain.CanaryResult{
		MaxLatency:         maxLatency,
		RequiredJSONFields: raw.Result.RequiredJSONFields,
		LifecyclePath:      strings.TrimSpace(raw.Result.LifecyclePath),
	}
	w.Cleanup = domain.CanaryCleanup{
		Kind:         raw.Cleanup.Kind,
		Prefix:       raw.Cleanup.Prefix,
		Acknowledged: raw.Cleanup.Acknowledged,
	}
	return w, nil
}

func canaryHeaders(raw []rawCanaryHeader) []domain.CanaryHeader {
	if len(raw) == 0 {
		return nil
	}
	out := make([]domain.CanaryHeader, 0, len(raw))
	for _, h := range raw {
		out = append(out, domain.CanaryHeader{
			Name:      strings.ToLower(strings.TrimSpace(h.Name)),
			Value:     h.Value,
			SecretRef: strings.TrimSpace(h.SecretRef),
		})
	}
	return out
}

func canaryDuration(uid, pos, raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil // the domain refuses what must be present; absence is its decision, not this one
	}
	secs, err := domain.CanaryDurationSeconds(raw)
	if err != nil {
		return 0, rejectf(ReasonDomainInvalid, uid, "%s: %v", pos, err)
	}
	return secs, nil
}

// canaryValueMap converts a YAML body into the closed algebra. A shape the algebra does not have is
// `unsupported_field` with its POSITION, which is the reason a reader can act on — "the bundle is
// invalid" is not.
func canaryValueMap(uid, pos string, raw map[string]any) (map[string]domain.CanaryValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]domain.CanaryValue, len(raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, err := canaryValueFromAny(uid, pos+"."+k, raw[k], 1)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func canaryValueFromAny(uid, pos string, raw any, depth int) (domain.CanaryValue, error) {
	if depth > domain.CanaryMaxBodyDepth {
		return domain.CanaryValue{}, rejectf(ReasonDomainInvalid, uid, "%s: nested deeper than %d", pos, domain.CanaryMaxBodyDepth)
	}
	switch t := raw.(type) {
	case string:
		return domain.CanaryValue{Kind: domain.CanaryValueString, Str: t}, nil
	case bool:
		return domain.CanaryValue{Kind: domain.CanaryValueBool, Bool: t}, nil
	case int:
		return domain.CanaryValue{Kind: domain.CanaryValueNumber, Num: json.Number(fmt.Sprintf("%d", t))}, nil
	case int64:
		return domain.CanaryValue{Kind: domain.CanaryValueNumber, Num: json.Number(fmt.Sprintf("%d", t))}, nil
	case float64:
		return domain.CanaryValue{Kind: domain.CanaryValueNumber, Num: json.Number(fmt.Sprintf("%v", t))}, nil
	case []any:
		list := make([]domain.CanaryValue, 0, len(t))
		for i, item := range t {
			v, err := canaryValueFromAny(uid, fmt.Sprintf("%s[%d]", pos, i), item, depth+1)
			if err != nil {
				return domain.CanaryValue{}, err
			}
			list = append(list, v)
		}
		return domain.CanaryValue{Kind: domain.CanaryValueList, List: list}, nil
	case map[string]any:
		// The ONE map shape that is not an object: `{secret_ref: <binding>}` is how a credential
		// legitimately enters a body (D3a). It must be exactly that and nothing beside it, so a
		// document cannot smuggle a second meaning into the same node.
		if ref, ok := t["secret_ref"]; ok {
			name, isString := ref.(string)
			if !isString || len(t) != 1 {
				return domain.CanaryValue{}, rejectf(ReasonUnsupportedField, uid,
					"%s: a secret_ref node takes exactly one string binding name", pos)
			}
			return domain.CanaryValue{Kind: domain.CanaryValueSecret, SecretRef: strings.TrimSpace(name)}, nil
		}
		obj := make(map[string]domain.CanaryValue, len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, err := canaryValueFromAny(uid, pos+"."+k, t[k], depth+1)
			if err != nil {
				return domain.CanaryValue{}, err
			}
			obj[k] = v
		}
		return domain.CanaryValue{Kind: domain.CanaryValueObject, Obj: obj}, nil
	case nil:
		return domain.CanaryValue{}, rejectf(ReasonUnsupportedField, uid, "%s: null is not a value this contract carries", pos)
	default:
		return domain.CanaryValue{}, rejectf(ReasonUnsupportedField, uid, "%s: unsupported value shape", pos)
	}
}

// canaryInlineSecretGuard walks EVERY position of the workflow looking for a literal where the
// contract expects a reference (D13). The flat-map guard stops at the first level and would pass
// exactly the documents this type introduces.
//
// What it can decide, and what it deliberately does not claim: a header whose NAME is
// credential-bearing must carry a binding, and a body key that names a credential must not carry a
// literal. A credential pasted into an ordinary header or under an innocuous key is NOT detectable —
// the same residual as FR-028's D7, in the same words, because it is the same undecidable thing.
func canaryInlineSecretGuard(uid string, w domain.CanaryWorkflow) error {
	for _, stage := range []struct {
		name    string
		headers []domain.CanaryHeader
	}{{"submit", w.Submit.Headers}, {"completion", w.Completion.Headers}} {
		for _, h := range stage.headers {
			name := strings.ToLower(strings.TrimSpace(h.Name))
			if h.Value == "" {
				continue
			}
			if domain.CanaryCredentialHeaders[name] || secretSettingKeys[name] {
				return rejectf(ReasonInlineSecret, uid,
					"workflow.%s header %q carries a literal; a credential is a secret_ref", stage.name, name)
			}
		}
	}
	var walk func(pos string, v domain.CanaryValue) error
	walk = func(pos string, v domain.CanaryValue) error {
		switch v.Kind {
		case domain.CanaryValueObject:
			for k, item := range v.Obj {
				if secretSettingKeys[strings.ToLower(strings.TrimSpace(k))] && item.Kind != domain.CanaryValueSecret {
					return rejectf(ReasonInlineSecret, uid,
						"%s.%s carries a secret inline; use a secret_ref node", pos, k)
				}
				if err := walk(pos+"."+k, item); err != nil {
					return err
				}
			}
		case domain.CanaryValueList:
			for i, item := range v.List {
				if err := walk(fmt.Sprintf("%s[%d]", pos, i), item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, field := range []struct {
		pos  string
		vals map[string]domain.CanaryValue
	}{{"workflow.submit.body", w.Submit.Body}, {"workflow.submit.multipart.fields", canaryMultipartFields(w)}} {
		for k, v := range field.vals {
			if secretSettingKeys[strings.ToLower(strings.TrimSpace(k))] && v.Kind != domain.CanaryValueSecret {
				return rejectf(ReasonInlineSecret, uid, "%s.%s carries a secret inline; use a secret_ref node", field.pos, k)
			}
			if err := walk(field.pos+"."+k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func canaryMultipartFields(w domain.CanaryWorkflow) map[string]domain.CanaryValue {
	if w.Submit.Multipart == nil {
		return nil
	}
	return w.Submit.Multipart.Fields
}

// buildCanaryMonitor is the async_canary branch of `buildMonitor`. It mirrors that function's shape
// deliberately — the same defaults, the same duration handling, the same "domain validates, the
// provider never re-implements a rule" contract — and adds exactly two things the nested schema
// needs: the workflow conversion and the recursive secret guard.
func buildCanaryMonitor(uid string, rm rawMonitor) (DesiredMonitor, error) {
	w, err := canaryWorkflowFromRaw(uid, rm.Workflow)
	if err != nil {
		return DesiredMonitor{}, err
	}
	// Before anything else: a bundle never carries a secret, and for this type the guard has to walk
	// every position rather than a flat map (D13).
	if err := canaryInlineSecretGuard(uid, w); err != nil {
		return DesiredMonitor{}, err
	}

	config, err := domain.CanaryConfig(w)
	if err != nil {
		return DesiredMonitor{}, rejectf(ReasonDomainInvalid, uid, "%s", err.Error())
	}

	m := domain.Monitor{
		Name:         strings.TrimSpace(rm.Name),
		Type:         domain.MonitorAsyncCanary,
		Conditions:   rm.Conditions,
		Tags:         rm.Tags,
		Region:       orDefault(rm.Region, fmtDefaultRegion),
		Enabled:      boolOr(rm.Enabled, true),
		AutoIncident: boolOr(rm.AutoIncident, true),
		DependsOn:    normStringSet(rm.DependsOn),
		Config:       config,
	}
	if m.Name == "" {
		return DesiredMonitor{}, rejectf(ReasonDomainInvalid, uid, "monitor `name` is required")
	}
	if strings.TrimSpace(rm.Target) != "" {
		// A canary's target is its workflow. Accepting a target here would let a bundle carry a
		// field nothing reads, which is how a document starts lying about what it configures.
		return DesiredMonitor{}, rejectf(ReasonUnsupportedField, uid, "an async_canary has no `target`; its workflow names its URLs")
	}

	var derr error
	if m.IntervalSeconds, derr = durSeconds(uid, "interval", rm.Interval, fmtDefaultIntervalSeconds); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.TimeoutSeconds, derr = durSeconds(uid, "timeout", rm.Timeout, fmtDefaultTimeoutSeconds); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.ConfirmIntervalSeconds, derr = durSeconds(uid, "confirm_interval", rm.ConfirmInterval, 0); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.RenotifySeconds, derr = durSeconds(uid, "renotify", rm.Renotify, 0); derr != nil {
		return DesiredMonitor{}, derr
	}
	if m.GraceSeconds, derr = durSeconds(uid, "grace", rm.Grace, 0); derr != nil {
		return DesiredMonitor{}, derr
	}
	m.Retries = intOr(rm.Retries, fmtDefaultRetries)
	m.FailureThreshold = intOr(rm.FailureThreshold, fmtDefaultFailureThreshold)

	m.Normalize()
	vm := m
	vm.ProjectID = "file-validate"
	if err := vm.Validate(); err != nil {
		return DesiredMonitor{}, rejectf(ReasonDomainInvalid, uid, "%s", err.Error())
	}
	return DesiredMonitor{UID: uid, Monitor: m, DependsOn: m.DependsOn, Hash: canonicalHash(uid, m)}, nil
}
