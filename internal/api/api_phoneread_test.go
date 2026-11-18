package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-041a D21, §4.5, §9 — the read side: `ref` on the incident views, the by-ref resolver, an
// incident's phone activity and a channel's deliveries. The fake models the store's contract; the
// store's own tests prove it against PostgreSQL.

type fakePhoneReads struct {
	refs       map[string]string
	deliveries []domain.PhoneDeliveryView // each carries its channel in ChannelID; incident in the map below
	incidentOf map[string]string          // delivery id -> incident id
}

func (f *fakeStore) phoneReads() *fakePhoneReads {
	if f.pr == nil {
		f.pr = &fakePhoneReads{refs: map[string]string{}, incidentOf: map[string]string{}}
	}
	return f.pr
}

func (f *fakeStore) IncidentRef(_ context.Context, id string) (string, error) {
	ref, ok := f.phoneReads().refs[id]
	if !ok {
		return "", store.ErrNotFound
	}
	return ref, nil
}

func (f *fakeStore) IncidentRefs(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if ref, ok := f.phoneReads().refs[id]; ok {
			out[id] = ref
		}
	}
	return out, nil
}

func (f *fakeStore) IncidentByRef(_ context.Context, ref string) (domain.Incident, error) {
	norm, ok := domain.NormalizeIncidentRef(ref)
	if !ok {
		return domain.Incident{}, store.ErrNotFound
	}
	for id, r := range f.phoneReads().refs {
		if r == norm {
			return f.GetIncident(context.Background(), id)
		}
	}
	return domain.Incident{}, store.ErrNotFound
}

