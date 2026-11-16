package prober

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// pgProber checks a PostgreSQL server: it connects (through the SSRF guard) and
// runs a query, default "SELECT 1". Success (no conditions) = connect + query
// succeed. Config keys: database, username, password (encrypted at rest),
// sslmode, query. The check latency is [RESPONSE_TIME].
//
// The connection is built from the monitor alone (sec-hardening-3 T-1a/T-1b, S15/S16). Nothing from
// the monitor passes through a libpq connection string, the TLS configuration is built here rather
// than by pgx.ParseConfig, and every field that decides where, how and as whom the probe connects is
// set after the parse, so neither the process environment nor the prober user's files reach a probe.
//
// An ABSENT sslmode executes as "prefer": that is the legacy reading of rows written before the
// `require` default existed (§8 decision 3). The dispatch gate refuses an absent key on a body-bound
// job (S19), and core states it before sealing, so on that carrier the absence never reaches here.
type pgProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// roots verifies the server under verify-ca/verify-full; nil means the system roots. Only tests
	// set it: a custom CA is not a monitor setting (sec-hardening-3 §7).
	roots *x509.CertPool
}

// pgConstantConnString is the ONLY string handed to pgx.ParseConfig. pgx.ConnectConfig requires a
// config made by ParseConfig, and ParseConfig always merges the PG* environment first; this string is
// merged last, so every key the environment could set to an unparsable value is named here. The host
// is a unix-socket path, so the parse never enters pgx's TLS setup (no certificate or key is read)
// and no socket is opened: the probe replaces the host before any dial. `passfile=”` keeps the
// parse from reading a password file. What still gets through (run-time parameters, SSLNegotiation,
// anything a PGSERVICE file adds) is cleared by resetPgConfig.
const pgConstantConnString = "host=/cerbix-no-socket port=5432 user=cerbix database=cerbix password='' passfile='' " +
	"connect_timeout=0 sslmode=disable target_session_attrs=any min_protocol_version=3.0 " +
	"max_protocol_version=3.0 channel_binding=prefer require_auth='' statement_cache_capacity=512 " +
	"description_cache_capacity=512 default_query_exec_mode=cache_statement"

// pgSSLModes is the closed list a STORED value may hold. "prefer" is the legacy value of the beta UI;
// an absent key is handled before this lookup.
var pgSSLModes = map[string]bool{"disable": true, "require": true, "verify-ca": true, "verify-full": true, "prefer": true}

func (p pgProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	host, port := hostPort(m.Target, 5432)

	mode, present := m.Config["sslmode"]
	if !present {
		mode = "prefer" // legacy rows only; see the type comment
	} else if !pgSSLModes[mode] {
		// Checked before anything else and never echoed: an explicit "", "allow", "REQUIRE" or a mode
		// with a second key ("require host=…") ends here, with no parse and no dial (S16).
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad config: unsupported sslmode"}
	}

	cfg, err := pgx.ParseConfig(pgConstantConnString)
	if err != nil {
		// The constant cannot fail on its own; only the environment can break it (a PGSERVICE naming a
		// missing file or service). pgx's text quotes the connection string, so it is not passed on.
		return Result{Connected: false, LatencyMS: elapsedMS(start),
			Msg: "bad config: the PG* environment of the prober breaks the PostgreSQL client"}
	}
	tlsConfig, fallbacks := pgTLS(mode, host, port, p.roots)
	resetPgConfig(cfg, pgConnSpec{
		host: host, port: port,
		user: m.Config["username"], password: m.Config["password"], database: m.Config["database"],
		tls: tlsConfig, fallbacks: fallbacks,
		dial: p.dial, // route every TCP dial, the fallback's included, through the SSRF guard
	})

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: pgConnectMessage(mode, err)}
	}
	defer func() { _ = conn.Close(ctx) }()

	query := m.Config["query"]
	if strings.TrimSpace(query) == "" {
		query = "SELECT 1"
	}
	if _, err := conn.Exec(ctx, query); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	return Result{Connected: true, LatencyMS: elapsedMS(start)}
}

// pgTLS builds the TLS configuration for a mode, with pgx's own ServerName rules (configTLS with
// sslsni on): a host name is sent as SNI, an IP literal is not (RFC 6066). verify-full always names
// the target, so an IP target is verified against the certificate's IP SANs. "prefer" is "require"
// plus ONE plaintext fallback to the same host and port — never another address.
func pgTLS(mode, host string, port uint16, roots *x509.CertPool) (*tls.Config, []*pgconn.FallbackConfig) {
	sni := host
	if net.ParseIP(host) != nil {
		sni = ""
	}
	switch mode {
	case "disable":
		return nil, nil
	case "verify-full":
		return &tls.Config{ServerName: host, RootCAs: roots}, nil
	case "verify-ca":
		c := &tls.Config{ServerName: sni, InsecureSkipVerify: true} //nolint:gosec // the chain is verified below, without the name, as pgx's verify-ca does
		c.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, 0, len(raw))
			for _, der := range raw {
				cert, err := x509.ParseCertificate(der)
				if err != nil {
					return fmt.Errorf("failed to parse certificate from server: %w", err)
				}
				certs = append(certs, cert)
			}
			if len(certs) == 0 {
				return errors.New("the server presented no certificate")
			}
			opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
			for _, cert := range certs[1:] {
				opts.Intermediates.AddCert(cert)
			}
			_, err := certs[0].Verify(opts)
			return err
		}
		return c, nil
	case "prefer":
		return &tls.Config{ServerName: sni, InsecureSkipVerify: true}, //nolint:gosec // prefer/require do not verify the server, by definition
			[]*pgconn.FallbackConfig{{Host: host, Port: port}}
	default: // "require"
		return &tls.Config{ServerName: sni, InsecureSkipVerify: true}, nil //nolint:gosec // require does not verify the server, by definition
	}
}

