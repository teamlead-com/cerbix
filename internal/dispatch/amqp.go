package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// dialAttempts/dialBackoff bound the startup wait for the broker to become
// reachable (a transient infra condition, distinct from config validity).
const (
	dialAttempts = 30
	dialBackoff  = time.Second
)

const (
	// Jobs are routed per region: checks.jobs.<region>. A worker consumes only its
	// region's queue; the scheduler publishes to the queue of each monitor's region.
	jobsQueuePrefix   = "checks.jobs."
	jobsV2QueuePrefix = "checks.jobs.v2."
	// Carrier generation 3 carries envelope v2 (the execution-bound AAD). It is a
	// physically separate queue, not a flag on the v2 one: a capability-1 consumer sitting
	// on a shared queue would take a message it cannot open, and a capability check does
	// not stop a consumer from consuming (func-secret-inventory §4.7, D-0160).
	jobsV3QueuePrefix = "checks.jobs.v3."
	// Carrier generation 4 carries job identity (FR-032). Physically separate for the same
	// reason generation 3 is: a v3 worker is not subscribed here, so it CANNOT receive a v4
	// delivery rather than being filtered out of one.
	jobsV4QueuePrefix = "checks.jobs.v4."
	// FR-029 invariant 6. A canary rides its OWN queue, named for the capability token the
	// executor announces: `checks.canary.<kind>@<version>.<region>`. Physically separate for the
	// same reason generation 3 is separate from generation 2 (D-0160) — a capability CHECK does
	// not stop a consumer from consuming, so a worker that cannot run an async transaction must be
	// unable to receive one, not merely discouraged. It is additive: every existing queue, binding
	// and consumer is untouched, so an old worker simply never sees a canary.
	//
	// The queue is also the ANNOUNCEMENT: consumers > 0 on it is what tells core the region has a
	// runner of that kind and version. That is why the token is in the name — a v2 runner binds a
	// different queue, and core can see the skew instead of publishing into a queue whose consumers
	// cannot honour the contract.
	canaryQueuePrefix = "checks.canary."
	// canaryV3Infix marks the envelope-bearing canary carrier. Two queues, not one, because
	// `secrets.enabled: false` must keep a SECRETLESS canary working on a worker that can open no
	// envelope at all — and that worker must still be physically unable to receive one.
	canaryV3Infix = "v3."
	// Test probes ("Test connection") are RPCs per region: the API publishes to
	// checks.tests.<region> with a reply queue; a worker in that region runs the
	// probe and replies. The queue is durable+auto-delete: it can be shared by N
	// workers (so it cannot be exclusive), recreates cleanly across broker/worker
	// restarts, and is removed after the last consumer leaves. Individual RPC
	// requests remain transient and time-bounded. Non-durable, non-exclusive queues
	// are rejected by RabbitMQ 4.3.
	testsQueuePrefix   = "checks.tests."
	testsV2QueuePrefix = "checks.tests.v2."
	testsV3QueuePrefix = "checks.tests.v3."
	resultsQueue       = "checks.results"
	// deadQueue holds poison messages (unparseable job/result bodies) that a consumer
	// would otherwise Nack-drop and lose. Forwarding them here preserves the raw body
	// for inspection. Results themselves carry NO time-TTL — a slow ingest must never
	// drop results — so poison forwarding is the deliberate, only dead-letter path.
	deadQueue       = "checks.dead"
	consumePrefetch = 16
	forwardBuffer   = 256
	// testRPCTimeout bounds the wait for a region worker's test reply when the
	// monitor carries no explicit timeout; otherwise timeout+testRPCSlack is used.
	testRPCTimeout = 20 * time.Second
	testRPCSlack   = 5 * time.Second
	// channelRetryBackoff paces a consumer's resubscribe after a CHANNEL-level
	// death while the connection is still alive (queue deleted/recreated,
	// basic.cancel, a 4xx channel exception, an Ack error). The connection
	// supervisor only fires on connection loss, so without this leg such a
	// consumer would park until the next full reconnect — the residual of the
	// silent-death class the supervisor was meant to close.
	channelRetryBackoff = 2 * time.Second
)

// queueForRegion names a per-region queue, and it is the ONE place the empty-region default lives.
//
// A5: there used to be one helper per generation per family, each restating `region == "" →
// DefaultRegion`, and the generation-4 jobs helper — the newest, written last — was the single
// sibling that omitted it. Unreachable today, because every publisher resolves the region before
// it gets here; a publisher with an empty region would have declared and published to
// `checks.jobs.v4.` and no consumer binds that name, so the jobs would have sat unconsumed until
// their TTL while every other generation routed correctly.
//
// One helper cannot have that shape. The generation tables below select a PREFIX and this
// function applies the default, so a seventh generation is a row in a table rather than a
// function someone has to remember to write correctly.
func queueForRegion(prefix, region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return prefix + region
}

// jobsQueuePrefixes and testsQueuePrefixes are the generation → queue-family mappings. They are
// tables and not switches so that the mapping and the default cannot be edited apart, and
// `JobsQueuePrefixes` below publishes the jobs table so that a reader of queue NAMES — the broker
// admin client — derives the set from the generations this package can publish rather than
// restating it (A6).
var (
	jobsQueuePrefixes = map[int]string{
		ProtocolV1: jobsQueuePrefix,
		ProtocolV2: jobsV2QueuePrefix,
		ProtocolV3: jobsV3QueuePrefix,
		ProtocolV4: jobsV4QueuePrefix,
	}
	testsQueuePrefixes = map[int]string{
		ProtocolV1: testsQueuePrefix,
		ProtocolV2: testsV2QueuePrefix,
		ProtocolV3: testsV3QueuePrefix,
	}
)