func (f *fakeStore) ListIncidentPhoneDeliveries(_ context.Context, incidentID string) ([]domain.PhoneDeliveryView, error) {
	out := []domain.PhoneDeliveryView{}
	for _, d := range f.phoneReads().deliveries {
		if f.phoneReads().incidentOf[d.ID] == incidentID {
			d.IncidentID, d.IncidentRef = "", ""
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeStore) ListChannelPhoneDeliveries(_ context.Context, channelID string, limit int) ([]domain.PhoneDeliveryView, error) {
	out := []domain.PhoneDeliveryView{}
	for _, d := range f.phoneReads().deliveries {
		if d.ChannelID != channelID || len(out) == limit {
			continue
		}
		if inc := f.phoneReads().incidentOf[d.ID]; inc != "" {
			d.IncidentID, d.IncidentRef = inc, f.phoneReads().refs[inc]
		}
		out = append(out, d)
	}
	return out, nil
}

type httpResult struct {
	Code int
	Body string
}

func doRes(h http.Handler, p authz.Principal, path string) *httpResult {
	rec := do(h, p, http.MethodGet, path, "")
	return &httpResult{rec.Code, rec.Body.String()}
}

const (
	prRef1  = "X7K2P9QM"
	prRef3  = "A1B2C3D4"
	prNum   = "+905551236869"
	prMask  = "+90•••••6869"
	prNumRe = "905551236869"
)

func phoneReadStore() *fakeStore {
	fs := seededStore()
	r := fs.phoneReads()
	r.refs["inc1"], r.refs["inc3"] = prRef1, prRef3
	fs.channels["nc-sms"] = domain.NotificationChannel{ID: "nc-sms", ProjectID: "p1", Type: domain.ChannelSMS, Name: "duty sms",
		Recipient: domain.PhoneRecipientNumber, NumberMasked: prMask, Enabled: true}
	masked := prMask
	code := "ok"
	r.deliveries = []domain.PhoneDeliveryView{
		{ID: "d1", ChannelID: "nc-sms", ChannelName: "duty sms", Kind: "sms", AlertKind: "down", NumberMasked: &masked,
			Reason: "sent", Notes: []string{}, ProviderCode: &code, Segments: 1, Attempts: 1},
		{ID: "d2", ChannelID: "nc-sms", ChannelName: "duty sms", Kind: "sms", AlertKind: "down",
			Reason: "not_deliverable", Notes: []string{"no_incident_ref"}, Attempts: 1},
		{ID: "d3", ChannelID: "nc3", ChannelName: "globex", Kind: "sms", AlertKind: "down", Reason: "sent", Notes: []string{}, Attempts: 1},
	}
	r.incidentOf["d1"] = "inc1"
	r.incidentOf["d3"] = "inc3"
	return fs
}

// noNumber fails when a response carries a plaintext number, in any spelling the fixtures use.
func noNumber(t *testing.T, label, body string) {
	t.Helper()
	if strings.Contains(body, prNumRe) || strings.Contains(body, "number_hash") || strings.Contains(body, "hash-") {
		t.Errorf("%s carries a number: %s", label, body)
	}
}

func TestIncidentViewsCarryTheRef(t *testing.T) {
	h := newHandler(phoneReadStore())
	rec := do(h, o1Viewer, http.MethodGet, "/api/v1/incidents/inc1", "")
	var det struct {
		ID  string `json:"id"`
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil || rec.Code != 200 || det.ID != "inc1" || det.Ref != prRef1 {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, o1Viewer, http.MethodGet, "/api/v1/projects/p1/incidents", "")
	var list []struct {
		ID  string `json:"id"`
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != 200 || len(list) != 1 || list[0].Ref != prRef1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	// The list keeps its existing keys next to the new one.
	var raw []map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"id", "project_id", "title", "status", "impact", "source", "ref"} {
		if _, ok := raw[0][k]; !ok {
			t.Errorf("list item lost key %q: %s", k, rec.Body.String())
		}
	}
}

func TestIncidentByRef(t *testing.T) {
	fs := phoneReadStore()
	h := newHandler(fs)
	get := func(p authz.Principal, ref string) *httpResult { return doRes(h, p, "/api/v1/incidents/by-ref/"+ref) }

	res := get(o1Viewer, prRef1)
	if res.Code != 200 || res.Body != `{"incident_id":"inc1","project_id":"p1","org_id":"o1"}`+"\n" {
		t.Fatalf("resolve = %d %q", res.Code, res.Body)
	}
	// A link typed by hand resolves to the same incident.
	if res := get(o1Viewer, strings.ToLower(prRef1)); res.Code != 200 {
		t.Fatalf("lower case = %d", res.Code)
	}
	// 404 for malformed, unknown, and invisible — the same answer for all three.
	var bodies []string
	for name, res := range map[string]*httpResult{
		"malformed":   get(o1Viewer, "not-a-ref"),
		"non ascii":   get(o1Viewer, "X7K2P9Q%C3%A9"),
		"unknown":     get(o1Viewer, "ZZZZZZZZ"),
		"invisible":   get(outsider, prRef1),
		"other org":   get(p1Viewer, prRef3),
		"anonymous":   get(authz.Principal{}, prRef1),
		"short":       get(o1Viewer, "X7K2"),
		"all aliases": get(o1Viewer, "UUUUUUUU"),
	} {
		if res.Code != http.StatusNotFound {
			t.Errorf("%s = %d %q, want 404", name, res.Code, res.Body)
		}
		bodies = append(bodies, res.Body)
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("404 bodies differ, which tells a probe which case it hit: %q vs %q", b, bodies[0])
		}
	}
	// 403 only AFTER the 404 step: a principal who sees the project but lacks project:read.
	noRead := authz.Principal{UserID: "tok", ViaToken: true, Actions: []authz.Action{authz.ActionProjectWrite},
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}
	if res := get(noRead, prRef1); res.Code != http.StatusForbidden {
		t.Errorf("visible without project:read = %d, want 403", res.Code)
	}
	if res := get(noRead, "ZZZZZZZZ"); res.Code != http.StatusNotFound {
		t.Errorf("unknown ref for the same principal = %d, want 404 (404 before 403)", res.Code)
	}
	if res := get(noRead, prRef3); res.Code != http.StatusNotFound {
		t.Errorf("another project's ref for the same principal = %d, want 404", res.Code)
	}
}

func TestIncidentPhoneActivity(t *testing.T) {
	h := newHandler(phoneReadStore())
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/incidents/inc1/phone-activity", "")
	if rec.Code != 200 {
		t.Fatalf("phone-activity = %d %s", rec.Code, rec.Body.String())
	}
	noNumber(t, "phone-activity", rec.Body.String())
	var body map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body) != 1 {
		t.Fatalf("body keys = %v, want deliveries only (R13)", body)
	}
	var d []map[string]any
	_ = json.Unmarshal(body["deliveries"], &d)
	if len(d) != 1 || d[0]["id"] != "d1" || d[0]["number_masked"] != prMask || d[0]["channel_name"] != "duty sms" {
		t.Fatalf("deliveries = %s", body["deliveries"])
	}
	for _, k := range []string{"id", "channel_id", "channel_name", "kind", "alert_kind", "number_masked", "reason", "notes",
		"provider_status", "provider_code", "provider_ref", "segments", "attempts", "created_at", "updated_at"} {
		if _, ok := d[0][k]; !ok {
			t.Errorf("delivery lacks %q", k)
		}
	}
	if _, ok := d[0]["incident_id"]; ok {
		t.Errorf("the incident's own list repeats the incident")
	}
	if rec := do(h, o1Admin, http.MethodGet, "/api/v1/incidents/inc3/phone-activity", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another organization's incident = %d, want 404", rec.Code)
	}
	// An incident with none is an empty array, never null.
	fs := phoneReadStore()
	fs.phoneReads().incidentOf = map[string]string{}
	rec = do(newHandler(fs), o1Viewer, http.MethodGet, "/api/v1/incidents/inc1/phone-activity", "")
	if rec.Body.String() != `{"deliveries":[]}`+"\n" {
		t.Fatalf("empty = %q", rec.Body.String())
	}
	// Access: hidden 404, no project:read 403, unknown 404.
	if rec := do(newHandler(phoneReadStore()), outsider, http.MethodGet, "/api/v1/incidents/inc1/phone-activity", ""); rec.Code != 404 {
		t.Errorf("outsider = %d, want 404", rec.Code)
	}
	noRead := authz.Principal{UserID: "tok", ViaToken: true, Actions: []authz.Action{authz.ActionProjectWrite},
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}
	if rec := do(newHandler(phoneReadStore()), noRead, http.MethodGet, "/api/v1/incidents/inc1/phone-activity", ""); rec.Code != 403 {
		t.Errorf("no project:read = %d, want 403", rec.Code)
	}
	if rec := do(newHandler(phoneReadStore()), o1Viewer, http.MethodGet, "/api/v1/incidents/nope/phone-activity", ""); rec.Code != 404 {
		t.Errorf("unknown = %d, want 404", rec.Code)
	}
}

func TestChannelPhoneDeliveries(t *testing.T) {
	fs := phoneReadStore()
	h := newHandler(fs)
	rec := do(h, p1Viewer, http.MethodGet, "/api/v1/notification-channels/nc-sms/phone-deliveries", "")
	if rec.Code != 200 {
		t.Fatalf("phone-deliveries = %d %s", rec.Code, rec.Body.String())
	}
	noNumber(t, "phone-deliveries", rec.Body.String())
	var body struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Deliveries) != 2 || body.Deliveries[0]["incident_id"] != "inc1" || body.Deliveries[0]["incident_ref"] != prRef1 {
		t.Fatalf("deliveries = %s", rec.Body.String())
	}
	if _, ok := body.Deliveries[1]["incident_id"]; ok {
		t.Errorf("a delivery with no incident names one: %v", body.Deliveries[1])
	}
	// The last 50 only.
	for i := 0; i < 70; i++ {
		fs.phoneReads().deliveries = append(fs.phoneReads().deliveries, domain.PhoneDeliveryView{
			ID: "x" + string(rune('a'+i%26)) + string(rune('a'+i/26)), ChannelID: "nc-sms", Kind: "sms", Reason: "sent", Notes: []string{}})
	}
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/notification-channels/nc-sms/phone-deliveries", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Deliveries) != 50 {
		t.Fatalf("got %d deliveries, want 50", len(body.Deliveries))
	}
	// Access.
	if rec := do(h, outsider, http.MethodGet, "/api/v1/notification-channels/nc-sms/phone-deliveries", ""); rec.Code != 404 {
		t.Errorf("outsider = %d, want 404", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/notification-channels/nc3/phone-deliveries", ""); rec.Code != 404 {
		t.Errorf("another org's channel = %d, want 404", rec.Code)
	}
	noRead := authz.Principal{UserID: "tok", ViaToken: true, Actions: []authz.Action{authz.ActionProjectWrite},
		Memberships: []domain.Membership{{OrgID: "o1", ProjectID: "p1", Role: domain.RoleEditor}}}
	if rec := do(h, noRead, http.MethodGet, "/api/v1/notification-channels/nc-sms/phone-deliveries", ""); rec.Code != 403 {
		t.Errorf("no project:read = %d, want 403", rec.Code)
	}
	if rec := do(h, p1Viewer, http.MethodGet, "/api/v1/notification-channels/nope/phone-deliveries", ""); rec.Code != 404 {
		t.Errorf("unknown = %d, want 404", rec.Code)
	}
	// A channel of one of the four existing types has no phone deliveries: an empty list.
	rec = do(h, p1Viewer, http.MethodGet, "/api/v1/notification-channels/nc1/phone-deliveries", "")
	if rec.Code != 200 || rec.Body.String() != `{"deliveries":[]}`+"\n" {
		t.Errorf("webhook channel = %d %q", rec.Code, rec.Body.String())
	}
}

