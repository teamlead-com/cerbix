package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/teamlead-com/cerbix/internal/deliveryfail"
)

// FR-041a R7: the machine `reason` of the phone routes' errors is what the SPA branches on, so the
// generated TS types must carry it wherever the server sends it — and must not promise it where the
// server does not. A plain 400 of the same route (an unreadable body from decodeJSONBody, a missing
// field) sends `{"error"}` alone, so `reason` is optional on ReasonError; its values are the closed
// set the handlers send. The telephony save's 409 is one of two bodies told apart by `reason`.

// phoneReasonResponses are the responses that carry a `reason` on some answer of the handler (the
// writeErrorReason calls of handlers_phone.go, handlers_phonechannels.go and handlers_telephony.go).
var phoneReasonResponses = []struct{ method, path, status string }{
	{"put", "/api/v1/me/phone", "400"},
	{"put", "/api/v1/me/phone", "409"},
	{"put", "/api/v1/me/phone", "429"},
	{"put", "/api/v1/me/phone", "502"},
	{"post", "/api/v1/me/phone/verify", "400"},
	{"post", "/api/v1/me/phone/verify", "410"},
	{"post", "/api/v1/me/phone/verify/send", "400"},
	{"post", "/api/v1/me/phone/verify/send", "409"},
	{"post", "/api/v1/me/phone/verify/send", "429"},
	{"post", "/api/v1/me/phone/verify/send", "502"},
	{"post", "/api/v1/projects/{projectID}/notification-channels", "400"},
	{"post", "/api/v1/projects/{projectID}/notification-channels", "409"},
	{"patch", "/api/v1/notification-channels/{channelID}", "400"},
	{"patch", "/api/v1/notification-channels/{channelID}", "409"},
	{"post", "/api/v1/notification-channels/{channelID}/verify/send", "400"},
	{"post", "/api/v1/notification-channels/{channelID}/verify/send", "409"},
	{"post", "/api/v1/notification-channels/{channelID}/verify/send", "429"},
	{"post", "/api/v1/notification-channels/{channelID}/verify/send", "502"},
	{"post", "/api/v1/notification-channels/{channelID}/verify", "400"},
	{"post", "/api/v1/notification-channels/{channelID}/verify", "409"},
	{"post", "/api/v1/notification-channels/{channelID}/verify", "410"},
	{"put", "/api/v1/settings/telephony", "400"},
	{"put", "/api/v1/settings/telephony", "409"},
	{"post", "/api/v1/settings/telephony/test-sms", "409"},
}

type openAPINode = map[string]any

