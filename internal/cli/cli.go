// Package cli is the cerbix command-line entrypoint. It dispatches subcommands
// and wires the operational server for the selected role.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/teamlead-com/cerbix/internal/agent"
	"github.com/teamlead-com/cerbix/internal/api"
	"github.com/teamlead-com/cerbix/internal/auth"
	"github.com/teamlead-com/cerbix/internal/buildinfo"
	"github.com/teamlead-com/cerbix/internal/config"
	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/events"
	"github.com/teamlead-com/cerbix/internal/httpsrv"
	"github.com/teamlead-com/cerbix/internal/ingest"
	"github.com/teamlead-com/cerbix/internal/logging"
	"github.com/teamlead-com/cerbix/internal/mailer"
	"github.com/teamlead-com/cerbix/internal/metrics"
	"github.com/teamlead-com/cerbix/internal/mqadmin"
	"github.com/teamlead-com/cerbix/internal/notify"
	"github.com/teamlead-com/cerbix/internal/outbox"
	"github.com/teamlead-com/cerbix/internal/prober"
	"github.com/teamlead-com/cerbix/internal/scheduler"
	"github.com/teamlead-com/cerbix/internal/secret"
	"github.com/teamlead-com/cerbix/internal/settings"
	"github.com/teamlead-com/cerbix/internal/store"
	"github.com/teamlead-com/cerbix/internal/subscribe"
	"github.com/teamlead-com/cerbix/internal/web"
	"github.com/teamlead-com/cerbix/internal/webhook"
	"github.com/teamlead-com/cerbix/internal/worker"
)

// validRoles are the process roles cerbix can run as. "all" runs every role in
// a single process (local dev); in production they are separate deployments.
var validRoles = map[string]bool{
	"all":       true,
	"api":       true,
	"scheduler": true,
	"worker":    true,
	"agent":     true,
}

const (
	dbPingInterval = 10 * time.Second
	workerPoolSize = 4
)

// Main runs the CLI and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "version":
		return runVersion()
	case "serve":
		return runServe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "reencrypt":
		return runReencrypt(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "cerbix — internal uptime & SLA monitoring")
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  cerbix serve --config <path> [--role all|api|scheduler|worker|agent] [--region <name>]")
	fmt.Fprintln(w, "  cerbix migrate --config <path>")
	fmt.Fprintln(w, "  cerbix reencrypt --config <path>")
	fmt.Fprintln(w, "  cerbix version")
}

func runVersion() int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(buildinfo.Current())
	return 0
}

// loadConfig loads and validates the config, logging a CRITICAL line and
// returning nil on failure (strict-only: fail fast before any runtime wiring).
// rabbitManagementClient builds the RabbitMQ management client for the region
// picker: the explicit management_url if set, else derived from the AMQP url. Nil
// (no error) when no RabbitMQ is configured.
func rabbitManagementClient(cfg config.RabbitMQConfig) (*mqadmin.Client, error) {
	switch {
	case cfg.ManagementURL != "":
		return mqadmin.New(cfg.ManagementURL)
	case cfg.URL != "":
		return mqadmin.FromAMQP(cfg.URL)
	default:
		return nil, nil
	}
}

// agentLiveWindow is how recently a pull agent must have heartbeated for its region to
// count as live (agents heartbeat every ~15s).
const agentLiveWindow = 45 * time.Second

// liveRegionsUnion reports a region live if it has a RabbitMQ consumer (mgmt, optional)
// OR a recent pull-agent heartbeat — so the region picker and region-worker alert cover
// both transports. A mgmt lookup error propagates (callers skip on error); the agent
// lookup is best-effort.
type liveRegionsUnion struct {
	mgmt interface {
		LiveJobRegions(context.Context) (map[string]bool, error)
	}
	store *store.Store
}

// newLiveRegions builds the union source (nil-safe on the optional mgmt client).
func newLiveRegions(mgmt *mqadmin.Client, st *store.Store) liveRegionsUnion {
	u := liveRegionsUnion{store: st}
	if mgmt != nil {
		u.mgmt = mgmt
	}
	return u
}

func (u liveRegionsUnion) LiveJobRegions(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if u.mgmt != nil {
		m, err := u.mgmt.LiveJobRegions(ctx)
		if err != nil {
			return nil, err
		}
		for k := range m {
			out[k] = true
		}
	}
	if ag, err := u.store.LiveAgentRegions(ctx, agentLiveWindow); err == nil {
		for k := range ag {
			out[k] = true
		}
	}
	return out, nil
}

