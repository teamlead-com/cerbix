# Spec: A rendering claims no more than the facts behind it (func-truthful-rendering)

> **Lifecycle.** DESIGN APPROVED, NOT IMPLEMENTED. The owner approved the scope and both
> forks put to him; the independent reviewer ruled on every geometry, encoding and
> tenant-context question at party [134]–[151]. No code and no mock exist yet. The mock is the
> next deliverable and must be approved before any frontend code (repository convention).

## 1. Purpose — the one rule

cerbix withholds a number it cannot defend. This specification extends that promise from
numbers to **pictures**: a rendering must not claim more than the facts behind it, and it must
be able to **distinguish the states it claims to distinguish**.

Three surfaces violate that today, and they fail in the same shape rather than three different
ones: each renders two genuinely different states identically, so the reader cannot tell them
apart and is not told that they cannot. A strip whose length does not mean duration; a chart
whose horizontal position does not mean time; an input whose contents do not say whether they
were saved.

This is a presentation package, in the manner of `func-hardening.md` and `func-audit-gaps.md`:
one rule, three surfaces, one iteration.

## 2. Requirement

- **FR-031** — the three surfaces of §5–§7 render only what the facts support, and every state
  they distinguish is distinguishable on screen.
- **NFR-025** — no timestamp is rendered without naming its zone. The requirement has **two
  halves, and only the first ships in this iteration**:
  - **NFR-025a — ships here.** The named formatter mechanism of §8 exists, and every timestamp
    rendered by the FR-031 surfaces of §5–§7 goes through it.
  - **NFR-025b — stays open.** The five pre-existing call sites enumerated in §9 are substituted
    onto that mechanism. NFR-025b remains `TODO` until they are, and iter-0174 may not be
    recorded as closing it.

  The halves are stated separately because an instance-wide sentence and §9's deliberate
  exclusion of those five sites cannot both be the shipped contract. This document asserted both
  in its first revision; corrected on reviewer P1 at party [153]. The follow-up is NOT weakened
  to resolve the contradiction — it keeps its own identifier, its own acceptance criterion and
  its own open status.

## 3. Non-goals — where the territory ends

D-0174 states what cerbix is not: it ingests no external telemetry and it is not an
observability platform. The owner raised the boundary himself, so it is written down rather
than assumed. **Out of scope, permanently for this package:** an ad-hoc query builder, a
user-chosen time-range picker on these panels, drag-to-zoom, panel composition, more than one
metric per panel, and a second series overlaid for comparison. What IS in scope is a hover
readout over data the page has already fetched, and geometry that means what it appears to
mean. The reviewer's finding at party [134] and again at [143] is recorded: none of the
accepted work crosses that line.

## 4. The findings this specification answers

Verified against the tree at `7cfb2ec`. Line numbers are that tree's.

### 4.1 The reliability timeline (`frontend/src/components/ReliabilityStrip.vue`, `frontend/src/components/ServiceReliability.vue`)

- **F1 — the x axis is bucket-count space, not clock space.** `reliabilityStepRollupTx` in
  `internal/store/servicereport.go` emits a point only for rollup steps that HAVE buckets;
  `ReliabilityStrip` lays those points out consecutively by `weight = p.buckets`, leaving no
  width for the time between them. A 30d window over a service with two and a half days of
  stored facts therefore fills 100% of the strip's width while the card's own KPI directly
  above it reads `storage 2873 of 43200 buckets`.
- **F2 — the gaps between ticks are decoration, not data.** Each tick is drawn at
  `x + w*0.08` with `width = w*0.84`, so the space between two neighbours is
  `0.08*(cell_left + cell_right)`. That is a hairline across ninety daily ticks and a canyon
  across three wide ones, and it reads as absence while meaning nothing. Measured against the
  owner's screenshot by pixel scan: drawn runs 51 / 516 / 337 px give cells 60.7 / 614.3 /
  401.2, predicting gaps of 54.0 px and 81.2 px against 54 px and 81 px measured, with the
  cells summing to 1076 px against a 1075 px strip. The model is exact; the holes are padding.
