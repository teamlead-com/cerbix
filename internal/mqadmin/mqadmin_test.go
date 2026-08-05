package mqadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromAMQPDerivesManagement(t *testing.T) {
	c, err := FromAMQP("amqp://u:p@broker:5672/")
	if err != nil {
		t.Fatalf("from amqp: %v", err)
	}
	if c.base != "http://broker:15672" || c.user != "u" || c.pass != "p" {
		t.Fatalf("derived = base=%q user=%q pass=%q", c.base, c.user, c.pass)
	}
	tls, _ := FromAMQP("amqps://x@broker/")
	if tls.base != "https://broker:15671" {
		t.Fatalf("amqps base = %q, want https:15671", tls.base)
	}
}

func TestLiveJobRegions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, _ := r.BasicAuth(); u != "u" {
			t.Errorf("missing basic auth")
		}
		_, _ = w.Write([]byte(`[
			{"name":"checks.jobs.core","consumers":2},
			{"name":"checks.jobs.geo1","consumers":0},
			{"name":"checks.jobs.geo2","consumers":1},
			{"name":"checks.results","consumers":1}
		]`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	c.user, c.pass = "u", "p"
	live, err := c.LiveJobRegions(context.Background())
	if err != nil {
		t.Fatalf("live regions: %v", err)
	}
	if !live["core"] || live["geo1"] || !live["geo2"] || len(live) != 2 {
		t.Fatalf("live = %#v, want {core,geo2}", live)
	}
}