// localTester adapts a local prober to api.RegionTester for the inproc (--role=all)
// build, where region is cosmetic and the probe runs in-process.
type localTester struct {
	run func(ctx context.Context, m domain.Monitor) domain.Heartbeat
}

func (l localTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	return l.run(ctx, m), nil
}

// pullTester runs "Test connection" for a pull-served region: it enqueues a one-off
// test, the region's agent claims and probes it, and this polls for the result — the
// pull-transport equivalent of the AMQP test-RPC.
type pullTester struct{ store *store.Store }

func (p pullTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	payload, err := json.Marshal(dispatch.CheckJob{Monitor: m})
	if err != nil {
		return domain.Heartbeat{}, err
	}
	ttl := m.TimeoutSeconds + 6 // agent poll (~1s) + probe + result post + margin
	if ttl <= 0 {
		ttl = 26
	}
	id, err := p.store.EnqueuePullTest(ctx, m.Region, payload, ttl)
	if err != nil {
		return domain.Heartbeat{}, err
	}
	deadline := time.NewTimer(time.Duration(ttl) * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return domain.Heartbeat{}, ctx.Err()
		case <-deadline.C:
			return domain.Heartbeat{}, fmt.Errorf("no agent responded in region %q", m.Region)
		case <-poll.C:
			raw, ok, err := p.store.GetPullTestResult(ctx, id)
			if err != nil {
				return domain.Heartbeat{}, err
			}
			if ok {
				var hb domain.Heartbeat
				if err := json.Unmarshal(raw, &hb); err != nil {
					return domain.Heartbeat{}, err
				}
				return hb, nil
			}
		}
	}
}

// regionRoutedTester dispatches "Test connection" by region: pull regions go to the
// pull tester (HTTP → agent), everything else to the AMQP fallback tester.
type regionRoutedTester struct {
	pullRegions map[string]bool
	pull        api.RegionTester
	fallback    api.RegionTester
}

func (t regionRoutedTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	region := m.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	if t.pullRegions[region] {
		return t.pull.RunTest(ctx, m)
	}
	return t.fallback.RunTest(ctx, m)
}

func loadConfig(path string) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		logging.Critical(logging.New(config.LogConfig{Level: "info", Format: "json"}, os.Stderr),
			"config_load_failed", "path", path, "error", err.Error())
		return nil
	}
	return cfg
}

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config YAML (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "migrate: --config is required")
		return 2
	}
	cfg := loadConfig(*configPath)
	if cfg == nil {
		return 1
	}
	logger := logging.New(cfg.Log, os.Stdout)
	if cfg.Database.DSN == "" {
		logging.Critical(logger, "migrate_requires_database", "hint", "set database.dsn")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, cfg.Database.DSN); err != nil {
		logging.Critical(logger, "db_migrate_failed", "error", err.Error())
		return 1
	}
	logger.Info("migrations_applied")
	return 0
}