- **F3 — winner-takes-all step colouring, applied to few wide ticks.** `tickOf` resolves a
  whole step to ONE state by §9.1's order (bad, then unknown, then good, then excluded). At
  ninety daily ticks a single red day is a fair overstatement, and the code comment defends it
  correctly. At three ticks stretched across the full width it renders a service whose own row
  reads `availability 99.667%` as a wall of red.
- **F4 — each segment strip is normalised to 100% width.** A one-day segment and a three-day
  segment come out the same length, so a segment's length says nothing about its duration.

### 4.2 The monitor's Response time panel (`frontend/src/views/MonitorDetailView.vue`)

- **P1 — zero- and null-latency failures are dropped from the series.** `latencySeries` ends
  in `.filter((v) => v > 0)`. A network timeout is NOT affected — `internal/prober/prober.go`
  records `LatencyMS: elapsedMS(start)` on failure, so a timeout is plotted at roughly the
  timeout — but an enumerable class of real failures carries no latency at all and vanishes:
  every composite evaluation (`internal/prober/composite.go` returns no latency on any path),
  a config refusal, an ICMP setup failure, and a canary whose workflow is unreadable. Push
  monitors are moot; the panel is hidden for them.
- **P2 — the x axis is a bare index.** Position carries no time, and after P1's filter index
  `i` does not even correspond to heartbeat `i`.
- **P3 — the reference line is drawn from another population.** The dashed line is the 24h
  window's `p95_latency_ms` while the drawn series is the last ≤60 heartbeats, which at a 60 s
  interval is one hour. Nothing on the panel says so.

### 4.3 The project objective card (`frontend/src/views/SlaView.vue`)

- **S1 — a successful save produces no signal.** `saveProjectObjective` sets `projErr` on
  failure and nothing on success. The sibling editor in the same file has the affordance: a
  brief ✓ driven by `savedId`.
- **S2 — the draft survives a successful save.** `saveProjectObjective` calls `load()` and
  never resets `projDraft`, while `clearProjectObjective` does reset it. After Save the input
  still holds the typed number beside a headline that now reads the same, so an unsent draft
  and a stored fact render identically. This is the owner's report — "after pressing Save the
  buttons just stay there and mislead" — and its mechanism.
- **S3 (P0) — the editor escapes the context reset.** `resetMaintenanceState()` resets seven
  pieces of editor and busy state, and its own comment states the intent: "…and so is every
  piece of editor and busy state." `projDraft`, `projErr` and `projSaving` are not among them.
  A value typed for project A and left unsaved is still in the box under project B, where Save
  writes it — a legitimate write the store cannot refuse, and not the one the operator meant;
  A's error text also renders under B's name.
- **S4 (P0) — neither writer takes a load generation.** `saveProjectObjective` and
  `clearProjectObjective` capture no `loadGen`, while every neighbouring writer in the file
  does and one of them says why: "the project moved under this response". A save fired under A
  that returns after the switch writes A's error into B's screen and clears B's busy flag.

## 5. Surface A — the reliability timeline

### 5.1 Geometry is clock time (answers F1, F2)

Cells are generated from the **requested UTC range at the rollup grain**, not from the points
the response happens to contain. A boundary fragment keeps a width proportional to its real
time extent. Time with no stored bucket is a cell of its own in the `not-stored` encoding of
§5.2, named in the legend — it occupies width, because it occupied time.

This **refines** the time-weighted ruling of party [218] P1-3 rather than reversing it: bucket
count was a proxy for time extent (`domain.CanonicalBucket` is one minute), the two agree
wherever storage is complete, and they diverge in exactly one place — a storage hole, where
the proxy compresses unmeasured time to zero width. A boundary fragment remains proportional
under both. Ruled at party [140].

Inter-tick padding is a **fixed** width, never a fraction of the tick, so the grid motif reads
as a grid at every tick count.

**A cell's DRAWN width is never floored**, even when its projected width is sub-pixel: a cell too
short to see at this zoom is a cell whose duration is too short to see, and widening it would
overstate the time it covers. Its **hit target** may be widened and is marked as a non-geometric
affordance, so a sub-pixel cell stays reachable by pointer and keyboard without its drawing claiming
the extra time. Reviewer P1 at party [178] found the first implementation flooring every cell's
drawn width, which on a 30-day axis drew anything under ~10.8 minutes as ~10.8 minutes.