// The public incident JSON and the feeds are what they were: a ref in the store changes nothing.
func TestPublicIncidentJSONAndFeedsIgnoreTheRef(t *testing.T) {
	type snap struct{ history, render, feed string }
	at := time.Now().Add(-2 * time.Hour)
	take := func(fs *fakeStore) snap {
		pastIncident(fs, "hx", at)
		h := newPublicHandler(fs)
		clock := regexp.MustCompile(`"(to|history_from)":"[^"]*"|<lastBuildDate>[^<]*</lastBuildDate>`)
		get := func(p string) string {
			rec := do(h, outsider, http.MethodGet, p, "")
			return clock.ReplaceAllString(rec.Body.String(), "")
		}
		return snap{get("/api/v1/public/status-pages/acme-status/history"), get("/api/v1/public/status-pages/acme-status"),
			get("/api/v1/public/status-pages/acme-status/feed")}
	}
	plain := take(seededStore())
	withRefs := phoneReadStore()
	withRefs.phoneReads().refs["hx"] = "HX7K2P9Q"
	got := take(withRefs)
	if got != plain {
		t.Errorf("public output changed with refs in the store:\n got %+v\nwant %+v", got, plain)
	}
	for name, body := range map[string]string{"history": got.history, "render": got.render, "feed": got.feed} {
		if strings.Contains(body, prRef1) || strings.Contains(body, "HX7K2P9Q") || strings.Contains(body, `"ref"`) {
			t.Errorf("public %s carries a ref: %s", name, body)
		}
	}
	// domain.Incident, the object of every webhook and outbox payload, has no ref key.
	b, _ := json.Marshal(domain.Incident{ID: "i", ProjectID: "p"})
	if strings.Contains(string(b), `"ref"`) {
		t.Errorf("domain.Incident JSON has a ref: %s", b)
	}
}

// D21 put `by-ref/{ref}` and the `{incidentID}/<name>` GET routes on ONE pattern (Router). That
// changed two method answers, which this pins (final review M-10): a GET of a POST-only incident
// subpath is the dispatcher's 404 where it used to be 405, and any other method on an unknown
// two-segment path is 405 where it used to be 404. A DELETE of a known subpath is 405, as before.
func TestIncidentSubrouteMethodAnswers(t *testing.T) {
	h := newHandler(phoneReadStore())
	for _, c := range []struct {
		method, path string
		code         int
		allow        string
	}{
		{http.MethodGet, "/api/v1/incidents/inc1/acknowledge", http.StatusNotFound, ""},
		{http.MethodGet, "/api/v1/incidents/inc1/nope", http.StatusNotFound, ""},
		{http.MethodDelete, "/api/v1/incidents/inc1/updates", http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{http.MethodDelete, "/api/v1/incidents/x/y", http.StatusMethodNotAllowed, "GET, HEAD"},
	} {
		rec := do(h, o1Admin, c.method, c.path, "")
		if rec.Code != c.code || rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s = %d Allow %q, want %d Allow %q", c.method, c.path, rec.Code, rec.Header().Get("Allow"), c.code, c.allow)
		}
	}
}
