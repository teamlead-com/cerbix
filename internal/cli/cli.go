// Package cli is the cerbix command-line entrypoint. It dispatches subcommands
// and wires the operational server for the selected role.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
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
	fpruntime "github.com/teamlead-com/cerbix/internal/fileprovider/runtime"
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

// roleServices is the single owner map for role-specific runtime components.
// Executors intentionally have no core control-plane services: in particular a
// master-less worker must never claim the generic outbox or mount the user API.
// API/scheduler/all are the master-key trust domain and own delivery today; the
// same materializing bit is reserved for the authoritative secret materializer.
type roleServices struct {
	api           bool
	coreDelivery  bool
	materializing bool
}

func servicesForRole(role string) roleServices {
	switch role {
	case "all", "api":
		return roleServices{api: true, coreDelivery: true, materializing: true}
	case "scheduler":
		return roleServices{coreDelivery: true, materializing: true}
	default: // worker/agent (unknown roles are rejected before this is called)
		return roleServices{}
	}
}

func buildCredentialKeyrings(cfg config.DispatchConfig) (dispatch.CredentialKeyrings, error) {
	build := func(path string, kr config.DispatchRegionKeys) (*dispatch.CredentialKeyring, error) {
		primary, err := kr.Primary.Bytes(path + ".primary.key")
		if err != nil {
			return nil, err
		}
		previous := make([]dispatch.CredentialKeyMaterial, 0, len(kr.Previous))
		for i, entry := range kr.Previous {
			key, err := entry.Bytes(fmt.Sprintf("%s.previous[%d].key", path, i))
			if err != nil {
				return nil, err
			}
			previous = append(previous, dispatch.CredentialKeyMaterial{ID: entry.ID, Key: key})
		}
		return dispatch.NewCredentialKeyring(dispatch.CredentialKeyMaterial{ID: kr.Primary.ID, Key: primary}, previous)
	}
	out := dispatch.CredentialKeyrings{Regions: make(map[string]*dispatch.CredentialKeyring, len(cfg.Regions))}
	for region, configured := range cfg.Regions {
		ring, err := build("security.dispatch.regions["+region+"]", configured)
		if err != nil {
			return dispatch.CredentialKeyrings{}, err
		}
		out.Regions[region] = ring
	}
	if cfg.Default != nil {
		ring, err := build("security.dispatch.default", *cfg.Default)
		if err != nil {
			return dispatch.CredentialKeyrings{}, err
		}
		out.Default = ring
	}
	return out, nil
}

const (
	dbPingInterval = 10 * time.Second
	workerPoolSize = 4
	// maxConcurrentReconciles bounds file-provider reconciles running at once, process-wide.
	maxConcurrentReconciles = 4
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
	run         func(ctx context.Context, m domain.Monitor) domain.Heartbeat
	credentials *dispatch.CredentialKeyring
}

func (l localTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	return l.run(ctx, m), nil
}

func (l localTester) RunJobTest(ctx context.Context, job dispatch.CheckJob) (domain.Heartbeat, error) {
	m := job.Monitor
	cleanup := func() {}
	if job.CredentialEnvelope != nil {
		if l.credentials == nil {
			return domain.Heartbeat{}, domain.ProbeError{Reason: domain.ProbeErrorNoDispatchKey}
		}
		var err error
		m, cleanup, err = l.credentials.MaterializeForProbe(job)
		if err != nil {
			return domain.Heartbeat{}, domain.ProbeError{Reason: dispatch.CredentialProbeErrorReason(err)}
		}
	}
	hb := l.run(ctx, m)
	cleanup()
	return hb, nil
}

// pullTester runs "Test connection" for a pull-served region: it enqueues a one-off
// test, the region's agent claims and probes it, and this polls for the result — the
// pull-transport equivalent of the AMQP test-RPC.
type pullTester struct{ store *store.Store }

func (p pullTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	return p.RunJobTest(ctx, dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV1})
}