### 5.2 Five states, five encodings (answers F3)

A cell is a **proportional stack** of the states measured within it, not one winning hue.
Ruled at party [143]:

| State | Encoding |
| --- | --- |
| good / down / excluded | their existing status hues |
| unknown | a solid `--ink-3` slice — a decided neutral verdict, never a status hue |
| not-stored | an inset/background slice with a **diagonal hatch AND a cell outline**, labelled "no stored bucket" in the legend |

`provisional` remains **opacity**, and opacity encodes nothing else. Neither `unknown` nor
`not-stored` may use it.

**`not-stored` is not `unknown`.** `unknown` is a decided verdict with `unknown_us` behind it;
a missing bucket row is the absence of a verdict. Unmeasured time is never painted as unknown,
and the product must actually hold an unknown bucket before an unknown slice is drawn.

**A PROBLEM IS NEVER HIDDEN. The floor is the first mechanism for keeping that, not the promise
itself.** The distinction is load-bearing and this document's earlier revisions did not draw it:
they promised that a `bad`, `unknown` or `excluded` slice "never renders thinner than a stated
minimum", which the implementation cannot deliver in every cell — height is bounded by the cell, so
the floor is a GRANT and a grant can run out. Reviewer P1 at party [184] refused the incoherence:
the normative sentence and invariant 6 said "never invisible" while the tests asserted sub-floor
heights, and a documented trade-off paragraph does not reconcile two promises that exclude each
other. The contract is now stated once, in two parts:

1. **The floor, where the cell can fund it.** A `bad`, `unknown` or `excluded` slice thinner than
   the stated minimum is granted the difference. The grant total is
   `min(requested, cap, available)` and is distributed **in proportion** to what each slice asked
   for, then taken from the funders exactly — so the stack totals the cell's height by construction
   rather than by a reconciliation pass. Funders, in order: sealed good, provisional good, and — as
   a last resort — the absence. `good` is never floored in either form, because provisional good
   time is good time that is merely unsealed and inflating it would buy no honesty.
2. **The marker, where it cannot.** A slice the grant could not bring up to the floor — the cap
   bound, or the cell had nothing to fund from — is **marked** with the same non-geometric
   vocabulary a sub-pixel segment gets, and the cell's readout **names the state and gives its
   exact duration**. The readout takes the slices the strip ACTUALLY DREW: whether a slice was
   funded depends on the strip's height, because the floor is fixed in pixels and the cap is a
   share of that height, so the same cell can be fully funded on the 30px overview and marked in a
   14px lane. A readout that recomputes the stack at some other height describes a picture nobody
   is looking at — reviewer P1 at party [186], who found the overview and the lane rendering two
   different readouts from one intention. There is one readout component for both, so they cannot
   drift again.

   **Reachability, stated so the tests are not read as more than they are.** The cap binds only
   when the eligible count exceeds `0.3 × height` — five or more at 14px, ten or more at 30px — and
   neither surface can produce that: a segment lane runs at 14px but excludes provisional points by
   construction, so it has at most three eligible slices, and the overview runs at 30px where the
   cap allows 18px against at most 12px of demand. The cap branch is therefore a property of the
   allocation, pinned by a unit test. The path a SURFACE reaches is the no-funder one: a fully
   stored cell with no good — a day of `unknown` with a minute of `bad` — has nothing to fund from,
   and both surfaces mark it. Height is bounded and can run out; a marker is not, so the promise lives there.
   This is the same move §5.3 makes on the other axis for the same reason: where geometry cannot
   show something, something that is not geometry says so.

Two overflows were found in this allocation before it settled, both by the reviewer and both
reproduced before acceptance. At party [178]: the floor was paid only out of `good`, so one second
of `bad` in a 34 px cell with none returned 35.9996 px. At party [180]: the cap clamped the
accumulated DEBT after every eligible slice had already been raised, so six near-zero problem
slices in a 14 px lane asked for 12 px, were billed 8.4 px, and the difference came from nobody —
17.6 px in a 14 px cell. The API produces all six of those states, because a sealed and a
provisional point each carry `bad`, `unknown` and `excluded`.

