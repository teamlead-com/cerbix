# Cross-cutting contract conformance

> **Revision 1 — commissioned for iter-0183 on 2026-09-19.** This specification adds no product
> feature and changes no public API. It turns the two defect classes repaired by iter-0181 and
> iter-0182 into mechanical repository gates: a request field may not exist on only some contract
> surfaces, a test double may not implement a stronger write contract than the real store, and a
> tenant-bearing reference must name one validation owner at every bypassable boundary.

## 1. Problem

The status-page service binding was declared in OpenAPI, generated into the TypeScript client,
sent by the SPA and persisted by the store, but absent from the HTTP decoder. Because unknown JSON
fields are refused, a valid request failed as malformed JSON. The API fake already implemented the
missing behaviour, so handler tests could not expose the production gap.

Alert routing had the inverse shape: handlers checked project ownership while direct store and SQL
writers could persist foreign references. Both failures came from separately maintained artifacts
answering the same question without an executable conformance rule.

## 2. Scope and non-goals

This contract covers three seams:

1. registered JSON write DTOs against their OpenAPI component schemas;
2. the component-create contract shared by the API fake and PostgreSQL store;
3. the tenant-reference owner inventory introduced by the two preceding repairs.

It does **not** generate Go handlers, add an ORM or repository framework, change an HTTP response,
repair deployed data, broaden Cerbix into an observability platform, or make configuration/runtime
failures self-healing.

## 3. Contract-surface rule

A registered JSON write contract has one named Go DTO and one OpenAPI component schema. A Go test
compares their JSON property sets and required sets exactly, then compares a bounded type model
(`string`, `boolean`, `integer`, `number`, `array`, `object`) plus declared formats such as `uuid` and
`int64`. Removing `service_id` from `createComponentRequest`, changing a required field to optional,
or changing a UUID/string/integer shape on only one side must fail by field name. Fixture mutations
prove requiredness, type and format mismatches are detected rather than merely implementing the
comparison code and assuming it works.

The TypeScript side remains generated from `openapi.yaml`; the existing CI schema-drift gate runs
`npm run gen:api` and refuses a changed `frontend/src/api/schema.d.ts`. The Go parity test plus that
generation gate form one chain:

`Go transport DTO` ⇄ `openapi.yaml` → `frontend/src/api/schema.d.ts`.

The initial registry is deliberately the complete status-page write family implicated by the
production defect, not a claim that every historical anonymous request struct has already been
converted:

| OpenAPI schema | Go DTO | Handler |
| --- | --- | --- |
| `CreateStatusPage` | `createStatusPageRequest` | `createStatusPage` |
| `UpdateStatusPage` | `updateStatusPageRequest` | `updateStatusPage` |
| `CreateComponent` | `createComponentRequest` | `createComponent` |
| `UpdateComponent` | `updateComponentRequest` | `updateComponent` |
| `ConversionTarget` | `conversionTargetBody` | preview and confirm conversion |

Every new or modified status-page JSON write shape extends this registry in the same change.

## 4. Fake/real store conformance

The API fake and PostgreSQL store execute the same component-create case inventory from
`internal/contracttest`. Each backend owns only fixture construction and error classification; it
may not select a smaller set of cases.

The shared cases cover manual, monitor, service and retained-pair creation; missing page and
binding; foreign organization; page-project mismatch; and a retained monitor/service pair whose
projects disagree. The fake must derive `source`, `source_project` and `org_id` exactly as the store
does. An injected infrastructure error remains outside the behavioural suite and keeps its explicit
handler regression because it tests HTTP classification rather than store conformance.

## 5. Tenant-reference owner inventory

The behavioural tests remain the authority. The inventory below is an index over their ownership
seams, and every key is bound to an executable PostgreSQL behavioural/direct-SQL regression. The
named domain/store reference fields carry `tenantref` owner tags; adding another cross-object `*ID`
field to the guarded family without registering an owner fails review tests. Tags do not enforce
tenancy by themselves: composite foreign keys, triggers and store validation remain the product guard.

| Key | Reference | Store owner | Schema owner | Runtime owner | Behavioural regression |
| --- | --- | --- | --- | --- | --- |
| `status-page-component-binding` | component → monitor/service | `internal/store/statuspage.go::componentSourceOf` | `internal/store/migrations/00081_status_projection.sql::components_page_scope_trg` | `internal/store/statuspageprojection.go::ServicePageProjections` | `internal/store/componentservicebinding_internal_test.go::TestACreateRefusesABindingOutsideThePagesProject` |
| `monitor-notification-channel` | monitor → notification channel | `internal/store/notifications.go::LinkMonitorChannel` | `internal/store/migrations/00106_alert_routing_tenancy.sql::monitor_notifications_channel_project_fkey` | `internal/store/notifications.go::ListMonitorChannels` | `internal/store/alert_routing_tenancy_internal_test.go::TestAlertRoutingSchemaRejectsDirectForeignProjectWrites` |
| `escalation-policy-target` | policy → channel/schedule | `internal/store/escalation.go::assertEscalationTargetsInProject` | `internal/store/migrations/00106_alert_routing_tenancy.sql::escalation_policy_tenant_guard_trg` | `internal/store/escalation.go::AdvanceEscalations` | `internal/store/alert_routing_tenancy_internal_test.go::TestAlertRoutingStoreRejectsForeignProjectReferences` |
| `oncall-schedule-participant` | schedule → channel | `internal/store/escalation.go::assertParticipantsAreChannelsTx` | `internal/store/migrations/00106_alert_routing_tenancy.sql::oncall_schedule_tenant_guard_trg` | `internal/store/escalation.go::AdvanceEscalations` | `internal/store/alert_routing_tenancy_internal_test.go::TestAlertRoutingStoreRejectsForeignProjectReferences` |
| `oncall-override-channel` | override → schedule/channel | `internal/store/escalation.go::AddOnCallOverride` | `internal/store/migrations/00106_alert_routing_tenancy.sql::oncall_overrides_channel_project_fkey` | `internal/store/escalation.go::AdvanceEscalations` | `internal/store/alert_routing_tenancy_internal_test.go::TestAlertRoutingSchemaRejectsDirectForeignProjectWrites` |
| `incident-escalation-snapshot-target` | snapshot → frozen channel/schedule | `internal/store/serviceincidents.go::snapshotEscalationPolicyTx` | `internal/store/migrations/00106_alert_routing_tenancy.sql::incident_escalation_snapshot_tenant_guard_trg` | `internal/store/escalation.go::AdvanceEscalations` | `internal/store/alert_routing_tenancy_internal_test.go::TestEscalationSnapshotRejectsForeignProjectTargets` |

Expected tenant refusals remain generic and carry no target identifier as a metric label. An absent
or foreign identifier must not become an existence oracle. Infrastructure failures keep their
cause and follow the existing logged 5xx path.

## 6. Acceptance invariants

1. The registered Go/OpenAPI property sets, required sets, bounded types and declared formats are
   equal in both directions, and mutation fixtures fail each added dimension.
2. The generated TypeScript schema is still checked against OpenAPI by CI.
3. Fake and PostgreSQL component-create tests iterate one shared case inventory.
4. The fake refuses every tenant/project/pair violation the store refuses.
5. The tenant-owner registry resolves every named file, symbol/constraint and regression test; every
   key invokes its PostgreSQL regression, and guarded cross-object ID fields name a registered owner.
6. No new public endpoint, response field, configuration key or metric is introduced.
7. Default tests remain hermetic; PostgreSQL conformance stays behind `CERBIX_TEST_DATABASE_DSN`.