// runReencrypt rewrites every stored secret under the current primary encryption
// key, using the full keyring (previous_keys) to read data still under an old key.
// Run it after rotating: set the new key as encryption_key, move the old to
// previous_keys, run this, then drop the old key.
func runReencrypt(args []string) int {
	fs := flag.NewFlagSet("reencrypt", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config YAML (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "reencrypt: --config is required")
		return 2
	}
	cfg := loadConfig(*configPath)
	if cfg == nil {
		return 1
	}
	logger := logging.New(cfg.Log, os.Stdout)
	if cfg.Database.DSN == "" {
		logging.Critical(logger, "reencrypt_requires_database", "hint", "set database.dsn")
		return 1
	}
	keys, err := cfg.Security.Keys()
	if err != nil {
		logging.Critical(logger, "encryption_key_invalid", "error", err.Error())
		return 1
	}
	if keys == nil {
		logging.Critical(logger, "reencrypt_requires_key", "hint", "set security.encryption_key")
		return 1
	}
	cipher, err := secret.New(keys...)
	if err != nil {
		logging.Critical(logger, "cipher_init_failed", "error", err.Error())
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database.DSN)
	if err != nil {
		logging.Critical(logger, "db_connect_failed", "error", err.Error())
		return 1
	}
	defer st.Close()
	st.WithCipher(cipher)
	webhooks, channels, err := st.ReencryptSecrets(ctx)
	if err != nil {
		logging.Critical(logger, "reencrypt_failed", "error", err.Error())
		return 1
	}
	logger.Info("reencrypt_complete", "webhooks", webhooks, "channels", channels)
	return 0
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config YAML (required)")
	role := fs.String("role", "all", "process role: all|api|scheduler|worker|agent")
	region := fs.String("region", "", "worker pool region (worker role); empty = core")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "serve: --config is required")
		return 2
	}
	if !validRoles[*role] {
		fmt.Fprintf(os.Stderr, "serve: invalid --role %q\n", *role)
		return 2
	}

	cfg := loadConfig(*configPath)
	if cfg == nil {
		return 1
	}

	logger := logging.New(cfg.Log, os.Stdout)
	info := buildinfo.Current()
	registry := metrics.New(info, *role)
	// In-process realtime bus: ingest publishes status changes, the SSE handler
	// streams them. Single-process (front with Redis pub/sub for multi-replica).
	broker := events.NewBroker()

	logger.Info("starting",
		"role", *role,
		"version", info.Version,
		"commit", info.Commit,
		"listen", cfg.Server.Listen)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The HTTP-pull agent is DB-less and broker-less: it only needs the prober and
	// outbound HTTPS to the central API. Handle it before any DB/auth/dispatcher setup.
	if *role == "agent" {
		return runAgent(ctx, cfg, *region, registry, logger)
	}

	// Database wiring. Configured DB → migrate + connect (fail-fast, no
	// self-healing); readiness then tracks live connectivity. No DB → scaffold
	// mode, ready immediately.
	var st *store.Store
	if cfg.Database.DSN != "" {
		if err := store.Migrate(ctx, cfg.Database.DSN); err != nil {
			logging.Critical(logger, "db_migrate_failed", "error", err.Error())
			return 1
		}
		opened, err := store.Open(ctx, cfg.Database.DSN)
		if err != nil {
			logging.Critical(logger, "db_connect_failed", "error", err.Error())
			return 1
		}
		st = opened
		defer st.Close()
		// Secret-at-rest encryption (validated in config; empty key = disabled).
		if keys, err := cfg.Security.Keys(); err != nil {
			logging.Critical(logger, "encryption_key_invalid", "error", err.Error())
			return 1
		} else if keys != nil {
			cipher, err := secret.New(keys...)
			if err != nil {
				logging.Critical(logger, "cipher_init_failed", "error", err.Error())
				return 1
			}
			st.WithCipher(cipher)
			logger.Info("secret_encryption_enabled", "keys", len(keys))
		}
		registry.SetDatabaseUp(true)
		logger.Info("database_connected")
		go pingDatabase(ctx, st, registry, logger)
	} else {
		logger.Info("database_disabled", "mode", "scaffold")
	}

	// Transactional outbox delivery. Incident webhooks and monitor-transition
	// notifications are enqueued by the store in the same transaction as the state
	// change; this worker delivers them with retry/backoff. Safe to run on every
	// role/replica that has a database (claims use FOR UPDATE SKIP LOCKED).
	// Instance-wide settings (branding, auth policy, alerting, monitor defaults, mail),
	// resolved DB→config→defaults and served from an atomic snapshot. Shared by the
	// outbox (alert silence), auth (login policy + mailer), and the API. The mailer
	// resolves its SMTP endpoint per send from these settings (live-reconfigurable).
	var settingsSvc *settings.Service
	var mail *mailer.Mailer
	if st != nil {
		settingsSvc = settings.New(st, settings.Bootstrap{
			MinPasswordLen:    cfg.Local.MinPasswordLength,
			SessionTTLSeconds: int(cfg.Session.TTL.Std().Seconds()),
			Mail: domain.MailSettings{
				Enabled: cfg.Mail.Enabled(), SMTPHost: cfg.Mail.SMTPHost, SMTPPort: cfg.Mail.SMTPPort,
				SMTPUsername: cfg.Mail.SMTPUsername, SMTPPassword: cfg.Mail.SMTPPassword,
				From: cfg.Mail.From, PublicBaseURL: cfg.Mail.PublicBaseURL,
			},
		}, logger)
		settingsSvc.Start(ctx)
		mail = mailer.NewLive(func() mailer.Settings {
			m := settingsSvc.Mail()
			return mailer.Settings{
				Enabled: m.Deliverable(), Host: m.SMTPHost, Port: m.Port(), Username: m.SMTPUsername,
				Password: m.SMTPPassword, From: m.From, PublicBaseURL: m.PublicBaseURL,
			}
		})

		subs := subscribe.New(st, mail)
		deliverer := incidentFanout{hooks: webhook.New(st, nil), subs: subs, logger: logger}
		go outbox.New(st, deliverer, notify.New(st, nil), registry, logger).
			WithMailer(mail).
			WithSilence(func() bool { return settingsSvc.Alerting().Silenced(time.Now()) }).
			Run(ctx)
	}

	// RabbitMQ management lookup (which worker pools have a live consumer): used by the
	// region picker (API) and region-worker alerting (scheduler). Best-effort — derived
	// from the AMQP URL unless overridden; nil when no broker is configured.
	mgmt, merr := rabbitManagementClient(cfg.RabbitMQ)
	if merr != nil {
		logger.Warn("mqadmin_init_failed", "error", merr.Error())
	}

	// Auth + API wiring. Requires a database (sessions, JIT users, and the OIDC
	// override all live in Postgres). OIDC is built asynchronously by StartOIDC —
	// discovery is a network call that must not block or crash startup — and can be
	// (re)configured at runtime from the Settings UI.
	var app http.Handler
	var apiHandler *api.Handler
	if st != nil {
		authn, err := auth.New(ctx, cfg, st, logger)
		if err != nil {
			logging.Critical(logger, "auth_init_failed", "error", err.Error())
			return 1
		}
		if mail != nil {
			authn.WithMailer(mail) // enables self-service password reset
		}
		if err := authn.EnsureBootstrapAdmin(ctx); err != nil {
			logging.Critical(logger, "bootstrap_admin_failed", "error", err.Error())
			return 1
		}
		authn.StartOIDC(ctx) // first sync + background reloader (non-fatal, retrying)
		authn.WithSettings(settingsSvc)
		appMux := http.NewServeMux()
		authn.Routes(appMux)
		apiHandler = api.New(st, logger, cfg.Local.MinPasswordLength).WithMetrics(registry).WithEvents(broker).WithOIDC(authn).WithSettings(settingsSvc)
		if mail != nil {
			apiHandler.WithMailer(mail)
		}
		// Region picker liveness = RabbitMQ consumers (mgmt) ∪ recent pull-agent heartbeats.
		apiHandler.WithLiveRegions(newLiveRegions(mgmt, st))
		// HTTP-pull agent endpoints self-authenticate with the agent token(s) and are
		// mounted outside the session-auth middleware (agents are not users). Auth is a
		// catch-all token and/or per-region tokens (an agent token scoped to its region).
		// Enable the pull transport (agent endpoints) when any pull config is present:
		// config tokens, per-region agents, or pull-served regions (database-managed
		// agent tokens can be issued at runtime regardless).
		if cfg.Pull.Token != "" || len(cfg.Pull.Agents) > 0 || len(cfg.Pull.Regions) > 0 {
			regionTokens := make(map[string]string, len(cfg.Pull.Agents))
			for _, a := range cfg.Pull.Agents {
				if a.Region != "" && a.Token != "" {
					regionTokens[a.Region] = a.Token
				}
			}
			// Long-poll wake source: LISTEN pull_jobs, fan out to held /agent/jobs requests.
			notifier := st.NewPullNotifier(logger)
			go notifier.Run(ctx)
			apiHandler.WithAgentToken(cfg.Pull.Token).WithAgentRegionTokens(regionTokens).
				WithAgentDBTokens().WithPullWaiter(notifier)
			appMux.Handle("/api/v1/agent/", apiHandler.AgentRouter())
		}
		appMux.Handle("/api/", authn.RequireAuth(apiHandler.Router()))
		// Public status-page rendering is unauthenticated; its more-specific prefix
		// takes precedence over the auth-gated /api/ handler (ServeMux longest-match).
		appMux.Handle("/api/v1/public/", apiHandler.PublicRouter())
		spa, err := web.New()
		if err != nil {
			logging.Critical(logger, "web_init_failed", "error", err.Error())
			return 1
		}
		appMux.Handle("/", spa) // catch-all: serve the embedded SPA
		// Access log around the whole app: API/auth at INFO, static at DEBUG.
		app = logging.AccessLog(logger, appMux)
		logger.Info("auth_enabled", "oidc_active", authn.OIDCActive(), "local", cfg.Local.Enabled)
	}

	// Checking pipeline. --role=all runs every role in one process over the
	// in-process dispatcher; distributed roles (api|scheduler|worker) use the
	// RabbitMQ dispatcher and each run only their part.
	var disp dispatch.Dispatcher
	switch {
	case *role == "all" && st != nil:
		disp = dispatch.NewInProc(0)
	case *role == "api" || *role == "scheduler" || *role == "worker":
		if cfg.RabbitMQ.URL == "" {
			logging.Critical(logger, "rabbitmq_required", "role", *role,
				"hint", "set rabbitmq.url for distributed roles")
			return 1
		}
		amqpd, err := dispatch.NewAMQP(cfg.RabbitMQ.URL, logger)
		if err != nil {
			logging.Critical(logger, "rabbitmq_connect_failed", "error", err.Error())
			return 1
		}
		// A worker consumes only its region's jobs queue (checks.jobs.<region>);
		// harmless no-op for scheduler/api (they don't consume jobs).
		amqpd.WithJobRegion(*region)
		amqpd.WithBrokerState(registry.SetBrokerUp) // cerbix_broker_up gauge
		disp = amqpd
	}
	if disp != nil {
		defer disp.Close()
		// The push endpoint publishes heartbeats into the same pipeline.
		if apiHandler != nil {
			apiHandler.WithResultSink(disp)
		}
		runner := prober.NewRunnerWithGuard(prober.NewGuard(cfg.Prober.AllowPrivateIPs, cfg.Prober.AllowMetadataIPs))
		if st != nil {
			runner.WithChildStatus(st.MonitorStatuses) // composite monitors read child statuses
		}
		if apiHandler != nil {
			// "Test connection" runs in the target region. Pull regions dispatch the probe
			// to their agent over HTTP (pull_tests); AMQP regions dispatch it to a worker
			// via a RabbitMQ RPC; inproc (--role=all) runs non-pull regions locally. The
			// pull routing wraps EITHER base so pull-region tests reach the agent even in
			// --role=all (matching how pull jobs are routed) — strict region affinity, no
			// silent fallback to the local prober.
			var base api.RegionTester
			if amqpd, ok := disp.(*dispatch.AMQP); ok {
				base = amqpd
			} else {
				base = localTester{run: runner.Run}
			}
			pullSet := make(map[string]bool, len(cfg.Pull.Regions))
			for _, r := range cfg.Pull.Regions {
				if r != "" {
					pullSet[r] = true
				}
			}
			if len(pullSet) > 0 && st != nil {
				apiHandler.WithTester(regionRoutedTester{pullRegions: pullSet, pull: pullTester{store: st}, fallback: base})
			} else {
				apiHandler.WithTester(base)
			}
		}
		startIngest := func() {
			ing := ingest.New(st, disp, registry, logger).WithEvents(broker)
			go ing.Run(ctx)
		}
		// Confirm-phase wake signals for the scheduler leader (LISTEN monitor_confirm):
		// a counted failure reschedules the next probe at the confirm interval
		// immediately, instead of waiting for the snapshot refresh.
		confirmSignals := func() <-chan string {
			cn := st.NewConfirmNotifier(logger)
			go cn.Run(ctx)
			ch, _ := cn.Subscribe() // scheduler lives for the whole process — no unsubscribe
			return ch
		}
		switch *role {
		case "all":
			go scheduler.New(st, disp, logger).WithRetentionDays(cfg.Heartbeats.RetentionDays).
				WithPullRegions(cfg.Pull.Regions). // pull-region jobs → pull_jobs (agent claims), NOT the in-proc worker
				WithPullMetrics(registry).         // per-region pull-queue depth/lag gauges
				WithLeaderState(registry).         // cerbix_scheduler_leader gauge
				WithConfirmSignals(confirmSignals()).Run(ctx)
			go worker.New(disp, runner, workerPoolSize, logger).Run(ctx)
			startIngest()
		case "scheduler":
			if st == nil {
				logging.Critical(logger, "scheduler_requires_database", "hint", "set database.dsn")
				return 1
			}
			sch := scheduler.New(st, disp, logger).WithRetentionDays(cfg.Heartbeats.RetentionDays).
				WithPullRegions(cfg.Pull.Regions).   // pull-served regions get jobs via pull_jobs, not AMQP
				WithPullMetrics(registry).           // per-region pull-queue depth/lag gauges
				WithLeaderState(registry).           // cerbix_scheduler_leader gauge
				WithConfirmSignals(confirmSignals()) // accelerated failure-confirmation probes
			// Alert when a region with enabled monitors loses its worker/agent. Liveness
			// unions RabbitMQ consumers with recent pull-agent heartbeats.
			sch.WithLiveRegions(newLiveRegions(mgmt, st))
			go sch.Run(ctx)
		case "worker":
			// Answer region-scoped "Test connection" RPCs for this worker's region.
			if amqpd, ok := disp.(*dispatch.AMQP); ok {
				amqpd.ServeTests(runner.Run)
			}
			go worker.New(disp, runner, workerPoolSize, logger).Run(ctx)
		case "api":
			if st == nil {
				logging.Critical(logger, "api_requires_database", "hint", "set database.dsn")
				return 1
			}
			startIngest()
		}
		workerRegion := *region
		if workerRegion == "" {
			workerRegion = domain.DefaultRegion
		}
		logger.Info("checking_pipeline_started", "role", *role, "workers", workerPoolSize, "region", workerRegion)
	}

	srv := httpsrv.New(cfg.Server, registry, app)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	registry.SetReady(true, "")

	select {
	case <-ctx.Done():
		logger.Info("shutdown_signal_received")
	case err := <-errCh:
		if err != nil {
			logging.Critical(logger, "http_server_failed", "error", err.Error())
			return 1
		}
	}

	registry.SetReady(false, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful_shutdown_failed", "error", err.Error())
		return 1
	}
	logger.Info("stopped")
	return 0
}