**Consequence, recorded as a deliberate change to the motif of §12.2 of
`docs/specs/func-service-reliability.md`:** the short-tick encoding is RETIRED. Inside a
proportional stack, height is the slice's *quantity*, so height is no longer available to carry
a slice's *identity*. Ruled at party [138].

### 5.3 Segment lanes on the shared axis (answers F4)

Each segment renders as a **lane on the same window axis** as the overview strip, vertically
aligned with it, at its real time width. A lane's horizontal extent claims duration, so it is
**never floored**.

A lane whose projected width falls below one device pixel is met by a **fixed-width anchored
marker** at the segment's start boundary — not by a widened lane. The marker is explicitly
**non-geometric**, carries a distinct "sub-pixel segment / not to scale" affordance, and its
tooltip carries the exact `[from, to)`, the duration and the figures. Colliding markers stack
**vertically** or collapse into a deterministic count marker; they are never spread
horizontally. Ruled at party [140], and the mock must draw this case.

The slice floor of §5.2 and this lane rule are **two different mechanisms for two different
axes**: height inside a cell carries quantity and tolerates a floor; width across the axis
carries duration and does not.

**A lane's readout carries its STORAGE verdict, and withholds availability without it.** §11.2
of `func-service-reliability.md` makes storage continuity and decidable coverage independent
questions that must **both** pass, and putting segments on a real time axis is what makes the
difference visible: a segment spanning 28 days with 300 stored minutes renders 27 days of
`not-stored` beside the figure it quotes. `coverage = 100%` says only that every observation
that *exists* was decidable, which a storage hole makes worthless as a warrant. Therefore:

- Each segment readout states **storage** — `contiguous`, or `<stored> of <extent> · incomplete`.
- A segment whose storage is incomplete quotes **no availability**: a dash with a **local display
  explanation and no code identity** — "records begin later in this segment" when the missing
  storage is a contiguous prefix, "missing records inside this segment" when it is interior.
  **It is presentation, not a payload reason.** The client may derive presentation from the series
  it already holds, but it may not manufacture a wire verdict whose canonical meaning belongs
  server-side, so the explanation never leaves the component, never enters metrics, audit or the
  API, and never drives reliability state. Ruled at party [176], correcting this document's first
  revision, which had the component asserting the canonical codes
  `window_precedes_materialization_era` and `storage_gap`. Publishing the canonical identifier for
  a segment is a follow-up server contract, not a client inference.
- **Coverage stays printed** as its own separately named fraction, because it answers a different
  question and hiding it would lose information the operator needs.

Ruled at party [161]. The mock's own drawing produced the defect: it printed
`availability 100% · coverage 100%` next to 27 days of hatch.

**Colliding marks cluster, by the same rule.** Revision-boundary and epoch marks collide exactly
as lanes do — several definition changes minutes apart put their marks inside one pixel at any
window zoom — so the rule above is extended rather than joined by a second convention for the
same problem:

- The events whose fixed marks collide are **clustered** into ONE mark.
- The mark is **anchored at the earliest real boundary** in the cluster, never at its midpoint or
  at a rounded position: the anchor is the only geometric claim a cluster mark makes, and it stays
  true.
- The mark carries the **count**, plus the exact local `[first, last]` extent with its numeric
  offset and the UTC instants beneath it (§8).
- Its readout **lists every event in the cluster in chronological order**. A count may not stand
  in for the changes it counts.

Ruled at party [157], as the extension this document's mock proposed.

### 5.4 The hover readout

The strip has no tooltip today — not one. Every cell gains one: its extent, its state
composition, the good/bad/unknown/excluded split with exact durations, its bucket count, and
its `provisional` / `repairing` flags. Times follow §8.

## 6. Surface B — the monitor's Response time panel

### 6.1 The series (answers P1, P2)

The panel draws the ≤60 heartbeats the page **already fetches**; no new request and no range
picker. Each point carries its whole heartbeat — timestamp, up, code, latency, message —
because a hover readout that names a check cannot be built from a bare number, and carrying the
heartbeat is what fixes P1 and P2 rather than a separate change.

Zero- and null-latency failures stop being dropped. They are drawn as a baseline mark, so the
enumerated classes of §4.2 are visible instead of absent.

