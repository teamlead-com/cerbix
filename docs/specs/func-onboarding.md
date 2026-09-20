# func-onboarding — first useful Cerbix journey (FR-036 / NFR-030)

> **Revision 5 — IMPLEMENTED in closed iter-0187; revision-4 mock OWNER-APPROVED.**
> The owner approved the exact revision-4 mock and authorized implementation inside iter-0187. The
> shipped SPA remains presentation-only over canonical organization, project, monitor, heartbeat and
> region facts: no onboarding schema, progress endpoint, synthetic data or new server metric exists.

## 1. Product problem

A newly authenticated user can create organizations, projects, monitors, services, notification
channels, status pages, and reliability policies, but Cerbix does not guide the order or explain when
the first trustworthy signal exists. Current empty states are local to pages. They do not form one
resumable journey and can send an operator into advanced reliability surfaces before a monitor has
produced a single observation.

The onboarding goal is not product-tour chrome. It is the shortest truthful path from an authorized
account to one real monitored target and an understandable first result, while preserving Cerbix's
strict configuration, tenancy, and evidence-first direction.

## 2. Requirements

- **FR-036 — Onboarding flow.** Cerbix offers an access-aware, resumable path through the minimum real
  resources needed to reach a first useful monitoring result. Every completed step is derived from
  canonical product state; the flow creates no sample or shadow objects.
- **NFR-030 — Truthful and non-destructive guidance.** Onboarding must not invent defaults outside
  existing owners, bypass permissions, hide failures, claim a monitor is healthy before evidence,
  duplicate business state, or trap an experienced operator in a mandatory wizard.

These requirements are implemented in iter-0187 after the owner approved revision 4 of the artifact.
The implementation evidence is recorded in [`iter-0187.md`](../iterations/iter-0187.md).

## 3. Design questions iter-0187 must answer

1. **Audience:** global admin creating the first organization, org admin creating a project, project
   editor creating the first monitor, invited viewer, and returning partially-onboarded user.
2. **Entry:** automatic landing, dashboard checklist, explicit “Get started”, or a combination; exact
   dismissal and re-entry behavior.
3. **Minimum success:** whether the first useful outcome is monitor creation, first terminal heartbeat,
   first service, or another evidence-backed milestone. The recommendation to validate is: first
   monitor plus first terminal result, with service/reliability setup offered next rather than required.
4. **Progress source:** which existing API facts prove each step, and whether any new read model is
   required. No client-only “completed” bit may contradict canonical resources.
5. **Permissions:** what a viewer sees, what an editor can complete, and how the flow hands off to an
   administrator without implying the viewer can perform the step.
6. **Monitor choice:** which types are suitable for the primary path and how push, file-managed,
   credentialed, regional, and asynchronous monitors branch without flattening their contracts.
7. **Waiting:** copy and behavior between create and first observation, including scheduler/worker
   unavailability, UNKNOWN, config refusal, and timeout.
8. **Exit:** skip, close, resume, and “do not show automatically” semantics; whether dismissal is per
   user, browser, organization, or project.
9. **Existing installations:** no surprise modal for users whose tenant already has meaningful state.
10. **Accessibility/mobile:** keyboard order, focus restoration, reduced motion, screen-reader progress,
    narrow layouts, and no color-only state.

## 4. Non-negotiable product constraints

- No fake monitor, synthetic green result, sample incident, default notification target, or hidden SLO
  policy is created for demonstration.
- Every mutation uses the existing authorized product endpoint unless the approved design proves a
  missing orchestration/read endpoint. A new endpoint may not duplicate validation already owned by
  transport/domain/store.
- Progress is recomputed from server facts on reload. A preference may remember dismissal, but cannot
  be the authority for resource completion.
- A user may leave at every step. Onboarding never intercepts deep links or blocks ordinary navigation.
- Errors remain specific and actionable. A failed read is not rendered as an empty tenant; a pending
  result is not rendered as success.
- Secret values, bearer tokens, push tokens, and credentials follow existing reveal/copy contracts and
  are never persisted in onboarding state or analytics.
- The flow uses Cerbix vocabulary already present in the product: organization, project, monitor,
  service, status page, reliability. It does not introduce a parallel “workspace/app/check” model.

## 5. Required design artifacts and gate

Iter-0187 produced, in this order:

1. a journey inventory of current empty states, routes, roles, and API facts;
2. a written state machine covering fresh, partial, complete, dismissed, forbidden, failed-read,
   waiting-for-result, and existing-installation states;
3. content/copy for each state, including failure and waiting language;
4. an interactive or multi-screen artifact mock under `docs/design/` covering desktop and narrow width;
5. an accessibility annotation pass;
6. an explicit API/data delta list — preferably empty — with owner for every proposed new fact;
7. an owner review and recorded approval or rejection.