// runAgent runs the HTTP-pull prober role: claim jobs for --region from the central
// API, probe them, post results, heartbeat. It serves only the ops endpoints
// (health/readyz/metrics) and blocks until ctx is cancelled.
func runAgent(ctx context.Context, cfg *config.Config, region string, registry *metrics.Registry, logger *slog.Logger) int {
	if cfg.Pull.ServerURL == "" || cfg.Pull.Token == "" {
		logging.Critical(logger, "agent_requires_pull_config", "hint", "set pull.server_url and pull.token for --role agent")
		return 1
	}
	if region == "" {
		region = domain.DefaultRegion
	}
	runner := prober.NewRunnerWithGuard(prober.NewGuard(cfg.Prober.AllowPrivateIPs, cfg.Prober.AllowMetadataIPs))
	go agent.New(cfg.Pull.ServerURL, cfg.Pull.Token, region, runner, logger).Run(ctx)
	logger.Info("agent_role_started", "region", region, "server", cfg.Pull.ServerURL)

	srv := httpsrv.New(cfg.Server, registry, nil)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	registry.SetReady(true, "")
	select {
	case <-ctx.Done():
		logger.Info("shutdown_signal_received")
	case err := <-errCh:
		if err != nil {
			logging.Critical(logger, "http_server_failed", "error", err.Error())
			return 1
		}
	}
	registry.SetReady(false, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful_shutdown_failed", "error", err.Error())
		return 1
	}
	logger.Info("stopped")
	return 0
}

