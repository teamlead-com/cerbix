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
	// Test probes ("Test connection") are RPCs per region: the API publishes to
	// checks.tests.<region> with a reply queue; a worker in that region runs the
	// probe and replies. The queue is durable+auto-delete: it can be shared by N
	// workers (so it cannot be exclusive), recreates cleanly across broker/worker
	// restarts, and is removed after the last consumer leaves. Individual RPC
	// requests remain transient and time-bounded. Non-durable, non-exclusive queues
	// are rejected by RabbitMQ 4.3.
	testsQueuePrefix   = "checks.tests."
	testsV2QueuePrefix = "checks.tests.v2."
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

// jobsQueueForRegion returns the per-region jobs queue name (empty → core).
func jobsQueueForRegion(region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return jobsQueuePrefix + region
}

func jobsV2QueueForRegion(region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return jobsV2QueuePrefix + region
}

// testsQueueForRegion returns the per-region test-RPC queue name (empty → core).
func testsQueueForRegion(region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return testsQueuePrefix + region
}

func testsV2QueueForRegion(region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return testsV2QueuePrefix + region
}

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

	jobRegion  string          // region this dispatcher's Jobs() consumes (worker); default core
	protocolV2 bool            // worker consumes versioned envelope jobs/tests too
	declaredMu sync.Mutex      // guards declared
	declared   map[string]bool // idempotent-declare cache for per-region job queues

	jobsOnce    sync.Once
	jobsCh      chan DeliveredJob
	resultsOnce sync.Once
	resultsCh   chan domain.Heartbeat
	testsOnce   sync.Once // guards the per-region test-RPC server (worker side)
	testsV2Once sync.Once
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

// WithProtocolV2 enables consumption of the physically separate v2 jobs/tests queues.
// Publishers route from CheckJob.ProtocolVersion; an old worker never calls this and
// therefore can never receive an envelope-bearing payload.
func (d *AMQP) WithProtocolV2(enabled bool) *AMQP {
	d.protocolV2 = enabled
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
	queue := jobsQueueForRegion(region)
	if job.ProtocolVersion == ProtocolV2 {
		if job.CredentialEnvelope == nil {
			return errors.New("dispatch: protocol v2 job is missing credential envelope")
		}
		queue = jobsV2QueueForRegion(region)
	} else if job.CredentialEnvelope != nil {
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
				select {
				case d.jobsCh <- DeliveredJob{Job: job, CarrierGeneration: generation}:
					return true
				case <-d.ctx.Done():
					return false
				}
			})
		}
		consumeQueue(jobsQueueForRegion(d.jobRegion), "jobs", ProtocolV1)
		if d.protocolV2 {
			consumeQueue(jobsV2QueueForRegion(d.jobRegion), "jobs.v2", ProtocolV2)
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
	queue := testsQueueForRegion(region)
	if job.ProtocolVersion == ProtocolV2 {
		if job.CredentialEnvelope == nil {
			return domain.Heartbeat{}, errors.New("dispatch: protocol v2 test is missing credential envelope")
		}
		queue = testsV2QueueForRegion(region)
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
func (d *AMQP) ServeTestsV2(run func(ctx context.Context, job CheckJob) (domain.Heartbeat, error)) error {
	var initialErr error
	d.testsV2Once.Do(func() {
		queue := testsV2QueueForRegion(d.jobRegion)
		ready := make(chan error, 1)
		go func() {
			for {
				conn, wake := d.current()
				serveTestsV2Once(d, conn, queue, run, ready)
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

func serveTestsV2Once(d *AMQP, conn *amqp.Connection, queue string, run func(context.Context, CheckJob) (domain.Heartbeat, error), ready chan<- error) {
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
			var job CheckJob
			if err := json.Unmarshal(msg.Body, &job); err != nil || job.ProtocolVersion != ProtocolV2 || job.CredentialEnvelope == nil {
				d.deadLetter("tests.v2", msg.Body)
				_ = msg.Nack(false, false)
				continue
			}
			hb, runErr := run(d.ctx, job)
			if runErr == nil && msg.ReplyTo != "" {
				if reply, err := json.Marshal(hb); err == nil {
					_ = ch.PublishWithContext(d.ctx, "", msg.ReplyTo, false, false, amqp.Publishing{ContentType: "application/json", Body: reply})
				}
			}
			_ = msg.Ack(false)
		}
	}
}

// ServeTests starts (once) a consumer of this dispatcher's region test-RPC queue
// (checks.tests.<region>) and answers each request by running run and publishing the
// heartbeat back to the delivery's ReplyTo. Only call it in a role that executes probes
// (worker). The durable, shared queue auto-deletes after its last worker disconnects,
// which makes a stale region cleanly unroutable for the API while supporting multiple
// workers and RabbitMQ 4.3 (which rejects transient non-exclusive queues). It returns
// only after the initial declaration and consumer registration succeed.
func (d *AMQP) ServeTests(run func(ctx context.Context, m domain.Monitor) domain.Heartbeat) error {
	var initialErr error
	d.testsOnce.Do(func() {
		queue := testsQueueForRegion(d.jobRegion)
		ready := make(chan error, 1)
		go func() {
			for {
				conn, wake := d.current()
				serveTestsOnce(d, conn, queue, run, ready)
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
func serveTestsOnce(d *AMQP, conn *amqp.Connection, queue string, run func(ctx context.Context, m domain.Monitor) domain.Heartbeat, ready chan<- error) {
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
			hb := run(d.ctx, job.Monitor)
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