**The x axis is real timestamps**, not an ordinal index. Ruled at party [134], against this
document's first proposal: an ordinal axis with a wall-clock tooltip still invites the reader to
measure slope as a rate, and a label cannot do work the geometry has to do.

### 6.2 No stroke across unprovable time, and absence rendered positively

A connected polyline over a real time axis draws a straight line through time that was never
measured — F1's failure on another surface. The frontend may not derive "a due check is missing"
from `interval_seconds` plus a tolerance: cadence, probe overlap and dispatch delay are
application semantics, and such a formula is a product contract only once it is specified and
tested, not a chart heuristic. Ruled at party [136].

**The line cannot be earned from the facts that exist, and the reason is structural rather than
numerical.** This document twice proposed an allowance from published values and was twice wrong;
the second attempt was rejected at party [166] on three verified readings of the tree:

- `internal/prober/prober.go` runs `Retries + 1` attempts, each under its **own**
  `context.WithTimeout(m.Timeout())`, so one run may take up to `(retries+1) × timeout` — not one
  timeout.
- `result.allowed_skew` is only step 4b's clock test in `internal/store/monitors.go`
  (`ts.Before(hb.JobIssuedAt.Add(-skew))`), bounding how far *before* its job's issue an
  observation may claim to be. It bounds neither queue delay nor delivery, and it has no place in
  a spacing allowance.
- `job_issued_at` is **not a column of `heartbeats`**: it rides the wire for correlation and is
  gone after ingest. No row anywhere records that a run was ever *expected*.

Therefore two received heartbeats bound **observed spacing** and nothing more. Broker or worker
trouble makes queue wait unbounded, leader absence leaves no trace, and cerbix cannot witness its
own absence. No additional term rescues a formula whose subject is not in the data.

**So the panel draws points, no connecting stroke and no area fill** — the fill implies the same
continuity as the line and goes with it — and absence is rendered **positively** instead of being
left as whitespace:

**The observation ruler.** A thin band beneath the plot area carries **one neutral tick per
recorded heartbeat**, at its real x position, and nothing between them. Its empty spans are the
rendering of unobserved time, uniform across the whole span, so a six-minute hole and an ordinary
minute are the same statement at different sizes. Its rules, ruled at party [171]:

- It is **not a status series**, and it neither alters nor overlays the latency points.
- An empty span means only **"no check recorded between adjacent points"** — not late, not
  missed, not healthy or unhealthy, not covered, not anomalous. The panel's subtitle states this
  invariant once, in those terms.
- Hover or focus on an empty span gives the exact local bounded interval with its numeric offset
  plus both UTC instants (§8). It creates **no claim outside the first and last point**.
- **No threshold of any kind.** A fixed pixel threshold was specified at party [169] and
  **withdrawn** at [171] as an inadmissible density-dependent selector: at 60 checks an hour on a
  1072px plot the ordinary spacing is 16.5px, so 12 CSS px would have marked all 59 intervals and
  distinguished the hole from a normal minute not at all — and any relative threshold would be the
  anomaly detector [166] removed, in legibility costume.
- Accessibility may merge adjacent sub-hit-target empty spans into **one focus target only**,
  preserving every tick and the real geometry; a merged readout states its actual outer bounds, or
  enumerates the intervals it includes. **Interaction grouping is never rendered as semantic
  grouping.**
- No new colour or status vocabulary: neutral ink and the existing focus treatment. **No server
  classifier, no allowance, no API field, no metric.**

**`before_history` and `after_retention`** are separate absence-**explanation** regions, drawn only
where the response can state their bounds truthfully, and labelled as absence — never as
continuity. They are not evidence of coverage and not a prerequisite for the ruler.

**Declared maintenance is out of scope here** (party [169] item 4): with no stroke, no value can
cross excluded time, so the truth condition holds by construction. A declared-maintenance band is
recorded as a named optional presentation item; the monitor page reads no maintenance windows
today, and adding a fetch for decorative context is not this iteration's business.

**What would earn the line back is FR-032** (`func-expected-run-ledger.md`), not a line item here:
a durable expected-run ledger is a reliability-data-model change, and only its acceptance criteria
may ever permit a stroke between adjacent covered windows.

### 6.3 The references (answers P3)