No implementation task may move to `IN_PROGRESS`, and no production source file may change, before
item 7. A technical review of the brief or mock is not owner approval.

## 6. Approved journey

The approved and implemented minimum path is:

1. choose or create an organization;
2. choose or create a project;
3. create one real monitor using the existing typed form;
4. wait for and explain the first terminal observation;
5. offer next actions: create a service, configure notifications, or publish a status page.

Steps 1–3 collapse when the resource already exists or the user lacks authority. Step 4 cannot be
checked off by creation alone. Step 5 is optional and must not turn the minimum path into a setup
catalog.

## 7. Evidence and privacy questions

The design must decide whether onboarding needs durable dismissal/progress state. If it does:

- name the owner and tenant scope;
- define retention/deletion with user and organization deletion;
- keep the schema free of credentials and target payloads;
- audit only security-relevant mutations, not view/click telemetry;
- define whether analytics exist at all. Default recommendation: no external analytics and only
  low-cardinality operational counters after implementation is approved.

## 8. Acceptance checklist for iter-0187

1. The state machine has no route where a read failure becomes “nothing configured”.
2. Every step names its canonical completion fact and authorization requirement.
3. The mock covers fresh global admin, invited viewer, partial progress, existing installation,
   scheduler/worker unavailable, first-result success, and first-result failure.
4. Skip/re-entry semantics are visible and non-coercive.
5. No hidden product defaults or duplicate validation owners are introduced.
6. Accessibility and narrow-width behavior are annotated.
7. The API delta is explicit and tenant-isolated.
8. The owner approves the exact mock revision before production implementation begins.

## 9. Iter-0187 plan and role split

| Priority | Task | Role |
| --- | --- | --- |
| P0 | Inventory routes, roles and empty states | Agents A/C |
| P0 | Define state machine and completion facts | Agents A/B/C |
| P0 | Produce multi-state artifact mock | Agent C/design |
| P1 | Threat-model permissions and secrets | Agent D + Agent B |
| P1 | Annotate accessibility and failure states | Agents B/C |
| P1 | Review API/data delta and observability | Agents A/D |
| P2 | Obtain explicit owner mock approval | Owner |
| P0 | Implement canonical-state guide and routing | Agent A |
| P1 | Add unit, component and live E2E coverage | Agent B |
| P1 | Record operational diagnosis and final evidence | Agents C/D |

## 10. Gates and deliverable

Iter-0187 runs docs-check, reference checks, artifact link validation, unit/component tests, frontend
type-check/build, the repository Go suite and the isolated live onboarding Playwright scenario. The
deliverable is the approved additive Dashboard guide plus synchronized operational and traceability
evidence. No migration or server contract is part of the delivery.

## 11. Non-goals

Marketing tours, demo data, billing, organization invitations, infrastructure provisioning, agent
installation automation, automatic alert-channel creation, automatic SLO/gate policies, and changing
the core resource model.

## 12. Journey inventory

### 12.1 Existing entry and mutation surfaces

| Need | Existing surface | Canonical fact | Authorization |
| --- | --- | --- | --- |
| Know the caller | `GET /api/v1/me` | user, global-admin flag and memberships | authenticated |
| Choose/create organization | workspace switcher; `GET/POST /api/v1/organizations` | returned organization row | read: visible membership/global admin; create: `global:manage` |
| Choose/create project | workspace switcher; `GET/POST /api/v1/organizations/{orgID}/projects` | returned project row | read: `org:read`; create: `org:manage` |
| List/create monitor | `/monitors`, `/monitors/new`; `GET/POST /api/v1/projects/{projectID}/monitors` | returned monitor row | read: `project:read`; create: `project:write` |
| Observe first result | monitor detail; `GET /api/v1/monitors/{monitorID}/heartbeats?limit=1` | newest heartbeat with `ts`, `up`, `latency_ms`, `code`, `msg` | `project:read` |
| Check worker region | typed monitor form; `GET /api/v1/regions` | selected region's `live` boolean | authenticated |

The existing organization/project dialogs and typed monitor form remain the mutation owners. The
onboarding surface links to them and re-reads canonical state after they close; it does not embed a
second validator or a reduced monitor form.

### 12.2 Current empty-state findings

- The workspace switcher already creates organizations for global admins and projects for org admins,
  but the dashboard does not sequence those actions.
- The monitor list already says `No monitors yet` and offers `Add the first one` to project writers,
  but it does not explain why a created monitor is still not a useful signal.
- The monitor detail already exposes recent heartbeats, push endpoint instructions, region choice and
  typed execution errors. These remain the detailed diagnostic surfaces.
- Viewer and editor capabilities are already computed from the central action model. Onboarding must
  consume those predicates, not compare role labels independently.
