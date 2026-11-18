package telephony

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The two body content types of generic_http (§4.1); each fixes how placeholders are encoded (§4.4).
const (
	ContentTypeJSON = "application/json"
	ContentTypeForm = "application/x-www-form-urlencoded"
)

// The placeholders a body template may use (D3), and nothing else.
const (
	placeholderTo   = "{{to}}"
	placeholderFrom = "{{from}}"
	placeholderText = "{{text}}"
)

var placeholders = []string{placeholderTo, placeholderFrom, placeholderText}

// GenericHTTPConfig is the instance's generic_http block (§4.1). Headers may carry credentials; they
// are sent and never echoed anywhere.
type GenericHTTPConfig struct {
	Method            string // POST | PUT
	URL               string
	Headers           map[string]string
	ContentType       string // ContentTypeJSON | ContentTypeForm
	BodyTemplate      string
	From              string
	ResponseCodeField string // optional top-level JSON key holding the gateway's error code (D34)
}

// GenericHTTP is an operator-defined SMS gateway request (D3). It cannot place calls.
type GenericHTTP struct {
	cfg    GenericHTTPConfig
	client *http.Client
}

// ErrTemplate is the class of every template refusal; the wrapped text says what is wrong and quotes
// no part of the template. The template is stored and shown in plain text, so it must not carry a
// credential — that belongs in a header, which is encrypted at rest (D29, R1) — but one put there
// by mistake is still not repeated by an error.
var ErrTemplate = errors.New("body template refused")

func templateErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrTemplate}, args...)...)
}

// ValidateTemplate checks a body template for its content type (§4.4, D3):
//   - only {{to}}, {{from}} and {{text}} are placeholders, and any other `{{` is refused;
//   - {{to}} and {{text}} both appear (the destination and the message have nowhere else to go);
//   - JSON: every placeholder sits inside a string VALUE — never in a key, never bare — and the
//     template with the placeholders replaced by a plain word is one valid JSON document;
//   - form: `name=value` pairs joined by `&`, every name non-empty and free of placeholders, and the
//     template with the placeholders replaced by a plain word parses as a form.
func ValidateTemplate(contentType, tmpl string) error {
	replaced, n, seen, err := replacePlaceholders(tmpl, templateWord)
	if err != nil {
		return err
	}
	if !seen[placeholderTo] || !seen[placeholderText] {
		return templateErr("it must contain %s and %s", placeholderTo, placeholderText)
	}
	switch contentType {
	case ContentTypeJSON:
		return validateJSONTemplate(replaced, n)
	case ContentTypeForm:
		return validateFormTemplate(replaced)
	default:
		return templateErr("unsupported content type")
	}
}

// templateWord stands in for a placeholder while a template is validated: a plain word, valid inside
// a JSON string and a form value, and invalid JSON when bare. It starts with a letter that is not a
// hex digit, so a `%` or `%c` written before a placeholder stays an incomplete escape and is refused:
// what is sent there is the value escaped (`%2B…`), and `%%2B…` is no form.
const templateWord = "xcerbixplaceholderword"

// replacePlaceholders replaces every placeholder by word and refuses any `{{` that does not start one.
func replacePlaceholders(tmpl, word string) (string, int, map[string]bool, error) {
	var b strings.Builder
	seen := map[string]bool{}
	n := 0
	rest := tmpl
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), n, seen, nil
		}
		b.WriteString(rest[:i])
		rest = rest[i:]
		matched := ""
		for _, p := range placeholders {
			if strings.HasPrefix(rest, p) {
				matched = p
				break
			}
		}
		if matched == "" {
			return "", 0, nil, templateErr("unknown placeholder; only %s, %s and %s are allowed",
				placeholderTo, placeholderFrom, placeholderText)
		}
		seen[matched] = true
		n++
		b.WriteString(word)
		rest = rest[len(matched):]
	}
}

// validateJSONTemplate walks the document's tokens: a placeholder found in a key is refused, and the
// placeholders found in string values must be all n of them (one outside a string would have made the
// document invalid; one produced by an escape sequence would be counted twice).
func validateJSONTemplate(doc string, n int) error {
	if !json.Valid([]byte(doc)) {
		return templateErr("not valid JSON with every placeholder inside a string value")
	}
	type frame struct{ object, wantKey bool }
	var stack []frame
	startValue := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].wantKey = true
		}
	}
	inValues := 0
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return templateErr("not valid JSON")
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				startValue()
				stack = append(stack, frame{object: true, wantKey: true})
			case '[':
				startValue()
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].wantKey {
				if strings.Contains(v, templateWord) {
					return templateErr("a placeholder may not be part of a key")
				}
				stack[len(stack)-1].wantKey = false
				continue
			}
			startValue()
			inValues += strings.Count(v, templateWord)
		default:
			startValue()
		}
	}
	if inValues != n {
		return templateErr("every placeholder must sit inside a JSON string value")
	}
	return nil
}

