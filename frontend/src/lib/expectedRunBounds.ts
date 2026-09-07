// FR-032 (G3): the ledger bounds the monitor detail panel needs, in ONE place.
//
// Neither of these is a client decision. `EXPECTED_RUN_RETENTION_DEFAULT_DAYS` is the server's own
// default for `ledger.expected_run_retention_days`, and `EXPECTED_RUN_RETENTION_MIN_DAYS` is the
// floor the API enforces — a range narrower than it is answerable on every instance, which is what
// makes it the safe fallback before the server has told the client its configured value.
//
// They were literals inside `MonitorDetailView.vue`, which is how a change on the server leaves a
// view asking for a range the server refuses. `expectedRunBounds.spec.ts` asserts each against the
// fixture `internal/domain/expectedrun_test.go` writes, the same shape the monitor and canary
// bounds already use.
export const EXPECTED_RUN_RETENTION_DEFAULT_DAYS = 14;
export const EXPECTED_RUN_RETENTION_MIN_DAYS = 2;
export const EXPECTED_RUN_RETENTION_MAX_DAYS = 90;
