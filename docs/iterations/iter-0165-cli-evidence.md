# iter-0165 — the change CLI against a running server, confirmed at the current head

> This file is EVIDENCE, not narrative. Every line below is raw stdout/stderr and an exit code from the
> commands quoted with them, against the image named here. `CERBIX_TOKEN` never reached this log: the
> capture substituted it in every argv, and `grep` for the literal returns zero.

Written because review [59] of the close-out party was right about two things at once: the earlier
attestation summarised where it should have transcribed, and a reproduction built from a later tree is not
a reproduction of a historical claim. So this file claims only what it is.

## What this is, and what it is not

| | |
| --- | --- |
| repository commit | `b320290c9ef6f0a19304816b991b62d3cc369b78` — the CURRENT head, not the attested `f8f8cb4` |
| image | `sha256:14ee5956800c710582ae63386c93c64714c4eeeb735e08e097fcef3a8fe6e928` — built by `make dev-build`, which `make dev-up` runs again, so this is the id the tested container actually ran |
| what it shows | that the D13 contract holds on the tree as it stands today, every exit class, raw |
| what it does NOT show | that the run recorded in `iter-0165-live-evidence.md` happened as written. That run was on `f8f8cb4` with image `sha256:0c560a1026a4`, and fourteen commits have landed since. This file does not reproduce it and does not claim to |
| suite alongside it | `make dev-test`: 58 passed, 1 skipped, 6.4 min, exit 0 (`rc=0` in the build, up and test logs) |

## Every exit class of D13, transcribed

`CERBIX_URL=http://localhost:8080`; a CI token `role: editor, actions: [gate:evaluate, change:record]`,
created for this run and revoked during it (row 18 is that revocation taking effect). The service was
created for this run and deleted after it; zero `cli-repro` services or tokens remain.