// JobsQueuePrefixes reports the generation → jobs-queue-prefix table, copied so a caller cannot
// edit the routing by editing the answer.
//
// It exists for A6. `internal/mqadmin` reads queue names off the broker and has to tell the legacy
// region queue `checks.jobs.<region>` apart from the generational ones, which share its prefix. It
// did that with a hand-written exclusion list, and the list was one generation behind: generation 4
// shipped and nothing excluded `checks.jobs.v4.`, so a v4 consumer registered the phantom region
// `v4.<region>` in the union feeding the region-worker alert. A list restated in a second package
// is a list that goes stale the next time a generation is added; a derived one cannot.
func JobsQueuePrefixes() map[int]string {
	out := make(map[int]string, len(jobsQueuePrefixes))
	for generation, prefix := range jobsQueuePrefixes {
		out[generation] = prefix
	}
	return out
}

// LegacyJobsQueuePrefix is the generation-1 jobs prefix, which every generational prefix EXTENDS.
// A caller matching queue names needs both: this to recognise the family, and the table above to
// subtract the generations that merely look like a region.
func LegacyJobsQueuePrefix() string { return jobsQueuePrefix }

// jobsQueueForGeneration maps a carrier generation to its physical queue. The mapping is
// explicit and total: a generation with no queue is a programming error, never a silent
// fallback onto an older carrier that its consumers could misread.
func canaryQueueForRegion(token, region string) string {
	return canaryQueuePrefix + domain.CanaryQueueSuffix(token, region)
}

func canaryV3QueueForRegion(token, region string) string {
	return canaryQueuePrefix + canaryV3Infix + domain.CanaryQueueSuffix(token, region)
}

// canaryQueueForGeneration picks the carrier a canary job rides. Generation 2 is deliberately
// absent: a canary secret binding requires a BODY-BOUND envelope (envelope v2, carrier 3), so a
// generation-2 canary carrier could only ever carry a job the executor's gate must refuse.
func canaryQueueForGeneration(token, region string, generation int) (string, bool) {
	switch generation {
	case ProtocolV1:
		return canaryQueueForRegion(token, region), true
	case ProtocolV3:
		return canaryV3QueueForRegion(token, region), true
	default:
		return "", false
	}
}

func jobsQueueForGeneration(region string, generation int) (string, bool) {
	prefix, ok := jobsQueuePrefixes[generation]
	if !ok {
		return "", false
	}
	return queueForRegion(prefix, region), true
}

func testsQueueForGeneration(region string, generation int) (string, bool) {
	prefix, ok := testsQueuePrefixes[generation]
	if !ok {
		return "", false
	}
	return queueForRegion(prefix, region), true
}

// TestRunner answers one test-RPC delivery. Both the legacy and the envelope-bearing test
// consumers take the SAME callback so neither can quietly grow its own rules: the carrier
// generation is stamped by whichever consumer received the message, and the callback runs
// the one executor gate (§4.7, D-0160). The v1 consumer used to call the prober directly
// with job.Monitor, which is how it became the one executor path with no gate at all.
type TestRunner func(ctx context.Context, delivered DeliveredJob) (domain.Heartbeat, error)

// AMQP is a RabbitMQ-backed Dispatcher for cross-process roles. Publishing is
// serialized on a dedicated channel; Jobs()/Results() lazily start a manual-ack
// consumer that forwards deliveries onto a buffered Go channel, so a process only
// consumes the queue it reads (a scheduler publishes jobs and never consumes
// them; a worker consumes jobs; the API consumes results).
//
// Ack policy: a delivery is acked once handed to the in-process channel. Losing a
// single check on a hard crash is acceptable — the scheduler re-emits on the next
// interval — and this keeps the transport seam free of an ack concept the
// Dispatcher interface does not expose.
type AMQP struct {
	url string // kept for the supervisor's redial loop

	connMu sync.RWMutex // guards conn and reconnectedCh
	conn   *amqp.Connection
	// reconnectedCh is closed (and replaced) each time the supervisor completes
	// a redial — consumers wait on it to resubscribe after a broker loss.
	reconnectedCh chan struct{}

	pubCh  *amqp.Channel
	pubMu  sync.Mutex
	logger *slog.Logger

	stateMu       sync.Mutex    // guards onBrokerState
	onBrokerState func(up bool) // optional broker-reachability gauge hook

	ctx    context.Context
	cancel context.CancelFunc

	jobRegion            string          // region this dispatcher's Jobs() consumes (worker); default core
	credentialCapability int             // highest envelope generation this worker can open (0 = none)
	ledgerCapability     int             // 1 = this worker can read job identity, so it may consume v4 (0 = none)
	canaryCapability     string          // the `<kind>@<version>` this worker announces ("" = no canary runner)
	declaredMu           sync.Mutex      // guards declared
	declared             map[string]bool // idempotent-declare cache for per-region job queues

	jobsOnce    sync.Once
	jobsCh      chan DeliveredJob
	resultsOnce sync.Once
	resultsCh   chan domain.Heartbeat
	testsOnce   sync.Once // guards the per-region test-RPC server (worker side)
	testsV2Once sync.Once
	testsV3Once sync.Once
}

// dialAndSetup opens a connection plus the publish channel and declares the
// durable results queue — the shared setup for the initial dial and every
// supervisor redial.
func dialAndSetup(url string) (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("dispatch: amqp channel: %w", err)
	}
	// Results share a single queue; per-region job queues are declared on demand
	// (by the publisher before publishing, and by a worker before consuming).
	if _, err := ch.QueueDeclare(resultsQueue, true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("dispatch: declare %s: %w", resultsQueue, err)
	}
	// Durable dead-letter sink for poison messages (see deadQueue / deadLetter).
	if _, err := ch.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("dispatch: declare %s: %w", deadQueue, err)
	}
	return conn, ch, nil
}