// pgConnSpec is everything the probe decides about a connection.
type pgConnSpec struct {
	host, user, password, database string
	port                           uint16
	tls                            *tls.Config
	fallbacks                      []*pgconn.FallbackConfig
	dial                           func(ctx context.Context, network, addr string) (net.Conn, error)
}

// pgResetFields names every exported field of pgconn.Config and pgx.ConnConfig that resetPgConfig
// owns or deliberately keeps. TestPostgresConfigResetCoversEveryField compares it with the structs by
// reflection, so a pgx upgrade that adds a field fails that test until this function decides it.
var pgResetFields = []string{
	"Host", "Port", "Database", "User", "Password", "TLSConfig", "Fallbacks", "ConnectTimeout", "DialFunc",
	"LookupFunc", "BuildFrontend", "BuildContextWatcherHandler", "OnPgError", // kept: the parse's, no environment input
	"RuntimeParams", "KerberosSrvName", "KerberosSpn", "SSLNegotiation",
	"AfterNetConnect", "ValidateConnect", "AfterConnect", "OnNotice", "OnNotification", "OAuthTokenProvider",
	"MinProtocolVersion", "MaxProtocolVersion", "ChannelBinding", "RequireAuth",
	"Tracer", "StatementCacheCapacity", "DescriptionCacheCapacity", "DefaultQueryExecMode",
}

// resetPgConfig sets, after the parse, every field that decides where, how and as whom the probe
// connects (sec-hardening-3 §2.6 item 3). It is a function over a parsed base, not inline code, so a
// test can run it on a base in which every field is poisoned: deleting any line here fails that test.
func resetPgConfig(cfg *pgx.ConnConfig, s pgConnSpec) {
	cfg.Host, cfg.Port = s.host, s.port
	cfg.User, cfg.Password, cfg.Database = s.user, s.password, s.database
	cfg.TLSConfig, cfg.Fallbacks = s.tls, s.fallbacks
	cfg.ConnectTimeout = 0 // the probe's context bounds the attempt
	cfg.DialFunc = s.dial
	// LookupFunc, BuildFrontend, BuildContextWatcherHandler and OnPgError stay the parse's: they take
	// nothing from the environment, and every address LookupFunc returns is dialed through DialFunc.
	cfg.RuntimeParams = map[string]string{}
	cfg.KerberosSrvName, cfg.KerberosSpn = "", ""
	cfg.SSLNegotiation = "" // an SSLRequest first; "direct" would skip it
	cfg.AfterNetConnect = nil
	cfg.ValidateConnect = nil
	cfg.AfterConnect = nil
	cfg.OnNotice = nil
	cfg.OnNotification = nil
	cfg.OAuthTokenProvider = nil
	cfg.MinProtocolVersion, cfg.MaxProtocolVersion = "3.0", "3.0"
	cfg.ChannelBinding = "prefer"
	cfg.RequireAuth = ""
	cfg.Tracer = nil
	cfg.StatementCacheCapacity, cfg.DescriptionCacheCapacity = 512, 512
	cfg.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
}

// pgConnectMessage names the cause of a TLS failure under the modes that demand TLS (§8 decision 2).
// The mode comes from the closed list, so nothing a user wrote is echoed.
func pgConnectMessage(mode string, err error) string {
	if mode == "require" || mode == "verify-ca" || mode == "verify-full" {
		// pgx returns this one as an unexported errors.New, so its text is the only handle on it.
		if strings.Contains(err.Error(), "server refused TLS connection") {
			return fmt.Sprintf("the server does not offer TLS, and sslmode is %q: set sslmode to \"disable\" if plaintext is intended", mode)
		}
		if certErr := pgCertificateError(err); certErr != nil {
			return fmt.Sprintf("TLS certificate not accepted (sslmode %q): %s", mode, certErr.Error())
		}
	}
	return err.Error()
}

// pgCertificateError finds the x509 failure in a connect error: crypto/tls wraps it in a
// CertificateVerificationError under verify-full, and verify-ca's own callback returns it bare.
func pgCertificateError(err error) error {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return verification.Err
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return unknown
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return hostname
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return invalid
	}
	return nil
}

// hostPort splits a "host:port" target, defaulting the port and tolerating a
// bare host. A bracketed IPv6 literal without a port ("[::1]") loses its brackets, so they never
// reach a dial, a TLS ServerName or an IP-literal test.
func hostPort(target string, defaultPort uint16) (string, uint16) {
	target = strings.TrimSpace(target)
	if h, p, err := net.SplitHostPort(target); err == nil {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil {
			return h, uint16(n)
		}
		return h, defaultPort
	}
	if len(target) > 2 && strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		return target[1 : len(target)-1], defaultPort
	}
	return target, defaultPort
}
