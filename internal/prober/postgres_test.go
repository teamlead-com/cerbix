package prober

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// sec-hardening-3 §2.8: the PostgreSQL probe, proved at the protocol level against a fake server
// that speaks just enough of the wire protocol. The server records the first message of every
// connection, the SNI of a TLS ClientHello and the startup parameters, answers an SSLRequest with
// 'S' (then TLS) or 'N', and can break the handshake on purpose.

// pgTLSAnswer is how the fake server answers an SSLRequest.
type pgTLSAnswer int

const (
	pgAnswerN         pgTLSAnswer = iota // 'N': no TLS here
	pgAnswerTLS                          // 'S', then a TLS handshake with the server's certificate
	pgAnswerBrokenTLS                    // 'S', then bytes that are not TLS, then close
)

// pgConnRecord is what the fake server saw on one connection.
type pgConnRecord struct {
	first      string // "SSLRequest" or "StartupMessage"
	sni        string // ServerName of the ClientHello, "" when none was sent
	tls        bool   // the startup message arrived over TLS
	protocol   uint32 // the startup message's protocol version
	params     map[string]string
	gotPass    bool // the client answered the cleartext-password request
	queryRanOK bool
}

type fakePG struct {
	t      *testing.T
	ln     net.Listener
	answer pgTLSAnswer
	cert   *tls.Certificate

	mu    sync.Mutex
	conns []pgConnRecord
}

func newFakePG(t *testing.T, answer pgTLSAnswer, cert *tls.Certificate) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePG{t: t, ln: ln, answer: answer, cert: cert}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakePG) port() string { return strconv.Itoa(f.ln.Addr().(*net.TCPAddr).Port) }

func (f *fakePG) records() []pgConnRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pgConnRecord(nil), f.conns...)
}

func (f *fakePG) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakePG) handle(raw net.Conn) {
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	rec := pgConnRecord{}
	idx := f.add(rec)
	defer func() { f.set(idx, rec) }()

	var conn net.Conn = raw
	backend := pgproto3.NewBackend(conn, conn)
	msg, err := backend.ReceiveStartupMessage()
	if err != nil {
		return
	}
	if _, ok := msg.(*pgproto3.SSLRequest); ok {
		rec.first = "SSLRequest"
		f.set(idx, rec)
		switch f.answer {
		case pgAnswerN:
			if _, err := raw.Write([]byte{'N'}); err != nil {
				return
			}
		case pgAnswerBrokenTLS:
			_, _ = raw.Write([]byte{'S'})
			_, _ = raw.Write([]byte("this is not a TLS server hello\n"))
			return
		case pgAnswerTLS:
			if _, err := raw.Write([]byte{'S'}); err != nil {
				return
			}
			var sni string
			tlsConn := tls.Server(raw, &tls.Config{
				Certificates: []tls.Certificate{*f.cert},
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					sni = hello.ServerName
					return nil, nil
				},
			})
			if err := tlsConn.Handshake(); err != nil {
				rec.sni = sni
				return
			}
			rec.sni, rec.tls = sni, true
			conn = tlsConn
			backend = pgproto3.NewBackend(conn, conn)
		}
		if msg, err = backend.ReceiveStartupMessage(); err != nil {
			return
		}
	} else {
		rec.first = "StartupMessage"
	}
	startup, ok := msg.(*pgproto3.StartupMessage)
	if !ok {
		return
	}
	rec.protocol = startup.ProtocolVersion
	rec.params = startup.Parameters
	f.set(idx, rec)

	backend.Send(&pgproto3.AuthenticationCleartextPassword{})
	if err := backend.Flush(); err != nil {
		return
	}
	if err := backend.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
		return
	}
	if m, err := backend.Receive(); err != nil {
		return
	} else if _, ok := m.(*pgproto3.PasswordMessage); !ok {
		return
	}
	rec.gotPass = true
	f.set(idx, rec)
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}
	for {
		m, err := backend.Receive()
		if err != nil {
			return
		}
		switch m.(type) {
		case *pgproto3.Query:
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			rec.queryRanOK = true
			f.set(idx, rec)
		case *pgproto3.Parse:
			backend.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Describe:
			backend.Send(&pgproto3.ParameterDescription{})
			backend.Send(&pgproto3.NoData{})
		case *pgproto3.Bind:
			backend.Send(&pgproto3.BindComplete{})
		case *pgproto3.Execute:
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			rec.queryRanOK = true
			f.set(idx, rec)
		case *pgproto3.Sync:
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Terminate:
			return
		}
		if err := backend.Flush(); err != nil {
			return
		}
	}
}

