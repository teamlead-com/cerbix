# cerbix frontend

Vue 3 + TypeScript (Vite) SPA for cerbix. Scaffolded in a later iteration
(Phase 4), driven by the design track (claude.ai/design) started after Phase 1.

Planned stack: Vue 3, Pinia, Vue Router, TanStack Query, uPlot/ECharts for
charts, and a TypeScript API client generated from `../openapi.yaml`. The built
static bundle is embedded into the Go binary via `embed.FS` and served by the
`api` role.

Screens (see `docs/specs/` and the plan's design-track section):
login + org/project switcher, project dashboard, monitor detail (SLA/error
budget/incident timeline), monitor editor, members & roles, notification
channels, API tokens, status page + editor, incident & postmortem editor.
