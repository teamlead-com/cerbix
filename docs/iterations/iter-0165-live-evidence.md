# iter-0165 — live run attestation (FR-025)

> Committed evidence, not a scratchpad reference: review [55] refused a claim that cited files outside
> the repository (a process restart had destroyed them, twice). Everything below was produced by the
> commands quoted here, against the image named here, and is reproducible by re-running them. No secret
> appears: the CI token's value is redacted at capture and the transcript was grep-verified.

## What was run, and against what

| field | value |
| --- | --- |
| repository commit | `f8f8cb4047679e38844cdb9c2dc53b65dcf87db9` (`f8f8cb4`, the tip of the range this file lands in) |
| image | `sha256:0c560a1026a4` — built by `make dev-build` from that commit's tree, run by `make dev-up` (single topology) |
| server clock at the run | 2026-08-31T05:50:42Z UTC (the CLI rows use instants inside `change.max_past`/`max_future` of it) |
| suite command | `CERBIX_TOPOLOGY=single CERBIX_URL=http://localhost:8080 ./e2e/run.sh` |
| suite result | **57 passed, 1 skipped, 6.4 min, exit 0** (the skip is the known idle-provider MaC UI case) |
| the three FR-025 tests | `changes.spec.ts:93` phases/replay/refusals/actor/decision-link (310 ms) · `:302` timeline, filters, cursor, bounds, comparison and its withholding (299 ms) · `:459` tenant isolation and the token allow-list (297 ms) |
| CLI binary | `go build -o /tmp/cerbix-cli ./cmd/cerbix` from the same commit |

## The suite's own lines for the FR-025 tests

```
✓  12 [chromium] › changes.spec.ts:93:7 › change intelligence › phases: the domain order, the identical replay, the refusals, the typed actor, and the decision link (310ms)
✓  13 [chromium] › changes.spec.ts:302:7 › change intelligence › the timeline: grouping, order, filters before the limit, the cursor, the bounds; the comparison and its withholding (299ms)
✓  14 [chromium] › changes.spec.ts:459:7 › change intelligence › tenant isolation on record/timeline/compare/incident-changes, and the token actions allow-list (297ms)
1 skipped
57 passed (6.4m)
```

## `cerbix change record`, live, with a CI token (`role: editor`, `actions: [gate:evaluate, change:record]`)

Every exit class of D13 against the running server. `CERBIX_URL=http://localhost:8080` and
`CERBIX_TOKEN=<redacted>` on every invocation; the token was revoked at the end and the service deleted
(both verified: no `cli-att` service or token remains).

```
01 started                         exit=0
  stdout: recorded change=01a05665-5610-7a58-a635-16d2fd923b25 kind=deploy phase=started
  stderr: —
02 succeeded ref                   exit=0
  stdout: recorded change=01a05665-5617-7c1d-990f-4182e99321df kind=deploy phase=succeeded
  stderr: —
03 identical replay                exit=0
  stdout: replayed change=01a05665-5617-7c1d-990f-4182e99321df kind=deploy phase=succeeded
  stderr: —
04 differing ref                   exit=2
  stdout: —
  stderr: cerbix: change: 409 phase_exists (ref): succeeded is already recorded with a different ref
05 failed after term               exit=2
  stdout: —
  stderr: cerbix: change: 409 phase_order (phase): succeeded already recorded
06 json (new ident)                exit=0
  stdout: {"replayed":false,"change":{"service_id":"6713519b-f2d2-48a0-bb1d-ff91146906d2","source":"github-actions","external_id":"att-2","kind":"rollback","id":"01a05665-562f-7031-af13-e337541f7338","phase":"succeeded","occurred_at":"2026-08-31T05:54:56Z","ref":"","url":"https://ci.example/run/2","actor_label":"token:e2e-cli-att2-1788155876","actor_user_id":null,"via_token":true,"recorded_at":"2026-08-31T05:57:56.911162Z"}}
  stderr: —
07 decision unknown                exit=2
  stdout: —
  stderr: cerbix: change: 400 decision_unknown (decision_id): 00000000-0000-0000-0000-000000000009 is not a decision of this service
26 url http                        exit=2
  stdout: —
  stderr: cerbix: change: 400 url_invalid (url): must start with https:// (http:// is refused)
17 source invalid                  exit=2
  stdout: —
  stderr: cerbix: change: 400 source_invalid (source): must match ^[a-z0-9][a-z0-9-]{0,63}$, got "Deploy_Bot"
18 kind invalid                    exit=2
  stdout: —
  stderr: cerbix: change: 400 kind_invalid (kind): must be one of deploy|rollback|flag, got "Deploy"
19 at out of bounds                exit=2
  stdout: —
  stderr: cerbix: change: 400 occurred_at_out_of_bounds (occurred_at): must be within 24h0m0s behind and 5m0s ahead of the server's current time 2026-08-31T05:57:33Z, got 2026-08-29T09:00:00Z
20 decision unknown                exit=2
  stdout: —
  stderr: cerbix: change: 400 occurred_at_out_of_bounds (occurred_at): must be within 24h0m0s behind and 5m0s ahead of the server's current time 2026-08-31T05:57:33Z, got 2026-08-31T09:11:00Z
21 explicit empty at               exit=2
  stdout: —
  stderr: cerbix: change: 400 occurred_at: must be an RFC3339 timestamp
22 unknown service                 exit=2
  stdout: —
  stderr: cerbix: change: 404 not found
23 missing --phase                 exit=2
  stdout: —
  stderr: change record: --phase is required
usage: cerbix change record --project <id> --service <id> --kind deploy|rollback|flag --phase started|succeeded|failed|cancelled --source <slug> --external-id <id> [--ref <label>] [--url <https url>] [--decision <id>] [--at <RFC3339>] [--json] [--timeout 10s]
24 bogus token                     exit=1
  stdout:
  stderr: cerbix: change: 401 unauthorized
25 no CERBIX_TOKEN                 exit=1
  stdout:
  stderr: cerbix: change: CERBIX_TOKEN is not set (the API token that authenticates to the server; environment only, never a flag)
26 revoked token                   exit=1
  stdout: —
  stderr: cerbix: change: 401 unauthorized
```