// NewAMQP dials the broker, declares the durable job/result queues, and starts
// the connection supervisor (runtime broker loss → redial + resubscribe).
func NewAMQP(url string, logger *slog.Logger) (*AMQP, error) {
	var (
		conn *amqp.Connection
		ch   *amqp.Channel
		err  error
	)
	for attempt := 1; attempt <= dialAttempts; attempt++ {
		if conn, ch, err = dialAndSetup(url); err == nil {
			break
		}
		if logger != nil {
			logger.Warn("amqp_dial_retry", "attempt", attempt, "max", dialAttempts, "error", err.Error())
		}
		time.Sleep(dialBackoff)
	}
	if err != nil {
		return nil, fmt.Errorf("dispatch: amqp dial after %d attempts: %w", dialAttempts, err)
	}
	if logger == nil {
		// The supervisor/redial paths log unconditionally; a nil logger would
		// panic on the first broker event. Default to a no-op sink.
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &AMQP{
		url:           url,
		conn:          conn,
		reconnectedCh: make(chan struct{}),
		pubCh:         ch,
		logger:        logger,
		ctx:           ctx,
		cancel:        cancel,
		jobRegion:     domain.DefaultRegion,
		declared:      map[string]bool{},
		jobsCh:        make(chan DeliveredJob, forwardBuffer),
		resultsCh:     make(chan domain.Heartbeat, forwardBuffer),
	}
	go d.supervise()
	return d, nil
}

// WithBrokerState wires a callback for the cerbix_broker_up gauge, invoked with
// false on broker loss and true on (re)connect. The dispatcher only exists after a
// successful dial, so wiring the sink immediately reports up. Optional; nil-safe.
func (d *AMQP) WithBrokerState(f func(up bool)) *AMQP {
	d.stateMu.Lock()
	d.onBrokerState = f
	d.stateMu.Unlock()
	d.setBrokerState(true)
	return d
}

// setBrokerState is a nil-safe, race-free gauge update.
func (d *AMQP) setBrokerState(up bool) {
	d.stateMu.Lock()
	f := d.onBrokerState
	d.stateMu.Unlock()
	if f != nil {
		f(up)
	}
}

// current returns the live connection and the signal channel that closes when
// the NEXT successful reconnect completes.
func (d *AMQP) current() (*amqp.Connection, <-chan struct{}) {
	d.connMu.RLock()
	defer d.connMu.RUnlock()
	return d.conn, d.reconnectedCh
}

// supervise watches the connection and redials on broker loss: exactly one
// broker_lost per outage, sparse broker_reconnecting during the backoff loop,
// one broker_reconnected on recovery. Graceful shutdown (ctx cancelled) is not
// a loss. Consumers never see their forwarding Go channels close — they wait
// on the reconnect signal and resubscribe.
func (d *AMQP) supervise() {
	for {
		conn, _ := d.current()
		closed := conn.NotifyClose(make(chan *amqp.Error, 1))
		select {
		case <-d.ctx.Done():
			return
		case amqpErr := <-closed:
			if d.ctx.Err() != nil {
				return // our own Close(), not a broker loss
			}
			reason := "connection closed"
			if amqpErr != nil {
				reason = amqpErr.Error()
			}
			d.logger.Warn("broker_lost", "error", reason)
			d.setBrokerState(false)
			if !d.redial() {
				return // shutdown while reconnecting
			}
		}
	}
}

// redial loops with exponential backoff (1s → 30s cap) until the broker is
// back or the dispatcher is closed. On success it swaps the connection and
// publish channel, resets the queue-declare cache, and wakes every waiting
// consumer. Returns false when interrupted by shutdown.
func (d *AMQP) redial() bool {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		select {
		case <-d.ctx.Done():
			return false
		case <-time.After(backoff):
		}
		conn, ch, err := dialAndSetup(d.url)
		if err != nil {
			// Sparse progress lines: first attempt, then roughly every fifth.
			if attempt == 1 || attempt%5 == 0 {
				d.logger.Warn("broker_reconnecting", "attempt", attempt, "error", err.Error())
			}
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		d.connMu.Lock()
		d.conn = conn
		wake := d.reconnectedCh
		d.reconnectedCh = make(chan struct{})
		d.connMu.Unlock()
		d.pubMu.Lock()
		d.pubCh = ch
		d.pubMu.Unlock()
		// Queues must be re-declared against the new connection.
		d.declaredMu.Lock()
		d.declared = map[string]bool{}
		d.declaredMu.Unlock()
		close(wake)
		d.logger.Info("broker_reconnected", "attempts", attempt)
		d.setBrokerState(true)
		return true
	}
}

// WithJobRegion sets which region's jobs queue Jobs() consumes (a worker's
// --region). Empty keeps the default (core).
func (d *AMQP) WithJobRegion(region string) *AMQP {
	if region != "" {
		d.jobRegion = region
	}
	return d
}

// WithCredentialCapability declares the highest ENVELOPE generation this executor can
// open, which decides which carrier queues it consumes. It is a level, not a boolean: a
// capability-1 executor must be physically unable to receive a generation-3 carrier, and
// widening the set it consumes is the only safe direction to move.
// Publishers route from CheckJob.ProtocolVersion; an old worker never calls this and
// therefore can never receive an envelope-bearing payload.
func (d *AMQP) WithCredentialCapability(capability int) *AMQP {
	d.credentialCapability = capability
	return d
}

// WithLedgerCapability declares that this executor understands job identity (FR-032), which is
// what binds it to the generation-4 queue. It is deliberately NOT the credential capability:
// identity applies to every monitor, so reusing the envelope number would tie an ordinary HTTP
// monitor's ledger eligibility to a capability its dispatch never needs (§13.0). Consuming the
// queue IS the announcement, the same rule the canary carrier follows.
func (d *AMQP) WithLedgerCapability(capability int) *AMQP {
	d.ledgerCapability = capability
	return d
}

// WithCanaryCapability declares the workflow token this executor can run, which decides whether it
// consumes the region's canary queues at all. Empty (the default, and every binary that predates
// FR-029) means the executor never sees a canary — it does not merely decline one it was handed.
//
// The caller must derive the token from the RUNNER, not from this binary's constant: an executor
// that announces a workflow its runner does not have would take the job and fail it, which is the
// one outcome the capability exists to prevent.
func (d *AMQP) WithCanaryCapability(token string) *AMQP {
	d.canaryCapability = token
	return d
}

// declareJobQueue idempotently declares a per-region job queue (cached). Publishing
// to an undeclared queue via the default exchange is silently dropped, so both the
// publisher and the consuming worker must ensure their queue exists.
func (d *AMQP) declareJobQueue(queue string) error {
	d.declaredMu.Lock()
	defer d.declaredMu.Unlock()
	if d.declared[queue] {
		return nil
	}
	if err := d.onPubChannel(func(ch *amqp.Channel) error {
		_, e := ch.QueueDeclare(queue, true, false, false, false, nil)
		return e
	}); err != nil {
		return err
	}
	d.declared[queue] = true
	return nil
}

// onPubChannel runs fn against the publish channel under pubMu (amqp channels are
// not safe for concurrent use). If fn fails, it reopens the channel on the CURRENT
// connection and retries once — a channel-level exception (basic.return, a 4xx, an
// ack error) closes pubCh while the TCP connection stays up, which the connection
// supervisor never sees, so without this a single bad publish would wedge the
// publisher forever behind a healthy broker_up=1. Durable queues survive the reopen
// (they're connection-independent), so the declare cache stays valid.
func (d *AMQP) onPubChannel(fn func(*amqp.Channel) error) error {
	d.pubMu.Lock()
	defer d.pubMu.Unlock()
	err := fn(d.pubCh)
	if err == nil {
		return nil
	}
	conn, _ := d.current()
	nch, rerr := conn.Channel()
	if rerr != nil {
		return err // connection is likely gone too; the supervisor will redial
	}
	if d.pubCh != nil {
		_ = d.pubCh.Close()
	}
	d.pubCh = nch
	d.logger.Warn("publish_channel_reopened", "error", err.Error())
	return fn(d.pubCh)
}

func (d *AMQP) publish(queue string, v any) error { return d.publishTo(queue, v, "") }

// publishTo publishes v to queue via the default exchange, optionally with a
// per-message TTL (Expiration, milliseconds as a string).
func (d *AMQP) publishTo(queue string, v any, expiration string) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("dispatch: marshal: %w", err)
	}
	return d.onPubChannel(func(ch *amqp.Channel) error {
		return ch.PublishWithContext(d.ctx, "", queue, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Expiration:   expiration,
			Body:         body,
		})
	})
}

