package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"git.example.com/monitoring/cerbix/internal/domain"
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
	jobsQueuePrefix = "checks.jobs."
	// Test probes ("Test connection") are RPCs per region: the API publishes to
	// checks.tests.<region> with a reply queue; a worker in that region runs the
	// probe and replies. The queue is auto-delete, so with no worker present the
	// request is unroutable and the caller times out ("no worker in region").
	testsQueuePrefix = "checks.tests."
	resultsQueue     = "checks.results"
	consumePrefetch  = 16
	forwardBuffer    = 256
	// testRPCTimeout bounds the wait for a region worker's test reply when the
	// monitor carries no explicit timeout; otherwise timeout+testRPCSlack is used.
	testRPCTimeout = 20 * time.Second
	testRPCSlack   = 5 * time.Second
)

// jobsQueueForRegion returns the per-region jobs queue name (empty → core).
func jobsQueueForRegion(region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return jobsQueuePrefix + region
}

// testsQueueForRegion returns the per-region test-RPC queue name (empty → core).
func testsQueueForRegion(region string) string {
	if region == "" {
		region = domain.DefaultRegion
	}
	return testsQueuePrefix + region
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
	conn   *amqp.Connection
	pubCh  *amqp.Channel
	pubMu  sync.Mutex
	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	jobRegion  string          // region this dispatcher's Jobs() consumes (worker); default core
	declaredMu sync.Mutex      // guards declared
	declared   map[string]bool // idempotent-declare cache for per-region job queues

	jobsOnce    sync.Once
	jobsCh      chan CheckJob
	resultsOnce sync.Once
	resultsCh   chan domain.Heartbeat
	testsOnce   sync.Once // guards the per-region test-RPC server (worker side)
}

// NewAMQP dials the broker and declares the durable job/result queues.
func NewAMQP(url string, logger *slog.Logger) (*AMQP, error) {
	var (
		conn *amqp.Connection
		err  error
	)
	for attempt := 1; attempt <= dialAttempts; attempt++ {
		if conn, err = amqp.Dial(url); err == nil {
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
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dispatch: amqp channel: %w", err)
	}
	// Results share a single queue; per-region job queues are declared on demand
	// (by the publisher before publishing, and by a worker before consuming).
	if _, err := ch.QueueDeclare(resultsQueue, true, false, false, false, nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dispatch: declare %s: %w", resultsQueue, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &AMQP{
		conn:      conn,
		pubCh:     ch,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		jobRegion: domain.DefaultRegion,
		declared:  map[string]bool{},
		jobsCh:    make(chan CheckJob, forwardBuffer),
		resultsCh: make(chan domain.Heartbeat, forwardBuffer),
	}, nil
}

// WithJobRegion sets which region's jobs queue Jobs() consumes (a worker's
// --region). Empty keeps the default (core).
func (d *AMQP) WithJobRegion(region string) *AMQP {
	if region != "" {
		d.jobRegion = region
	}
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
	d.pubMu.Lock()
	_, err := d.pubCh.QueueDeclare(queue, true, false, false, false, nil)
	d.pubMu.Unlock()
	if err != nil {
		return err
	}
	d.declared[queue] = true
	return nil
}

func (d *AMQP) publish(queue string, v any) error { return d.publishTo(queue, v, "") }

// publishTo publishes v to queue via the default exchange, optionally with a
// per-message TTL (Expiration, milliseconds as a string).
func (d *AMQP) publishTo(queue string, v any, expiration string) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("dispatch: marshal: %w", err)
	}
	d.pubMu.Lock()
	defer d.pubMu.Unlock()
	return d.pubCh.PublishWithContext(d.ctx, "", queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Expiration:   expiration,
		Body:         body,
	})
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
func (d *AMQP) Jobs() <-chan CheckJob {
	d.jobsOnce.Do(func() {
		queue := jobsQueueForRegion(d.jobRegion)
		if err := d.declareJobQueue(queue); err != nil {
			d.logger.Error("dispatch_declare_jobs", "queue", queue, "error", err.Error())
			return
		}
		go consume(d, queue, func(body []byte) bool {
			var job CheckJob
			if err := json.Unmarshal(body, &job); err != nil {
				d.logger.Error("dispatch_bad_job", "error", err.Error())
				return false
			}
			select {
			case d.jobsCh <- job:
				return true
			case <-d.ctx.Done():
				return false
			}
		})
	})
	return d.jobsCh
}

// Results starts (once) a consumer of the results queue and returns the
// forwarding channel. Only call it in a role that ingests results (api).
func (d *AMQP) Results() <-chan domain.Heartbeat {
	d.resultsOnce.Do(func() {
		go consume(d, resultsQueue, func(body []byte) bool {
			var hb domain.Heartbeat
			if err := json.Unmarshal(body, &hb); err != nil {
				d.logger.Error("dispatch_bad_result", "error", err.Error())
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
	region := m.Region
	if region == "" {
		region = domain.DefaultRegion
	}
	queue := testsQueueForRegion(region)

	// A dedicated channel keeps this RPC off the shared publish channel; the
	// exclusive, auto-delete reply queue self-cleans when the channel closes, and
	// its uniqueness makes any delivery on it unambiguously our reply.
	ch, err := d.conn.Channel()
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: test channel: %w", err)
	}
	defer ch.Close()
	replyQ, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: reply queue: %w", err)
	}
	replies, err := ch.Consume(replyQ.Name, "", true, true, false, false, nil)
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: consume reply: %w", err)
	}

	timeout := testRPCTimeout
	if s := m.TimeoutSeconds; s > 0 {
		timeout = time.Duration(s)*time.Second + testRPCSlack
	}
	body, err := json.Marshal(CheckJob{Monitor: m})
	if err != nil {
		return domain.Heartbeat{}, fmt.Errorf("dispatch: marshal test: %w", err)
	}
	if err := ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
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
		return hb, nil
	}
}

// ServeTests starts (once) a consumer of this dispatcher's region test-RPC queue
// (checks.tests.<region>) and answers each request by running run and publishing the
// heartbeat back to the delivery's ReplyTo. Only call it in a role that executes probes
// (worker). The queue is auto-delete so it disappears when this worker disconnects,
// which makes a stale region cleanly unroutable for the API.
func (d *AMQP) ServeTests(run func(ctx context.Context, m domain.Monitor) domain.Heartbeat) {
	d.testsOnce.Do(func() {
		queue := testsQueueForRegion(d.jobRegion)
		go func() {
			ch, err := d.conn.Channel()
			if err != nil {
				d.logger.Error("dispatch_test_channel", "queue", queue, "error", err.Error())
				return
			}
			defer ch.Close()
			if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
				d.logger.Error("dispatch_declare_tests", "queue", queue, "error", err.Error())
				return
			}
			msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
			if err != nil {
				d.logger.Error("dispatch_consume_tests", "queue", queue, "error", err.Error())
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
		}()
	})
}

// consume opens a dedicated channel, consumes queue with manual ack + prefetch,
// and calls handle for each body; handle returns whether the delivery was
// forwarded (ack) or should be dropped (nack, no requeue for a bad payload).
func consume(d *AMQP, queue string, handle func(body []byte) bool) {
	ch, err := d.conn.Channel()
	if err != nil {
		d.logger.Error("dispatch_consumer_channel", "queue", queue, "error", err.Error())
		return
	}
	defer ch.Close()
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

// Close stops consumers and releases the connection.
func (d *AMQP) Close() error {
	d.cancel()
	if d.pubCh != nil {
		_ = d.pubCh.Close()
	}
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}