// pingDatabase periodically checks connectivity and reflects it in readiness and
// the cerbix_database_up metric until ctx is cancelled.
func pingDatabase(ctx context.Context, st *store.Store, reg *metrics.Registry, logger *slog.Logger) {
	t := time.NewTicker(dbPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := st.Ping(pctx)
			cancel()
			if err != nil {
				reg.SetDatabaseUp(false)
				reg.SetReady(false, "database unreachable")
				logger.Error("db_ping_failed", "error", err.Error())
				continue
			}
			reg.SetDatabaseUp(true)
			reg.SetReady(true, "")
		}
	}
}

// incidentFanout delivers an incident outbox event to webhooks (whose result
// governs retry) and, best-effort, to confirmed status-page subscribers. It
// satisfies outbox.WebhookDeliverer.
type incidentFanout struct {
	hooks  *webhook.Dispatcher
	subs   *subscribe.Notifier // nil when mail is not configured
	logger *slog.Logger
}

func (f incidentFanout) Deliver(ctx context.Context, ev domain.IncidentEvent) error {
	err := f.hooks.Deliver(ctx, ev)
	if f.subs != nil {
		if e := f.subs.Deliver(ctx, ev); e != nil {
			f.logger.Warn("subscriber_email_failed", "error", e.Error())
		}
	}
	return err
}