func (f *fakePG) add(r pgConnRecord) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = append(f.conns, r)
	return len(f.conns) - 1
}

func (f *fakePG) set(i int, r pgConnRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[i] = r
}

// dialRecorder is the probe's dial: a plain dial that records every address it was asked for.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func (d *dialRecorder) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// testCA issues leaf certificates from a fresh root.
type testCA struct {
	t    *testing.T
	pool *x509.CertPool
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cerbix test root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{t: t, pool: pool, cert: cert, key: key}
}

func (ca *testCA) leaf(dnsNames []string, ips []net.IP) *tls.Certificate {
	ca.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		ca.t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "cerbix test leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		ca.t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func pgMonitor(target string, config map[string]string) domain.Monitor {
	c := map[string]string{"username": "u", "database": "d", "password": "pw"}
	maps.Copy(c, config)
	return domain.Monitor{Type: domain.MonitorPostgres, Target: target, TimeoutSeconds: 5, Config: c}
}

func sslmode(mode string) map[string]string { return map[string]string{"sslmode": mode} }

func runPG(t *testing.T, p pgProber, m domain.Monitor) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Probe(ctx, m)
}

func TestPostgresProbeAsksForTLSAsSSLModeSays(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{"require", sslmode("require"), "SSLRequest"},
		{"verify-ca", sslmode("verify-ca"), "SSLRequest"},
		{"verify-full", sslmode("verify-full"), "SSLRequest"},
		{"prefer", sslmode("prefer"), "SSLRequest"},
		{"absent (legacy)", nil, "SSLRequest"},
		{"disable", sslmode("disable"), "StartupMessage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakePG(t, pgAnswerN, nil)
			d := &dialRecorder{}
			_ = runPG(t, pgProber{dial: d.dial}, pgMonitor("127.0.0.1:"+srv.port(), tc.config))
			recs := srv.records()
			if len(recs) == 0 {
				t.Fatal("the server saw no connection")
			}
			if recs[0].first != tc.want {
				t.Fatalf("first message = %q, want %q", recs[0].first, tc.want)
			}
		})
	}
}

func TestPostgresProbeRefusesAServerWithoutTLS(t *testing.T) {
	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			srv := newFakePG(t, pgAnswerN, nil)
			d := &dialRecorder{}
			res := runPG(t, pgProber{dial: d.dial}, pgMonitor("127.0.0.1:"+srv.port(), sslmode(mode)))
			want := `the server does not offer TLS, and sslmode is "` + mode + `": set sslmode to "disable" if plaintext is intended`
			if res.Connected || res.Msg != want {
				t.Fatalf("result = connected %v, %q; want down with %q", res.Connected, res.Msg, want)
			}
		})
	}
	t.Run("absent key executes as prefer", func(t *testing.T) {
		srv := newFakePG(t, pgAnswerN, nil)
		d := &dialRecorder{}
		target := "127.0.0.1:" + srv.port()
		res := runPG(t, pgProber{dial: d.dial}, pgMonitor(target, nil))
		if !res.Connected {
			t.Fatalf("absent sslmode with 'N' must reconnect in plaintext: %q", res.Msg)
		}
		recs := srv.records()
		if len(recs) != 2 || recs[0].first != "SSLRequest" || recs[1].first != "StartupMessage" {
			t.Fatalf("connections = %+v, want an SSLRequest then a plaintext StartupMessage", recs)
		}
		for _, a := range d.dialed() {
			if a != target {
				t.Fatalf("dialed %q, want only %q", a, target)
			}
		}
	})
	t.Run("explicit empty value is refused", func(t *testing.T) {
		srv := newFakePG(t, pgAnswerN, nil)
		d := &dialRecorder{}
		res := runPG(t, pgProber{dial: d.dial}, pgMonitor("127.0.0.1:"+srv.port(), sslmode("")))
		if res.Connected || res.Msg != "bad config: unsupported sslmode" || len(d.dialed()) != 0 {
			t.Fatalf("explicit \"\" = connected %v, %q, dials %v; want bad config and no dial", res.Connected, res.Msg, d.dialed())
		}
	})
}