// deadLetter best-effort forwards a poison message (an unparseable body a consumer
// is about to Nack-drop) to the durable dead-letter queue, tagged with its source, so
// it survives for inspection instead of vanishing. A failure here is logged, never
// fatal — the message is dropped as before in that rare case.
func (d *AMQP) deadLetter(source string, body []byte) {
	err := d.onPubChannel(func(ch *amqp.Channel) error {
		return ch.PublishWithContext(d.ctx, "", deadQueue, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Headers:      amqp.Table{"x-cerbix-source": source},
			Body:         body,
		})
	})
	if err != nil {
		d.logger.Error("dispatch_dead_letter_failed", "source", source, "error", err.Error())
	}
}

// PublishJob routes a check job to its region's queue (checks.jobs.<region>).
// Composite monitors are pinned to the core pool (they need the database). A TTL of
// roughly one interval is set so a job for a region with no live worker expires
// rather than piling up (the scheduler re-emits next tick).
func (d *AMQP) PublishJob(_ context.Context, job CheckJob) error {
	region := job.Monitor.Region
	if job.Monitor.Type == domain.MonitorComposite {
		region = domain.DefaultRegion
	}
	generation := job.ProtocolVersion
	if generation == 0 {
		generation = ProtocolV1
	}
	if job.Monitor.Type == domain.MonitorAsyncCanary {
		return d.publishCanaryJob(job, region, generation)
	}
	queue, ok := jobsQueueForGeneration(region, generation)
	if !ok {
		return fmt.Errorf("dispatch: no jobs carrier for generation %d", generation)
	}
	// Generations 2 and 3 EXIST to carry an envelope, so one is required. Generation 4 does not:
	// it carries job identity, which applies to every monitor including those with no secrets
	// (§13.0). Requiring an envelope there would make an ordinary HTTP monitor's ledger
	// eligibility depend on a capability its dispatch never needs — and it would be unpublishable.
	// A v4 job MAY still carry an envelope, for a monitor that has one.
	if generation >= ProtocolV2 && generation < ProtocolV4 && job.CredentialEnvelope == nil {
		return fmt.Errorf("dispatch: generation %d job is missing credential envelope", generation)
	}
	if generation == ProtocolV1 && job.CredentialEnvelope != nil {
		return errors.New("dispatch: credential envelope cannot be published to a v1 queue")
	}
	if err := d.declareJobQueue(queue); err != nil {
		return fmt.Errorf("dispatch: declare %s: %w", queue, err)
	}
	var expiration string
	if s := job.Monitor.IntervalSeconds; s > 0 {
		expiration = strconv.Itoa(s * 1000)
	}
	return d.publishTo(queue, job, expiration)
}

// publishCanaryJob routes a canary onto its own carrier. The envelope rule here is EXACT rather than
// the generic "generation 2 and up must carry one": a canary needs an envelope precisely when its
// workflow binds a secret, so a secretless canary rides the plain carrier and one with bindings must
// not ride it. Stated this way the publisher refuses both mistakes — a stripped envelope and an
// envelope on the carrier a capability-0 worker consumes — instead of only the first.
func (d *AMQP) publishCanaryJob(job CheckJob, region string, generation int) error {
	queue, err := canaryCarrierFor(job, region, generation)
	if err != nil {
		return err
	}
	if err := d.declareJobQueue(queue); err != nil {
		return fmt.Errorf("dispatch: declare %s: %w", queue, err)
	}
	var expiration string
	if s := job.Monitor.IntervalSeconds; s > 0 {
		expiration = strconv.Itoa(s * 1000)
	}
	return d.publishTo(queue, job, expiration)
}

