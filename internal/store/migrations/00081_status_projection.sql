-- +goose Up
-- FR-021 phase 4 (spec §15.0/§15.5, design gate cleared [310]): the status-page service
-- projection, the component source discriminator, the bounded public render, and the composite
-- retire lifecycle.

-- ── The component gains its own tenant identity ──────────────────────────────────────────
--
-- `components` reaches its org only through `status_pages` today, so a service_id alone would
-- bind service→project and leave component→org unbound: a direct writer could hang an org-B
-- service on an org-A page. The row therefore carries org_id, and `source_project` — the project
-- of its BINDINGS, deliberately NOT the page's scope, because an org-level page legitimately
-- holds components from several projects.
ALTER TABLE status_pages ADD CONSTRAINT status_pages_id_org_key UNIQUE (id, org_id);

ALTER TABLE components ADD COLUMN org_id uuid;
ALTER TABLE components ADD COLUMN source_project uuid;
ALTER TABLE components ADD COLUMN source text;
ALTER TABLE components ADD COLUMN service_id uuid;
-- The structural CAS of the preview/confirm contract: this counter is compared inside the
-- confirming transaction, and the page-level one below bumps on ANY component mutation because
-- the preview shows the page SUMMARY (a neighbour's edit changes what the operator consented to).
ALTER TABLE components ADD COLUMN revision bigint NOT NULL DEFAULT 0;
ALTER TABLE status_pages ADD COLUMN component_generation bigint NOT NULL DEFAULT 0;

-- Backfill is total and deterministic, and changes no rendered output: a monitor binding means
-- the component was monitor-backed; everything else — including rows with neither binding nor
-- status — is a manual component whose status an operator has not set.
UPDATE components c
   SET org_id = sp.org_id
  FROM status_pages sp
 WHERE sp.id = c.status_page_id;
UPDATE components c
   SET source_project = m.project_id
  FROM monitors m
 WHERE m.id = c.monitor_id;
UPDATE components SET source = CASE WHEN monitor_id IS NOT NULL THEN 'monitor' ELSE 'manual' END;

ALTER TABLE components ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE components ALTER COLUMN source SET NOT NULL;

ALTER TABLE components ADD CONSTRAINT components_source_chk
    CHECK (source IN ('monitor', 'service', 'manual'));
-- The source is a DISCRIMINATOR, not the presence of a column: the ACTIVE binding is required,
-- while the inactive one stays DORMANT so a revert can restore it. An exclusivity CHECK over
-- "which column is populated" cannot coexist with reversibility.
ALTER TABLE components ADD CONSTRAINT components_active_binding_chk
    CHECK ((source = 'monitor' AND monitor_id IS NOT NULL)
        OR (source = 'service' AND service_id IS NOT NULL)
        OR  source = 'manual');
-- ANY binding implies a project, and no binding implies no project. The rule keys on the
-- PRESENCE OF A BINDING, never on `source`: a composite FK is MATCH SIMPLE, so
-- `(monitor_id, source_project)` with a NULL project is not enforced at all — which would let a
-- row `source='manual', source_project=NULL, monitor_id=<another org's monitor>` keep a foreign
-- binding dormant with no tenant check anywhere.
ALTER TABLE components ADD CONSTRAINT components_binding_project_chk
    CHECK (
        (monitor_id IS NULL AND service_id IS NULL AND source_project IS NULL)
        OR ((monitor_id IS NOT NULL OR service_id IS NOT NULL) AND source_project IS NOT NULL)
    );
-- `no_data` is a COMPUTED statement that measurement is absent — never something an operator
-- types. A manual component says what its operator knows, or says nothing.
ALTER TABLE components ADD CONSTRAINT components_manual_no_data_chk
    CHECK (manual_status <> 'no_data');

-- Tenant-safe references on every axis. The monitor keeps the SHIPPED column-targeted SET NULL
-- (a deleted monitor's component already becomes manual today — §15.0 records that as an
-- inherited exception); the service is RESTRICT, because SET NULL would be the automatic
-- conversion invariant 70 forbids and CASCADE would silently delete a customer-visible row.
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_monitor_id_fkey;
ALTER TABLE components ADD CONSTRAINT components_org_fkey
    FOREIGN KEY (status_page_id, org_id) REFERENCES status_pages (id, org_id) ON DELETE CASCADE;
ALTER TABLE components ADD CONSTRAINT components_source_project_fkey
    FOREIGN KEY (source_project, org_id) REFERENCES projects (id, org_id) ON DELETE CASCADE;
ALTER TABLE components ADD CONSTRAINT components_monitor_fkey
    FOREIGN KEY (monitor_id, source_project) REFERENCES monitors (id, project_id)
    ON DELETE SET NULL (monitor_id);

-- The SET NULL above cannot stand alone: it clears the binding and leaves `source = 'monitor'`,
-- which `components_active_binding_chk` then rejects — so an ordinary monitor delete would fail
-- outright. The transition therefore happens BEFORE the FK action, in a trigger, because that is
-- the only place every path goes through: the API delete AND the D-0150 project cascade, which
-- deletes monitors through FK actions with no application involved.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION monitor_delete_release_components() RETURNS trigger AS $$
BEGIN
    -- PAGE FIRST, in id order: the UPDATE below fires the generation trigger, which takes each
    -- page AFTER these component rows are locked. Every other path locks page→component, so
    -- without this the cascade would be the one direction that can cycle.
    PERFORM 1 FROM status_pages
     WHERE id IN (SELECT DISTINCT status_page_id FROM components WHERE monitor_id = OLD.id)
     ORDER BY id FOR UPDATE;
    UPDATE components c
       SET source = CASE WHEN c.source = 'monitor' THEN 'manual' ELSE c.source END,
           monitor_id = NULL,
           -- `source_project` is the project OF THE BINDINGS: it survives only while one needs it.
           source_project = CASE WHEN c.service_id IS NOT NULL THEN c.source_project ELSE NULL END,
           updated_at = now()
     WHERE c.monitor_id = OLD.id;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER monitor_delete_release_components_trg
    BEFORE DELETE ON monitors
    FOR EACH ROW EXECUTE FUNCTION monitor_delete_release_components();
ALTER TABLE components ADD CONSTRAINT components_service_fkey
    FOREIGN KEY (service_id, source_project) REFERENCES services (id, project_id)
    ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS components_service_idx ON components (service_id);

-- ── The page-scope rule that a CHECK cannot express ──────────────────────────────────────
--
-- "A project-scoped page admits only components of THAT project" needs to read status_pages,
-- which a CHECK cannot do, and the (status_page_id, org_id) FK proves org only. It is a DEFERRED
-- CONSTRAINT TRIGGER on BOTH sides: inserting a foreign component, and narrowing a page's scope
-- afterwards. Deferred so a legitimate multi-statement rearrangement inside one transaction is
-- judged at COMMIT, on the final state.
-- DEFERRED is not a LOCK: it means "validated at COMMIT against THIS transaction's snapshot", so
-- an insert and a page-narrowing could each pass against a snapshot that did not contain the other
-- and both commit, leaving a project-scoped page holding another project's component. The page row
-- is the serialization point: this side takes it FOR UPDATE, and the narrowing side row-locks it
-- through its own UPDATE, so exactly one of the two survives in either order.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION components_page_scope_guard() RETURNS trigger AS $$
DECLARE
    page_project uuid;
    page_found   boolean := false;
BEGIN
    SELECT project_id, true INTO page_project, page_found
      FROM status_pages WHERE id = NEW.status_page_id FOR UPDATE;
    IF NOT page_found THEN
        -- The page is gone in this transaction (cascade); the component is leaving with it.
        RETURN NEW;
    END IF;
    -- An org-level page (NULL scope) admits any project of its org; the org itself is already
    -- enforced by the composite FKs above.
    IF page_project IS NOT NULL AND NEW.source_project IS NOT NULL
       AND NEW.source_project <> page_project THEN
        RAISE EXCEPTION 'component_project_outside_page_scope: component binds project % on a page scoped to project %',
            NEW.source_project, page_project
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION status_pages_scope_guard() RETURNS trigger AS $$
DECLARE
    offending uuid;
BEGIN
    IF NEW.project_id IS NOT NULL THEN
        SELECT c.source_project INTO offending
          FROM components c
         WHERE c.status_page_id = NEW.id
           AND c.source_project IS NOT NULL
           AND c.source_project <> NEW.project_id
         LIMIT 1;
        IF offending IS NOT NULL THEN
            RAISE EXCEPTION 'page_scope_conflicts_with_components: page scoped to project % still holds a component binding project %',
                NEW.project_id, offending
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- The binding columns are in the watch list because a CONVERSION can move a component to another
-- project's service; watching `source_project` alone would miss the write that changes it.
CREATE CONSTRAINT TRIGGER components_page_scope_trg
    AFTER INSERT OR UPDATE OF status_page_id, source_project, monitor_id, service_id ON components
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION components_page_scope_guard();

CREATE CONSTRAINT TRIGGER status_pages_scope_trg
    AFTER UPDATE OF project_id ON status_pages
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION status_pages_scope_guard();

-- ── The bounded public render ────────────────────────────────────────────────────────────
--
-- The public render is unauthenticated and already N+1 over components; a service component
-- multiplies that by 90 days of facts. A create-time cap alone does nothing about a page that is
-- already enormous, so the ceiling is PERSISTED at max(50, current) and may only SHRINK: an
-- oversized page cannot grow, and each remediation lowers it permanently. The absolute
-- fail-closed public ceiling lives in code (§15.0) because it protects the process, not the row.
ALTER TABLE status_pages ADD COLUMN component_ceiling integer NOT NULL DEFAULT 50;
UPDATE status_pages sp
   SET component_ceiling = GREATEST(50, (SELECT count(*) FROM components c WHERE c.status_page_id = sp.id));
ALTER TABLE status_pages ADD CONSTRAINT status_pages_ceiling_chk CHECK (component_ceiling >= 1);

-- ── The counters are DB-OWNED, and so is the ceiling's shrink rule ────────────────────────
--
-- "ANY component mutation bumps the page generation" cannot be application discipline while FK
-- actions are part of the contract: a project cascade deletes components with no application on
-- the path, so a surviving ORG-level page's generation would stay put and a preview for a
-- DIFFERENT component on that page could still be confirmed after a neighbour had vanished.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION component_revision_bump() RETURNS trigger AS $$
BEGIN
    -- Only when the writer did not already advance it, so an explicit UPDATE and this trigger
    -- cannot fight over the value.
    IF NEW.revision = OLD.revision THEN
        NEW.revision := OLD.revision + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER component_revision_bump_trg
    BEFORE UPDATE ON components
    FOR EACH ROW EXECUTE FUNCTION component_revision_bump();

-- The ceiling only ever SHRINKS, and a removal is what lowers it: an oversized page cannot grow
-- back to the size it inherited, and each remediation is permanent.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION component_page_generation_bump() RETURNS trigger AS $$
DECLARE
    page uuid := COALESCE(NEW.status_page_id, OLD.status_page_id);
    remaining integer;
BEGIN
    -- A MOVE between pages changes BOTH: the old page lost a line, so a preview taken against it
    -- has to die too. `COALESCE(NEW, OLD)` alone bumped only the destination and left the source's
    -- CAS live ([318] P1-3). One statement over both ids, so two concurrent moves cannot cycle.
    IF TG_OP = 'UPDATE' AND NEW.status_page_id IS DISTINCT FROM OLD.status_page_id THEN
        UPDATE status_pages sp
           SET component_generation = sp.component_generation + 1,
               component_ceiling = LEAST(sp.component_ceiling,
                                         GREATEST(50, (SELECT count(*) FROM components c
                                                        WHERE c.status_page_id = sp.id))),
               updated_at = now()
         WHERE sp.id IN (OLD.status_page_id, NEW.status_page_id);
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        SELECT count(*) INTO remaining FROM components WHERE status_page_id = page;
        UPDATE status_pages sp
           SET component_generation = sp.component_generation + 1,
               component_ceiling = LEAST(sp.component_ceiling, GREATEST(50, remaining)),
               updated_at = now()
         WHERE sp.id = page;
        RETURN OLD;
    END IF;
    UPDATE status_pages sp
       SET component_generation = sp.component_generation + 1, updated_at = now()
     WHERE sp.id = page;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER component_page_generation_bump_trg
    AFTER INSERT OR UPDATE OR DELETE ON components
    FOR EACH ROW EXECUTE FUNCTION component_page_generation_bump();

-- STRICTLY shrink-only, which is what §15.0 says. An earlier version admitted a raise up to
-- `max(50, count)` so the inherited value would be reachable — but the backfill above runs BEFORE
-- this trigger exists, so nothing legitimate ever needs to raise it, and the allowance only
-- weakened the invariant ([318] P1-3).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION status_page_ceiling_shrink_only() RETURNS trigger AS $$
BEGIN
    IF NEW.component_ceiling > OLD.component_ceiling THEN
        RAISE EXCEPTION 'status_page_ceiling_may_only_shrink: refusing to raise the ceiling of page % from % to %',
            OLD.id, OLD.component_ceiling, NEW.component_ceiling
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER status_page_ceiling_shrink_only_trg
    BEFORE UPDATE OF component_ceiling ON status_pages
    FOR EACH ROW EXECUTE FUNCTION status_page_ceiling_shrink_only();

-- ── The composite lifecycle ──────────────────────────────────────────────────────────────
--
-- ONE stored fact for the link, rendered from both ends, so there is no pair to fall out of
-- sync; deliberately non-unique, because two composites may legitimately be superseded by one
-- service. SET NULL on service deletion: losing the successor is a lost annotation, not a
-- corrupted monitor.
ALTER TABLE monitors ADD COLUMN superseded_by_service_id uuid;
ALTER TABLE monitors ADD CONSTRAINT monitors_superseded_by_fkey
    FOREIGN KEY (superseded_by_service_id, project_id) REFERENCES services (id, project_id)
    ON DELETE SET NULL (superseded_by_service_id);
CREATE INDEX IF NOT EXISTS monitors_superseded_by_idx ON monitors (superseded_by_service_id)
    WHERE superseded_by_service_id IS NOT NULL;

-- `retired_at` is the LIFECYCLE statement; `enabled` remains the execution switch. Retire sets
-- both in one transaction (§15.5): retired_at alone would leave a "retired" monitor probing and
-- paging, because the scheduler, dead-man, ingest and SLO paths all key on enabled.
ALTER TABLE monitors ADD COLUMN retired_at timestamptz;
CREATE INDEX IF NOT EXISTS monitors_retired_idx ON monitors (project_id)
    WHERE retired_at IS NOT NULL;

-- +goose Down
DROP TRIGGER IF EXISTS status_page_ceiling_shrink_only_trg ON status_pages;
DROP FUNCTION IF EXISTS status_page_ceiling_shrink_only();
DROP TRIGGER IF EXISTS component_page_generation_bump_trg ON components;
DROP FUNCTION IF EXISTS component_page_generation_bump();
DROP TRIGGER IF EXISTS component_revision_bump_trg ON components;
DROP FUNCTION IF EXISTS component_revision_bump();
DROP TRIGGER IF EXISTS monitor_delete_release_components_trg ON monitors;
DROP FUNCTION IF EXISTS monitor_delete_release_components();
ALTER TABLE monitors DROP COLUMN IF EXISTS retired_at;
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_superseded_by_fkey;
ALTER TABLE monitors DROP COLUMN IF EXISTS superseded_by_service_id;
ALTER TABLE status_pages DROP CONSTRAINT IF EXISTS status_pages_ceiling_chk;
ALTER TABLE status_pages DROP COLUMN IF EXISTS component_ceiling;
DROP TRIGGER IF EXISTS status_pages_scope_trg ON status_pages;
DROP TRIGGER IF EXISTS components_page_scope_trg ON components;
DROP FUNCTION IF EXISTS status_pages_scope_guard();
DROP FUNCTION IF EXISTS components_page_scope_guard();
DROP INDEX IF EXISTS components_service_idx;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_service_fkey;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_monitor_fkey;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_source_project_fkey;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_org_fkey;
ALTER TABLE components ADD CONSTRAINT components_monitor_id_fkey
    FOREIGN KEY (monitor_id) REFERENCES monitors (id) ON DELETE SET NULL;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_manual_no_data_chk;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_binding_project_chk;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_active_binding_chk;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_source_chk;
ALTER TABLE status_pages DROP COLUMN IF EXISTS component_generation;
ALTER TABLE components DROP COLUMN IF EXISTS revision;
ALTER TABLE components DROP COLUMN IF EXISTS service_id;
ALTER TABLE components DROP COLUMN IF EXISTS source;
ALTER TABLE components DROP COLUMN IF EXISTS source_project;
ALTER TABLE components DROP COLUMN IF EXISTS org_id;
ALTER TABLE status_pages DROP CONSTRAINT IF EXISTS status_pages_id_org_key;