- A failed workspace/list/heartbeat read currently cannot be distinguished from an empty array in
  every client path. Onboarding therefore owns explicit load/error state and never renders a failed
  read as an empty step.

## 13. Implemented journey contract

### 13.1 Entry

Onboarding is an **inline dashboard guide**, not a modal and not a route guard.

1. It appears automatically only when the selected scope is incomplete and its local dismissal key is
   absent.
2. `Get started` in the shell reopens it explicitly while the selected scope is incomplete.
3. Closing it never changes canonical completion. `Do not show automatically` records only a local
   presentation preference.
4. Deep links, the workspace switcher and all normal navigation stay usable while it is open.
5. A selected project with at least one monitor and at least one heartbeat is an existing installation:
   the guide does not appear automatically and does not add a surprise completion modal.
6. On an existing installation, explicit `Get started` opens a compact, collapsible guide panel above
   the normal Dashboard KPI, availability and monitor-card content. The panel never replaces, filters,
   hides or mutates those cards; closing it restores the same Dashboard layout and data.
7. The review artifact introduces no parallel Cerbix visual system. Its product viewport is a static
   transcription of the current `AppShell`, `DashboardView`, `Kpi`, `MonitorCard`, `StatusPill`,
   `UptimeBar` and `Sparkline` rules; only the review controls outside that viewport are artifact chrome.

### 13.2 Minimum success

The first useful outcome is **one real monitor plus its first persisted heartbeat**. Monitor creation
alone is partial progress. A first heartbeat with `up=false` still completes onboarding because it is
real, actionable evidence; the completion state says the target failed and links to its monitor detail.
Service, notification, status-page and reliability setup are optional next actions and never part of
the completion denominator.

### 13.3 Monitor choice

- The primary recommendation is HTTP. TCP and DNS are equally valid low-friction alternatives.
- Push is a deliberate branch: creation is followed by sending one real heartbeat to the generated
  endpoint. The token is shown/copyable only under the existing monitor-detail contract.
- Credentialed types, synthetic and async canary link to the full typed form and its project-secret
  contracts; onboarding does not accept or retain credentials.
- Region selection remains in the typed form. If the selected region reports `live=false`, the guide
  says that no worker is connected and points to operations; it does not silently switch to `core`.
- Composite is not offered as a first-monitor recommendation because it requires existing children.
- File-managed monitors count as canonical monitors. Their desired state stays read-only and the guide
  points to the provider when creation or correction belongs in files.

## 14. State machine

The machine is recomputed whenever the guide opens, after a linked mutation returns, on project/org
selection change, and while waiting for a result. `failed_read` has precedence over every apparent
empty state.

| State | Canonical predicate | Primary copy/action | Exit |
| --- | --- | --- | --- |
| `loading` | required reads unresolved | `Checking your Cerbix workspace…` | read completes or fails |
| `failed_read` | any required org/project/monitor/heartbeat read fails | `We couldn't verify setup. Nothing has been marked incomplete.` Retry; ordinary navigation stays available | retry succeeds |
| `fresh_no_org` | organization list is successfully empty | global admin: create organization; others: `You don't have access to an organization yet. Ask a Cerbix global admin.` | organization appears |
| `org_no_project` | selected organization exists; project list successfully empty | org admin: create project; others: `An organization admin needs to create or grant access to a project.` | project appears |
| `project_no_monitor` | selected project exists; monitor list successfully empty | writer: choose monitor type; viewer: `You can view setup progress, but an editor or admin must create the first monitor.` | monitor appears |
| `monitor_exists` | at least one readable monitor; none has a heartbeat | choose the most recently created enabled monitor as the resume candidate; allow choosing another | candidate selected |
| `waiting_for_result` | candidate exists; heartbeat list successfully empty; selected region is live or unknown | `Monitor saved. Waiting for its first real result — not healthy yet.` Poll with increasing interval while visible; offer monitor detail and leave | heartbeat appears, read fails, or liveness changes |
| `worker_unavailable` | candidate is pull-based and selected region is known `live=false` | `No worker is connected in {region}. Cerbix cannot run this monitor there yet.` Link to monitor detail and runbook; never switch region automatically | worker becomes live or monitor changes |
| `scheduler_unknown` | region is live, no heartbeat arrives beyond the displayed expected wait, and no scheduler fact exists | `No result yet. A connected worker does not prove the scheduler issued a run.` Link to diagnostics; do not assert outage | heartbeat appears or proposed diagnostic fact resolves it |
| `first_result_up` | newest heartbeat exists and `up=true` | `First result received: UP.` Show timestamp, latency/code when present, and monitor detail | complete |
| `first_result_down` | newest heartbeat exists and `up=false` | `First result received: DOWN.` Show server message/code without rewriting it; `Open monitor` is primary | complete |
| `complete_existing` | any readable monitor has at least one heartbeat before automatic entry | no automatic guide; explicit reopen adds a compact completion panel above the unchanged Dashboard cards | collapse or close the panel |
| `dismissed` | incomplete canonical state plus matching local preference | do not auto-open; explicit `Get started` ignores dismissal for that invocation | explicit reopen or clear preference |
| `forbidden_handoff` | current step mutation is not granted | identify the required capability and handoff role; never show a disabled control without explanation | authorized user changes canonical state |