// canaryCarrierFor is the routing DECISION, separated from the publish so it can be exercised
// without a broker: which queue a canary belongs on, or why it belongs on none.
func canaryCarrierFor(job CheckJob, region string, generation int) (string, error) {
	token := domain.CanaryCapabilityRequiredByConfig(job.Monitor.Config)
	bindings := len(domain.CanarySecretRefKeys(job.Monitor.Config)) > 0
	if bindings && generation != ProtocolV3 {
		return "", fmt.Errorf("dispatch: a canary secret binding requires carrier generation %d, got %d", ProtocolV3, generation)
	}
	if !bindings {
		// Not pedantry: the v3 canary queue is consumed only by envelope-capable workers, so
		// emitting a secretless canary there would narrow, silently, which regions can run it.
		generation = ProtocolV1
	}
	queue, ok := canaryQueueForGeneration(token, region, generation)
	if !ok {
		return "", fmt.Errorf("dispatch: no canary carrier for generation %d", generation)
	}
	if bindings == (job.CredentialEnvelope == nil) {
		if bindings {
			return "", errors.New("dispatch: canary with a secret binding is missing its credential envelope")
		}
		return "", errors.New("dispatch: canary with no secret binding cannot carry a credential envelope")
	}
	return queue, nil
}

// PublishResult publishes a result heartbeat to the durable results queue.
func (d *AMQP) PublishResult(_ context.Context, hb domain.Heartbeat) error {
	return d.publish(resultsQueue, hb)
}

// Jobs starts (once) a consumer of the jobs queue and returns the forwarding
// channel. Only call it in a role that executes jobs (worker).
func (d *AMQP) Jobs() <-chan DeliveredJob {
	d.jobsOnce.Do(func() {
		// The QUEUE a message was consumed from is the carrier generation, and it is the
		// only trustworthy source for it: the body's own ProtocolVersion is attacker-
		// editable (§4.7, D-0160). Each consumer therefore stamps its own generation and
		// the payload's claim is never consulted here.
		consumeQueue := func(queue, source string, generation int) {
			go consume(d, queue, true, func(body []byte) bool {
				var job CheckJob
				if err := json.Unmarshal(body, &job); err != nil {
					d.logger.Error("dispatch_bad_job", "error", err.Error())
					d.deadLetter(source, body)
					return false
				}
				// FR-032 invariant 10i. A generation-4 delivery is DEFINED by carrying job
				// identity, so one missing a field is a PROTOCOL VIOLATION and not a
				// rolling-upgrade case. The distinction is drawn on the CARRIER — the queue this
				// message was consumed from — and never on the body's own ProtocolVersion, which
				// is the field an attacker can edit and the tolerant branch below is one `if`
				// away from covering.
				//
				// The same absence on an OLDER carrier is ordinary: an executor that predates
				// generation 4 drops the unknown field, the result returns with DueAt zero, and
				// the window is simply not ledger-eligible. That is the case `RequireLedgerFields`
				// exists to separate.
				//
				// Dead-lettered rather than dropped: `deadLetter` forwards the poison body to the
				// durable queue so it survives for inspection instead of vanishing, and a silent
				// drop would satisfy any assertion that only checks "no probe ran".
				if RequireLedgerFields(generation) {
					if missing := job.LedgerFieldsMissing(); missing != "" {
						d.logger.Error("dispatch_v4_job_missing_identity",
							"monitor_id", job.Monitor.ID, "field", missing, "carrier", generation)
						d.deadLetter(source, body)
						return true
					}
				}
				// Generations 2 and 3 get their isolation from the QUEUE: an executor that cannot
				// open envelope v1 is not bound to `checks.jobs.v2.<region>` at all, so it cannot
				// receive one. Generation 4 cannot inherit that, and the gap is not hypothetical.
				//
				// The v4 queue is bound on the LEDGER capability, which is deliberately independent
				// of the envelope one (§13.0) — job identity applies to every monitor, so tying it
				// to secrets would make an ordinary HTTP monitor's eligibility depend on something
				// its dispatch never needs. But a generation-4 job MAY carry an envelope:
				// `CarrierFor` returns 4 for a credentialed monitor in a ledger-ready region and
				// `envelopeForCarrier(4)` is v2. So in a region whose workers do NOT all run the
				// same `secrets.dispatch_envelope` setting, an envelope-bearing v4 delivery can
				// reach a worker that declared no envelope capability — which the publisher cannot
				// rule out, because its readiness check is EXISTENTIAL over the region.
				//
				// So the seam enforces what the queue no longer can. Dead-lettered rather than
				// probed: the message is intact and another worker in the region can be given it by
				// an operator, while running it would produce a credential failure attributed to
				// the monitor instead of to the fleet's configuration.
				if job.CredentialEnvelope != nil && d.credentialCapability < job.CredentialEnvelope.V {
					d.logger.Error("dispatch_job_envelope_above_capability",
						"monitor_id", job.Monitor.ID, "carrier", generation,
						"envelope", job.CredentialEnvelope.V, "capability", d.credentialCapability)
					d.deadLetter(source, body)
					return true
				}
				// The carrier -> envelope mapping is EXACT, and the check above is only a CEILING:
				// it stops an envelope NEWER than this executor can open and says nothing about one
				// that is older than its carrier defines. A v1 envelope on the generation-3 queue
				// passed it, passed the gate (which only refused generation 1), and reached the
				// prober with no execution-body binding — the property generation 3 exists to add.
				//
				// One predicate, two dispositions, exactly as `RequireLedgerFields` above: here the
				// poison body is dead-lettered so it survives for inspection, and the pull and
				// in-process paths get the typed refusal from `ValidateAndMaterialize`, which
				// enforces the same rule for every path that does not pass through this consumer.
				if err := CarrierEnvelopeAdmissible(generation, job.CredentialEnvelope); err != nil {
					d.logger.Error("dispatch_job_envelope_carrier_mismatch",
						"monitor_id", job.Monitor.ID, "carrier", generation,
						"envelope", job.CredentialEnvelope.V, "error", err.Error())
					d.deadLetter(source, body)
					return true
				}
				select {
				case d.jobsCh <- DeliveredJob{Job: job, CarrierGeneration: generation}:
					return true
				case <-d.ctx.Done():
					return false
				}
			})
		}
		consumeQueue(queueForRegion(jobsQueuePrefix, d.jobRegion), "jobs", ProtocolV1)
		// One carrier per envelope generation this executor can open, and no others.
		if d.credentialCapability >= EnvelopeV1 {
			consumeQueue(queueForRegion(jobsV2QueuePrefix, d.jobRegion), "jobs.v2", ProtocolV2)
		}
		if d.credentialCapability >= EnvelopeV2 {
			consumeQueue(queueForRegion(jobsV3QueuePrefix, d.jobRegion), "jobs.v3", ProtocolV3)
		}
		// Generation 4 is bound on its OWN capability, never on the credential one: job identity
		// applies to every monitor, so gating it on an envelope capability would make an ordinary
		// HTTP monitor's eligibility depend on something its dispatch never needs (§13.0).
		if d.ledgerCapability >= 1 {
			consumeQueue(queueForRegion(jobsV4QueuePrefix, d.jobRegion), "jobs.v4", ProtocolV4)
		}
		// FR-029 invariant 6: consuming the canary queue IS this executor's announcement, so it is
		// bound only when the runner in this process actually has the workflow. The envelope-bearing
		// canary carrier is bound under exactly the same condition as jobs.v3, for the same reason.
		if d.canaryCapability != "" {
			consumeQueue(canaryQueueForRegion(d.canaryCapability, d.jobRegion), "canary", ProtocolV1)
			if d.credentialCapability >= EnvelopeV2 {
				consumeQueue(canaryV3QueueForRegion(d.canaryCapability, d.jobRegion), "canary.v3", ProtocolV3)
			}
		}
	})
	return d.jobsCh
}