func (p pullTester) RunJobTest(ctx context.Context, job dispatch.CheckJob) (domain.Heartbeat, error) {
	m := job.Monitor
	payload, err := json.Marshal(job)
	if err != nil {
		return domain.Heartbeat{}, err
	}
	ttl := m.TimeoutSeconds + 6 // agent poll (~1s) + probe + result post + margin
	if ttl <= 0 {
		ttl = 26
	}
	var id string
	if job.ProtocolVersion == dispatch.ProtocolV2 {
		id, err = p.store.EnqueuePullTestV2(ctx, m.Region, payload, ttl)
	} else {
		id, err = p.store.EnqueuePullTest(ctx, m.Region, payload, ttl)
	}
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
				if hb.ProbeError != nil {
					return domain.Heartbeat{}, *hb.ProbeError
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
	pull        jobRegionTester
	fallback    jobRegionTester
}

type jobRegionTester interface {
	RunJobTest(ctx context.Context, job dispatch.CheckJob) (domain.Heartbeat, error)
}

func (t regionRoutedTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	return t.RunJobTest(ctx, dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV1})
}

func (t regionRoutedTester) RunJobTest(ctx context.Context, job dispatch.CheckJob) (domain.Heartbeat, error) {
	region := job.Monitor.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	if t.pullRegions[region] {
		return t.pull.RunJobTest(ctx, job)
	}
	return t.fallback.RunJobTest(ctx, job)
}

type materializingTester struct {
	store  *store.Store
	routes jobRegionTester
}

func (t materializingTester) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	item, err := t.store.MaterializeTestExecutionConfig(ctx, m)
	if err != nil {
		return domain.Heartbeat{}, err
	}
	if item.Reason != "" {
		return domain.Heartbeat{}, fmt.Errorf("credential test rejected: %s", item.Reason)
	}
	return t.routes.RunJobTest(ctx, item.Job)
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

