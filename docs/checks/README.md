# docs/checks

Immutable dated review/audit snapshots. Never edited after publication.

## Naming

```
YYYY-MM-DD-<kind>[-vN].md
```

- `<kind>`: e.g. `implementation-review`, `security-review`.
- `-vN`: optional revision within the same day.

Examples: `2026-08-01-implementation-review-v1.md`, `2026-08-01-security-review-v1.md`.

Reviews reference requirement IDs from `docs/status.md` and record findings; fixes land
in a later iteration with their own report in `docs/iterations/`.