`UNKNOWN` is reserved for product states that already use that vocabulary. The onboarding wait state
uses `Waiting for first result`, not `UNKNOWN`, because there is not yet an observation to classify.

## 15. Copy and interaction rules

- Never say `healthy`, `operational`, `configured`, or `done` before a heartbeat exists.
- A DOWN first result is success for the journey and failure for the target; both facts are said in the
  same panel.
- Error copy preserves server error detail where the existing surface exposes it. Onboarding may add a
  next action but may not replace the error with `Try again later` alone.
- The progress label is factual: `Organization`, `Project`, `Monitor`, `First result`. Optional next
  actions sit outside the four-step progress element.
- `Skip for now` closes once. `Do not show automatically` persists the local preference. Both leave an
  explicit `Get started` re-entry.
- When context changes, focus moves to the guide heading only if the change was initiated inside the
  guide; otherwise the guide does not steal focus.

## 16. Dismissal, privacy and deletion

No durable server onboarding row is designed. Automatic-display dismissal is a `localStorage`
preference scoped by authenticated user id plus `project_id`, or user plus `org_id`/`account` before a
project exists. It stores only the boolean preference and its scope ids: no target, credentials,
heartbeat, role or progress snapshot. A different user on the same browser has a different key.
Clearing site data removes it; a deleted server user leaves an inert browser key that no other user
reads. Canonical completion is never written to browser storage.

There is no external analytics. A future iteration may add only low-cardinality operational
counters for guide read failures and state transitions after a separate metrics review; user ids,
resource ids, target values and monitor types are forbidden labels.

## 17. Accessibility and narrow-width annotations

- The guide is a labelled region, not an ARIA modal. The four steps are an ordered list; current state
  uses `aria-current="step"`; completion uses text plus icon, never color alone.
- Async load/wait/error updates use one polite live region. Repeated polling does not repeatedly
  announce unchanged copy.
- Every action is reachable in document order. Escape closes only when focus is inside the guide and
  returns focus to the invoking `Get started` control.
- The waiting indicator stops animation under `prefers-reduced-motion`; time and status remain text.
- At narrow width the step rail becomes a vertical list, metadata wraps, buttons become full-width,
  and no horizontal swipe is required. The review artifact's width toggle demonstrates this state.
- Copy buttons retain visible text feedback; secret/token values remain under their existing reveal
  contracts and are not repeated in the guide.

## 18. API/data delta and implementation boundary

### 18.1 Reused without change

Organization, project and monitor completion plus first-result success/failure are fully derivable
from existing tenant-isolated list/detail endpoints. The implementation requests only the selected
scope and uses `limit=1` while polling the selected candidate. No onboarding progress endpoint,
sample-data endpoint, orchestration mutation or schema row is needed.

### 18.2 One explicit diagnostic gap

The current product exposes worker-region liveness but no authenticated fact proving whether a
scheduler is live and successfully issuing runs for the selected monitor. The mock therefore renders
`scheduler_unknown` honestly and does not call it `scheduler unavailable`.

The owner chose the second bounded contract for iter-0187:

1. a future iteration may extend an authenticated operational read model with a low-cardinality scheduler
   readiness/last-success fact owned by scheduler/ops, without monitor ids or tenant payloads; or
2. **Implemented:** keep the guide's wording at `No result yet` and route operators to existing deployment readiness
   and metrics outside the SPA.

This choice does not change completion facts and authorizes no scheduler endpoint inside iter-0187.

## 19. Review artifact and approval status

The exact revision-4 review artifact is [`mock-onboarding.html`](../design/mock-onboarding.html). It
covers fresh global admin, org admin without a project, invited viewer, partial progress, waiting,
worker unavailable, scheduler-unknown diagnosis, first-result UP, first-result DOWN, failed read,
existing installation with the guide coexisting above the normal Dashboard, push setup and narrow
width. The product shell and Dashboard data components follow the current frontend source rather than
an approximate theme reconstruction.

The owner approved revision 4 on 2026-09-19 and explicitly authorized production implementation in
iter-0187. Revision 5 records the resulting implementation and verification evidence; the approved
revision-4 HTML remains immutable review evidence.
