# func-onboarding — first useful Cerbix journey (FR-036 / NFR-030)

> **Revision 1 — DESIGN BRIEF for iter-0187, commissioned 2026-09-19. NOT DESIGNED, NOT
> APPROVED, NOT IMPLEMENTED.** The owner explicitly requires a separate design and an approved
> artifact mock before implementation. Iter-0187 is therefore a design-only product iteration; no
> schema, API, store, router, or SPA behavior is authorized by this file.

## 1. Product problem

A newly authenticated user can create organizations, projects, monitors, services, notification
channels, status pages, and reliability policies, but Cerbix does not guide the order or explain when
the first trustworthy signal exists. Current empty states are local to pages. They do not form one
resumable journey and can send an operator into advanced reliability surfaces before a monitor has
produced a single observation.

The onboarding goal is not product-tour chrome. It is the shortest truthful path from an authorized
account to one real monitored target and an understandable first result, while preserving Cerbix's
strict configuration, tenancy, and evidence-first direction.

## 2. Requirements to design

- **FR-036 — Onboarding flow.** Cerbix offers an access-aware, resumable path through the minimum real
  resources needed to reach a first useful monitoring result. Every completed step is derived from
  canonical product state; the flow creates no sample or shadow objects.
- **NFR-030 — Truthful and non-destructive guidance.** Onboarding must not invent defaults outside
  existing owners, bypass permissions, hide failures, claim a monitor is healthy before evidence,
  duplicate business state, or trap an experienced operator in a mandatory wizard.

These requirements stay `TODO` until iter-0187 produces and the owner approves the design contract and
mock. Approval authorizes a later implementation iteration, not implementation inside iter-0187.

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

## 5. Required design artifacts

Iter-0187 must produce, in this order:

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

## 6. Candidate journey to validate, not an approved contract

The first mock should test this hypothesis:

1. choose or create an organization;
2. choose or create a project;
3. create one real monitor using the existing typed form;
4. wait for and explain the first terminal observation;
5. offer next actions: create a service, configure notifications, or publish a status page.

Steps 1–3 collapse when the resource already exists or the user lacks authority. Step 4 cannot be
checked off by creation alone. Step 5 is optional and must not turn the minimum path into a setup
catalog. The design iteration may replace this hypothesis if research against the current product
shows a better Cerbix-native path.

## 7. Evidence and privacy questions

The design must decide whether onboarding needs durable dismissal/progress state. If it does:

- name the owner and tenant scope;
- define retention/deletion with user and organization deletion;
- keep the schema free of credentials and target payloads;
- audit only security-relevant mutations, not view/click telemetry;
- define whether analytics exist at all. Default recommendation: no external analytics and only
  low-cardinality operational counters after implementation is approved.

## 8. Approval checklist for iter-0187

1. The state machine has no route where a read failure becomes “nothing configured”.
2. Every step names its canonical completion fact and authorization requirement.
3. The mock covers fresh global admin, invited viewer, partial progress, existing installation,
   scheduler/worker unavailable, first-result success, and first-result failure.
4. Skip/re-entry semantics are visible and non-coercive.
5. No hidden product defaults or duplicate validation owners are introduced.
6. Accessibility and narrow-width behavior are annotated.
7. The API delta is explicit and tenant-isolated.
8. The owner approves the exact mock revision before an implementation iteration is numbered.

## 9. Iter-0187 plan and role split — design only

| Priority | Task | Role |
| --- | --- | --- |
| P0 | Inventory routes, roles and empty states | Agents A/C |
| P0 | Define state machine and completion facts | Agents A/B/C |
| P0 | Produce multi-state artifact mock | Agent C/design |
| P1 | Threat-model permissions and secrets | Agent D + Agent B |
| P1 | Annotate accessibility and failure states | Agents B/C |
| P1 | Review API/data delta and observability | Agents A/D |
| P2 | Obtain explicit owner mock approval | Owner |
| P2 | Record decision and future implementation scope | Agent C |

## 10. Gates and deliverable

Iter-0187 runs docs-check, reference checks, artifact link validation, and a structured independent
review against §8. It does not claim Go, migration, frontend, or Playwright implementation evidence
unless the owner explicitly opens a later implementation iteration.

The deliverable is an approved design revision or a documented rejection with unresolved questions.
It is not production onboarding.

## 11. Non-goals

Marketing tours, demo data, billing, organization invitations, infrastructure provisioning, agent
installation automation, automatic alert-channel creation, automatic SLO/gate policies, and changing
the core resource model.