// Results starts (once) a consumer of the results queue and returns the
// forwarding channel. Only call it in a role that ingests results (api).
func (d *AMQP) Results() <-chan domain.Heartbeat {
	d.resultsOnce.Do(func() {
		go consume(d, resultsQueue, false, func(body []byte) bool {
			var hb domain.Heartbeat
			if err := json.Unmarshal(body, &hb); err != nil {
				d.logger.Error("dispatch_bad_result", "error", err.Error())
				d.deadLetter("results", body)
				return false
			}
			select {
			case d.resultsCh <- hb:
				return true
			case <-d.ctx.Done():
				return false
			}
		})
	})
	return d.resultsCh
}

// RunTest dispatches a one-off probe to a worker in the monitor's region and waits
// for the result (RPC over a temporary, exclusive reply queue). This runs on the API
// side so a geo target is probed from its own region, not from core. It returns an
// error if no worker in that region answers within the timeout — the request queue is
// auto-delete, so with no live worker the publish is unroutable and the caller times
// out. Composite/push types are rejected by the caller before reaching here.
func (d *AMQP) RunTest(ctx context.Context, m domain.Monitor) (domain.Heartbeat, error) {
	return d.RunJobTest(ctx, CheckJob{Monitor: m, ProtocolVersion: ProtocolV1})
}

func (d *AMQP) RunJobTest(ctx context.Context, job CheckJob) (domain.Heartbeat, error) {
	m := job.Monitor
	region := m.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	generation := job.ProtocolVersion
	if generation == 0 {
		generation = ProtocolV1
	}
	queue, okQueue := testsQueueForGeneration(region, generation)
	if !okQueue {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: no tests carrier for generation %d", generation)
	}
	if generation >= ProtocolV2 {
		if job.CredentialEnvelope == nil {
			return domain.Heartbeat{}, fmt.Errorf("dispatch: generation %d test is missing credential envelope", generation)
		}
	} else if job.CredentialEnvelope != nil {
		return domain.Heartbeat{}, errors.New("dispatch: credential test cannot use a v1 queue")
	}

	// A dedicated channel keeps this RPC off the shared publish channel; the
	// exclusive, auto-delete reply queue self-cleans when the channel closes, and
	// its uniqueness makes any delivery on it unambiguously our reply.
	conn, _ := d.current()
	ch, err := conn.Channel()
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: test channel: %w", err)
	}
	defer func() { _ = ch.Close() }()
	replyQ, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: reply queue: %w", err)
	}
	replies, err := ch.Consume(replyQ.Name, "", true, true, false, false, nil)
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: consume reply: %w", err)
	}
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))

	timeout := testRPCTimeout
	if s := m.TimeoutSeconds; s > 0 {
		timeout = time.Duration(s)*time.Second + testRPCSlack
	}
	body, err := json.Marshal(job)
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: marshal test: %w", err)
	}
	if err := ch.PublishWithContext(ctx, "", queue, true, false, amqp.Publishing{
		ContentType: "application/json",
		ReplyTo:     replyQ.Name,
		Expiration:  strconv.FormatInt(timeout.Milliseconds(), 10),
		Body:        body,
	}); err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: publish test: %w", err)
	}

	select {
	case <-ctx.Done():
		return domain.Heartbeat{}, ctx.Err()
	case returned := <-returns:
		return domain.Heartbeat{}, fmt.Errorf("no worker queue for region %q (AMQP %d %s)", region, returned.ReplyCode, returned.ReplyText)
	case <-time.After(timeout):
		return domain.Heartbeat{}, fmt.Errorf("no worker responded in region %q", region)
	case msg, ok := <-replies:
		if !ok {
			return domain.Heartbeat{}, fmt.Errorf("dispatch: reply channel closed")
		}
		var hb domain.Heartbeat
		if err := json.Unmarshal(msg.Body, &hb); err != nil {
			return domain.Heartbeat{}, fmt.Errorf("dispatch: decode test reply: %w", err)
		}
		if hb.ProbeError != nil {
			return domain.Heartbeat{}, *hb.ProbeError
		}
		return hb, nil
	}
}

// ServeTestsV2 consumes only the physically separate envelope test queue. It is started
// exclusively by a worker with a validated regional dispatch keyring. It returns only
// after the initial queue declaration and consumer registration succeed, forming a
// startup-readiness barrier.
func (d *AMQP) ServeTestsV2(run TestRunner) error {
	return d.serveTestsGeneration(&d.testsV2Once, queueForRegion(testsV2QueuePrefix, d.jobRegion), ProtocolV2, run)
}

// ServeTestsV3 consumes the generation-3 test carrier — the one that carries envelope v2.
// It exists for the same reason the v3 JOBS consumer does: a generation with a publisher
// and no consumer is a queue that fills until TTL, and "jobs AND tests, AMQP AND pull" is
// the contract, not a slogan. Started only by a worker that declared capability 2.
func (d *AMQP) ServeTestsV3(run TestRunner) error {
	return d.serveTestsGeneration(&d.testsV3Once, queueForRegion(testsV3QueuePrefix, d.jobRegion), ProtocolV3, run)
}

