# Spec: Transport resilience (func-transport-resilience)

## Purpose

Two transport-level survivability gaps. (1) The SSE stream can die silently on mobile
network switches: the server's keepalive is an SSE *comment*, invisible to the browser
`EventSource` API, so the client can neither detect the death nor distinguish a dead
socket from a healthy-quiet one. (2) A runtime RabbitMQ outage kills AMQP consumers
permanently and silently: the delivery channel closes, the consume loop returns, the
process stays "healthy" while probing stops until a restart; meanwhile the scheduler
logs a publish error per due monitor per tick.

## Iterations

| # | Iteration | Scope |
|---|---|---|
| 1 | iter-0070 | **SSE watchdog.** The server keepalive becomes a real `event: ping` (any bytes keep proxies alive — the comment is redundant once a real event exists). The client stamps `lastSeen` on every event (status/ping/open) and a 10s-interval watchdog recreates the EventSource after >75s of silence (two missed pings) — `started` stays true so the existing "reconnecting" chip shows until the new connection opens. |
| 2 | iter-0071 | **AMQP connection supervisor.** `NotifyClose`-driven: on broker loss log exactly one `broker_lost` WARN, redial with exponential backoff (1s→30s cap, occasional `broker_reconnecting`), on success one `broker_reconnected` INFO; re-open the publish channel and re-subscribe every consumer (jobs per region, results, tests). Internal Go channels never close — workers ride out the outage without restarts. Publish paths take the current channel under an RWMutex (the supervisor swaps it). Graceful shutdown (`ctx.Done`) is distinguished from broker loss — no false `broker_lost` on exit. The scheduler aggregates publish failures into one WARN per tick (`jobs_publish_failed count=N`) instead of a line per monitor. |

## Non-goals

Buffering/replaying jobs lost during an outage (the schedule re-issues on the next
tick — at-least-once by cadence); RabbitMQ cluster awareness; client-side SSE backoff
tuning (EventSource's native reconnect handles the error-ful cases).

## Acceptance

iter-0070: `curl` on the stream shows `event: ping` within 26s; vue-tsc green; suite
green. iter-0071: unit test for the supervisor's state transitions; live check on the
**distributed** compose profile (single runs inproc — AMQP unused): kill rabbitmq →
one `broker_lost`, sparse `broker_reconnecting`; start rabbitmq → one
`broker_reconnected` and probing resumes with no container restarts.