The monitor's **timeout is stated in the panel header always**. It is drawn as a reference line
**only when it lies inside the computed plot extent**, and otherwise the header says so — for
example `timeout 10s — outside this scale`. That is a factual boundary, not a threshold: this
document's first proposal was a 25% trigger, which the reviewer correctly called a magic
number, and it is doubly unnecessary because a timeout that actually occurs is recorded at
roughly the timeout (§4.2 P1) and so raises the extent to include it.

`p95` is either recomputed over the drawn series or labelled with the window it comes from, so
two populations are never presented as one.

Hovering a point highlights the matching row in Recent checks: one object, one hover.

## 7. Surface C — the project objective card on /sla

The card becomes a **read-only stored state with an explicit Edit**. Edit opens the editor;
a successful, generation-guarded save closes it. The owner chose this over keeping the editor
permanently open, and the reviewer recommended the same, for one structural reason: a closed
editor cannot hold a stale draft, so S2 is retired by construction rather than by remembering
to reset a ref. This is a change to an **approved** mock
(`docs/design/mock-project-objective.html`, iter-0155) and is recorded as such, not as a new
drawing.

**An open form is not a substitute for a draft-state model.** All five requirements below hold
regardless of the card's shape, because visibility alone cannot prevent a stale async
assignment:

1. A successful save **clears the draft**, so the input falls back to its placeholder, which
   already renders the stored objective. State then reads by construction: empty input agrees
   with stored, filled input is an unsent draft.
2. A **bounded success confirmation**, on the shape the sibling editor in the same file already
   uses.
3. **Save is disabled** when the draft is empty or canonically equal to the stored value, by
   the existing one objective rule (`frontend/src/lib/objective.ts`, D-0165). A button that
   would do nothing is not offered.
4. The **context reset covers** `projDraft`, `projErr` and `projSaving`, applied **before any
   of the next project's data renders**.
5. **Both writers take a load generation** and gate every assignment on it, as every
   neighbouring writer in the file already does.

Requirements 4 and 5 are **complementary, not alternatives**: one stops a stale draft being
displayed, the other stops a stale response being written. Both are P0 (S3, S4).

## 8. The shared time mechanism (NFR-025's half that ships here)

**Identity is UTC; presentation is local.** The canonical grain, the requested range, bucket and
cell identity and every arithmetic stay UTC — a local-day grain would stop the cells
decomposing the number printed above them, and two viewers in different zones would read
different per-cell figures for one published availability. DST would make a "day" 23 or 25
hours, non-whole-hour offsets (+05:30, +05:45) would leave hourly cells unaligned, and
tz-aware truncation would put an IANA zone on the API for a display concern. Ruled at party
[143]. No timezone preference or setting is wanted in v1.

Every human-readable time renders in the **browser's zone with the numeric offset named**, and
the tooltip carries the **UTC instant on its own line** for correlation with logs.

**A UTC cell is never labelled as the viewer's calendar day.** A UTC day shown to a UTC+5
viewer starts at 05:00 their time, so the tooltip states the cell's real local extent — the
form `01.09 05:00 → 02.09 05:00 (UTC+05)` — and never their `01.09`. Labelling a UTC bucket as
a local calendar day is a boundary lie of exactly this package's kind.

**The mechanism is two named functions, not one formatter**: one for an **instant** (a
heartbeat's timestamp, a change mark) and one for a **UTC cell extent** (start→end plus offset,
plus the UTC line). There is deliberately no generic "format date" that could be handed a
bucket and produce a local calendar day. Formatting resolves the zone offset **at the instant**
via `Intl`, never from a cached current offset — a 30d window in late March or late October
crosses a DST boundary, so this is the ordinary case rather than an edge. Ruled at party [143].

It ships as one tested module under `frontend/src/lib/`, called from the surfaces of §5–§7.

## 9. NFR-025b — the `toLocaleString` sites, DONE 2026-09-04 — and NFR-025c, which is not

The SPA already renders browser-local times in four files without naming a zone, while the
reliability card renders UTC dates, and nothing on screen says which is which. That predates
this package; it is not created by it, and it is not fixed by it either. It is recorded as a
**mechanical** follow-up rather than prose, with its locations named so it cannot evaporate:

