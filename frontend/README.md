# cerbix frontend

The Vue 3 + TypeScript SPA. It is not deployed on its own — it ships inside the Go binary, in
two distinct steps that are easy to conflate:

1. `npm run build` writes **`frontend/dist/`**, which is **gitignored**. Nothing embeds it.
2. `make spa-snapshot` (repo root) builds, then replaces the **tracked** snapshot at
   **`internal/web/dist`** — `rm -rf` + `cp`, `Makefile:136-141`. That directory is what
   `//go:embed all:dist` compiles in (`internal/web/web.go`).

So a build alone never changes what a binary serves; only the snapshot step does. One binary then
serves the SPA, the REST API and `/auth` from a single origin — **there is no nginx** in any image
or compose file here; put Traefik or your own ingress in front for TLS.

## Stack

| Concern | What is actually used |
| --- | --- |
| Framework | Vue 3 (`<script setup>`), TypeScript |
| Build | Vite; `npm run build` is `run-p type-check build-only`, so **`vue-tsc` failing fails the build** |
| Routing | Vue Router — `src/router/index.ts` |
| State | Pinia — `src/stores/` (`session`, `workspace`, `live`, `branding`, `ui`) |
| API | `openapi-fetch` over types generated from `../openapi.yaml` |
| Styling | Tailwind CSS v4 via `@tailwindcss/postcss` |
| Utilities | VueUse, `unplugin-auto-import`, `unplugin-vue-components`, `vite-svg-loader` |
| Tests | Vitest + jsdom, specs colocated as `*.spec.ts` |

**Charts are hand-rolled SVG.** There is no charting library, by choice: the panels state what
the data supports and refuse to draw what it does not, which a general-purpose chart component
cannot express. See `docs/specs/func-truthful-rendering.md`.

## Layout

```
src/
  api/        client.ts (openapi-fetch), schema.d.ts (GENERATED), contract.ts
  components/ reusable pieces, each with its own *.spec.ts where it carries logic
  composables/
  lib/        pure logic extracted OUT of components so it can be tested directly
  router/     index.ts
  stores/     Pinia
  views/      one per route
```

`src/lib/` is where the rules live. A component that owns a rule cannot be tested without
mounting it, so geometry, formatting and validation are pure modules — `reliabilitygeometry.ts`,
`latencypanel.ts`, `wallclock.ts`, `scenarioBindings.ts`, `canaryWorkflow.ts` — and the component
renders what they return.

## The generated schema is a contract, not a convenience

`src/api/schema.d.ts` is generated; never edit it. After **any** change to `../openapi.yaml`,
regenerate it (`npm run gen:api`) and commit the result.

`src/api/contract.ts` holds compile-time assertions about the published wire schema. They live
there rather than in a spec file because `tsconfig.json` excludes `src/**/*.spec.ts` from type
checking — a type assertion written inside a spec is never evaluated at all.

## Commands

With node installed these are ordinary npm scripts, and that is the path CI takes — node 22 via
`actions/setup-node`, then `npm ci && npm run build && npm test` (`.github/workflows/tests.yml`):

```bash
# from frontend/
npm ci
npm run build      # vue-tsc + vite; a type error fails it
npm test           # vitest + jsdom
npm run gen:api    # after any openapi.yaml change
```

On a machine without node, the same commands run in Docker against the version CI uses. `--user`
and `npm_config_cache` are not optional there: without them the container writes `dist/` and
`node_modules/` as **root**, and the next run fails with `EACCES` because its own `npm ci` cannot
delete what root wrote.

```bash
docker run --rm --user "$(id -u):$(id -g)" -e npm_config_cache=/tmp/.npm \
  -v "$PWD":/app -w /app node:22-alpine sh -c "npm ci && npm run build"
docker run --rm --user "$(id -u):$(id -g)" -e npm_config_cache=/tmp/.npm \
  -v "$PWD":/app -w /app node:22-alpine sh -c "npm ci && npm test"
docker run --rm --user "$(id -u):$(id -g)" -e npm_config_cache=/tmp/.npm \
  -v "$PWD":/app -v "$PWD/../openapi.yaml":/openapi.yaml -w /app \
  node:22-alpine npm run gen:api
```

If a tree was already built as root, hand it back before either path works — the ids must expand
on the HOST, not inside `sh -c`, where `id -u` is 0:

```bash
docker run --rm -v "$PWD":/app alpine chown -R "$(id -u):$(id -g)" /app/dist /app/node_modules
```

**After ANY frontend change, from the repo root: `make spa-snapshot`.** It rebuilds the SPA and
replaces the committed `internal/web/dist`. A stale snapshot is invisible on every `make dev-*`
path — those build the image themselves — but `go build` and `go install` embed exactly what is
committed there, which once served a three-week-old UI.

The full set of repo-level commands is in [`../CLAUDE.md`](../CLAUDE.md); the routes and their
behavior are specified in [`../docs/specs/`](../docs/specs/).