Rows 01–03 are D13's exit 0 (recorded, recorded, replayed with the SAME change id); 04–05, 07 and
16–23 are exit 2 — the contract's own refusals printed verbatim, stdout empty; 24–26 are exit 1 —
transport and credentials, never a decision. Row 21 (`--at ""`) is the fix of review [52]: an explicitly
given empty flag travels verbatim and the server, not the CLI, refuses it. Rows 01–03 also pin the
identical-replay rule end to end: the same body twice returns the same id and writes nothing.

## Reproducing

```bash
make dev-build && make dev-up
CERBIX_TOPOLOGY=single CERBIX_URL=http://localhost:8080 ./e2e/run.sh tests/changes.spec.ts   # targeted
CERBIX_TOPOLOGY=single CERBIX_URL=http://localhost:8080 ./e2e/run.sh                          # full suite
```

Leave ~60 s between a targeted and a full run: both spend the admin principal's 30 records/minute bucket
(§5a), and back-to-back runs earn a `429 principal_rate` that is the limiter working, not a failure.

## The UI test, added after the run above

`e2e/tests/changes-ui.spec.ts` drives the `Changes` card in a browser against the same stack: both groups
listed with the right `data-terminal`, the started-only row's compare cell `no-terminal`, the terminal
row's cell settling to a figure/pending/withheld word (never a partial number), the 6 h horizon control
changing the cell's `data-horizon`, the hop to the comparison view with both sides typed, and the hop to
the per-service timeline listing both groups.

```
Running 2 tests using 1 worker

  ✓  1 [setup] › auth.setup.ts:5:6 › authenticate as the bootstrap admin (609ms)
  ✓  2 [chromium] › changes-ui.spec.ts:43:7 › change intelligence UI › the card lists both groups, compares only the terminal one, and links on to the comparison and the timeline (762ms)

  2 passed (2.0s)
```

The full suite with it (started 95 s after the targeted run, so the admin's record bucket had refilled):

```
✓  12 [chromium] › changes-ui.spec.ts:43:7 › change intelligence UI › the card lists both groups, compares only the terminal one, and links on to the comparison and the timeline (731ms)
1 skipped
58 passed (6.2m)
exit=0
```

The counts above the UI test (57/1) and these (58/1) differ by exactly that one test; the single skip is
the same documented one, `file-providers.spec.ts:19`. No `429` in either run.

## Corrections (reviews [59], [60], [61])

The transcript above is what those commands printed, and it stays as it is — a record of a run is not
something to rewrite. Its LABELS were wrong in three ways, and a reader must not have to discover that
themselves.

**Row 20 is mislabelled.** It says `decision unknown`; its own stderr says `occurred_at_out_of_bounds` for
`2026-08-31T09:11:00Z`, which is the FUTURE side of the window, the counterpart of row 19's past side. The
run did not exercise `decision_unknown` at that row. (It is exercised in
[`iter-0165-cli-evidence.md`](iter-0165-cli-evidence.md) row 07, and the API tests have covered it
throughout.)

**The number 26 is used twice** — once for `url http` (exit 2) and once for `revoked token` (exit 1) — and
the numbering has gaps: 06 and 07 exist, 08 to 16 do not. The numbers are labels from the harness that
produced them, not a contiguous index, and reading them as one is what made the next error possible.

**The summary sentence is wrong about its own ranges.** It says "04–05, 07 and 16–23 are exit 2"; there is
no row 16, and the `url http` row it means is labelled 26. The exit CLASSES it states are right; the row
numbers naming them are not.

**Scope.** This file records a run at `f8f8cb4` with image `sha256:0c560a1026a4`. It has not been
reproduced at that commit. `iter-0165-cli-evidence.md` is a CURRENT-HEAD confirmation with its own image
and a complete raw transcript of every exit class, including the exit 1 credentials cases and the
explicitly empty `--at`; it says plainly that it does not reproduce this one.

One more correction, of a claim made in the party rather than in this file: the current-head run was
reported with image `sha256:27bc11c370b6`. That is the manifest exported by the first `make dev-build`;
`make dev-up` runs `dev-build` again, so the image the tested container actually ran is the second one.
The id in `iter-0165-cli-evidence.md` is that one.
