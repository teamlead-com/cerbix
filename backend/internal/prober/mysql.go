package prober

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"git.example.com/monitoring/cerbix/internal/domain"
)

// mysqlGuardDial routes go-sql-driver connections through the SSRF guard. The
// driver's dialer registry is global, so the runner sets this once at build time
// (mutex-guarded so concurrent runner construction / probes are race-free).
var (
	mysqlDialMu    sync.RWMutex
	mysqlGuardDial func(ctx context.Context, addr string) (net.Conn, error)
)

func setMySQLGuardDial(fn func(ctx context.Context, addr string) (net.Conn, error)) {
	mysqlDialMu.Lock()
	mysqlGuardDial = fn
	mysqlDialMu.Unlock()
}

func init() {
	mysql.RegisterDialContext("cerbixguard", func(ctx context.Context, addr string) (net.Conn, error) {
		mysqlDialMu.RLock()
		fn := mysqlGuardDial
		mysqlDialMu.RUnlock()
		if fn == nil {
			return nil, errors.New("mysql guard dialer not configured")
		}
		return fn(ctx, addr)
	})
}

// mysqlProber checks a MySQL/MariaDB server: connect (through the guard) and run a
// query, default "SELECT 1". Config keys: database, username, password (encrypted
// at rest), query. Latency is [RESPONSE_TIME].
type mysqlProber struct{}

func (mysqlProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	host, port := hostPort(m.Target, 3306)
	cfg := mysql.NewConfig()
	cfg.Net = "cerbixguard"
	cfg.Addr = net.JoinHostPort(host, strconv.Itoa(int(port)))
	cfg.User = m.Config["username"]
	cfg.Passwd = m.Config["password"]
	cfg.DBName = m.Config["database"]
	cfg.Timeout = m.Timeout()

	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	query := m.Config["query"]
	if strings.TrimSpace(query) == "" {
		query = "SELECT 1"
	}
	if _, err := db.ExecContext(ctx, query); err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	return Result{Connected: true, LatencyMS: elapsedMS(start)}
}