// notificationEgressGuard builds the SSRF guard for OUTBOUND alert delivery
// (webhook/Slack/notify HTTP + SMTP) from the notification_egress policy — which
// defaults to deny-private — NOT the prober policy (which allows private for
// operator-chosen probe targets). Isolated so a regression that rewired delivery back
// to cfg.Prober is caught by a test rather than reopening SSRF silently.
func notificationEgressGuard(cfg *config.Config) prober.Guard {
	return prober.NewGuard(cfg.NotificationEgress.AllowPrivateIPs, cfg.NotificationEgress.AllowMetadataIPs)
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
	// Role-dependent secret-inventory key presence/absence (spec func-secret-inventory
	// §4.1/§4.7): structural keyring validity is checked in config.Validate; which keys
	// this role must (or must NOT) hold is a pure config-owned rule — fail-fast here,
	// before any runtime wiring, for every role including the DB-less agent.
	if err := cfg.ValidateSecretsForRole(*role, *region); err != nil {
		logging.Critical(logger, "secrets_role_config_invalid", "role", *role, "error", err.Error())
		return 1
	}
	credentialRings, err := buildCredentialKeyrings(cfg.Security.Dispatch)
	if err != nil {
		logging.Critical(logger, "dispatch_keyring_init_failed", "error", err.Error())
		return 1
	}
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

	// bg tracks the long-lived background goroutines (outbox, ingest, scheduler,
	// worker, LISTEN notifiers) so shutdown DRAINS them — waits for each Run(ctx) to
	// return after ctx is cancelled — instead of exiting while one is mid-write.
	var bg sync.WaitGroup
	spawn := func(f func()) {
		bg.Add(1)
		go func() { defer bg.Done(); f() }()
	}

	// The HTTP-pull agent is DB-less and broker-less: it only needs the prober and
	// outbound HTTPS to the central API. Handle it before any DB/auth/dispatcher setup.
	if *role == "agent" {
		return runAgent(ctx, cfg, *region, credentialRings, registry, logger)
	}
	owned := servicesForRole(*role)

	// Database wiring. Configured DB → migrate + connect (fail-fast, no
	// self-healing); readiness then tracks live connectivity. No DB → scaffold
	// mode, ready immediately.
	var st *store.Store
	if cfg.Database.DSN != "" {
		if err := store.Migrate(ctx, cfg.Database.DSN); err != nil {
			logging.Critical(logger, "db_migrate_failed", "error", err.Error())
			return 1
		}
		// Only api/all actually spawn file-provider leaders (see startFileProviders), so only
		// those roles need the pool sized for the leader pins; other roles stay at the floor.
		fpCount := 0
		if *role == "all" || *role == "api" {
			fpCount = len(cfg.Providers.File)
		}
		opened, err := store.Open(ctx, cfg.Database.DSN, store.WithFileProviderPool(fpCount, maxConcurrentReconciles))
		if err != nil {
			logging.Critical(logger, "db_connect_failed", "error", err.Error())
			return 1
		}
		st = opened
		// Log the ACTUAL configured cap (an operator may set a larger valid pool_max_conns)
		// alongside the computed minimum, so the line never understates the real pool size.
		logger.Info("db_pool_sized",
			"actual", st.PoolMaxConns(),
			"required_min", store.RequiredMaxConns(fpCount, maxConcurrentReconciles),
			"file_providers", fpCount)
		defer st.Close()
		// Result-ingest timestamp policy (spec func-result-protocol): skew bound + the
		// retention floor below which a result is ignored (= the raw heartbeat window).
		st.WithResultPolicy(cfg.Result.AllowedSkew.Std(), time.Duration(cfg.Heartbeats.RetentionDays)*24*time.Hour)
		st.WithResultRevisionMode(cfg.Result.RevisionMode)
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
			// Readiness-gated one-time backfill: encrypt any push token still stored as
			// plaintext (migration 00053 seed). Fail-fast — never serve with a plaintext
			// bearer token when a key is configured. Idempotent + concurrency-safe.
			if n, err := st.BackfillPushTokenEnc(ctx); err != nil {
				logging.Critical(logger, "push_token_backfill_failed", "error", err.Error())
				return 1
			} else if n > 0 {
				logger.Info("push_token_backfill_complete", "converted", n)
			}
		}
		st.WithSecretsEnabled(cfg.Secrets.Enabled)
		if owned.materializing && cfg.Secrets.EnvelopeEnforced() {
			st.WithCredentialKeyrings(credentialRings)
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
	if st != nil && owned.coreDelivery {
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

		// Harden outbound alert delivery against SSRF: webhook/Slack/notify HTTP and SMTP
		// dial through an egress guard (resolve → policy check → pinned IP; redirect +
		// DNS-rebind included). This uses the SEPARATE notification_egress policy, which
		// defaults to deny-private — NOT the prober policy (which allows private, since a
		// probe target is operator-chosen). A notification destination can be set by a
		// project editor, so it must not reach loopback/RFC1918/ULA/CGNAT/metadata unless
		// the deployment explicitly opts in. (OIDC is a distinct operator-trusted path and
		// is not routed through this guard — see D-0141.)
		egress := notificationEgressGuard(cfg)
		mailer.SetEgressDial(egress.EgressDialContext())
		subs := subscribe.New(st, mail)
		deliverer := incidentFanout{hooks: webhook.New(st, egress.HTTPClient(10*time.Second)), subs: subs, logger: logger}
		ob := outbox.New(st, deliverer, notify.New(st, egress.HTTPClient(10*time.Second)), registry, logger).
			WithMailer(mail).
			WithSilence(func() bool { return settingsSvc.Alerting().Silenced(time.Now()) })
		spawn(func() { ob.Run(ctx) })
	}

	// RabbitMQ management lookup (which worker pools have a live consumer): used by the
	// region picker (API) and region-worker alerting (scheduler). Best-effort — derived
	// from the AMQP URL unless overridden; nil when no broker is configured.
	var mgmt *mqadmin.Client
	if owned.api || owned.materializing {
		var merr error
		mgmt, merr = rabbitManagementClient(cfg.RabbitMQ)
		if merr != nil {
			logger.Warn("mqadmin_init_failed", "error", merr.Error())
		}
	}

	// Auth + API wiring. Requires a database (sessions, JIT users, and the OIDC
	// override all live in Postgres). OIDC is built asynchronously by StartOIDC —
	// discovery is a network call that must not block or crash startup — and can be
	// (re)configured at runtime from the Settings UI.
	var app http.Handler
	var apiHandler *api.Handler
	if st != nil && owned.api {
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
		apiHandler = api.New(st, logger, cfg.Local.MinPasswordLength).WithMetrics(registry).WithEvents(broker).WithOIDC(authn).WithSettings(settingsSvc).
			WithSecretsEnabled(cfg.Secrets.Enabled)
		if mail != nil {
			apiHandler.WithMailer(mail)
		}
		if st != nil {
			// Push results apply through a dedicated recorder (trusted entrypoint, not the
			// shared ResultSink), running the same post-commit reconciler (SSE + incidents)
			// as scheduled ingest and publishing status changes to the shared broker.
			apiHandler.WithPushRecorder(ingest.NewPushRecorder(st, ingest.NewReconciler(st, broker, registry, logger), registry, logger))
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

	// Monitoring as Code file providers (FR-017): owned only by api/all (spec §12). Startup
	// is fail-fast — a configured provider needs a DB and a readable directory here.
	if err := startFileProviders(ctx, cfg, *role, st, registry, apiHandler, logger, spawn); err != nil {
		logging.Critical(logger, "file_provider_startup_failed", "error", err.Error())
		return 1
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
		amqpd.WithJobRegion(*region).WithProtocolV2(*role == "worker" && cfg.Secrets.EnvelopeEnforced())
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
			var base jobRegionTester
			if amqpd, ok := disp.(*dispatch.AMQP); ok {
				base = amqpd
			} else {
				local := localTester{run: runner.Run}
				if ring, ok := credentialRings.ForRegion(domain.DefaultRegion); ok && cfg.Secrets.EnvelopeEnforced() {
					local.credentials = ring
				}
				base = local
			}
			pullSet := make(map[string]bool, len(cfg.Pull.Regions))
			for _, r := range cfg.Pull.Regions {
				if r != "" {
					pullSet[r] = true
				}
			}
			routes := regionRoutedTester{pullRegions: pullSet, pull: pullTester{store: st}, fallback: base}
			if cfg.Secrets.EnvelopeEnforced() {
				apiHandler.WithTester(materializingTester{store: st, routes: routes})
			} else {
				apiHandler.WithTester(routes)
			}
		}
		startIngest := func() {
			ing := ingest.New(st, disp, registry, logger).WithEvents(broker)
			spawn(func() { ing.Run(ctx) })
		}
		// Confirm-phase wake signals for the scheduler leader (LISTEN monitor_confirm):
		// a counted failure reschedules the next probe at the confirm interval
		// immediately, instead of waiting for the snapshot refresh.
		confirmSignals := func() <-chan string {
			cn := st.NewConfirmNotifier(logger)
			spawn(func() { cn.Run(ctx) })
			ch, _ := cn.Subscribe() // scheduler lives for the whole process — no unsubscribe
			return ch
		}
		// Config-change wake signals for the scheduler leader (LISTEN monitor_config_changed):
		// a committed file-provider apply forces a snapshot reload on the next tick instead of
		// waiting out refreshEvery (spec §12).
		configSignals := func() <-chan struct{} {
			cn := st.NewConfigNotifier(logger)
			spawn(func() { cn.Run(ctx) })
			ch, _ := cn.Subscribe()
			return ch
		}
		switch *role {
		case "all":
			sch := scheduler.New(st, disp, logger).WithRetentionDays(cfg.Heartbeats.RetentionDays).
				WithCredentialEnvelopes(cfg.Secrets.EnvelopeEnforced()).
				WithSecretResolutionMetrics(registry).
				WithLocalCredentialRegions(domain.DefaultRegion).
				WithPullRegions(cfg.Pull.Regions).                                  // pull-region jobs → pull_jobs (agent claims), NOT the in-proc worker
				WithPullMetrics(registry).                                          // per-region pull-queue depth/lag gauges
				WithLeaderState(registry).                                          // cerbix_scheduler_leader gauge
				WithReconciler(ingest.NewReconciler(st, broker, registry, logger)). // dead-man DOWN → SSE + incident
				WithConfirmSignals(confirmSignals()).
				WithConfigSignals(configSignals()) // file-apply config changes wake the leader
			spawn(func() { sch.Run(ctx) })
			wk := worker.New(disp, runner, workerPoolSize, logger)
			if ring, ok := credentialRings.ForRegion(domain.DefaultRegion); ok && cfg.Secrets.EnvelopeEnforced() {
				wk.WithCredentialKeyring(ring).WithCredentialReadiness(registry)
			}
			spawn(func() { wk.Run(ctx) })
			startIngest()
		case "scheduler":
			if st == nil {
				logging.Critical(logger, "scheduler_requires_database", "hint", "set database.dsn")
				return 1
			}
			sch := scheduler.New(st, disp, logger).WithRetentionDays(cfg.Heartbeats.RetentionDays).
				WithCredentialEnvelopes(cfg.Secrets.EnvelopeEnforced()).
				WithSecretResolutionMetrics(registry).
				WithPullRegions(cfg.Pull.Regions).                                  // pull-served regions get jobs via pull_jobs, not AMQP
				WithPullMetrics(registry).                                          // per-region pull-queue depth/lag gauges
				WithLeaderState(registry).                                          // cerbix_scheduler_leader gauge
				WithReconciler(ingest.NewReconciler(st, broker, registry, logger)). // dead-man DOWN → incident/outbox (SSE only if this process serves it)
				WithConfirmSignals(confirmSignals()).                               // accelerated failure-confirmation probes
				WithConfigSignals(configSignals())                                  // file-apply config changes wake the leader
			// Alert when a region with enabled monitors loses its worker/agent. Liveness
			// unions RabbitMQ consumers with recent pull-agent heartbeats.
			sch.WithLiveRegions(newLiveRegions(mgmt, st))
			if mgmt != nil {
				sch.WithCredentialLiveRegions(mgmt)
			}
			spawn(func() { sch.Run(ctx) })
		case "worker":
			// Answer region-scoped "Test connection" RPCs for this worker's region.
			if amqpd, ok := disp.(*dispatch.AMQP); ok {
				amqpd.ServeTests(runner.Run)
				if cfg.Secrets.EnvelopeEnforced() {
					workerRegion := *region
					if workerRegion == "" {
						workerRegion = domain.DefaultRegion
					}
					ring, _ := credentialRings.ForRegion(workerRegion) // role validation guarantees coverage
					amqpd.ServeTestsV2(func(ctx context.Context, job dispatch.CheckJob) (domain.Heartbeat, error) {
						m, cleanup, err := ring.MaterializeForProbe(job)
						if err != nil {
							reason := dispatch.CredentialProbeErrorReason(err)
							registry.RecordExecutorProbeError(reason)
							if reason != domain.ProbeErrorUnsupportedVersion {
								registry.SetCredentialReady(false, reason)
							}
							return dispatch.ProbeErrorHeartbeat(job, reason), nil
						}
						defer cleanup()
						registry.SetCredentialReady(true, "")
						return runner.Run(ctx, m), nil
					})
				}
			}
			wk := worker.New(disp, runner, workerPoolSize, logger)
			workerRegion := *region
			if workerRegion == "" {
				workerRegion = domain.DefaultRegion
			}
			if ring, ok := credentialRings.ForRegion(workerRegion); ok && cfg.Secrets.EnvelopeEnforced() {
				wk.WithCredentialKeyring(ring).WithCredentialReadiness(registry)
			}
			spawn(func() { wk.Run(ctx) })
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
	httpErr := srv.Shutdown(shutdownCtx)
	if httpErr != nil {
		logger.Error("graceful_shutdown_failed", "error", httpErr.Error())
	}
	// Drain background goroutines so none is left mid-write. ctx is cancelled by the
	// signal (NotifyContext); stop() also covers the errCh exit path. Bounded so a
	// wedged goroutine can't hang the process forever.
	stop()
	drained := make(chan struct{})
	go func() { bg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		logger.Warn("background_drain_timeout")
	}
	if httpErr != nil {
		return 1
	}
	logger.Info("stopped")
	return 0
}

// runAgent runs the HTTP-pull prober role: claim jobs for --region from the central
// API, probe them, post results, heartbeat. It serves only the ops endpoints
// (health/readyz/metrics) and blocks until ctx is cancelled.
func runAgent(ctx context.Context, cfg *config.Config, region string, credentialRings dispatch.CredentialKeyrings, registry *metrics.Registry, logger *slog.Logger) int {
	if cfg.Pull.ServerURL == "" || cfg.Pull.Token == "" {
		logging.Critical(logger, "agent_requires_pull_config", "hint", "set pull.server_url and pull.token for --role agent")
		return 1
	}
	if region == "" {
		region = domain.DefaultRegion
	}
	runner := prober.NewRunnerWithGuard(prober.NewGuard(cfg.Prober.AllowPrivateIPs, cfg.Prober.AllowMetadataIPs))
	a := agent.New(cfg.Pull.ServerURL, cfg.Pull.Token, region, runner, logger)
	if ring, ok := credentialRings.ForRegion(region); ok && cfg.Secrets.EnvelopeEnforced() {
		a.WithCredentialKeyring(ring).WithCredentialHealth(registry)
	}
	go a.Run(ctx)
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

// startFileProviders launches the Monitoring-as-Code file providers for an api/all process
// (spec §12: only these roles own the component; scheduler/worker/agent parse the same
// config but never watch the directory). Fail-fast per §4.1: a configured provider requires
// a database, and its directory must exist and be readable at startup. Each provider runs
// its own leader-elected reconcile loop under the shutdown WaitGroup via spawn.
func startFileProviders(ctx context.Context, cfg *config.Config, role string, st *store.Store, registry *metrics.Registry, apiHandler *api.Handler, logger *slog.Logger, spawn func(func())) error {
	if len(cfg.Providers.File) == 0 {
		return nil
	}
	if role != "all" && role != "api" {
		return nil // owned only by api/all; other roles neither watch nor require the directory
	}
	if st == nil {
		return fmt.Errorf("file providers require a database (role %s)", role)
	}
	names := make([]string, 0, len(cfg.Providers.File))
	for name := range cfg.Providers.File {
		names = append(names, name)
	}
	sort.Strings(names)
	// Provider leadership advisory keys are derived from the provider name; refuse to start if
	// two configured names collide on one key, so leadership is collision-free by construction
	// rather than merely improbable (§12).
	if err := fpruntime.AssertDistinctLeaderKeys(names); err != nil {
		return err
	}
	// One process-local status registry backs the diagnostics API (§15): every provider
	// (writer) registers at New so configured-but-idle providers surface before their first
	// reconcile; the api handler reads a snapshot on demand.
	statusReg := fpruntime.NewStatusRegistry()
	if apiHandler != nil {
		apiHandler.WithFileProviderStatus(fpStatusAdapter{reg: statusReg})
	}
	// One semaphore shared by every provider bounds concurrent reconciles process-wide (§17).
	reconcileSem := make(chan struct{}, maxConcurrentReconciles)
	for _, name := range names {
		pc := cfg.Providers.File[name]
		info, err := os.Stat(pc.Directory)
		if err != nil {
			return fmt.Errorf("file provider %q: directory %q not readable at startup: %w", name, pc.Directory, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("file provider %q: %q is not a directory", name, pc.Directory)
		}
		// Stat proves existence but not that the directory can be ENUMERATED (a dir with no
		// execute/read bit stats fine yet fails ReadDir). Fail fast on that too (§4.1).
		if dh, derr := os.Open(pc.Directory); derr != nil {
			return fmt.Errorf("file provider %q: directory %q not openable at startup: %w", name, pc.Directory, derr)
		} else if _, derr := dh.ReadDir(1); derr != nil && derr != io.EOF {
			_ = dh.Close()
			return fmt.Errorf("file provider %q: directory %q not enumerable at startup: %w", name, pc.Directory, derr)
		} else {
			_ = dh.Close()
		}
		// Register a zero leadership gauge BEFORE election so every configured provider always
		// exports `cerbix_file_provider_leader{provider}` — otherwise, after the sole leader
		// disappears, the remaining followers export no series and the NoLeader alert's
		// `max by(provider)(…leader) == 0` matches an empty vector and never fires (§16).
		if registry != nil {
			registry.SetFileProviderLeader(name, false)
		}
		p := fpruntime.New(name, pc, fpruntime.NewStoreApplier(st), logger).
			WithMetrics(registry).WithReconcileLimiter(reconcileSem).WithStatus(statusReg)
		spawn(func() { p.Run(ctx) })
		logger.Info("file_provider_started", "provider", name, "directory", pc.Directory)
	}
	return nil
}

// fpStatusAdapter bridges the reconcile runtime's StatusRegistry to the api package's
// FileProviderStatusSource, mapping each snapshot entry to the API DTO. Go has no structural
// typing across packages, so this thin adapter avoids the api package importing runtime.
type fpStatusAdapter struct{ reg *fpruntime.StatusRegistry }

func (a fpStatusAdapter) FileProviderRuntimeStatuses() []api.FileProviderRuntimeStatus {
	snap := a.reg.Snapshot()
	out := make([]api.FileProviderRuntimeStatus, 0, len(snap))
	for _, s := range snap {
		out = append(out, api.FileProviderRuntimeStatus{
			Provider:        s.Provider,
			ScopeType:       s.ScopeType,
			ScopeOrg:        s.ScopeOrg,
			ScopeProject:    s.ScopeProject,
			Leader:          s.Leader,
			LastScanUnix:    s.LastScanUnix,
			LastSuccessUnix: s.LastSuccessUnix,
			LastError:       s.LastError,
			Managed:         s.Managed,
			Orphaned:        s.Orphaned,
			BundleErrors:    s.Rejected,
		})
	}
	return out
}
