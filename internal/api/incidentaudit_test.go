package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/authz"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/store"
)

// FR-026 at the API level: every audited route reaches the store WITH a principal, and a route that
// refuses reaches it with nothing at all. The fake records `<action>|<label>` for each principal door,
// so a handler that drops the actor on the way through shows up as a missing or unlabelled entry
// rather than as a silently unaudited write.

func TestEveryAuditedRouteCarriesItsPrincipalToTheStore(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	cases := []struct {
		name, method, path, body, want string
		as                             authz.Principal
	}{
		{"manual create", http.MethodPost, "/api/v1/projects/p1/incidents",
			`{"title":"db slow","impact":"major","status":"investigating","body":"opened"}`, "incident.create|oa", o1Admin},
		{"receiver create", http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
			`{"alerts":[{"status":"firing","fingerprint":"fp-audit","labels":{"alertname":"X","severity":"critical"}}]}`,
			"incident.create|apitoken:t", tokenEditor},
		{"status change", http.MethodPost, "/api/v1/incidents/inc1/updates",
			`{"status":"identified","body":"found it"}`, "incident.status|oa", o1Admin},
		{"note that changes nothing", http.MethodPost, "/api/v1/incidents/inc1/updates",
			`{"body":"still on it"}`, "incident.note|oa", o1Admin},
		{"acknowledgement", http.MethodPost, "/api/v1/incidents/inc1/acknowledge", ``, "incident.acknowledge|oa", o1Admin},
		// A postmortem needs a RESOLVED incident, and the only seeded one lives in o2.
		{"postmortem", http.MethodPut, "/api/v1/incidents/inc3/postmortem",
			`{"body":"what happened"}`, "incident.postmortem|ga", globalAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs.auditActors = nil
			rec := do(h, tc.as, tc.method, tc.path, tc.body)
			if rec.Code >= 300 {
				t.Fatalf("%s = %d: %s", tc.name, rec.Code, rec.Body.String())
			}
			if len(fs.auditActors) != 1 || fs.auditActors[0] != tc.want {
				t.Fatalf("%s reached the store as %v, want [%s]", tc.name, fs.auditActors, tc.want)
			}
		})
	}
}