func readOpenAPINodes(t *testing.T) openAPINode {
	t.Helper()
	data, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc openAPINode
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

func node(v any, keys ...string) openAPINode {
	for _, k := range keys {
		m, _ := v.(openAPINode)
		if m == nil {
			return nil
		}
		v = m[k]
	}
	m, _ := v.(openAPINode)
	return m
}

// deref follows a local $ref ("#/components/…") to its node, and returns the ref's last segment.
func deref(doc openAPINode, n openAPINode) (openAPINode, string) {
	ref, _ := n["$ref"].(string)
	if ref == "" {
		return n, ""
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	return node(doc, parts...), parts[len(parts)-1]
}

func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// sentReasons is every `reason…` string constant of the package: the codes writeErrorReason sends,
// and services_without_route, which the telephony save's 409 sends with its list.
func sentReasons(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "reason") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						out = append(out, v)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestReasonErrorIsHonestForEveryPhoneError(t *testing.T) {
	doc := readOpenAPINodes(t)
	reasonError := node(doc, "components", "schemas", "ReasonError")
	if reasonError == nil {
		t.Fatal("components.schemas.ReasonError is missing")
	}
	for _, r := range stringsOf(reasonError["required"]) {
		if r == "reason" {
			t.Error("ReasonError requires `reason`, but a plain 400 of the same routes (decodeJSONBody) sends only `error`")
		}
	}
	var want []string
	for _, r := range sentReasons(t) {
		if r != reasonServicesWithoutRoute {
			want = append(want, r)
		}
	}
	if got := stringsOf(node(reasonError, "properties", "reason")["enum"]); !reflect.DeepEqual(got, want) {
		t.Errorf("ReasonError.reason enum = %v; want the reasons the handlers send, %v", got, want)
	}

	for _, r := range phoneReasonResponses {
		resp := node(doc, "paths", r.path, r.method, "responses", r.status)
		if resp == nil {
			t.Errorf("%s %s: no %s response", strings.ToUpper(r.method), r.path, r.status)
			continue
		}
		resp, _ = deref(doc, resp)
		schema := node(resp, "content", "application/json", "schema")
		if r.path == "/api/v1/settings/telephony" && r.status == "409" {
			continue // TestTelephonySaveConflictIsTellableByReason
		}
		if _, name := deref(doc, schema); name != "ReasonError" {
			t.Errorf("%s %s %s sends a `reason` and is documented as %q; want ReasonError",
				strings.ToUpper(r.method), r.path, r.status, name)
		}
	}
}

// The 409 of a telephony save is either `encryption_key_missing` or `services_without_route` with its
// list. A `oneOf` over two schemas a single body can both match is no union at all; the generated type
// must narrow on `reason`.
func TestTelephonySaveConflictIsTellableByReason(t *testing.T) {
	doc := readOpenAPINodes(t)
	resp, _ := deref(doc, node(doc, "paths", "/api/v1/settings/telephony", "put", "responses", "409"))
	schema := node(resp, "content", "application/json", "schema")
	if node(schema, "discriminator") == nil || node(schema, "discriminator")["propertyName"] != "reason" {
		t.Fatalf("the 409 schema has no discriminator on `reason`: %v", schema)
	}
	mapping := node(schema, "discriminator", "mapping")
	var keys []string
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{reasonEncryptionKeyMissing, reasonServicesWithoutRoute}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("discriminator mapping %v; want %v", keys, want)
	}
	branches, _ := schema["oneOf"].([]any)
	if len(branches) != len(keys) {
		t.Fatalf("oneOf has %d branches; want one per reason", len(branches))
	}
	for _, reason := range keys {
		target, _ := mapping[reason].(string)
		branch, name := deref(doc, openAPINode{"$ref": target})
		if branch == nil {
			t.Fatalf("mapping %s → %s resolves to nothing", reason, target)
		}
		required := stringsOf(branch["required"])
		if i := sort.SearchStrings(required, "reason"); i == len(required) || required[i] != "reason" {
			t.Errorf("%s does not require `reason`", name)
		}
		if got := stringsOf(node(branch, "properties", "reason")["enum"]); !reflect.DeepEqual(got, []string{reason}) {
			t.Errorf("%s.reason enum = %v; want exactly [%s]", name, got, reason)
		}
	}
}

// FR-041a review P3-3: the test SMS's `transport_class` is documented as exactly deliveryfail's closed
// set, so the generated TS type is the set the server can send.
func TestTelephonyTestSMSTransportClassEnumIsTheClassifierSet(t *testing.T) {
	doc := readOpenAPINodes(t)
	prop := node(doc, "components", "schemas", "TelephonyTestSMSResult", "properties", "transport_class")
	if prop == nil {
		t.Fatal("TelephonyTestSMSResult has no transport_class")
	}
	raw, _ := prop["enum"].([]any)
	var got []string
	for _, v := range raw {
		s, _ := v.(string)
		got = append(got, s)
	}
	want := deliveryfail.Classes()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transport_class enum = %q, want deliveryfail.Classes() %q", got, want)
	}
	if req, _ := node(doc, "components", "schemas", "TelephonyTestSMSResult")["required"].([]any); len(req) > 0 {
		for _, r := range req {
			if r == "transport_class" {
				t.Fatal("transport_class is required; it is absent when the provider answered")
			}
		}
	}
}
