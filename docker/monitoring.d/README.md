# Monitoring-as-Code bundle directory (dev stack)

This directory is mounted read-only into the dev `cerbix` container at
`/etc/cerbix/monitoring.d` and watched by the `platform` file provider configured in
`config.dev.yaml` (instance scope).

The file provider reads only `*.yaml` / `*.yml` files. `example.yaml.example` is intentionally
**not** matched, so out of the box the provider is idle (no monitors are created).

To exercise Monitoring as Code (and the `e2e/tests/file-providers.spec.ts` UI assertions):

1. Copy `example.yaml.example` to `example.yaml`.
2. Edit its `organization:` / `project:` to slugs that exist in your dev database.
3. The provider reconciles it within a couple of seconds — no restart. The monitors appear as
   read-only "Managed by file" in the UI, and `GET /api/v1/admin/file-providers` shows the
   bundle + runtime status.

Removing the file again orphans then disables (never hard-deletes) the monitors after the
orphan grace period.