func (d *AMQP) serveTestsGeneration(once *sync.Once, queue string, generation int, run TestRunner) error {
	var initialErr error
	once.Do(func() {
		ready := make(chan error, 1)
		go func() {
			for {
				conn, wake := d.current()
				serveEnvelopeTestsOnce(d, conn, queue, generation, run, ready)
				ready = nil
				select {
				case <-d.ctx.Done():
					return
				case <-wake:
				case <-time.After(channelRetryBackoff):
				}
			}
		}()
		initialErr = <-ready
	})
	return initialErr
}

// deadLetterSourceForTests names the test carrier a poison delivery was dropped from.
// It is derived from the generation rather than written per call site, because the one
// hardcoded "tests.v2" label was attached to deliveries from the v3 queue too, and a
// dead-letter header that names the wrong carrier sends the reader to the wrong consumer.
func deadLetterSourceForTests(generation int) string {
	return fmt.Sprintf("tests.v%d", generation)
}

func serveEnvelopeTestsOnce(d *AMQP, conn *amqp.Connection, queue string, generation int, run TestRunner, ready chan<- error) {
	ch, err := conn.Channel()
	if err != nil {
		d.logger.Error("dispatch_test_v2_channel", "queue", queue, "error", err.Error())
		signalTestConsumerReady(ready, fmt.Errorf("dispatch: v2 test channel: %w", err))
		return
	}
	defer func() { _ = ch.Close() }()
	if _, err := ch.QueueDeclare(queue, true, true, false, false, nil); err != nil {
		d.logger.Error("dispatch_declare_tests_v2", "queue", queue, "error", err.Error())
		signalTestConsumerReady(ready, fmt.Errorf("dispatch: declare %s: %w", queue, err))
		return
	}
	msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		d.logger.Error("dispatch_consume_tests_v2", "queue", queue, "error", err.Error())
		signalTestConsumerReady(ready, fmt.Errorf("dispatch: consume %s: %w", queue, err))
		return
	}
	signalTestConsumerReady(ready, nil)
	for {
		select {
		case <-d.ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			// The QUEUE is the carrier generation here too, exactly as on the jobs path
			// (§4.7, D-0160): the body's own ProtocolVersion is attacker-editable and is
			// never consulted. It USED to be, against a hardcoded ProtocolV2, in a function
			// that serves the v3 carrier as well — so every generation-3 Test Connection was
			// dead-lettered by the consumer bound to its own queue, and the caller, which is
			// answered only on success, waited out its timeout and reported the region as
			// having no worker. What made it invisible: the inproc dev stack never takes this
			// path, and the distributed gate that does was red for an unrelated reason.
			//
			// What remains is the one property a generation >= 2 test carrier is DEFINED by:
			// it carries a credential envelope. A delivery without one is a protocol
			// violation on this queue and is dead-lettered for inspection.
			var job CheckJob
			if err := json.Unmarshal(msg.Body, &job); err != nil || job.CredentialEnvelope == nil {
				d.refuseTest(ch, msg, generation, job, domain.ProbeErrorDecryptAuthFailed)
				continue
			}
			// ...and it must be the envelope THIS carrier defines. Presence alone admitted a
			// generation-1 envelope onto the generation-3 test queue, where it opened without the
			// execution-body binding that generation exists to provide. Same predicate as the jobs
			// consumer and the materializer gate, so the three cannot drift.
			if err := CarrierEnvelopeAdmissible(generation, job.CredentialEnvelope); err != nil {
				d.logger.Error("dispatch_test_envelope_carrier_mismatch",
					"queue", queue, "carrier", generation,
					"envelope", job.CredentialEnvelope.V, "error", err.Error())
				d.refuseTest(ch, msg, generation, job, CredentialProbeErrorReason(err))
				continue
			}
			hb, runErr := run(d.ctx, DeliveredJob{Job: job, CarrierGeneration: generation})
			if runErr != nil {
				// The runner itself refused. It is not a poison BODY — nothing to dead-letter —
				// but the caller still gets an answer rather than a timeout.
				d.replyTest(ch, msg, ProbeErrorHeartbeat(job, CredentialProbeErrorReason(runErr)))
				_ = msg.Ack(false)
				continue
			}
			d.replyTest(ch, msg, hb)
			_ = msg.Ack(false)
		}
	}
}

// replyTest publishes one heartbeat back to a test RPC's reply queue, when it asked for one.
func (d *AMQP) replyTest(ch *amqp.Channel, msg amqp.Delivery, hb domain.Heartbeat) {
	if msg.ReplyTo == "" {
		return
	}
	reply, err := json.Marshal(hb)
	if err != nil {
		return
	}
	_ = ch.PublishWithContext(d.ctx, "", msg.ReplyTo, false, false, amqp.Publishing{
		ContentType: "application/json", Body: reply,
	})
}

// refuseTest answers a REFUSED test delivery and then disposes of the body.
//
// A refusal used to be silence: the consumer dead-lettered the message and published nothing, so
// the caller waited out its RPC timeout and reported `no worker responded in region X` — the exact
// sentence an operator gets when the region is empty. That indistinguishability cost this arc a
// whole debugging cycle when the generation-3 carrier was admitting nothing, and it would cost the
// next reader the same. Owner's decision (2026-09-06) on the item the reviewer fenced: the caller
// gets the typed reason instead.
//
// Both dispositions are kept, because they answer different people: the REPLY tells the operator
// who is waiting, and the DEAD LETTER keeps the poison body for whoever investigates afterwards
// (`docs/runbook.md`). The probe-error vocabulary is NOT widened — the reason comes from
// `CredentialProbeErrorReason`, the same bounded map the executors already answer with, so a
// prober still learns nothing about which way its forgery was wrong.
func (d *AMQP) refuseTest(ch *amqp.Channel, msg amqp.Delivery, generation int, job CheckJob, reason string) {
	d.replyTest(ch, msg, ProbeErrorHeartbeat(job, reason))
	d.deadLetter(deadLetterSourceForTests(generation), msg.Body)
	_ = msg.Nack(false, false)
}