```
01 started                     exit=0
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id cli-1788220108 --kind deploy --phase started
  stdout: recorded change=01a05a39-6d30-7678-ba97-9d21b318f2f7 kind=deploy phase=started
  stderr: —
02 succeeded ref               exit=0
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id cli-1788220108 --kind deploy --phase succeeded --ref v4.2.1
  stdout: recorded change=01a05a39-6d3b-7f6e-9b69-c2295e3f4e25 kind=deploy phase=succeeded
  stderr: —
03 identical replay            exit=0
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id cli-1788220108 --kind deploy --phase succeeded --ref v4.2.1
  stdout: replayed change=01a05a39-6d3b-7f6e-9b69-c2295e3f4e25 kind=deploy phase=succeeded
  stderr: —
04 differing ref               exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id cli-1788220108 --kind deploy --phase succeeded --ref v9.9.9
  stdout: —
  stderr: cerbix: change: 409 phase_exists (ref): succeeded is already recorded with a different ref
05 phase after term            exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id cli-1788220108 --kind deploy --phase failed
  stdout: —
  stderr: cerbix: change: 409 phase_order (phase): succeeded already recorded
06 json new ident              exit=0
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id j-cli-1788220108 --kind rollback --phase succeeded --url https://ci.example/run/2 --json
  stdout: {"replayed":false,"change":{"service_id":"d8b1223d-346f-42e9-90a0-bbc9daaa54e3","source":"github-actions","external_id":"j-cli-1788220108","kind":"rollback","id":"01a05a39-6d59-7746-a775-22442ea465e8","phase":"succeeded","occurred_at":"2026-08-31T23:48:28Z","ref":"","url":"https://ci.example/run/2","actor_label":"token:cli-repro-1788220095","actor_user_id":null,"via_token":true,"recorded_at":"2026-08-31T23:48:28.121294Z"}}
  stderr: —
07 decision unknown            exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id d-cli2-1788220122 --kind deploy --phase started --decision not-a-uuid
  stdout: —
  stderr: cerbix: change: 400 decision_unknown (decision_id): "not-a-uuid" is not a decision of this service
08 url http                    exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id u-cli2-1788220122 --kind deploy --phase started --url http://ci.example/run/9
  stdout: —
  stderr: cerbix: change: 400 url_invalid (url): must start with https:// (http:// is refused)
09 source invalid              exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source Deploy_Bot --external-id s-cli2-1788220122 --kind deploy --phase started
  stdout: —
  stderr: cerbix: change: 400 source_invalid (source): must match ^[a-z0-9][a-z0-9-]{0,63}$, got "Deploy_Bot"
10 kind invalid                exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id k-cli2-1788220122 --kind Deploy --phase started
  stdout: —
  stderr: cerbix: change: 400 kind_invalid (kind): must be one of deploy|rollback|flag, got "Deploy"
11 at out of bounds past       exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id p-cli2-1788220122 --kind deploy --phase started --at 2026-08-29T09:00:00Z
  stdout: —
  stderr: cerbix: change: 400 occurred_at_out_of_bounds (occurred_at): must be within 24h0m0s behind and 5m0s ahead of the server's current time 2026-08-31T23:48:42Z, got 2026-08-29T09:00:00Z
12 at out of bounds future     exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id f-cli2-1788220122 --kind deploy --phase started --at 2026-09-02T09:11:00Z
  stdout: —
  stderr: cerbix: change: 400 occurred_at_out_of_bounds (occurred_at): must be within 24h0m0s behind and 5m0s ahead of the server's current time 2026-08-31T23:48:42Z, got 2026-09-02T09:11:00Z
13 explicit empty --at         exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id e-cli2-1788220122 --kind deploy --phase started --at ''
  stdout: —
  stderr: cerbix: change: 400 occurred_at: must be an RFC3339 timestamp
14 unknown service             exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service 0191c2a4-7f3e-4c1b-9a2d-00000000dead --source github-actions --external-id n-cli2-1788220122 --kind deploy --phase started
  stdout: —
  stderr: cerbix: change: 404 not found
15 missing --phase             exit=2
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id m-cli2-1788220122 --kind deploy
  stdout: —
  stderr: change record: --phase is required
usage: cerbix change record --project <id> --service <id> --kind deploy|rollback|flag --phase started|succeeded|failed|cancelled --source <slug> --external-id <id> [--ref <label>] [--url <https url>] [--decision <id>] [--at <RFC3339>] [--json] [--timeout 10s]
16 bogus token                 exit=1
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id b-cli3-1788220136 --kind deploy --phase started
  stdout: —
  stderr: cerbix: change: 401 unauthorized
17 no CERBIX_TOKEN             exit=1
  argv:   /tmp/cerbix-cli change record --project e15f2fa2-dc30-4321-b531-dc8ebe356e99 --service d8b1223d-346f-42e9-90a0-bbc9daaa54e3 --source github-actions --external-id x-cli3-1788220136 --kind deploy --phase started
  stdout: —
  stderr: cerbix: change: CERBIX_TOKEN is not set (the API token that authenticates to the server; environment only, never a flag)
18 revoked token               exit=1
  argv:   cerbix change record … (token revoked a moment earlier)
  stdout:
  stderr: cerbix: change: 401 unauthorized
```

## What the rows establish

* **exit 0** — 01, 02, 06 record; 03 is the identical replay returning the SAME change id as 02, so a
  repeated body writes nothing and says so.
* **exit 2** — the server's own refusal, printed verbatim, stdout empty: a conflicting ref (409
  `phase_exists`), a phase after a terminal one (409 `phase_order`), and the 400s for `decision_id`,
  `url`, `source`, `kind`, both ends of the `occurred_at` window, an explicitly empty `--at`, an unknown
  service (404) and a missing required flag with its usage line.
* **exit 1** — credentials and transport, never a decision: a bogus token, no `CERBIX_TOKEN` at all, and a
  token revoked while the CLI still holds it.

Row 13 is review [52]'s fix seen end to end: `--at ""` travels verbatim and the SERVER refuses it, rather
than the CLI silently substituting the invocation instant.

Row 07 is worth a second look. Its refusal now comes from the API handler, before the request is admitted
against the §5a bounds (review [8]), where it used to come from the store afterwards. The wire answer is
byte-identical — same status, same code, same field, same sentence — which is what that fix claimed and
what this transcript demonstrates against a running server rather than a fake.

## Reproducing

```
make dev-build && make dev-up          # dev-up runs dev-build again; the second image id is the live one
make dev-test                          # 58 passed, 1 skipped
go build -o /tmp/cerbix-cli ./cmd/cerbix
CERBIX_URL=http://localhost:8080 CERBIX_TOKEN=<a CI token> /tmp/cerbix-cli change record …
```