func TestPostgresPreferFallsBackToTheSameHostAndPort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer pgTLSAnswer
	}{
		{"server answers N", pgAnswerN},
		{"handshake fails", pgAnswerBrokenTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakePG(t, tc.answer, nil)
			d := &dialRecorder{}
			target := "127.0.0.1:" + srv.port()
			res := runPG(t, pgProber{dial: d.dial}, pgMonitor(target, sslmode("prefer")))
			if !res.Connected {
				t.Fatalf("prefer must fall back to plaintext: %q", res.Msg)
			}
			dialed := d.dialed()
			if len(dialed) != 2 || dialed[0] != target || dialed[1] != target {
				t.Fatalf("dials = %v, want exactly two, both to %s", dialed, target)
			}
			recs := srv.records()
			if len(recs) != 2 || recs[0].first != "SSLRequest" || recs[1].first != "StartupMessage" || recs[1].tls {
				t.Fatalf("connections = %+v, want an SSLRequest, then a plaintext StartupMessage", recs)
			}
		})
	}
}

func TestPostgresVerifyFullChecksTheCertificate(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t) // a root the probe does not trust
	cases := []struct {
		name      string
		cert      *tls.Certificate
		host      string
		mode      string
		wantUp    bool
		wantSNI   string
		checkSNI  bool
		wantInMsg string
	}{
		{"name cert, name target, verify-full", ca.leaf([]string{"localhost"}, nil), "localhost", "verify-full", true, "localhost", true, ""},
		{"trusted cert for another name, verify-ca passes", ca.leaf([]string{"other.test"}, nil), "localhost", "verify-ca", true, "localhost", true, ""},
		{"trusted cert for another name, verify-full refuses", ca.leaf([]string{"other.test"}, nil), "localhost", "verify-full", false, "", false, `TLS certificate not accepted (sslmode "verify-full"): `},
		{"name cert at an IP target, verify-full refuses", ca.leaf([]string{"localhost"}, nil), "127.0.0.1", "verify-full", false, "", false, `TLS certificate not accepted (sslmode "verify-full"): `},
		{"IP SAN at an IP target, verify-full", ca.leaf(nil, []net.IP{net.ParseIP("127.0.0.1")}), "127.0.0.1", "verify-full", true, "", true, ""},
		{"untrusted chain, verify-ca refuses", other.leaf([]string{"localhost"}, nil), "localhost", "verify-ca", false, "", false, `TLS certificate not accepted (sslmode "verify-ca"): `},
		{"untrusted chain, verify-full refuses", other.leaf([]string{"localhost"}, nil), "localhost", "verify-full", false, "", false, `TLS certificate not accepted (sslmode "verify-full"): `},
		{"untrusted chain, require completes the handshake", other.leaf([]string{"localhost"}, nil), "localhost", "require", true, "localhost", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakePG(t, pgAnswerTLS, tc.cert)
			d := &dialRecorder{}
			res := runPG(t, pgProber{dial: d.dial, roots: ca.pool}, pgMonitor(net.JoinHostPort(tc.host, srv.port()), sslmode(tc.mode)))
			if res.Connected != tc.wantUp {
				t.Fatalf("connected = %v (%q), want %v", res.Connected, res.Msg, tc.wantUp)
			}
			if tc.wantInMsg != "" && !strings.HasPrefix(res.Msg, tc.wantInMsg) {
				t.Fatalf("message %q, want the prefix %q", res.Msg, tc.wantInMsg)
			}
			if tc.checkSNI {
				var tlsRecs []pgConnRecord
				for _, r := range srv.records() {
					if r.tls {
						tlsRecs = append(tlsRecs, r)
					}
				}
				if len(tlsRecs) == 0 || tlsRecs[0].sni != tc.wantSNI {
					t.Fatalf("TLS connections %+v, want SNI %q", tlsRecs, tc.wantSNI)
				}
			}
		})
	}
}