// ServeTests starts (once) a consumer of this dispatcher's region test-RPC queue
// (checks.tests.<region>) and answers each request by running run and publishing the
// heartbeat back to the delivery's ReplyTo. Only call it in a role that executes probes
// (worker). The durable, shared queue auto-deletes after its last worker disconnects,
// which makes a stale region cleanly unroutable for the API while supporting multiple
// workers and RabbitMQ 4.3 (which rejects transient non-exclusive queues). It returns
// only after the initial declaration and consumer registration succeed.
func (d *AMQP) ServeTests(run TestRunner) error {
	var initialErr error
	d.testsOnce.Do(func() {
		queue := queueForRegion(testsQueuePrefix, d.jobRegion)
		ready := make(chan error, 1)
		go func() {
			for {
				conn, wake := d.current()
				serveTestsOnce(d, conn, queue, ProtocolV1, run, ready)
				ready = nil
				select {
				case <-d.ctx.Done():
					return
				case <-wake:
					// Connection reconnected — re-declare and resume.
				case <-time.After(channelRetryBackoff):
					// The auto-delete tests queue vanished on a consumer gap (a
					// channel-level event, not connection loss) — re-declare and
					// resume on the same connection.
				}
			}
		}()
		initialErr = <-ready
	})
	return initialErr
}

// serveTestsOnce runs one test-RPC consume session on conn; returns on
// shutdown or when the delivery channel dies.
func serveTestsOnce(d *AMQP, conn *amqp.Connection, queue string, generation int, run TestRunner, ready chan<- error) {
	ch, err := conn.Channel()
	if err != nil {
		d.logger.Error("dispatch_test_channel", "queue", queue, "error", err.Error())
		signalTestConsumerReady(ready, fmt.Errorf("dispatch: test channel: %w", err))
		return
	}
	defer func() { _ = ch.Close() }()
	if _, err := ch.QueueDeclare(queue, true, true, false, false, nil); err != nil {
		d.logger.Error("dispatch_declare_tests", "queue", queue, "error", err.Error())
		signalTestConsumerReady(ready, fmt.Errorf("dispatch: declare %s: %w", queue, err))
		return
	}
	msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		d.logger.Error("dispatch_consume_tests", "queue", queue, "error", err.Error())
		signalTestConsumerReady(ready, fmt.Errorf("dispatch: consume %s: %w", queue, err))
		return
	}
	signalTestConsumerReady(ready, nil)
	for {
		select {
		case <-d.ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			var job CheckJob
			if err := json.Unmarshal(m.Body, &job); err != nil {
				d.logger.Error("dispatch_bad_test", "error", err.Error())
				_ = m.Nack(false, false)
				continue
			}
			// The QUEUE this consumer is attached to is the carrier generation, exactly as
			// on the jobs path: the body's own claim is never consulted (§4.7, D-0160).
			hb, err := run(d.ctx, DeliveredJob{Job: job, CarrierGeneration: generation})
			if err != nil {
				d.logger.Error("dispatch_test_rejected", "queue", queue, "error", err.Error())
			}
			if m.ReplyTo != "" {
				if reply, err := json.Marshal(hb); err == nil {
					_ = ch.PublishWithContext(d.ctx, "", m.ReplyTo, false, false, amqp.Publishing{
						ContentType: "application/json",
						Body:        reply,
					})
				}
			}
			_ = m.Ack(false)
		}
	}
}

func signalTestConsumerReady(ready chan<- error, err error) {
	if ready != nil {
		ready <- err
	}
}

// consume keeps a manual-ack consumer alive across broker outages: each pass
// runs on the current connection until its delivery channel dies, then waits
// for the supervisor's reconnect signal and resubscribes. The forwarding Go
// channel the caller reads never closes — a worker rides the outage out.
func consume(d *AMQP, queue string, declare bool, handle func(body []byte) bool) {
	_ = declare // consumeOnce now always ensures the queue (see below)
	for {
		conn, wake := d.current()
		consumeOnce(d, conn, queue, handle)
		select {
		case <-d.ctx.Done():
			return
		case <-wake:
			// Connection reconnected — resubscribe on the fresh connection.
		case <-time.After(channelRetryBackoff):
			// Channel died while the connection stayed up — resubscribe on the
			// same live connection after a short backoff (wake would never fire).
		}
	}
}

// consumeOnce runs one consume session; it returns when the dispatcher shuts
// down or the delivery channel dies (broker loss or channel error).
func consumeOnce(d *AMQP, conn *amqp.Connection, queue string, handle func(body []byte) bool) {
	ch, err := conn.Channel()
	if err != nil {
		d.logger.Error("dispatch_consumer_channel", "queue", queue, "error", err.Error())
		return
	}
	defer func() { _ = ch.Close() }()
	// Declare on every session, not via the publisher's declare cache: the
	// reason we're (re)subscribing may be that the queue was deleted, which the
	// cache would not know about. Durable, matching dialAndSetup/declareJobQueue.
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		d.logger.Error("dispatch_consumer_declare", "queue", queue, "error", err.Error())
		return
	}
	if err := ch.Qos(consumePrefetch, 0, false); err != nil {
		d.logger.Error("dispatch_qos", "queue", queue, "error", err.Error())
		return
	}
	msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		d.logger.Error("dispatch_consume", "queue", queue, "error", err.Error())
		return
	}
	for {
		select {
		case <-d.ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			if handle(m.Body) {
				_ = m.Ack(false)
			} else {
				_ = m.Nack(false, false)
			}
		}
	}
}

// Close stops consumers and releases the connection. It reads conn/pubCh under
// the same locks redial() swaps them under, so a shutdown racing a reconnect is
// safe.
func (d *AMQP) Close() error {
	d.cancel()
	d.pubMu.Lock()
	pubCh := d.pubCh
	d.pubMu.Unlock()
	if pubCh != nil {
		_ = pubCh.Close()
	}
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}