| Site | File |
| --- | --- |
| SLA report timestamps | `frontend/src/views/SlaView.vue` |
| escalation window | `frontend/src/views/EscalationView.vue` |
| outbox row dates | `frontend/src/views/AdminOutboxView.vue` |
| silence-until, and token last-used | `frontend/src/views/SettingsView.vue` (two call sites) |

**AC-NFR-025b — DONE 2026-09-04.** Each of the five call sites across these four files calls the
§8 instant renderer.

**The clause that used to follow that sentence was FALSE, and it was false when it was written at
iter-0174:** "so no rendered timestamp anywhere in the SPA is missing its zone". Substituting the
`toLocaleString` sites does not achieve that, because a `Date` can be rendered by hand —
`toISOString().slice(0,10)`, `getUTC*`, `get*` — and **29 such matches exist across 13 product
files**, several of them user-visible with no zone at all. Reviewer P1 at party [195] found it; the
over-claim is this document's, repeated by me when I reported the substitution done. Two
renderings were added to §8's mechanism for what the legacy sites needed, both instant-specific and
neither a generic formatter: `instantLabelShort` (minute precision, offset kept — a shorter
rendering does not get to drop the part the requirement is about) and `instantRangeLabel` (a window
between two instants an operator chose, distinct from a UTC cell's extent; it names the offset once
when both ends share it and twice when the window crosses a DST change, because then it genuinely
has two).

**The enforcement is a guard, not five edits.** A source scan in `wallclock.spec.ts` asserts that
NO product file renders a timestamp through `toLocaleString`/`toLocaleDateString`/
`toLocaleTimeString` — the exact construct that produced these five sites. Reverting any one of them
fails it by name, which is how *that* construct stays gone rather than being removed once. A surface
assertion in `SlaView.spec.ts` reaches what an operator actually reads.

### NFR-025c — the hand-rolled date surface, `TODO`

A **different and larger** surface from NFR-025b, and not a substitution: each site needs a
decision, not a rename.

| What it is | Where |
| --- | --- |
| a bare UTC date shown as a fact | `AgentTokensPanel.vue` (created/revoked), `SecretsPanel.vue` (created/rotated), `MembersPanel.vue` (added), `StatusPagesView.vue` (subscriber), `MonitorDetailView.vue` (created/updated), `PublicStatusView.vue`, `lib/gate.ts`, `lib/gateLedger.ts` |
| a UTC date that is IDENTITY and should say so rather than be converted | `ServiceReliability.vue` `dayLabel` — a segment's range |
| a compact clock, UTC, mostly unlabelled | `lib/changes.ts`, `lib/changesTimeline.ts` — one rendering there already says `` Z`` |
| already honest in its own words | `PublicStatusView.vue` (` UTC`), `lib/changes.ts` (` Z`) |
| **legitimately bare, and must stay so** | `datetime-local` INPUT values (`lib/gate.ts`, `SettingsView.vue`) — the HTML format is local and offset-free; day-grid MAP KEYS (`DashboardView.vue`, `MonitorDetailView.vue`, `PublicStatusView.vue`) and day-boundary comparisons (`gateLedger.ts`) — never rendered |

**AC-NFR-025c:** every site in the first three rows renders through a named §8 function — a UTC
date says UTC, a local one names its offset — and the last row is exempt with its reason recorded.

**The remainder is BOUNDED rather than described.** `wallclock.spec.ts` carries a ratchet: the
hand-rolled idiom is counted per file and compared against an enumerated list with a reason each. A
new file or a new call fails it, and so does a listed count that shrinks without the list being
updated — a stale allow-list is how a ratchet rots. Both directions are mutation-verified.

**NFR-025 is `IN_PROGRESS`, not `DONE`:** (a) the mechanism and the FR-031 surfaces, done at
iter-0174; (b) the five `toLocaleString` sites, done 2026-09-04; (c) this. Any row that says
otherwise repeats the over-claim above.

Deliberately not in this package's scope: widening it to four unrelated views. The reviewer
approved this split at party [143] on that ground.

## 10. Invariants

Discharged as a SET in `docs/traceability.md`.

1. A cell's width is its real time extent; unstored time occupies width and is drawn in the
   `not-stored` encoding.
