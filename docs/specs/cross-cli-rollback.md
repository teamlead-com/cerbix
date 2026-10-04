# Cobra CLI rollback — canonical recovery procedure

The shipped migration is not a one-commit change, so rollback is **not** a revert of one commit. The
CLI implementation changed in `6a72f647635cc59b67e327aa71489425b5114679` and
`07474d0b2537ccc9026de5883ac8fcf3d9a44d37`. The docs-only lifecycle and integration commits
`c810b65901446de37221f0b7f747ef9ed6e8f607` and `1ca047ac469b861483de81daf9133d52340a97b9`
do not carry CLI implementation. Reverting only the parser correction reintroduces known defects;
reverting only the initial migration leaves corrective source above a removed foundation; reverting only
integration documentation does not change runtime behavior. A whole-repository Git range also contains
documentation and historical records that cannot safely be rewound mechanically.

The preferred recovery contract is **path-scoped restoration** of the approved CLI/module path set to
the baseline tree `f6320fc1777e3ecd7201b06c21df64f2df936a12`. Restore together the production files
under `internal/cli/`, their CLI tests, `go.mod`, and `go.sum`. Paths absent from that baseline, including
parser-boundary files, must be removed as part of restoring the baseline tree, not left alongside the
native parser. Review the exact path diff and dependency graph before authorizing the operation; this
procedure intentionally gives no single-commit `git revert` command. Do not perform the restoration without separate owner approval.

Update long-lived documentation forward-only to describe the actually restored native parser: `README.md`,
`docs/overview.md`, `docs/specs/README.md`, `docs/specs/cross-cli-command-tree.md`, this procedure,
`docs/status.md`, `docs/traceability.md`, `docs/decisions.md`, and `CHANGELOG.md`. **Closed iteration
reports** are immutable closure snapshots: do not restore or edit them. Record the rollback and its
approval in a *new* numbered iteration and decision. No database, API, config, frontend, or persisted-data
rollback is required by this CLI-only change.

After path restoration and documentation reconciliation, require `go mod tidy -diff` (or a controlled
`go mod tidy`), CLI and full Go tests including the race suite, `go build`, `go vet`, `make docs-check`,
and a rebuilt real-binary compatibility matrix for command positions, help, version, and writer errors.
A partially green or unreviewed tree is not a rollback completion state.