// The receiver's RESOLVE is the second half of D1's "the receiver's create and its resolve", and it is
// the one a system door hid: it used to reach `AddIncidentUpdateBySystem` and audit nothing.
func TestTheAlertmanagerResolveIsAudited(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	fire := `{"alerts":[{"status":"firing","fingerprint":"fp-r","labels":{"alertname":"X"}}]}`
	if rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager", fire); rec.Code != http.StatusOK {
		t.Fatalf("firing = %d", rec.Code)
	}
	fs.auditActors = nil
	rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
		`{"alerts":[{"status":"resolved","fingerprint":"fp-r"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body.String())
	}
	// A resolve is a STATUS change, not a note (D4): the vocabulary does not grow a word per state.
	if len(fs.auditActors) != 1 || !strings.HasPrefix(fs.auditActors[0], "incident.status|") {
		t.Fatalf("the receiver's resolve audited %v, want one incident.status row", fs.auditActors)
	}
	if !strings.HasSuffix(fs.auditActors[0], "|apitoken:t") {
		t.Fatalf("the resolve lost its principal: %v", fs.auditActors)
	}
}

// A refused write reaches no door, so it cannot leave a row: the 403 happens before the store and the
// 404 of a foreign project happens before it too.
func TestARefusedWriteReachesNoDoor(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)

	fs.auditActors = nil
	if rec := do(h, o1Viewer, http.MethodPost, "/api/v1/incidents/inc1/updates",
		`{"status":"identified","body":"nope"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer update = %d, want 403", rec.Code)
	}
	if len(fs.auditActors) != 0 {
		t.Fatalf("a refused write reached the store: %v", fs.auditActors)
	}

	fs.auditActors = nil
	if rec := do(h, tokenOtherOrg, http.MethodPost, "/api/v1/projects/p1/incidents",
		`{"title":"x","impact":"major","status":"investigating"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project create = %d, want 404", rec.Code)
	}
	if len(fs.auditActors) != 0 {
		t.Fatalf("a foreign-project write reached the store: %v", fs.auditActors)
	}
}

// D5's token rule at the handler seam: a token principal has no user row, so its audit identity is the
// label. A session user's is the uuid. Both must survive `auditActor`.
func TestTheActorShapeMatchesThePrincipalKind(t *testing.T) {
	fs := seededStore()
	h := newHandler(fs)
	fs.auditActors = nil
	if rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/incidents",
		`{"title":"by token","impact":"minor","status":"investigating"}`); rec.Code >= 300 {
		t.Fatalf("token create = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.auditActors) != 1 || !strings.HasSuffix(fs.auditActors[0], "|apitoken:t") {
		t.Fatalf("token actor = %v", fs.auditActors)
	}

	fs.auditActors = nil
	if rec := do(h, o1Admin, http.MethodPost, "/api/v1/projects/p1/incidents",
		`{"title":"by user","impact":"minor","status":"investigating"}`); rec.Code >= 300 {
		t.Fatalf("user create = %d", rec.Code)
	}
	if len(fs.auditActors) != 1 || !strings.HasSuffix(fs.auditActors[0], "|oa") {
		t.Fatalf("session actor = %v", fs.auditActors)
	}
	_ = domain.IncidentInvestigating
}

// D8b at the seam it is a contract at: what the receiver ANSWERS when it loses a concurrent duplicate
// delivery. The race itself is proven against the real database in `internal/store`; here the two
// errors it produces are injected, because a fake cannot hold a partial unique index and pretending it
// can would prove nothing about either half.
func TestADuplicateDeliveryThatLosesTheRaceIsIgnoredNot500(t *testing.T) {
	t.Run("the firing that loses the index", func(t *testing.T) {
		fs := seededStore()
		fs.createIncidentErr = store.ErrAlreadyOpen
		h := newHandler(fs)
		rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
			`{"alerts":[{"status":"firing","fingerprint":"fp-race","labels":{"alertname":"X"}}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("the loser answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var res struct{ Opened, Resolved, Ignored int }
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if res.Opened != 0 || res.Ignored != 1 {
			t.Fatalf("the loser reported %+v, want opened 0 / ignored 1", res)
		}
	})

	t.Run("the resolve that loses the status guard", func(t *testing.T) {
		fs := seededStore()
		h := newHandler(fs)
		if rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
			`{"alerts":[{"status":"firing","fingerprint":"fp-race2","labels":{"alertname":"X"}}]}`); rec.Code != http.StatusOK {
			t.Fatalf("firing = %d", rec.Code)
		}
		fs.addIncidentUpdateErr = store.ErrIncidentTerminal
		rec := do(h, tokenEditor, http.MethodPost, "/api/v1/projects/p1/alerts/alertmanager",
			`{"alerts":[{"status":"resolved","fingerprint":"fp-race2"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("the loser answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var res struct{ Opened, Resolved, Ignored int }
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if res.Resolved != 0 || res.Ignored != 1 {
			t.Fatalf("the loser reported %+v, want resolved 0 / ignored 1", res)
		}
	})

	// The mapping is scoped to the receiver: an operator resolving an already-resolved incident on
	// the HUMAN route still gets the answer they get today.
	t.Run("the human route is unaffected", func(t *testing.T) {
		fs := seededStore()
		fs.addIncidentUpdateErr = store.ErrIncidentTerminal
		h := newHandler(fs)
		rec := do(h, o1Admin, http.MethodPost, "/api/v1/incidents/inc1/updates",
			`{"status":"resolved","body":"already done"}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("the human route swallowed a terminal incident: %d %s", rec.Code, rec.Body.String())
		}
	})
}
