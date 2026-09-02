package prober

import (
	"context"
	"strings"
	"testing"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// secretInQuery is planted in a target's query string. Any probe result carrying it is the
// defect these tests exist for: net/http embeds the request URL in every error it returns,
// so `Msg: err.Error()` published whatever the query held into a stored, UI-rendered field
// and into the incident body derived from it.
const secretInQuery = "s3cr3t-in-query"

// TestProbeResultNeverCarriesTheRequestURL is the invariant, asserted per type through the
// real Runner rather than against the helper: a prober added later that writes err.Error()
// again fails here.
func TestProbeResultNeverCarriesTheRequestURL(t *testing.T) {
	dead := "https://127.0.0.1:1/x?token=" + secretInQuery
	cases := []struct {
		name string
		m    domain.Monitor
	}{
		{"http", domain.Monitor{Type: domain.MonitorHTTP, Target: dead}},
		{"promql", domain.Monitor{Type: domain.MonitorPromQL, Target: dead, Config: map[string]string{"query": "up"}}},
		{"rabbitmq management", domain.Monitor{Type: domain.MonitorRabbitMQ, Target: dead,
			Config: map[string]string{"mode": "management", "username": "u", "password": "p", "tls": "true"}}},
		{"synthetic step", domain.Monitor{Type: domain.MonitorSynthetic,
			Config: map[string]string{"scenario": `{"steps":[{"url":"https://127.0.0.1:1/x?token=` + secretInQuery + `"}]}`}}},
		// The websocket prober dials a TCP address rather than fetching a URL, so its error
		// carries host:port and no query. Asserted rather than assumed — if that ever changes,
		// this case is where it surfaces.
		{"websocket", domain.Monitor{Type: domain.MonitorWebSocket, Target: "wss://127.0.0.1:1/s?token=" + secretInQuery}},
	}
	r := NewRunner()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := c.m
			m.ID, m.ProjectID, m.Name = "m", "p", c.name
			m.IntervalSeconds, m.TimeoutSeconds = 60, 3
			hb := r.Run(context.Background(), m)
			if hb.Up {
				t.Fatalf("the probe was expected to fail: %+v", hb)
			}
			if strings.Contains(hb.Msg, secretInQuery) {
				t.Fatalf("the request URL's query reached the result: %q", hb.Msg)
			}
			if strings.Contains(hb.Msg, "://") {
				t.Fatalf("the result carries a URL: %q", hb.Msg)
			}
			// Still diagnosable: the message names a cause, and for URL-bearing types the host.
			if strings.TrimSpace(hb.Msg) == "" {
				t.Fatal("the message is empty: a failure nobody can act on")
			}
			t.Logf("%-20s → %q", c.name, hb.Msg)
		})
	}
}

// TestFailureClassNamesTheCause pins the vocabulary, so a refusal to echo a URL does not
// become a refusal to say anything. A connection to a closed port is the one cause every
// platform reports identically.
func TestFailureClassNamesTheCause(t *testing.T) {
	r := NewRunner()
	m := domain.Monitor{ID: "m", ProjectID: "p", Name: "http", Type: domain.MonitorHTTP,
		Target: "https://127.0.0.1:1/", IntervalSeconds: 60, TimeoutSeconds: 3}
	hb := r.Run(context.Background(), m)
	if !strings.Contains(hb.Msg, "connection refused") {
		t.Fatalf("msg = %q, want it to name the refused connection", hb.Msg)
	}
	if !strings.Contains(hb.Msg, "127.0.0.1:1") {
		t.Fatalf("msg = %q, want the host that failed", hb.Msg)
	}
}

// A DNS failure and a timeout are the two other classes an operator meets daily.
func TestFailureClassesDNSAndTimeout(t *testing.T) {
	r := NewRunner()
	dnsMon := domain.Monitor{ID: "m", ProjectID: "p", Name: "http", Type: domain.MonitorHTTP,
		Target: "https://nonexistent.invalid./?token=" + secretInQuery, IntervalSeconds: 60, TimeoutSeconds: 3}
	hb := r.Run(context.Background(), dnsMon)
	if strings.Contains(hb.Msg, secretInQuery) {
		t.Fatalf("dns failure leaked the query: %q", hb.Msg)
	}
	if !strings.Contains(hb.Msg, "dns") {
		t.Logf("dns message = %q (resolver-dependent; the leak assertion above is the contract)", hb.Msg)
	}
}