func TestPostgresProbeSendsSNIAsPgxDoes(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.leaf([]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	for _, mode := range []string{"require", "verify-ca"} {
		for _, tc := range []struct{ host, wantSNI string }{
			{"localhost", "localhost"},
			{"127.0.0.1", ""},
			{"::1", ""},
		} {
			t.Run(mode+" "+tc.host, func(t *testing.T) {
				srv := newFakePG(t, pgAnswerTLS, cert)
				// The probe dials whatever the target resolves to; this dial sends every address to
				// the IPv4 listener so that `::1` is testable on a host without an IPv6 listener.
				dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", srv.ln.Addr().String())
				}
				res := runPG(t, pgProber{dial: dial, roots: ca.pool}, pgMonitor(net.JoinHostPort(tc.host, srv.port()), sslmode(mode)))
				if !res.Connected {
					t.Fatalf("probe failed: %q", res.Msg)
				}
				recs := srv.records()
				if len(recs) == 0 || !recs[0].tls || recs[0].sni != tc.wantSNI {
					t.Fatalf("connections %+v, want TLS with SNI %q", recs, tc.wantSNI)
				}
			})
		}
	}
}

func TestHostPortStripsIPv6Brackets(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantHost string
		wantPort uint16
	}{
		{"[::1]", "::1", 5432},
		{"[::1]:6543", "::1", 6543},
		{"db:5432", "db", 5432},
		{"db", "db", 5432},
	} {
		host, port := hostPort(tc.in, 5432)
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("hostPort(%q) = (%q, %d), want (%q, %d)", tc.in, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestPostgresProbeRefusesAnSSLModeOutsideTheList(t *testing.T) {
	for _, v := range []string{"allow", "REQUIRE", "", "require host=a,127.0.0.2", "require sslrootcert=/etc/hosts", "require service=x"} {
		t.Run(strconv.Quote(v), func(t *testing.T) {
			d := &dialRecorder{}
			res := runPG(t, pgProber{dial: d.dial}, pgMonitor("127.0.0.1:1", sslmode(v)))
			if res.Connected || res.Msg != "bad config: unsupported sslmode" {
				t.Fatalf("result = connected %v, %q; want bad config: unsupported sslmode", res.Connected, res.Msg)
			}
			if got := d.dialed(); len(got) != 0 {
				t.Fatalf("dialed %v, want no dial at all", got)
			}
		})
	}
}

func TestPostgresProbeIgnoresTheEnvironment(t *testing.T) {
	ca := newTestCA(t)
	srv := newFakePG(t, pgAnswerTLS, ca.leaf(nil, []net.IP{net.ParseIP("127.0.0.1")}))
	decoy := newFakePG(t, pgAnswerN, nil)
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.crt")
	passfile := filepath.Join(dir, "pgpass")
	servicefile := filepath.Join(dir, "pg_service.conf")
	for path, body := range map[string]string{
		garbage:     "not a certificate\n",
		passfile:    "*:*:*:*:leaked\n",
		servicefile: "[decoy]\nhost=127.0.0.1\nport=" + decoy.port() + "\napplication_name=fromservice\nkrbsrvname=k\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"PGAPPNAME": "leak", "PGOPTIONS": "-c search_path=x", "PGTZ": "UTC", "PGHOST": "127.0.0.1",
		"PGPORT": "abc", "PGCONNECT_TIMEOUT": "abc", "PGTARGETSESSIONATTRS": "bogus",
		"PGSSLNEGOTIATION": "direct", "PGCHANNELBINDING": "require", "PGREQUIREAUTH": "scram-sha-256",
		"PGMINPROTOCOLVERSION": "3.2", "PGSSLROOTCERT": garbage, "PGPASSFILE": passfile,
		"PGSERVICE": "decoy", "PGSERVICEFILE": servicefile,
	} {
		t.Setenv(k, v)
	}

	d := &dialRecorder{}
	target := "127.0.0.1:" + srv.port()
	res := runPG(t, pgProber{dial: d.dial, roots: ca.pool}, pgMonitor(target, sslmode("verify-full")))
	if !res.Connected {
		t.Fatalf("probe failed under a hostile environment: %q", res.Msg)
	}
	recs := srv.records()
	if len(recs) != 1 {
		t.Fatalf("connections = %+v, want exactly one", recs)
	}
	r := recs[0]
	if r.first != "SSLRequest" || !r.tls {
		t.Fatalf("first message %q (tls %v), want an SSLRequest then TLS", r.first, r.tls)
	}
	if r.protocol != 196608 {
		t.Fatalf("startup protocol %d, want 3.0 (196608)", r.protocol)
	}
	for _, k := range []string{"application_name", "options", "timezone"} {
		if v, ok := r.params[k]; ok {
			t.Fatalf("startup message carries %s=%q from the environment", k, v)
		}
	}
	if !r.gotPass {
		t.Fatal("the probe did not answer the cleartext-password request: PGREQUIREAUTH/PGCHANNELBINDING leaked")
	}
	for _, a := range d.dialed() {
		if a != target {
			t.Fatalf("dialed %q, want only the target %s", a, target)
		}
	}
	if got := decoy.records(); len(got) != 0 {
		t.Fatalf("the decoy named by PGHOST/PGSERVICE received %d connection(s)", len(got))
	}

	t.Run("PGSERVICE naming a missing service", func(t *testing.T) {
		t.Setenv("PGSERVICE", "missing")
		d := &dialRecorder{}
		res := runPG(t, pgProber{dial: d.dial, roots: ca.pool}, pgMonitor(target, sslmode("verify-full")))
		want := "bad config: the PG* environment of the prober breaks the PostgreSQL client"
		if res.Connected || res.Msg != want || len(d.dialed()) != 0 {
			t.Fatalf("result = connected %v, %q, dials %v; want %q and no dial", res.Connected, res.Msg, d.dialed(), want)
		}
	})
}

// TestPostgresConfigResetCoversEveryField: (1) the reset's field list is exactly the exported field
// set of pgconn.Config and pgx.ConnConfig, so a pgx upgrade that adds one fails here; (2) on a base in
// which every field the reset owns is poisoned, every such field ends at the table's value, so
// deleting any line of the reset fails here too.
func TestPostgresConfigResetCoversEveryField(t *testing.T) {
	var fields []string
	for _, typ := range []reflect.Type{reflect.TypeOf(pgconn.Config{}), reflect.TypeOf(pgx.ConnConfig{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() || f.Anonymous { // ConnConfig embeds Config, walked on its own
				continue
			}
			fields = append(fields, f.Name)
		}
	}
	sort.Strings(fields)
	listed := append([]string(nil), pgResetFields...)
	sort.Strings(listed)
	if !reflect.DeepEqual(fields, listed) {
		t.Fatalf("the reset lists %v\nthe structs have %v", listed, fields)
	}

	base, err := pgx.ParseConfig("host=/poison port=1 user=poison database=poison password=poison " +
		"application_name=poison krbsrvname=poison krbspn=poison sslnegotiation=direct " +
		"target_session_attrs=read-write min_protocol_version=3.2 max_protocol_version=latest " +
		"channel_binding=require require_auth=scram-sha-256 statement_cache_capacity=1 " +
		"description_cache_capacity=1 default_query_exec_mode=exec connect_timeout=7")
	if err != nil {
		t.Fatal(err)
	}
	kept := struct {
		lookup, frontend, watcher, onPgError uintptr
	}{
		reflect.ValueOf(base.LookupFunc).Pointer(), reflect.ValueOf(base.BuildFrontend).Pointer(),
		reflect.ValueOf(base.BuildContextWatcherHandler).Pointer(), reflect.ValueOf(base.OnPgError).Pointer(),
	}
	poisonTLS := &tls.Config{ServerName: "poison"}
	base.TLSConfig = poisonTLS
	base.Fallbacks = []*pgconn.FallbackConfig{{Host: "poison", Port: 1, TLSConfig: poisonTLS}}
	base.AfterNetConnect = func(context.Context, *pgconn.Config, net.Conn) (net.Conn, error) { return nil, errors.New("poison") }
	base.AfterConnect = func(context.Context, *pgconn.PgConn) error { return errors.New("poison") }
	base.OnNotice = func(*pgconn.PgConn, *pgconn.Notice) {}
	base.OnNotification = func(*pgconn.PgConn, *pgconn.Notification) {}
	base.OAuthTokenProvider = func(context.Context) (string, error) { return "poison", nil }
	base.Tracer = poisonTracer{}
	if base.ValidateConnect == nil || base.KerberosSrvName == "" || base.SSLNegotiation != "direct" ||
		base.RuntimeParams["application_name"] != "poison" || base.ChannelBinding != "require" ||
		base.RequireAuth == "" || base.MinProtocolVersion != "3.2" || base.ConnectTimeout == 0 ||
		base.StatementCacheCapacity != 1 || base.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatalf("the poisoned base did not take: %+v", base)
	}

	var dialed bool
	dial := func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, errors.New("dial") }
	wantTLS := &tls.Config{ServerName: "db"}
	wantFallbacks := []*pgconn.FallbackConfig{{Host: "db", Port: 5432}}
	resetPgConfig(base, pgConnSpec{
		host: "db", port: 5432, user: "u", password: "pw", database: "d",
		tls: wantTLS, fallbacks: wantFallbacks, dial: dial,
	})

	check := func(name string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("field %s still holds the poison (or the wrong value) after the reset", name)
		}
	}
	check("Host", base.Host == "db")
	check("Port", base.Port == 5432)
	check("User", base.User == "u")
	check("Password", base.Password == "pw")
	check("Database", base.Database == "d")
	check("TLSConfig", base.TLSConfig == wantTLS)
	check("Fallbacks", len(base.Fallbacks) == 1 && base.Fallbacks[0] == wantFallbacks[0])
	check("ConnectTimeout", base.ConnectTimeout == 0)
	_, _ = base.DialFunc(context.Background(), "tcp", "x")
	check("DialFunc", dialed)
	check("LookupFunc", reflect.ValueOf(base.LookupFunc).Pointer() == kept.lookup)
	check("BuildFrontend", reflect.ValueOf(base.BuildFrontend).Pointer() == kept.frontend)
	check("BuildContextWatcherHandler", reflect.ValueOf(base.BuildContextWatcherHandler).Pointer() == kept.watcher)
	check("OnPgError", reflect.ValueOf(base.OnPgError).Pointer() == kept.onPgError)
	check("RuntimeParams", base.RuntimeParams != nil && len(base.RuntimeParams) == 0)
	check("KerberosSrvName", base.KerberosSrvName == "")
	check("KerberosSpn", base.KerberosSpn == "")
	check("SSLNegotiation", base.SSLNegotiation == "")
	check("AfterNetConnect", base.AfterNetConnect == nil)
	check("ValidateConnect", base.ValidateConnect == nil)
	check("AfterConnect", base.AfterConnect == nil)
	check("OnNotice", base.OnNotice == nil)
	check("OnNotification", base.OnNotification == nil)
	check("OAuthTokenProvider", base.OAuthTokenProvider == nil)
	check("MinProtocolVersion", base.MinProtocolVersion == "3.0")
	check("MaxProtocolVersion", base.MaxProtocolVersion == "3.0")
	check("ChannelBinding", base.ChannelBinding == "prefer")
	check("RequireAuth", base.RequireAuth == "")
	check("Tracer", base.Tracer == nil)
	check("StatementCacheCapacity", base.StatementCacheCapacity == 512)
	check("DescriptionCacheCapacity", base.DescriptionCacheCapacity == 512)
	check("DefaultQueryExecMode", base.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement)
}

type poisonTracer struct{}

func (poisonTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (poisonTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
