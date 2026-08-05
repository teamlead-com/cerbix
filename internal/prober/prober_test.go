package prober

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/teamlead-com/cerbix/internal/domain"
)

func httpMonitor(target string, conditions ...string) domain.Monitor {
	return domain.Monitor{
		ID: "m", ProjectID: "p", Name: "n", Type: domain.MonitorHTTP, Target: target,
		IntervalSeconds: 60, TimeoutSeconds: 5, Conditions: conditions,
	}
}

func TestHTTPProberDefaultStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK body"))
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()

	r := NewRunner()
	if hb := r.Run(context.Background(), httpMonitor(up.URL)); !hb.Up || hb.Code != 200 {
		t.Fatalf("up server: %+v", hb)
	}
	if hb := r.Run(context.Background(), httpMonitor(down.URL)); hb.Up {
		t.Fatalf("500 server should be down: %+v", hb)
	}
}

func TestHTTPProberConditions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"UP"}`))
	}))
	defer srv.Close()
	r := NewRunner()

	if hb := r.Run(context.Background(), httpMonitor(srv.URL, "[STATUS] == 200", `[BODY] contains "UP"`)); !hb.Up {
		t.Fatalf("conditions should pass: %+v", hb)
	}
	if hb := r.Run(context.Background(), httpMonitor(srv.URL, "[STATUS] == 201")); hb.Up {
		t.Fatalf("status condition should fail: %+v", hb)
	}
	if hb := r.Run(context.Background(), httpMonitor(srv.URL, `[BODY] contains "DOWN"`)); hb.Up {
		t.Fatalf("body condition should fail: %+v", hb)
	}
}

func TestHTTPProberConnectionFailure(t *testing.T) {
	r := NewRunner()
	// Reserved TEST-NET address / closed port → connection failure, retried then down.
	m := httpMonitor("http://127.0.0.1:1")
	m.Retries = 1
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("unreachable target should be down: %+v", hb)
	}
}

func TestTCPProber(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	r := NewRunner()
	openMon := domain.Monitor{ID: "m", ProjectID: "p", Name: "n", Type: domain.MonitorTCP, Target: ln.Addr().String(), IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), openMon); !hb.Up {
		t.Fatalf("open port should be up: %+v", hb)
	}

	closedMon := openMon
	closedMon.Target = "127.0.0.1:1"
	if hb := r.Run(context.Background(), closedMon); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestUnsupportedType(t *testing.T) {
	r := NewRunner()
	m := domain.Monitor{ID: "m", ProjectID: "p", Name: "push", Type: domain.MonitorPush}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("push has no active prober; should be down: %+v", hb)
	}
}

func TestHTTPProberUsesMethod(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Method
	}))
	defer srv.Close()
	run := NewRunner()

	m := httpMonitor(srv.URL)
	m.Method = "POST"
	if hb := run.Run(context.Background(), m); !hb.Up {
		t.Fatalf("want up: %+v", hb)
	}
	if seen != "POST" {
		t.Fatalf("server saw %q, want POST", seen)
	}
	// Empty method defaults to GET.
	m.Method = ""
	_ = run.Run(context.Background(), m)
	if seen != "GET" {
		t.Fatalf("empty method → server saw %q, want GET", seen)
	}
}

func TestDNSProber(t *testing.T) {
	r := NewRunner()
	// localhost resolves offline (hermetic).
	m := domain.Monitor{ID: "d", ProjectID: "p", Name: "dns", Type: domain.MonitorDNS, Target: "localhost", IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("localhost should resolve: %+v", hb)
	}
	// A body condition can assert the resolved address.
	m.Conditions = []string{`[BODY] contains "127.0.0.1"`}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("expected 127.0.0.1 in resolved set: %+v", hb)
	}
	// An unresolvable name is down.
	m.Conditions = nil
	m.Target = "does-not-exist.invalid"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf(".invalid should not resolve: %+v", hb)
	}
}

func TestTLSProber(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://") // host:port on 127.0.0.1

	r := NewRunner()
	m := domain.Monitor{ID: "t", ProjectID: "p", Name: "tls", Type: domain.MonitorTLS, Target: addr, IntervalSeconds: 60, TimeoutSeconds: 5}
	// Handshake succeeds against a live cert → up.
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("valid TLS server should be up: %+v", hb)
	}
	// httptest certs are valid well beyond a day, so a generous expiry threshold passes...
	m.Conditions = []string{"[CERT_EXPIRY] > 0"}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("cert should have positive days to expiry: %+v", hb)
	}
	// ...and an impossible threshold fails the condition.
	m.Conditions = []string{"[CERT_EXPIRY] > 100000"}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("100000-day threshold should fail: %+v", hb)
	}
	// A closed port is down.
	m.Conditions = nil
	m.Target = "127.0.0.1:1"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestGRPCProber(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	r := NewRunner()
	m := domain.Monitor{ID: "g", ProjectID: "p", Name: "grpc", Type: domain.MonitorGRPC, Target: lis.Addr().String(), IntervalSeconds: 60, TimeoutSeconds: 5}

	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("SERVING should be up: %+v", hb)
	}
	// Flip to NOT_SERVING → down.
	hs.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("NOT_SERVING should be down: %+v", hb)
	}
	// Closed port → down.
	m.Target = "127.0.0.1:1"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestCompositeProber(t *testing.T) {
	statuses := map[string]domain.MonitorStatus{
		"a": domain.StatusUp,
		"b": domain.StatusUp,
		"c": domain.StatusDown,
	}
	r := NewRunner().WithChildStatus(func(_ context.Context, _ []string) (map[string]domain.MonitorStatus, error) {
		return statuses, nil
	})
	comp := func(children, mode string) domain.Monitor {
		return domain.Monitor{
			ID: "x", ProjectID: "p", Name: "comp", Type: domain.MonitorComposite,
			IntervalSeconds: 60, Config: map[string]string{"children": children, "mode": mode},
		}
	}

	// mode "all": up only if every child is up.
	if hb := r.Run(context.Background(), comp("a,b", "all")); !hb.Up {
		t.Fatalf("all-up should be up: %+v", hb)
	}
	if hb := r.Run(context.Background(), comp("a,c", "all")); hb.Up {
		t.Fatalf("all with a down child should be down: %+v", hb)
	}
	// mode "any": up if at least one child is up.
	if hb := r.Run(context.Background(), comp("a,c", "any")); !hb.Up {
		t.Fatalf("any with one up child should be up: %+v", hb)
	}
	if hb := r.Run(context.Background(), comp("c", "any")); hb.Up {
		t.Fatalf("any with only down children should be down: %+v", hb)
	}
	// A missing child counts as not-up.
	if hb := r.Run(context.Background(), comp("a,missing", "all")); hb.Up {
		t.Fatalf("missing child should make 'all' down: %+v", hb)
	}
}

// TestCompositeQuorum covers the multi-region-set mode (D-0101): the composite
// goes down only when at least M children vote down; pending/missing children
// count as down votes; the message names the vote tally.
func TestCompositeQuorum(t *testing.T) {
	statuses := map[string]domain.MonitorStatus{
		"a": domain.StatusUp,
		"b": domain.StatusUp,
		"c": domain.StatusDown,
		"d": domain.StatusDown,
	}
	r := NewRunner().WithChildStatus(func(_ context.Context, _ []string) (map[string]domain.MonitorStatus, error) {
		return statuses, nil
	})
	comp := func(children, quorum string) domain.Monitor {
		return domain.Monitor{
			ID: "x", ProjectID: "p", Name: "q", Type: domain.MonitorComposite,
			IntervalSeconds: 60, Config: map[string]string{"children": children, "mode": "quorum", "quorum": quorum},
		}
	}

	// 1/3 down < quorum 2 → up.
	if hb := r.Run(context.Background(), comp("a,b,c", "2")); !hb.Up {
		t.Fatalf("1 of 3 down under quorum 2 must be up: %+v", hb)
	}
	// 2/3 down ≥ quorum 2 → down, with the tally in the message.
	hb := r.Run(context.Background(), comp("a,c,d", "2"))
	if hb.Up || !strings.Contains(hb.Msg, "2/3 children down (quorum 2)") {
		t.Fatalf("2 of 3 down at quorum 2 must be down with tally: %+v", hb)
	}
	// quorum 1 behaves like "all" (any single down vote trips it).
	if hb := r.Run(context.Background(), comp("a,b,c", "1")); hb.Up {
		t.Fatalf("quorum 1 with one down child must be down: %+v", hb)
	}
	// quorum N behaves like "any" inverted (down only when everyone is down).
	if hb := r.Run(context.Background(), comp("a,c,d", "3")); !hb.Up {
		t.Fatalf("quorum 3 with one up child must be up: %+v", hb)
	}
	if hb := r.Run(context.Background(), comp("c,d,missing", "3")); hb.Up {
		t.Fatalf("quorum 3 with all down (incl. missing vote) must be down: %+v", hb)
	}
}

func TestCompositeWithoutLookup(t *testing.T) {
	// No child-status lookup wired → composite is down (not a panic).
	r := NewRunner()
	m := domain.Monitor{ID: "x", ProjectID: "p", Name: "c", Type: domain.MonitorComposite, IntervalSeconds: 60, Config: map[string]string{"children": "a"}}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("composite without lookup should be down: %+v", hb)
	}
}

func TestPostgresProber(t *testing.T) {
	dsn := os.Getenv("CERBIX_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set CERBIX_TEST_DATABASE_DSN to run the postgres prober test")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	r := NewRunner()
	m := domain.Monitor{
		ID: "pg", ProjectID: "p", Name: "pg", Type: domain.MonitorPostgres,
		Target: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), IntervalSeconds: 60, TimeoutSeconds: 5,
		Config: map[string]string{"database": cfg.Database, "username": cfg.User, "password": cfg.Password, "sslmode": "disable"},
	}
	// A live server with the default SELECT 1 → up.
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("live postgres should be up: %+v", hb)
	}
	// A query against a missing relation → down.
	m.Config["query"] = "SELECT * FROM cerbix_does_not_exist_xyz"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("failing query should be down: %+v", hb)
	}
	// Wrong password → down.
	m.Config["query"] = ""
	m.Config["password"] = "definitely-wrong"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("wrong password should be down: %+v", hb)
	}
}

func TestRedisProber(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if !strings.HasPrefix(line, "*") {
						continue
					}
					n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
					cmd := ""
					for i := 0; i < n; i++ {
						_, _ = r.ReadString('\n') // $len
						arg, _ := r.ReadString('\n')
						if i == 0 {
							cmd = strings.ToUpper(strings.TrimSpace(arg))
						}
					}
					switch cmd {
					case "AUTH":
						_, _ = c.Write([]byte("+OK\r\n"))
					case "PING":
						_, _ = c.Write([]byte("+PONG\r\n"))
					default:
						_, _ = c.Write([]byte("-ERR unknown\r\n"))
					}
				}
			}(c)
		}
	}()

	r := NewRunner()
	m := domain.Monitor{ID: "r", ProjectID: "p", Name: "redis", Type: domain.MonitorRedis, Target: ln.Addr().String(), IntervalSeconds: 60, TimeoutSeconds: 5}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("PING should be up: %+v", hb)
	}
	// With a password, AUTH precedes PING.
	m.Config = map[string]string{"password": "secret"}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("auth + PING should be up: %+v", hb)
	}
	// Closed port → down.
	m.Config = nil
	m.Target = "127.0.0.1:1"
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed port should be down: %+v", hb)
	}
}

func TestPromQLProber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000,"0.42"]}}`))
	}))
	defer srv.Close()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer empty.Close()

	r := NewRunner()
	m := domain.Monitor{ID: "pq", ProjectID: "p", Name: "promql", Type: domain.MonitorPromQL, Target: srv.URL, IntervalSeconds: 60, TimeoutSeconds: 5, Config: map[string]string{"query": "up"}}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("scalar result should be up: %+v", hb)
	}
	// [RESULT] threshold via the query value 0.42.
	m.Conditions = []string{"[RESULT] < 0.9"}
	if hb := r.Run(context.Background(), m); !hb.Up {
		t.Fatalf("0.42 < 0.9 should pass: %+v", hb)
	}
	m.Conditions = []string{"[RESULT] > 0.9"}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("0.42 > 0.9 should fail: %+v", hb)
	}
	// Empty result → down.
	m.Conditions = nil
	m.Target = empty.URL
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("empty vector should be down: %+v", hb)
	}
	// Missing query → down.
	m.Target = srv.URL
	m.Config = map[string]string{}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("no query should be down: %+v", hb)
	}
}

func TestMySQLProberClosedPort(t *testing.T) {
	// No MySQL server available; a closed port must fail fast (down), exercising
	// the guarded dialer + connector path without a live server.
	r := NewRunner()
	m := domain.Monitor{ID: "my", ProjectID: "p", Name: "mysql", Type: domain.MonitorMySQL, Target: "127.0.0.1:1", IntervalSeconds: 60, TimeoutSeconds: 3, Config: map[string]string{"username": "root", "database": "x"}}
	if hb := r.Run(context.Background(), m); hb.Up {
		t.Fatalf("closed MySQL port should be down: %+v", hb)
	}
}
