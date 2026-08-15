package prober

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// pgProber checks a PostgreSQL server: it connects (through the SSRF guard) and
// runs a query, default "SELECT 1". Success (no conditions) = connect + query
// succeed. Config keys: database, username, password (encrypted at rest),
// sslmode, query. The check latency is [RESPONSE_TIME].
//
// sslmode: reference-based monitors are normalized to `require` by the domain before they
// ever reach here (§4.2/§4.8), so this fallback applies only to rows written before that
// contract existed. It stays "prefer" ON PURPOSE: raising it would silently flip existing
// monitors from opportunistic to mandatory TLS and start failing every plaintext server
// they were pointed at. Changing the transport posture of already-running checks is a
// migration decision, not a prober default.
type pgProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p pgProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	host, port := hostPort(m.Target, 5432)

	sslmode := m.Config["sslmode"]
	if sslmode == "" {
		sslmode = "prefer" // legacy rows only; see the type comment
	}
	cfg, err := pgx.ParseConfig("sslmode=" + sslmode)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: "bad config: " + err.Error()}
	}
	cfg.Host = host
	cfg.Port = port
	cfg.User = m.Config["username"]
	cfg.Password = m.Config["password"]
	cfg.Database = m.Config["database"]
	cfg.DialFunc = p.dial // route the TCP dial through the SSRF guard

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
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

// hostPort splits a "host:port" target, defaulting the port and tolerating a
// bare host.
func hostPort(target string, defaultPort uint16) (string, uint16) {
	target = strings.TrimSpace(target)
	if h, p, err := net.SplitHostPort(target); err == nil {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil {
			return h, uint16(n)
		}
		return h, defaultPort
	}
	return target, defaultPort
}
