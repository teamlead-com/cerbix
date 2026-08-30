# Spec: Incident context — heuristic RCA context for auto-incidents

**Iteration:** iter-0037 · **Complexity:** S–M (the lightest of the batch) · **SPA:** no new
components (uses the existing incident timeline) → no artifact mockup required.

## Pain point
When a service goes down the incident is empty: the on-call engineer assembles the picture of
"what else went down, what error, which region" by hand. 80% of this routine can be automated without AI.

## Mechanism
- **Trigger:** outbox `TopicIncidentEvent` with status opened for an **auto**-incident
  (opened by a monitor). Manual/API incidents are not touched.
- **Context assembly** (store, a single window of ±5 minutes from the incident start):
  1. **Co-failures:** monitors of the same project that went down within the window — count + names (top 5).
  2. **Error classes:** classification of the `msg`/`code` of their failing heartbeats —
     timeout / dns / connection refused / tls / http-5xx / http-4xx / body-assert / other;
     dominant class.
  3. **Geo:** whether all failures are in one region (by `monitors.region`).
  4. **Root cause via the dependency graph** — to be added in iter-0040 (reserve the field
     in the report: if a down ancestor exists → "likely root cause: <parent>").
- **Rendering:** human-readable text, for example:
  "⚡ Context: 7 monitors of this project went down within ±5m (api, worker, cache, …);
  dominant error: connection refused; all in region geo1."
- **Where it is written:** `incident_updates` (author = system) — automatically visible in the
  timeline UI and in the feeds. **Idempotency:** exactly one context update per incident (checked
  against a marker prefix of existing updates before insertion).
- A context assembly failure must not break delivery of the original event (best-effort:
  WARN log, event delivered).
- **A second system note (FR-025, iter-0165):** at the same `opened` delivery of a SERVICE auto-incident
  the outbox worker also appends `🚀 Changes: <n> preceded this incident — …` from `LinkPrecedingChanges`
  (`internal/store/change.go`), naming the changes recorded on the service and on its `probable_root`
  upstreams within `change.correlation_window` — "preceded", never "caused"; best-effort exactly as this
  note is (`func-change-intelligence.md` D7).
- **The shared marker guard:** both notes are `author = system` rows of `incident_updates` written through
  the same idempotency guard — `NOT EXISTS (… WHERE incident_id = $1 AND author = 'system' AND body LIKE
  '<marker>%')` — under distinct unique prefixes (`domain.IncidentContextMarker` `⚡ Context:`,
  `domain.ChangesMarker` `🚀 Changes:`, and `⏸ Suppressed:` for suppression), so a retried delivery writes
  neither twice and any new system note must bring its own unique prefix.

## Layers
- **store:** `IncidentContext(ctx, incident) (domain.IncidentContext, error)` — a single
  window query (co-failures + classes + regions); `AppendSystemIncidentUpdate(ctx, id, text)`
  with an idempotency marker.
- **domain:** `IncidentContext` (CoFailures []name, DominantErrorClass, SingleRegion,
  ReservedRootCause) + `Render()` text; classifier `ClassifyProbeError(msg, code)` —
  a pure function.
- **outbox:** in the `TopicIncidentEvent` case, after successful webhook delivery — if opened
  and auto → assemble and attach the context (best-effort).

## Tests
- domain: `ClassifyProbeError` table-driven (timeout/dns/refused/tls/5xx/4xx/other), Render.
- store: IncidentContext — co-failures inside/outside the window, dominant class, one region
  vs different ones; idempotency of AppendSystemIncidentUpdate.
- outbox: opened auto-incident → exactly one system update; repeated event delivery —
  no duplicate; a non-auto incident — no context; a store error — the event is still delivered.
- E2E: take down 3+ monitors of one project → a system summary in the incident timeline
  with the error class and the list of co-failures.

## Out of scope
LLM summarization (a separate decision, default off); root cause via dependencies (iter-0040
will fill the already prepared field); analysis of the response body beyond the existing `msg`.

## Acceptance
An incident opened by a monitor gets one system summary entry in the timeline ≤10s after opening;
`-race` green; the E2E scenario above reproduces on a live stack.