func validateFormTemplate(body string) error {
	if body == "" {
		return templateErr("empty form")
	}
	for _, pair := range strings.Split(body, "&") {
		name, _, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return templateErr("every part must be name=value")
		}
		if strings.Contains(name, templateWord) {
			return templateErr("a placeholder may only be a value, never a name")
		}
	}
	if _, err := url.ParseQuery(body); err != nil {
		return templateErr("not a valid form")
	}
	return nil
}

// NewGenericHTTP validates the configuration and builds the provider. client is the notification
// egress guard's client in production; nil gets a fallback that refuses redirects the same way.
// No error quotes the URL, a header or the template. Only the headers are encrypted at rest (D29,
// R1); the URL and the template are stored and shown in plain text and must not carry a credential.
// An error quotes none of the three all the same, so a credential put in one by mistake goes no
// further.
func NewGenericHTTP(cfg GenericHTTPConfig, client *http.Client) (*GenericHTTP, error) {
	if cfg.Method != http.MethodPost && cfg.Method != http.MethodPut {
		return nil, errors.New("generic_http: method must be POST or PUT")
	}
	if cfg.ContentType != ContentTypeJSON && cfg.ContentType != ContentTypeForm {
		return nil, errors.New("generic_http: content type must be " + ContentTypeJSON + " or " + ContentTypeForm)
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("generic_http: invalid URL")
	}
	for k, v := range cfg.Headers {
		if !validHeaderName(k) || strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("generic_http: invalid header")
		}
	}
	if err := ValidateTemplate(cfg.ContentType, cfg.BodyTemplate); err != nil {
		return nil, fmt.Errorf("generic_http: %w", err)
	}
	return &GenericHTTP{cfg: cfg, client: defaultClient(client)}, nil
}

// validHeaderName is RFC 9110's token.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func (g *GenericHTTP) Name() string        { return "generic_http" }
func (g *GenericHTTP) SupportsVoice() bool { return false }

// PlaceCall is refused without a request: generic_http is SMS only (D3).
func (g *GenericHTTP) PlaceCall(context.Context, string, string) Result {
	return Result{Outcome: Rejected, Code: CodeVoiceUnsupported}
}

// SendSMS renders the body — each placeholder substituted as a VALUE in the content type, in one
// pass — and sends it. 2xx is success (§4.5); a failure reported inside a 2xx is not supported.
func (g *GenericHTTP) SendSMS(ctx context.Context, to, text string) Result {
	req, err := http.NewRequestWithContext(ctx, g.cfg.Method, g.cfg.URL,
		strings.NewReader(g.render(to, text)))
	if err != nil {
		return Result{Outcome: Rejected}
	}
	for k, v := range g.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", g.cfg.ContentType)

	res, body := do(g.client, req)
	if res.HTTPStatus != 0 && res.Outcome != Sent && g.cfg.ResponseCodeField != "" {
		res.Code = responseCode(body, g.cfg.ResponseCodeField)
	}
	return res
}

func (g *GenericHTTP) render(to, text string) string {
	esc := url.QueryEscape
	if g.cfg.ContentType == ContentTypeJSON {
		esc = jsonStringContent
	}
	// strings.Replacer substitutes in one pass: a value that spells a placeholder stays literal.
	return strings.NewReplacer(
		placeholderTo, esc(to),
		placeholderFrom, esc(g.cfg.From),
		placeholderText, esc(text),
	).Replace(g.cfg.BodyTemplate)
}

// jsonStringContent is s escaped as the inside of a JSON string literal, without the quotes.
func jsonStringContent(s string) string {
	b, _ := json.Marshal(s) // a Go string always marshals
	return string(b[1 : len(b)-1])
}

// responseCode reads one top-level key of a JSON object answer, a string or a number kept as written,
// and keeps it only when it matches the D34 pattern.
func responseCode(body []byte, field string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	raw := bytes.TrimSpace(obj[field])
	if len(raw) == 0 {
		return ""
	}
	var s string
	switch {
	case raw[0] == '"':
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		s = n.String()
	default:
		return ""
	}
	if validCode(s) {
		return s
	}
	return ""
}