1a. A cell's DRAWN width is never floored, even when its projection is sub-pixel; its hit target
   may be widened and says it is not geometry.
2. Inter-tick padding is a fixed width, independent of tick width.
3. A boundary fragment's width stays proportional to its real time extent.
4. `not-stored` and `unknown` are distinct encodings, and unmeasured time is never drawn as
   `unknown`.
5. `unknown` carries no status hue; `provisional` is the only user of opacity.
6. A PROBLEM IS NEVER HIDDEN. A `bad`, `unknown` or `excluded` slice thinner than the stated
   minimum is granted the difference where the cell can fund it; no other state is floored, and
   `provisional` good time is not. This is a grant, not an absolute floor — the claim that such a
   slice never renders thinner than the minimum is withdrawn, because height is bounded by the cell.
6a. A cell's stack totals exactly its height, by construction: the grant total is
   `min(requested, cap, available)`, distributed in proportion to what each slice asked for and
   taken from the funders — sealed good, then provisional good, then absence — exactly.
6b. A slice the grant could not bring up to the floor is MARKED with the non-geometric vocabulary,
   and the cell's readout names the state and gives its exact duration. The promise of invariant 6
   is kept there when height runs out.
7. The exact durations and bucket counts behind every cell are reachable from that cell.
8. No timeline surface uses height to carry a slice's identity.
9. A segment lane's width is never floored.
9a. A segment states its storage verdict, and quotes no availability while that storage is
   incomplete — a dash with a display-only explanation carrying no code identity, which never
   leaves the component. Coverage remains printed as its own separately named fraction.
10. A sub-pixel segment is represented by a non-geometric marker that says it is not to scale,
    and colliding markers never spread horizontally.
10a. A cluster mark is anchored at the earliest real event in its cluster, carries its count and
    exact extent, and its readout lists every event in the cluster in chronological order.
11. The Response time panel draws every heartbeat it fetched, including those with no latency.
12. No stroke and no fill spans time whose continuity is not proven.
12a. Unobserved time on the latency panel is rendered positively by the observation ruler, whose
    empty spans carry one meaning uniformly — no check was recorded between adjacent points — with
    no threshold, no server classifier and no claim outside the first and last point.
12b. Interaction grouping on the ruler is never rendered as semantic grouping.
13. The monitor's timeout is always stated, and drawn only inside the plot extent.
14. No two populations are presented as one number or one line without naming both.
15. Bucket and cell identity, the requested range and all arithmetic are UTC.
16. Every time rendered by an FR-031 surface names its zone, and a UTC cell is never labelled
    as a local calendar day. Instance-wide coverage is NFR-025b (§9), which this iteration does
    not discharge — invariant 16 is satisfied by the §5–§7 surfaces alone.
17. Zone offsets are resolved at the instant, never from a cached current offset.
18. A successful save clears its draft and confirms itself.
19. A control that would do nothing is disabled.
20. Draft, error and busy state reset on a context change, before the next context renders.
21. Every writer gates its assignments on the load generation it started under.

## 11. Deliverables (process)

1. This specification and the decision record — **before** the mock, at the reviewer's
   instruction.
2. The UI mock, including the two cases the reviewer asked to see drawn: a **sub-pixel segment**
   with its marker, and a **UTC cell tooltip** showing its true local extent. Owner approval
   gates all frontend code.
3. Implementation, `-race`, `npm test`, `npm run build`, `make spa-snapshot`, and the live
   browser suite.
4. The iteration report, `docs/decisions.md`, and the FR-031 / NFR-025 rows of
   `docs/status.md` and `docs/traceability.md` — FR-031 and **NFR-025a** as `DONE`, **NFR-025b**
   as `TODO` with its five sites named. A row that collapses the two is a false claim (§9).

## 12. Declined and out of scope

- Recording the heartbeat request's `limit` bound as a spec constraint — declined by the owner.
  Stated once here as context and nowhere as a rule: raw heartbeats are retained for
  `heartbeats.retention_days` (default 30), while one request returns at most 1000 rows.
- A timezone preference or setting (§8).
- Widening NFR-025's call-site substitution into this iteration (§9).
- Everything in §3.
