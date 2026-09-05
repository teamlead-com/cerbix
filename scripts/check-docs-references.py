#!/usr/bin/env python3
"""Fail when a LIVING document cites a file, symbol or test that the tree does not have.

The class of drift this catches is real and was found by hand once: `docs/status.md` cited
`TestPartitionAdoptionFencesConcurrentInserts` and `TestComponentMutationsTakeThePageFirst`, and
neither name has ever existed in the repository — the properties were proven by differently-named
tests, so the evidence was real and unrunnable as written. Renames produce the same effect more
often: a test is renamed with its behaviour intact and the row that cites it silently stops
resolving.

WHAT IS CHECKED — only documents that are edited in place (AGENTS "Documentation Layout"):
  docs/status.md, docs/traceability.md, docs/specs/*.md, docs/overview.md, docs/runbook.md,
  docs/project-description.md, README.md, CLAUDE.md
Iteration reports (`docs/iterations/*.md`) and review snapshots (`docs/checks/*`) are IMMUTABLE by
contract: they record what was true when they were written, and a rename afterwards does not make
them wrong. They are deliberately not checked for references.

`docs/decisions.md` is a MIXED case and used to be described here as simply historical, which was
wrong twice over: AGENTS lists it among the documents edited in place, and FR-025 §10 promises that
a retired spelling is refused in every LIVING document. Its reference checking stays off — a D-record
naming a path that has since moved is still an accurate record of that decision — but the FR-025
stale-spelling guard DOES scan it, because a decision introducing `change_events` or `caused_by`
would be a live claim about the product's vocabulary, not a historical one (review [49]).

WHAT IS CHECKED IN THEM:
  * backticked repo paths (`internal/...go`, `frontend/src/...vue`, `migrations/000NN_*.sql`, ...)
  * backticked `Test*` identifiers, which must exist as a Go `func Test...` or a TS/Vue test name

Brace shorthand is expanded: `store/{users,sessions}.go` checks both files.
Anything intentionally unresolvable lives in ALLOWED below, each with a reason.
"""
import glob
import os, re, sys, glob, itertools
from pathlib import Path


def read(path, **kw):
    """Read a document, closing the handle.

    Every call site used a bare `open(...).read()`, which leaks the descriptor until the garbage
    collector notices and makes the suite print a ResourceWarning per scanned document (review
    [54]). One reader, so no call site can forget again.
    """
    return Path(path).read_text(encoding='utf-8', **kw)


# CHANGELOG.md is a LIVING document for this gate's purpose even though it is a historical
# record: its RELEASE NOTES cite code paths a reader is expected to open, and it carried a
# `backend/internal/...` path — a tree layout this repository has never had — for weeks precisely
# because nothing checked it (found while cutting v0.1.5-beta.1).
# `docs/roadmap.md` joined on 2026-09-03 (owner). It was outside the guard while being exactly the
# kind of document the guard is for: it cites code paths and specs a reader is expected to open, it
# is edited in place rather than superseded, and it had already carried three claims that had
# stopped being true. Found while mutation-testing the checker — an injected broken citation there
# was not caught, because the file was never read.
LIVING = ['docs/status.md', 'docs/traceability.md', 'docs/overview.md', 'docs/runbook.md',
          'docs/project-description.md', 'docs/roadmap.md', 'README.md', 'CLAUDE.md',
          'CHANGELOG.md',
          # The other READMEs joined on 2026-09-04 (owner: "bring the READMEs 1:1 with the code").
          # Nothing had ever read them, and it showed: `frontend/README.md` described a "planned"
          # stack with TanStack Query and uPlot/ECharts, neither of which was ever adopted, and
          # called the SPA unscaffolded years into its life.
          'frontend/README.md', 'docs/specs/README.md', 'docs/checks/README.md',
          'docker/monitoring.d/README.md'] + sorted(glob.glob('docs/specs/*.md'))

ALLOWED = {
    'ActionServiceRead':  'a name the spec says must NOT exist (§4 "Service is not a security boundary")',
    'ActionServiceWrite': 'a name the spec says must NOT exist (§4 "Service is not a security boundary")',
    'docs/alerts.yaml':   'named by AGENTS as a long-lived artifact; not created yet — alert rules live in deploy/alerts/',
    'config.yaml':        'generic filename in prose, not a repo path',
    'docs/iterations/iter-NNNN.md': 'a template name, not a file',
    'docs/iterations/iter-XXXX.md': 'a template name, not a file',
    '000NN_project_secrets.sql':    'a placeholder in a spec written before the number was assigned',
    'schema.sql': 'generic filename in prose',
    '2026-08-01-implementation-review-v1.md': 'an illustration of the docs/checks naming rule, not a file',
    '2026-08-01-security-review-v1.md':       'an illustration of the docs/checks naming rule, not a file',
}

PATH_RE = re.compile(r'`([A-Za-z0-9_./{},\-]+\.(?:go|ts|vue|sql|ya?ml|md|json|sh|html))`')
# Markdown LINK TARGETS too, not only backticked paths: the broken `backend/internal/...`
# reference in CHANGELOG.md lived inside a link and was invisible to the backtick form for
# weeks. Absolute urls and pure anchors are excluded — this is about repo-relative files.
LINK_RE = re.compile(r'\]\((?!https?:|#|mailto:)([A-Za-z0-9_./{},\-]+\.(?:go|ts|vue|sql|ya?ml|md|json|sh|html))(?:#[^)]*)?\)')
TEST_RE = re.compile(r'`(Test[A-Za-z0-9_]{4,})`')
BRACE_RE = re.compile(r'\{([^{}]*)\}')

def expand(tok):
    m = BRACE_RE.search(tok)
    if not m:
        return [tok]
    return list(itertools.chain.from_iterable(
        expand(tok[:m.start()] + part + tok[m.end():]) for part in m.group(1).split(',')))

GONE_RE = re.compile(r'\b(deleted|removed|retired|renamed|rewritten|became|replaced|superseded|never existed|dropped)\b', re.I)

def excused(line, end):
    """A citation is fine when the sentence around it says the artifact is gone.

    `docs/traceability.md` says "`MembersView.vue` deleted" and that is exactly right: the row
    documents the removal, so the name must appear and must not resolve. The window is deliberately
    narrow (60 chars before, 160 after) so an unrelated "removed" elsewhere on a long table row does
    not excuse an unrelated broken citation.
    """
    return GONE_RE.search(line[max(0, end - 60):end + 160]) is not None

# Directories that hold no citable source and are enormous: walking them for every citation is
# what made this checker take 79 seconds on a developer's machine (measured 2026-09-03, with
# `resolves` alone accounting for 47 s of it).
PRUNED_DIRS = {'.git', 'node_modules', 'dist', 'test-results', '__pycache__', '.venv',
               'playwright-report', '.pytest_cache'}

_TREE = None

def tree_index():
    """Every citable path in the repo, indexed ONCE.

    The old implementation ran `glob.glob('**/' + tok, recursive=True)` per citation — a full
    recursive walk of the tree for each of ~1800 lookups. Same answers, one walk: a set of
    repo-relative paths, plus a suffix map so a citation like `store/heartbeats.go` still
    resolves the way a `**/` glob resolved it.
    """
    global _TREE
    if _TREE is not None:
        return _TREE
    paths, suffixes = set(), {}
    for root, dirs, files in os.walk('.'):
        dirs[:] = [d for d in dirs if d not in PRUNED_DIRS]
        for f in files:
            rel = os.path.relpath(os.path.join(root, f), '.').replace(os.sep, '/')
            paths.add(rel)
            suffixes.setdefault(f, []).append(rel)
    _TREE = (paths, suffixes)
    return _TREE

def resolves(tok):
    if os.path.exists(tok):
        return True
    # Docs cite paths RELATIVELY to their own directory (`../internal/store/monitors.go` from
    # `docs/`), and by suffix (`store/heartbeats.go`, `migrations/00039_x.sql`). The `**/` glob
    # this replaced happened to resolve both, because expanding `**/` past a `..` lands back at
    # the root; the index has to strip the climb explicitly or every relative citation in
    # `status.md` reads as broken — which is exactly what the first version of this function did.
    cand = tok
    while cand.startswith('../') or cand.startswith('./'):
        cand = cand.split('/', 1)[1]
    if os.path.exists(cand):
        return True
    paths, suffixes = tree_index()
    if cand in paths:
        return True
    base = cand.rsplit('/', 1)[-1]
    needle = '/' + cand
    return any(p.endswith(needle) for p in suffixes.get(base, ()))

_TEST_TOKENS = None

def test_tokens(src):
    r"""Every `Test…` identifier that appears anywhere in the source, collected ONCE.

    The old check ran `re.search(r'\bfunc\s+' + name + r'\b', src)` per cited name over the
    whole concatenated source — 60 of the checker's 79 seconds. A cited name is matched by
    TEST_RE, so it is always a `Test[A-Za-z0-9_]{4,}` token, and membership in the token set
    answers both halves of the old condition (declared as a func, or mentioned anywhere).

    It is also STRICTER in one way, deliberately: the old `name in src` was a substring test, so
    a doc citing `TestFoo` passed on the strength of an unrelated `TestFooBar` in the tree. The
    token set does not do that, and a citation that only ever passed that way is a stale citation
    this checker exists to catch.
    """
    global _TEST_TOKENS
    if _TEST_TOKENS is None:
        _TEST_TOKENS = set(re.findall(r'\bTest[A-Za-z0-9_]{3,}', src))
    return _TEST_TOKENS

def source_text():
    parts = []
    for pat in ('internal/**/*.go', 'cmd/**/*.go', 'frontend/src/**/*.ts', 'frontend/src/**/*.vue',
                'e2e/**/*.ts', 'internal/store/migrations/*.sql'):
        for f in glob.glob(pat, recursive=True):
            parts.append(read(f, errors='ignore'))
    return '\n'.join(parts)

DISCHARGE_DOC = 'docs/traceability.md'
INV_HEADING = '### Invariants (§19 for 1–74, §16.8 for 75–91)'
MATRIX_HEADING = '### Required test matrix (§16.10, written before the phase-5 code)'
FR022_HEADING = '### FR-022 invariants (§6 of func-service-incidents.md)'
FR022_MATRIX_HEADING = '### FR-022 required test matrix (§7, written before the code)'
FR023_HEADING = '### FR-023 invariants (§6 of func-service-escalation.md)'
FR023_MATRIX_HEADING = '### FR-023 required test matrix (§7, written before the code)'
# FR-025 (func-change-intelligence.md): §6 is compared as a SET like FR-021's sections, because the spec
# says so ("Twenty-three, compared as a SET against the traceability map by `make docs-check`"); §7 has
# nine scenario GROUPS, one row each.
FR025_SPEC = 'docs/specs/func-change-intelligence.md'
# Reference-checked: no. Vocabulary-guarded: yes. See the module docstring (review [49]).
DECISIONS_DOC = 'docs/decisions.md'
FR025_HEADING = '### FR-025 invariants (§6 of func-change-intelligence.md)'
FR025_MATRIX_HEADING = '### FR-025 required test matrix (§7, written before the code)'
# FR-026 (func-incident-audit.md): §6 as a SET, same discipline. It has no §7 table of its own in the
# map — the matrix is discharged inside the invariant rows, which is what the spec's §7 groups by.
FR026_SPEC = 'docs/specs/func-incident-audit.md'
FR026_HEADING = '### FR-026 invariants (§6 of func-incident-audit.md)'
# FR-029 (func-async-canary.md): §6 as a SET. Two rows are PARTIAL and say what is missing; the gate
# checks that every invariant HAS a row, which is what stops a carried-forward phase from evaporating.
FR029_SPEC = 'docs/specs/func-async-canary.md'
FR029_HEADING = '### FR-029 invariants (§6 of func-async-canary.md)'


def discharge_rows(text, heading):
    """Rows of the numbered table that follows `heading`, as ({number: cell}, [duplicate numbers]).

    The duplicates are returned rather than dropped. Writing straight into a dict made a second row
    for the same number OVERWRITE the first and vanish — so a table could carry two rows for
    invariant 1, be short one invariant, and still satisfy a count and a required-key check (review
    [49] of the close-out party, reproduced in memory: 23 rows expected, 23 found, no error)."""
    i = text.find(heading)
    if i < 0:
        return None, []
    out, dups = {}, []
    for line in text[i:].split('\n')[1:]:
        if line.startswith('### ') or line.startswith('## '):
            break
        cells = [c.strip() for c in line.split('|')]
        if len(cells) < 5 or not cells[1].isdigit():
            continue
        n = int(cells[1])
        if n in out:
            dups.append(n)
            continue
        out[n] = cells[3]
    return out, dups


ROW_STATUS_DOCS = ['docs/status.md']
STATUSES = {'TODO', 'IN_PROGRESS', 'DONE'}


def split_row(line):
    """Split a markdown table row on UNESCAPED pipes.

    A `\\|` inside a cell is a literal pipe (a shell pipeline in an evidence cell, a route list in a
    requirement), and splitting on it would report a false break. Splitting on the raw character
    instead is how this checker's first version accused four correct rows and missed the one that was
    actually broken.
    """
    return re.split(r'(?<!\\)\|', line)


def check_row_statuses():
    """Every requirement row states one of AGENTS' three statuses and nothing else.

    Two ways a row goes wrong, both seen in this repository: a status cell written as
    `IN_PROGRESS (UI pending a mock)` — a status plus a parenthetical, which is prose in a field the
    process defines as an enum — and an unescaped pipe earlier in the row, which silently shifts every
    later cell so the status column holds a fragment of the requirement text.
    """
    bad = []
    for doc in ROW_STATUS_DOCS:
        if not os.path.exists(doc):
            continue
        for n, line in enumerate(read(doc).splitlines(), 1):
            if not re.match(r'\| (AC|DoD|FR|NFR)-', line):
                continue
            cells = split_row(line.rstrip('\n'))
            if len(cells) < 5:
                bad.append((doc, n, 'row', f'{len(cells) - 2} cells, want at least 3'))
                continue
            status = cells[3].strip()
            if status not in STATUSES:
                bad.append((doc, n, 'status', f'{status!r} is not one of TODO/IN_PROGRESS/DONE'))
    return bad



# A spec's banner claims the feature is not built yet; status.md says a requirement of that spec is
# DONE. Both cannot be true, and the failure mode is one-directional: a banner is written once, at the
# moment the spec is authored, and nothing in the process ever brings the author back to it. FR-022 and
# FR-023 carried "Nothing is implementable until this file has been reviewed" for weeks after shipping,
# and an operator reading the spec to learn what cerbix does was told the feature did not exist.
UNBUILT_CLAIM_RE = re.compile(
    r'nothing (?:is|here is) implementable|awaiting (?:adversarial )?review|awaiting a UI mock',
    re.IGNORECASE)
REQ_RE = re.compile(r'\b((?:FR|NFR)-\d{3,})\b')
# The banner lives at the top; prose further down may legitimately quote the old gate as history.
BANNER_LINES = 30


# Spellings that FR-024's earlier revisions used for contracts that have since changed. A normative
# sentence carrying one of them offers an implementer the wrong contract, and five review rounds each
# found one. Scope (revision 6): the gate spec ENTIRELY, including the normative schema fence — only a
# fence opened with the info-string `retired-spellings` is a quotation and is skipped; the FR-024 and
# NFR-019 rows of docs/status.md; and docs/decisions.md within any `## D-` section whose heading names
# FR-024. A line is exempt only as a blockquote or when it uses the phrases a supersession note uses to
# quote what it supersedes.
GATE_STALE = [
    (re.compile(r'max_seal_lag\b(?!_seconds)'), 'max_seal_lag without _seconds'),
    (re.compile(r'1m\.\.24h'), '1m..24h (the floor is derived, 300..86400 s)'),
    (re.compile(r'minimum of (the )?applicable leases'), 'lease-only facts_fresh_until'),
    (re.compile(r'first statement supplies'), '"first statement" (it is the first SNAPSHOT-BEARING statement)'),
    # revision 6 (party [33] P1-4): the row-DELETE purge and its metrics, the month-wide partitions,
    # and the unbounded revision list were all replaced in revision 7.
    (re.compile(r'decision_purge_batch|purge_backlog_rows|oldest_eligible_seconds'), 'revision-6 purge vocabulary'),
    (re.compile(r'partition per calendar month|monthly RANGE'), 'revision-6 partition period (it is one UTC day)'),
    (re.compile(r'fact_revision_ids'), 'fact_revision_ids (it is the bounded fact_revisions object)'),
    # revision 7 (party [35]): the unpruned id, the wrong partition count, the false recovery claim,
    # creation under ACCESS EXCLUSIVE, and a lifetime "bound" that ignored cadence and backlog.
    (re.compile(r'carries no time|\b373\b|full list is recoverable'), 'revision-7 identity/evidence claim'),
    (re.compile(r'IF NOT EXISTS[^\n]{0,60}PARTITION OF'), 'CREATE … IF NOT EXISTS … PARTITION OF (partitions are built standalone and ATTACHed)'),
    (re.compile(r'retention \+ 1 day`'), 'retention + 1 day as a bound (revision 7)'),
    # revision 8 (party [37]): one-cadence-short lifetime, the impossible-duplicate 500, the clockless
    # prefilter, unbudgeted creation, and maintenance errors counted in the evaluation family.
    (re.compile(r'retention \+ 1 day \+ (decision_)?purge_every'), 'revision-8 lifetime formula (it is two boundaries, see D10)'),
    (re.compile(r'ledger_identity|without touching the database|\bunbudgeted\b'), 'revision-8 read/creation contract'),
    (re.compile(r'evaluate_errors_total\{kind="partition_identity"\}'), 'partition_identity belongs to cerbix_gate_maintenance_errors_total'),
    # revision 9 (party [39]): index inventory, strict drop predicate, list item shape, count-only
    # creation bound, and a threat row that rate-bounded reads.
    (re.compile(r'the two indexes|(?<!UNIQUE )INDEX \(id\)'), 'revision-9 index inventory (four per partition, no parent (id) index)'),
    (re.compile(r'older than one `decision_purge_every`'), 'revision-9 strict drop predicate (it is <= one cadence, inclusive)'),
    (re.compile(r'plus `state`, `action`'), 'revision-9 list item shape (state is always present; action absent for NOT_CONFIGURED)'),
    (re.compile(r'2 × 2 s × create_max'), 'revision-9 count-only creation bound (creation is time-reserved at 12 s)'),
    (re.compile(r'within the rate and concurrency bounds'), 'revision-9 threat row (ledger reads take no rate token)'),
    # revision 10 (party [41]): the row-skipping cursor, the client-chosen override action, the unstable
    # release oracle, the one-scan EXPLAIN claim, the between-operations time check, and "history" for
    # the active-only read.
    (re.compile(r"the extra row's existence|from the `LIMIT \+ 1` row"), 'revision-10 cursor (encode from the last RETURNED row)'),
    (re.compile(r'policy_revision, action, reason'), 'revision-10 override body (no client action)'),
    (re.compile(r'pool holds one connection fewer'), 'revision-10 release oracle (pid never re-borrowed + successor acquires)'),
    (re.compile(r'ONE index-range scan'), 'revision-10 EXPLAIN claim (Append with matching child indexes)'),
    (re.compile(r'no later than t = 12 s with budget'), 'revision-10 between-operations check (clamped per statement)'),
    (re.compile(r'`GET …/override` history'), 'revision-10 called the active-only read history'),
    # revision 11 (party [43]): additive timer admission, a stale route count, revoker fields "null
    # until revoked", a false row bound on override history, and a 30 s + 3 s timeline.
    (re.compile(r'lock_timeout \+ statement_timeout'), 'revision-11 additive admission (the clamp is a wall bound)'),
    (re.compile(r'six policy/override routes'), 'revision-11 route count (there are eight)'),
    (re.compile(r'null until revoked'), 'revision-11 revoker fields (system closures set revoked_at; the human triple is manual-only)'),
    (re.compile(r'seven-day regime keeps that small'), 'revision-11 false row bound on override history'),
    (re.compile(r'[Ee]ach pass (is )?bounded to `subCadenceTimeout`'), 'revision-11 timeline (work ≤ 27 s + cleanup ≤ 3 s in one 30 s lifecycle)'),
    # revision 12 (party [45]): attribution wording wrong for token revokers, a status that moved with
    # housekeeping, and revision 9's snapshot claim for the listing.
    (re.compile(r'non-null for `manual`|has a human revoker'), 'revision-12 attribution wording (present for manual; user id nullable for tokens)'),
    (re.compile(r'same status before and after|cannot duplicate or skip an item'), 'revision-12 status claim / revision-9 snapshot claim'),
    (re.compile(r'a new row has a later `evaluated_at`'), 'revision-9 false pagination proof (evaluated_at is not commit time)'),
    # post-approval (party [49]): the design was confirmed in [47]; living text must not say a
    # confirmation is still pending or that the acceptance map is a draft.
    (re.compile(r'Two gates remain before code|focused confirmation of THIS revision|the review\'s focused confirmation and an approved UI mock|draft, numbered on acceptance'), 'pre-approval lifecycle wording (design approved at revision 13, D-0201)'),
]
GATE_SPEC = 'docs/specs/func-reliability-gate.md'
GATE_FIXTURE_FENCE = '```retired-spellings'
GATE_QUOTING = re.compile(r'at the time|renamed in revision', re.I)
GATE_STATUS_ROWS = re.compile(r'^\| (FR-024|NFR-019) \|')
GATE_DECISION_HEADING = re.compile(r'^## D-\d+ .*FR-024')


def gate_stale_findings(path, lines):
    """The retired-spelling findings for one document, as (path, line, kind, message). Pure, so the
    fixture tests can drive it without files."""
    bad = []
    in_fixture = False
    in_fence = False
    in_gate_section = False
    for n, line in enumerate(lines, 1):
        if line.startswith('```'):
            if in_fence or in_fixture:
                in_fence = in_fixture = False
            elif line.strip() == GATE_FIXTURE_FENCE:
                in_fixture = True
            else:
                in_fence = True
            continue
        if in_fixture:
            continue
        if path.endswith('decisions.md'):
            if line.startswith('## '):
                in_gate_section = bool(GATE_DECISION_HEADING.match(line))
            if not in_gate_section:
                continue
        elif path.endswith('status.md'):
            if not GATE_STATUS_ROWS.match(line):
                continue
        if line.startswith('>') or GATE_QUOTING.search(line):
            continue
        for rx, label in GATE_STALE:
            if rx.search(line):
                bad.append((path, n, 'stale', f'retired FR-024 spelling: {label}'))
    return bad


def gate_duplicate_headers(text):
    heads = re.findall(r'^(service_gate_\w+)\s+\(', text, re.M)
    return [h for h in sorted(set(heads)) if heads.count(h) > 1]


def check_gate_stale_spellings():
    bad = []
    for path in (GATE_SPEC, 'docs/status.md', 'docs/decisions.md'):
        try:
            lines = read(path).split('\n')
        except FileNotFoundError:
            continue
        bad += gate_stale_findings(path, lines)
    try:
        text = read(GATE_SPEC)
    except FileNotFoundError:
        return bad
    for h in gate_duplicate_headers(text):
        bad.append((GATE_SPEC, 0, 'stale', f'schema table {h} declared more than once'))
    return bad


# Spellings FR-025's design retired (func-change-intelligence.md §10). Refused in the spec itself —
# outside §10, which is where the list is stated — and in every living document; a blockquote or a
# sentence that says "retired spelling" is quoting, not prescribing. `scopes` alone is not on the list:
# it is the OIDC word everywhere else in the tree.
CHANGE_STALE = [
    (re.compile(r'deployment_events|change_events'), 'the table is service_changes'),
    (re.compile(r'caused_by|root_cause_change'), 'the field is preceded_by and the note says "preceded"'),
    (re.compile(r'change:read'), 'reads are project:read'),
    (re.compile(r'token_scopes'), 'the token list is actions'),
]
CHANGE_GUARD_SECTION = re.compile(r'^## 10\.')


# Every document the FR-025 vocabulary guard reads. `docs/decisions.md` is here and NOT in LIVING:
# it is exempt from reference checking (a D-record may name a path that has since moved) but not
# from the vocabulary guard, which §10 promises for every living document and which AGENTS' own
# classification of decisions.md as edited-in-place demands (review [49]). A module-level list so a
# test can assert what is scanned instead of trusting a comment.
def change_guard_docs():
    return [FR025_SPEC] + [d for d in LIVING if d != FR025_SPEC] + [DECISIONS_DOC]


def check_change_stale_spellings(paths=None):
    bad = []
    for path in paths if paths is not None else change_guard_docs():
        if not os.path.exists(path):
            continue
        in_guard = False
        for n, line in enumerate(read(path).split('\n'), 1):
            if path == FR025_SPEC and line.startswith('## '):
                in_guard = bool(CHANGE_GUARD_SECTION.match(line))
            if in_guard or line.startswith('>') or 'retired spelling' in line:
                continue
            for rx, label in CHANGE_STALE:
                if rx.search(line):
                    bad.append((path, n, 'stale', f'retired FR-025 spelling: {label}'))
    return bad


# A real customer/project name leaked into the FR-029 design documents and into four test files as
# example data — the owner found it in the phase-F mock on 2026-09-03. It is scrubbed, and this keeps
# it scrubbed: example data names nobody. The guard reads the whole tree rather than only the living
# documents, because the leak reached `_test.go` fixtures too, and a checker that watched the docs
# alone would have missed most of it.
CUSTOMER_NAMES = [re.compile(r'charla', re.I)]
NAME_GUARD_DIRS = ('docs', 'internal', 'e2e', 'cmd', 'scripts')
NAME_GUARD_SKIP = ('node_modules', 'dist', '.git')


def check_customer_names():
    """No real customer or project name anywhere in the tree. Example data is a placeholder."""
    bad = []
    for root_dir in NAME_GUARD_DIRS:
        for dirpath, dirnames, filenames in os.walk(root_dir):
            dirnames[:] = [d for d in dirnames if d not in NAME_GUARD_SKIP]
            for fn in filenames:
                if not fn.endswith(('.md', '.go', '.html', '.yaml', '.yml', '.ts', '.vue', '.py')):
                    continue
                path = os.path.join(dirpath, fn)
                if os.path.abspath(path) == os.path.abspath(__file__):
                    continue  # the guard states the name it forbids
                try:
                    lines = read(path).split('\n')
                except (FileNotFoundError, UnicodeDecodeError):
                    continue
                for n, line in enumerate(lines, 1):
                    for rx in CUSTOMER_NAMES:
                        if rx.search(line):
                            bad.append((path, n, 'name',
                                        'a real customer/project name in example data — use a placeholder'))
    return bad


def check_spec_banners():
    """No spec says it is unbuilt while status.md marks one of its requirements DONE."""
    status_path = 'docs/status.md'
    if not os.path.exists(status_path):
        return []
    # The REQUIREMENT's own row is the authority, not the acceptance rows that cite it: an AC row
    # names its requirement only when its prose happens to, so reading those would make the gate's
    # coverage depend on wording.
    done = set()
    for line in read(status_path).splitlines():
        cells = split_row(line.rstrip('\n'))
        if len(cells) < 5:
            continue
        req = cells[1].strip()
        if REQ_RE.fullmatch(req) and cells[3].strip() == 'DONE':
            done.add(req)

    bad = []
    for spec in sorted(glob.glob('docs/specs/*.md')):
        lines = read(spec).splitlines()[:BANNER_LINES]
        head = '\n'.join(lines)
        # The requirements a spec is ABOUT are named in its title line.
        owned = set(REQ_RE.findall(lines[0])) if lines else set()
        shipped = sorted(owned & done)
        if not shipped:
            continue
        # A banner that DECLARES its delivery is allowed to quote the claim it replaced — the
        # correction reads better with the old words in it, and the status line above them is the
        # authority. Only an undeclared spec is measured by its prose.
        if re.search(r'STATUS:\s*\**\s*(DELIVERED|SUPERSEDED|SHIPPED)', head, re.IGNORECASE):
            continue
        for n, line in enumerate(lines, 1):
            if UNBUILT_CLAIM_RE.search(line):
                bad.append((spec, n, 'stale-status',
                            f'says unbuilt while {", ".join(shipped)} is DONE in status.md'))
                break
    return bad


# The two sections of the FR-021 spec that STATE invariants. Named here so a heading rename is a
# LOUD failure rather than a silently empty set — an earlier version derived a count and a renamed
# heading would simply have produced zero, accepting anything.
FR021_INV_SECTIONS = ('## 19.', '### 16.8')


def fr021_invariant_numbers():
    """The SET of invariant numbers the FR-021 spec states.

    A set, not a maximum. `max()` checked 1..max and never noticed a discharge row above it, nor a
    hole below it: adding traceability row 104 with no spec invariant, and deleting spec invariant
    102 while keeping 103, both passed. The discharge map has to match this set EXACTLY — a missing
    key is an unchecked requirement and an extra one is a requirement nobody made."""
    text = read('docs/specs/func-service-reliability.md')
    # Collected as a LIST first. Folding straight into a set hid a duplicate: two `103.` entries with
    # different text passed, and the discharge map could only ever match one of them, so half a
    # requirement was silently unchecked.
    seen = []
    for start in FR021_INV_SECTIONS:
        i = text.find(start)
        if i < 0:
            raise SystemExit(f'check-docs-references: FR-021 invariant section {start!r} is gone; '
                             'the invariant gate has nothing to compare the discharge map against')
        section = text[i:]
        end = re.search(r'\n#{2,3} (?!19\.|16\.8)', section)
        if end:
            section = section[:end.start()]
        seen.extend(int(m) for m in re.findall(r'^\s{0,4}(\d{1,3})\.\s', section, re.M))
    if not seen:
        raise SystemExit('check-docs-references: the FR-021 spec states no invariants at all')
    dupes = sorted({n for n in seen if seen.count(n) > 1})
    if dupes:
        raise SystemExit('check-docs-references: the FR-021 spec states invariant number(s) '
                         f'{dupes} more than once — the discharge map can only match one of them, '
                         'so the other is a requirement nothing checks')
    return set(seen)


def spec_invariant_numbers(spec, label):
    """The SET of invariant numbers §6 of `spec` states — FR-021's discipline: a set, not a
    maximum, a renamed section a loud failure, a duplicate number a loud failure."""
    text = read(spec)
    i = text.find('\n## 6.')
    if i < 0:
        raise SystemExit(f'check-docs-references: {spec} has no "## 6." section; the {label} '
                         'invariant gate has nothing to compare the discharge map against')
    section = text[i + 1:]
    end = re.search(r'\n## ', section)
    if end:
        section = section[:end.start()]
    seen = [int(m) for m in re.findall(r'^\s{0,4}(\d{1,3})\.\s', section, re.M)]
    if not seen:
        raise SystemExit(f'check-docs-references: the {label} spec states no invariants at all')
    dupes = sorted({n for n in seen if seen.count(n) > 1})
    if dupes:
        raise SystemExit(f'check-docs-references: the {label} spec states invariant number(s) '
                         f'{dupes} more than once')
    return set(seen)


def fr025_invariant_numbers():
    return spec_invariant_numbers(FR025_SPEC, 'FR-025')


def fr026_invariant_numbers():
    return spec_invariant_numbers(FR026_SPEC, 'FR-026')


def fr029_invariant_numbers():
    return spec_invariant_numbers(FR029_SPEC, 'FR-029')


def check_invariant_set(src, text, expected, heading=INV_HEADING, label='FR-021'):
    """The FR-021 invariant table's keys must EQUAL the spec's numbers — both directions.

    Contiguity is checked too, because these are written as a numbered list and a hole in it is a
    typo rather than a decision. Say which numbers, not merely that the counts differ: the point of
    the map is that a reader can follow it."""
    bad = []
    rows, dups = discharge_rows(text, heading)
    if rows is None:
        return [(DISCHARGE_DOC, 0, 'discharge', f'the {label} invariant table is missing entirely')]
    for n in sorted(set(dups)):
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'{label} invariant {n} has MORE THAN ONE discharge row — the second used to '
                    f'overwrite the first, so the table could be one invariant short and still count right'))
    holes = sorted(set(range(1, max(expected) + 1)) - expected)
    if holes:
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'the {label} spec skips invariant number(s) {holes} — a numbered list with a '
                    f'hole is a typo, and the gate cannot tell it from a deletion'))
    for n in sorted(expected - set(rows)):
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'{label} invariant {n} is stated in the spec and has no discharge row'))
    for n in sorted(set(rows) - expected):
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'discharge row {n} names an invariant the {label} spec does not state'))
    for n in sorted(expected & set(rows)):
        bad += discharge_row_evidence(src, rows[n], n, f'{label} invariant')
    return bad


def discharge_row_evidence(src, cell, n, label):
    """A row must name a test that EXISTS or an INSPECTION: reason. Shared, so the set-compared
    invariant table and the contiguous tables hold each other to the same standard."""
    bad = []
    names = re.findall(r'`(Test[A-Za-z0-9_]+)`', cell)
    if names:
        for name in names:
            if name not in test_tokens(src):
                bad.append((DISCHARGE_DOC, 0, 'discharge', f'{label} {n} cites missing {name}'))
    elif 'INSPECTION:' not in cell and 'spec.ts' not in cell:
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'{label} {n} names neither a test nor an INSPECTION: reason'))
    return bad


def check_discharge(src):
    """FR-021's invariants are compared as a SET against the spec (see `check_invariant_set`); its 24
    required scenarios and FR-022/FR-023's tables are contiguous 1..N. Every entry must have a row,
    and every row must name a test that exists or an INSPECTION: reason — that is what makes "done"
    a checkable claim instead of a memory of thirty iteration reports."""
    bad = []
    text = read(DISCHARGE_DOC)
    # The FR-021 invariant count is READ from the spec, not written here. Hard-coding 91 meant a
    # discharge row above that number was neither required nor checked, so twelve invariants existed
    # only in the map — a claim nobody had made in the requirement it was being checked against.
    fr021 = fr021_invariant_numbers()
    # The FR-021 invariants are compared as a SET; the other tables are still contiguous 1..N.
    bad += check_invariant_set(src, text, fr021)
    bad += check_invariant_set(src, text, fr025_invariant_numbers(), FR025_HEADING, 'FR-025')
    bad += check_invariant_set(src, text, fr026_invariant_numbers(), FR026_HEADING, 'FR-026')
    bad += check_invariant_set(src, text, fr029_invariant_numbers(), FR029_HEADING, 'FR-029')
    for heading, count, label in ((MATRIX_HEADING, 24, 'scenario'),
                                  (FR022_HEADING, 16, 'FR-022 invariant'),
                                  (FR022_MATRIX_HEADING, 16, 'FR-022 scenario'),
                                  (FR023_HEADING, 16, 'FR-023 invariant'),
                                  (FR023_MATRIX_HEADING, 19, 'FR-023 scenario'),
                                  (FR025_MATRIX_HEADING, 9, 'FR-025 scenario')):
        rows, dups = discharge_rows(text, heading)
        if rows is None:
            bad.append((DISCHARGE_DOC, 0, 'discharge', f'the {label} table is missing entirely'))
            continue
        for n in sorted(set(dups)):
            bad.append((DISCHARGE_DOC, 0, 'discharge',
                        f'{label} {n} has MORE THAN ONE row — the second silently replaced the first'))
        # EXACT key equality, not merely "every required number is present". A required-key loop let
        # a tenth row sit in a nine-scenario matrix unnoticed, which is a scenario nobody agreed to
        # measured as if they had (review [50]).
        for n in sorted(set(rows) - set(range(1, count + 1))):
            bad.append((DISCHARGE_DOC, 0, 'discharge',
                        f'{label} table has a row numbered {n}, but the matrix has {count} entries'))
        for n in range(1, count + 1):
            cell = rows.get(n)
            if cell is None:
                bad.append((DISCHARGE_DOC, 0, 'discharge', f'{label} {n} has no row'))
                continue
            bad += discharge_row_evidence(src, cell, n, label)
    return bad


# --- enumerations the TREE can settle --------------------------------------------------------
#
# Every guard below exists because the enumeration it checks had ALREADY rotted when it was
# written (2026-09-04, while reconciling the READMEs with the code): `README.md` said 16 check
# types against 17 constants; `README.md` and `internal/store/migrate.go` both said FIVE
# migrations use the column-list `ON DELETE SET NULL` form while six do, because 00093 was added
# without touching either sentence; `docs/specs/README.md` indexed 35 of 40 specs; and the
# Monitoring-as-Code bundle README listed 13 supported types and declared `promql` unavailable
# through files three days after D-0145's addendum admitted it.
#
# The shape is the same every time: a number or a list, written by hand, about something the tree
# already knows. Nothing read it back, so it drifted silently and a reader had no way to tell.

# The README shows DISPLAY names, so this map is the asserted bridge between them and the wire
# values. Reviewer P1 at party [206]: the first version of the guard checked only the COUNT, so a
# same-count substitution — `gRPC` swapped for `GraphQL` — passed while the 1:1 property the README
# states was false. Keys must be exactly the MonitorType values, which means a NEW monitor type
# fails here until someone writes its label. That is the intent, not an inconvenience.
CHECK_TYPE_LABELS = {
    'http': 'HTTP', 'tcp': 'TCP', 'icmp': 'ICMP', 'dns': 'DNS', 'tls': 'TLS-cert',
    'grpc': 'gRPC', 'postgres': 'PostgreSQL', 'mysql': 'MySQL', 'redis': 'Redis',
    'promql': 'PromQL', 'rabbitmq': 'RabbitMQ', 'websocket': 'WebSocket', 'ssh': 'SSH',
    'composite': 'composite', 'push': 'push', 'synthetic': 'synthetic',
    'async_canary': 'async canary',
}

# Phrasings that ASSERT a residual exists. A negation ("no row is `PARTIAL` any more") must stay
# legal — it is the true sentence a fully discharged map deserves — so the guard enumerates the
# positive forms instead of trying to detect absence of negation. The sentence that shipped twice
# contained the word "no" about something else entirely, which is why a negation heuristic fails
# here and an enumeration does not.
PARTIAL_CLAIM_RES = [
    re.compile(r'\b(?:one|a|the)\b(?:\s+\S+){0,3}\s+row\s+is\s+`PARTIAL`'),
    re.compile(r'\bremaining\s+`PARTIAL`'),
    re.compile(r'`PARTIAL`\s+and\s+names'),
]

NUMBER_WORDS = {1: 'one', 2: 'two', 3: 'three', 4: 'four', 5: 'five', 6: 'six', 7: 'seven',
                8: 'eight', 9: 'nine', 10: 'ten', 11: 'eleven', 12: 'twelve'}


def flatten(text):
    """Collapse a Go comment block or a wrapped Markdown sentence onto one line."""
    return re.sub(r'\s+', ' ', re.sub(r'\n\s*//', '', text))


def monitor_type_values(src=None):
    """{constant: wire value} for every MonitorType, e.g. {'MonitorHTTP': 'http'}."""
    src = read('internal/domain/monitor.go') if src is None else src
    return dict(re.findall(r'\b(Monitor[A-Za-z]+)\s+MonitorType\s*=\s*"([a-z_]+)"', src))


def setnull_migrations():
    """Numeric prefixes of migrations using PostgreSQL 15's column-list ON DELETE SET NULL."""
    return sorted(os.path.basename(p)[:5] for p in glob.glob('internal/store/migrations/*.sql')
                  if re.search(r'ON DELETE SET NULL \(', read(p)))


def mac_supported_types(src=None):
    """Wire values of fileSupportedTypes — what a Monitoring-as-Code bundle may declare."""
    src = read('internal/fileprovider/bundle.go') if src is None else src
    block = re.search(r'var fileSupportedTypes = map\[domain\.MonitorType\]bool\{(.*?)\n\}', src, re.S)
    if not block:
        return None
    vals = monitor_type_values()
    return sorted(vals[c] for c in re.findall(r'domain\.(Monitor[A-Za-z]+):\s*true', block.group(1))
                  if c in vals)


def traceability_partial_rows():
    """{requirement: (PARTIAL row count, [spec files named in the heading])}.

    Counts table ROWS, not the word. The FR-029 preamble says "No row is `PARTIAL —` any more",
    and counting mentions would read that sentence as evidence of the thing it denies.
    """
    out = {}
    for part in re.split(r'\n## ', read('docs/traceability.md')):
        head = part.split('\n', 1)[0]
        m = re.match(r'((?:FR|NFR)-\d+)', head)
        if not m:
            continue
        rows = [l for l in part.splitlines() if l.startswith('|') and 'PARTIAL' in l]
        specs = re.findall(r'`((?:func|sec|ops)-[a-z0-9-]+\.md)`', head)
        out[m.group(1)] = (len(rows), specs)
    return out


def check_partial_claims():
    """A document may not announce a residual its own discharge map does not have.

    Reviewer P1 at party [206]. `func-async-canary.md` still said "one row is `PARTIAL` and names
    what is missing" after BOTH residuals closed — invariant 6 at iter-0171 (D-0231) and invariant
    13's audit-trail clause at iter-0172 (D-0233) — and the README synchronization pass copied that
    sentence into `docs/specs/README.md`. A false residual introduced by the commit whose whole
    purpose was removing false claims, which is the strongest argument for checking it here: the
    map is the fact, and a banner is only a claim about it.
    """
    bad = []
    by_spec = {s: (n, req)
               for req, (n, specs) in traceability_partial_rows().items() for s in specs}
    for path in sorted(glob.glob('docs/specs/*.md')):
        base = os.path.basename(path)
        if base not in by_spec or by_spec[base][0]:
            continue
        req = by_spec[base][1]
        for n, line in enumerate(read(path).splitlines(), 1):
            if any(r.search(line) for r in PARTIAL_CLAIM_RES):
                bad.append((path, n, 'partial',
                            f'claims a `PARTIAL` row; the {req} map in docs/traceability.md has none'))
    # The index repeats these banners, which is exactly how the false residual spread.
    idx = 'docs/specs/README.md'
    for n, line in enumerate(read(idx).splitlines(), 1):
        m = re.match(r'\| `((?:func|sec|ops)-[a-z0-9-]+\.md)`', line)
        if not m or m.group(1) not in by_spec or by_spec[m.group(1)][0]:
            continue
        if any(r.search(line) for r in PARTIAL_CLAIM_RES):
            bad.append((idx, n, 'partial',
                        f'the {m.group(1)} row claims a `PARTIAL`; the '
                        f'{by_spec[m.group(1)][1]} map has none'))
    return bad


def check_type_list_findings(stated_count, listed, wire, labels=None):
    """Compare README's check-type highlight against the MonitorType set. Returns messages.

    PURE on purpose (reviewer P2 at party [208]): the mutations that matter here — a same-count
    substitution, a same-count duplication — were demonstrated by a harness that edited the real
    README and put it back, which is manual evidence and not a test. With the comparison separated
    from the file reading, they are fixtures.

    `listed` is the enumeration text. Parentheticals are stripped first because they are prose
    ABOUT one type that names others ("scripted multi-step HTTP flows" would make `HTTP` occur
    twice), which is the difference between a real second entry and a mention.
    """
    labels = CHECK_TYPE_LABELS if labels is None else labels
    out = []
    for t in sorted(wire - set(labels)):
        out.append(f'MonitorType `{t}` has no entry in CHECK_TYPE_LABELS, so nothing checks that '
                   f'README.md lists it')
    for t in sorted(set(labels) - wire):
        out.append(f'CHECK_TYPE_LABELS names `{t}`, which is not a MonitorType')
    if stated_count != len(wire):
        out.append(f'says {stated_count} check types, but internal/domain/monitor.go declares '
                   f'{len(wire)}')
    stripped = re.sub(r'\([^)]*\)', ' ', listed)
    for t in sorted(wire & set(labels)):
        label = labels[t]
        hits = len(re.findall(r'(?<![\w-])' + re.escape(label) + r'(?![\w-])', stripped))
        if hits != 1:
            out.append(f'MonitorType `{t}` should appear in the highlight exactly once as '
                       f'"{label}"; found {hits}')
    return out


def check_fr032_discharge(body, test_source, spec='docs/specs/func-expected-run-ledger.md',
                          testfile='internal/store/revisiontimeline_internal_test.go',
                          section='### 17.3', ends='### 17.4', row='13a'):
    """An FR-032 discharge section's citation and BOTH of its §17.2 row counts, against artefacts.

    Parameterized over (section, test file, row) so a second discharged invariant reuses the ONE
    mechanism instead of copying it: 10j's discharge in §17.5 is checked by this same function.
    A guard that gets copied per case is a guard that drifts per case.

    PURE: takes the spec text and the test file's source, returns findings. Extracted so the
    fixture tests can call THIS rather than re-implement it — reviewer [321] pointed out that my
    first fixtures reproduced the regexes, so deleting the guard would have left six of seven
    tests passing. A test that models a mechanism instead of invoking it is evidence of nothing,
    which is the defect this whole requirement keeps teaching me.

    A discharge map that claims "these tests prove it" must name the tests that exist, in both
    directions, and its counts must be checked against the ARTEFACTS rather than against another
    sentence. The count guard covered only the test number at first, leaving the mutation number
    beside it free — a guard over one of two adjacent claims invites trust in the other.
    """
    out = []
    label = section.replace('### ', '§')
    # The row's claim is read FIRST, because the section's absence is only innocent while nothing
    # claims to be discharged by it. Returning silently on a missing section let the whole section
    # be deleted with the row still advertising "N tests, M mutations killed" — the citation then
    # guards nothing and says so to nobody.
    claim = re.search(r'\| ' + re.escape(row) + r' \|[^|]*\|[^|]*\|[^|]*\| (\d+) tests, (\d+) mutations',
                      body)
    if section not in body:
        if claim:
            out.append(f"§17.2's {row} row claims {claim.group(1)} tests and {claim.group(2)} "
                       f'mutations discharged by {label}, which is not in the document')
        return out
    sec = body.split(section, 1)[1].split(ends, 1)[0]
    cited = set(re.findall(r'`(Test[A-Za-z0-9_]+)`', sec))
    declared = set(re.findall(r'^func (Test[A-Za-z0-9_]+)', test_source, re.M))
    for name in sorted(declared - cited):
        out.append(f'{testfile} declares {name}, which {label} does not cite — a test that '
                   f'discharges nothing, or a discharge map that undercounts itself')
    for name in sorted(cited - declared):
        out.append(f'{label} cites {name}, which is not declared in {testfile}')
    muts = len(re.findall(r'^\d+\. ', sec, re.M))
    m = claim
    if not m:
        out.append(f"§17.2's {row} row no longer states its test and mutation counts in the form "
                   'the guard reads')
    else:
        if int(m.group(1)) != len(declared):
            out.append(f"§17.2's {row} row says {m.group(1)} tests; {testfile} declares "
                       f'{len(declared)}')
        if int(m.group(2)) != muts:
            out.append(f"§17.2's {row} row says {m.group(2)} mutations; {label} enumerates {muts}")
    if muts == 0:
        out.append(f'{label} enumerates no mutations as a numbered list, so the row\'s mutation '
                   'count is checked against nothing')
    return out


def paragraphs(text):
    """`text` as claim-sized units, each flattened to one line.

    A count and the iteration it belongs to are routinely a LINE APART in prose — these documents
    wrap at 100 columns — so a line-based reader scan misses exactly the drift it exists to catch,
    which is how the first version of the iter-0178 guard let a mutated "three P1s" through.

    But a MARKDOWN TABLE has no blank lines between its rows, and each row is its own claim. Joined
    into one unit, one row's number is attributed to whichever iteration another row happens to
    name — which immediately misread `iter-0177`'s own rows ABOUT a drift as the drift. So a line
    beginning with `|` is a unit by itself.
    """
    units = []
    for block in re.split(r'\n\s*\n', text):
        prose = []
        for line in block.split('\n'):
            if line.lstrip().startswith('|'):
                units.append(' '.join(line.split()))
            else:
                prose.append(line)
        if prose:
            units.append(' '.join(' '.join(prose).split()))
    return units


def unquoted(text):
    """`text` with double-quoted spans removed.

    A number inside quotation marks is a CITATION of a claim, not a claim: these documents keep the
    wrong number visible on purpose — "it first said \u201cthree P1s\u201d while the table held five" is
    the record of the correction, and a guard that reads it as a fresh drift punishes the honesty
    it exists to enforce. Same reasoning as `check_fr032_audit_totals` reading only its declared
    **Result:** span.
    """
    return re.sub(r'[\u201c"\u2018\u2019\'][^\u201d"\u2018\u2019\']*[\u201d"\u2018\u2019\']', ' ', text)


def regions_naming(line, name):
    """The spans of `line` that belong to iteration `name`.

    `status.md` packs a CHAIN of iterations into one line — "iter-0178 ... Previous: iter-0177 ..."
    — so a line-wide scan attributes every count in it to every iteration named in it, and a
    sentence-wide scan does no better, because a count often sits a sentence away from the name it
    belongs to. The rule that holds for this document's shape: text after an `iter-NNNN` mention
    belongs to THAT iteration until the next mention.

    Without this, the guard added for iter-0178 immediately reported iter-0177's own "EIGHT
    findings" as iter-0178's drift — a false positive that would have taught the next reader to
    ignore it.
    """
    marks = [(m.start(), m.group(0)) for m in re.finditer(r'iter-\d{4}', line)]
    out = []
    for i, (start, mark) in enumerate(marks):
        end = marks[i + 1][0] if i + 1 < len(marks) else len(line)
        if mark == name:
            out.append(line[start:end])
    return out


def check_iteration_finding_counts(iteration='docs/iterations/iter-0177.md',
                                   readers=('docs/status.md', 'docs/traceability.md',
                                            'docs/decisions.md')):
    """An iteration's finding count, DERIVED from its own findings table.

    `iter-0177`'s count drifted three times in one evening. The line that opened the iteration said
    three, the line that closed it said four, and both survived in the same sentence; then the
    traceability head said three while the iteration, the status cell and the decision said four —
    written, in that instance, at the very moment the fourth finding was being fixed. `docs-check`
    was green over all of it, because a citation guard validates that what IS written resolves and
    can say nothing about a number that stopped agreeing with its own table.

    This is the same shape as `check_fr032_audit_totals` one level up: the number lives beside the
    rows it summarises, so it is derived from them rather than compared between prose and prose. Any
    document that states "<n> findings" in a sentence naming this iteration must agree with the
    table, and a document that names the iteration without a count is fine — the guard is about
    disagreement, not about mandatory phrasing.
    """
    out = []
    if not os.path.exists(iteration):
        return out
    body = read(iteration)
    # The findings table: rows whose first cell is a finding number, `| 0 |` through `| 9 |`.
    rows = re.findall(r'^\| (\d+) \|', body, re.M)
    if not rows:
        out.append(f'{iteration} states findings but has no numbered findings table to derive the '
                   f'count from; a total beside no rows is a number nobody can check')
        return out
    derived = len(rows)
    words = {1: 'one', 2: 'two', 3: 'three', 4: 'four', 5: 'five', 6: 'six', 7: 'seven',
             8: 'eight', 9: 'nine', 10: 'ten'}
    name = os.path.basename(iteration).removesuffix('.md')
    spelled = words.get(derived)
    for doc in (iteration,) + tuple(readers):
        if not os.path.exists(doc):
            continue
        for line in paragraphs(read(doc)):
            if name not in line:
                continue
            scoped = unquoted(' '.join(regions_naming(line, name)))
            for stated in re.findall(r'\b(\d+|' + '|'.join(words.values()) + r')\s+findings\b',
                                     scoped, re.I):
                low = stated.lower()
                value = int(low) if low.isdigit() else next(
                    (n for n, w in words.items() if w == low), None)
                if value is not None and value != derived:
                    out.append(f'{doc} says {stated} findings for {name}; its findings table holds '
                               f'{derived}{" (" + spelled + ")" if spelled else ""}. A count that '
                               f'drifts from its own rows is the one number nobody re-derives')
    return out

def check_iter0178_findings(iteration='docs/iterations/iter-0178.md',
                            readers=('docs/status.md', 'docs/decisions.md',
                                     'docs/traceability.md')):
    """iter-0178's SEVERITY and ORIGIN split, derived from its own findings table.

    `check_iteration_finding_counts` above derives a total from the same shape and did not fire
    here, because the claim that drifted was phrased "three P1s on the response" rather than
    "three findings". A guard is only as wide as the sentence it matches — so this one reads the
    severity column and the origin column and holds the declared **Tally:** span, plus any reader
    document that states a P0/P1 count beside this iteration's name, to what the rows actually say.

    The table is `| # | Severity | Found in | ... |`; a row's origin is "the slice" or "the
    response". Both are derived; neither is written twice.
    """
    out = []
    if not os.path.exists(iteration):
        return out
    body = read(iteration)
    rows = re.findall(r'^\| (\d+) \| *(P[01]) *\| *([^|]*?) *\|', body, re.M)
    if not rows:
        out.append(f'{iteration} has no `| # | Severity | Found in |` findings table, so every '
                   f'count it states about itself is a number nobody can re-derive')
        return out
    total = len(rows)
    p0 = sum(1 for _, sev, _ in rows if sev == 'P0')
    p1 = sum(1 for _, sev, _ in rows if sev == 'P1')
    in_slice = sum(1 for _, _, where in rows if 'slice' in where)
    on_response = sum(1 for _, _, where in rows if 'response' in where)
    if in_slice + on_response != total:
        out.append(f'{iteration}: {total - in_slice - on_response} findings row(s) name neither '
                   f'"the slice" nor "the response" as their origin, so the split cannot be derived')
    words = {1: 'one', 2: 'two', 3: 'three', 4: 'four', 5: 'five', 6: 'six', 7: 'seven',
             8: 'eight', 9: 'nine', 10: 'ten'}

    def value(token):
        low = token.lower()
        return int(low) if low.isdigit() else next((n for n, w in words.items() if w == low), None)

    number = r'(\d+|' + '|'.join(words.values()) + r')'
    # The declared tally span is where the totals live, so it is checked exhaustively.
    tally = re.search(r'\*\*Tally:(.+?)\*\*', body, re.S)
    if not tally:
        out.append(f'{iteration} has a findings table and no **Tally: ...** span; the totals must '
                   f'live where a reader looks for them or the table summarises itself to nobody')
    else:
        span = tally.group(1)
        for pattern, derived, label in (
                (number + r'\s+findings', total, 'findings'),
                (number + r'\s+P0s?', p0, 'P0'),
                (number + r'\s+P1s?', p1, 'P1'),
                (number + r'\s+in the slice', in_slice, 'in the slice'),
                (number + r'\s+on the response', on_response, 'on the response')):
            found = [value(m) for m in re.findall(pattern, span, re.I)]
            found = [v for v in found if v is not None]
            if not found:
                out.append(f'{iteration}: the **Tally:** span states no {label} count, so that '
                           f'number is claimed elsewhere and derived nowhere')
            for v in found:
                if v != derived:
                    out.append(f'{iteration}: the **Tally:** span says {v} {label}; its findings '
                               f'table holds {derived}')
    # Reader documents may state the severity split beside this iteration's name; if they do, it
    # must be the table's.
    name = os.path.basename(iteration).removesuffix('.md')
    for doc in readers:
        if not os.path.exists(doc):
            continue
        for line in paragraphs(read(doc)):
            if name not in line:
                continue
            scoped = unquoted(' '.join(regions_naming(line, name)))
            for pattern, derived, label in ((number + r'\s+P0s?', p0, 'P0'),
                                            (number + r'\s+P1s?', p1, 'P1')):
                for m in re.findall(pattern, scoped, re.I):
                    v = value(m)
                    if v is not None and v != derived:
                        out.append(f'{doc} says {m} {label} for {name}; its findings table holds '
                                   f'{derived}. The count that drifted here was phrased "P1s", '
                                   f'which is why the findings-count guard did not see it')
    return out

def check_fr032_audit_totals(body, spec='docs/specs/func-expected-run-ledger.md'):
    """FR-032 §17.2's own totals, DERIVED from its discharge table instead of read beside it.

    The section stated "65 invariants ... 30 to specify" while the table under it already held 66
    rows and 28, through 13a's addition and §17.4's two specifications. A total written beside its
    own table is the one number nobody re-derives, so every number the section claims about itself
    is checked against the rows — including the per-phase breakdown of what is still to specify,
    and including rows whose status is unreadable, which would otherwise leave the audit silently.
    """
    out = []
    if '### 17.2' not in body:
        return out
    sec = body.split('### 17.2', 1)[1].split('### 17.3', 1)[0]
    rows = re.findall(r'^\| ([0-9]+[a-z]*) \|([^|]*)\|[^|]*\|([^|]*)\|', sec, re.M)
    if not rows:
        out.append(f'{spec} §17.2 states totals but has no discharge rows to derive them from')
        return out

    # Only the heading line and the bold **Result: ...** span are read as CLAIMS. The prose around
    # them quotes the superseded numbers on purpose — this spec keeps every rejection, so a guard
    # that scanned all of §17.2 would report the drift it documents. Scanning a declared region
    # instead makes the guard stricter: the totals must live where a reader looks for them.
    claim = re.search(r'\*\*Result:(.+?)\*\*', sec, re.S)
    if not claim:
        out.append(f'{spec} §17.2 has no bold **Result: ...** span, which is the one place its '
                   f'totals are checked; without it the table below summarises itself to nobody')
        return out
    regions = (('its heading', sec.split('\n', 1)[0]), ('its **Result:** span', claim.group(1)))

    def bucket(cell):
        text = cell.strip().strip('*')
        for key in ('TO SPECIFY', 'SPECIFIED', 'DISCHARGED', 'covered', 'n/a'):
            if key in text:
                return key
        return None

    tally, phases, unreadable = {}, {}, []
    for num, phase, status in rows:
        key = bucket(status)
        if key is None:
            unreadable.append(num)
            continue
        tally[key] = tally.get(key, 0) + 1
        if key == 'TO SPECIFY':
            phases[phase.strip()] = phases.get(phase.strip(), 0) + 1
    if unreadable:
        out.append(f"§17.2 row(s) {', '.join(unreadable)} carry no status this guard recognises, so "
                   f"they count toward no total and would drop out of the audit unnoticed")

    # Both regions are checked SEPARATELY and both must carry every tally. Requiring it merely
    # "somewhere in §17.2" let a tally be deleted from one place while the other still satisfied
    # the guard — and the two drifting apart from each other is the same failure as either one
    # drifting from the table.
    for where, region in regions:
        for label, pattern, derived in (
                ('invariants', r'(\d+) invariants', len(rows)),
                ('covered', r'(\d+) covered', tally.get('covered', 0)),
                ('specified', r'(\d+) specified', tally.get('SPECIFIED', 0)),
                ('discharged', r'(\d+) discharged', tally.get('DISCHARGED', 0)),
                ('withdrawn', r'(\d+) withdrawn', tally.get('n/a', 0)),
                ('to specify', r'(\d+) (?:still )?to specify', tally.get('TO SPECIFY', 0))):
            stated = re.findall(pattern, region, re.I)
            if not stated:
                out.append(f'§17.2 {where} no longer states how many rows are "{label}"; the total '
                           f'it dropped is {derived}, and a total nobody states is one nobody checks')
                continue
            for value in stated:
                if int(value) != derived:
                    out.append(f'§17.2 {where} says {value} {label}; its table holds {derived}')

    named = {p: int(n) for n, p in re.findall(r'(\d+) in ([A-E][0-9]?)\b', claim.group(1))}
    if named != phases:
        out.append(f'§17.2 breaks "to specify" down as {named or "nothing"}; its table holds '
                   f'{phases} (a phase whose rows are unspecified must appear, with its own count, '
                   f'because the breakdown is what tells the next phase whether it may start)')
    return out


def check_fr032_drain_surfaces(spec_body, migration_sources, store_sources,
                               spec='docs/specs/func-expected-run-ledger.md'):
    """§13.0's rollback-and-drain rule against the surfaces and terminal paths in the TREE.

    The approved paragraph called `pull_jobs` TTL expiry "the" drain mechanism and then generalized
    a job-shaped sentence to all "in-flight V4 work" — while V4-capable `pull_tests` rows exist too,
    and each surface has TWO terminal paths, not one (reviewer [387]). A prose rule about a SET is
    worth nothing unless the set is derived: the surfaces come from the migrations, and the terminal
    paths from every store function that DELETEs from one of them.

    A surface qualifies only if its EFFECTIVE CHECK admits generation 4 — reviewer P0 at [390].
    Carrying a `protocol_version` constraint is not the same as being able to hold a V4 row, and the
    first version of this guard conflated them: a future pull table capped at 1..3 would have forced
    irrelevant §13.0 text. My own "third surface" mutation added `IN (1)` and called it V4-capable,
    so that evidence rested on the same false premise it was meant to disprove.

    Three details decide correctness. Migrations are ordered by their **parsed numeric version**,
    not by path: five-digit padding makes lexical and numeric order agree today, and the guard's
    correctness must not rest on a future filename keeping that formatting (reviewer [402]) — with
    versions 9 and 10, lexical order puts 10 first and the older definition would win. Only each
    migration's **Up half** counts: `00101`'s own Down narrows both CHECKs back to `IN (1, 2, 3)`,
    so reading Downs empties the set and the guard would then pass on any wording at all. And a later Up that drops the column removes the surface, while
    one that drops the constraint without replacing it does NOT — an unconstrained column accepts 4,
    so staying in the set is the conservative reading.

    `TRUNCATE` is scanned too, with ONE allowlisted exclusion: `(*Store).TruncateAll` in
    `internal/store/store.go` — compared as a NORMALIZED repo-relative path, not by suffix — the
    test helper that empties every table. It removes V4 rows and
    belongs in no operational drain rule. Anything else truncating an effective V4 surface is
    REPORTED — reviewer [392] — because "the scan pattern happens not to match it" is not a
    decision, and a truncate on an operational path is either a terminal path §13.0 must describe
    or a bug. The allowlist is the exact receiver and function, not the file: another function in
    `store.go` truncating a surface is still reported.

    PURE: takes the spec text, the migration SQL and the store sources as {path: text}.
    """
    out = []
    admits = {}

    def version(path):
        m = re.match(r'(\d+)', os.path.basename(path))
        return (int(m.group(1)) if m else -1, path)

    for path in sorted(migration_sources, key=version):
        up = migration_sources[path].split('-- +goose Down', 1)[0]
        for table, listed in re.findall(
                r'ADD CONSTRAINT ([a-z_]+)_protocol_version_check\s+CHECK \(protocol_version IN \(([^)]*)\)\)', up):
            admits[table] = {v.strip() for v in listed.split(',')}
        for table in re.findall(r'ALTER TABLE ([a-z_]+)[^;]*DROP COLUMN (?:IF EXISTS )?protocol_version', up):
            admits.pop(table, None)
    surfaces = {t for t, versions in admits.items() if '4' in versions}
    if not surfaces:
        out.append(f'{spec} §13.0: no migration leaves a `protocol_version` CHECK admitting '
                   f'generation 4, so the drain rule is checked against an empty set. Reading a '
                   f"migration's Down half does this — 00101's Down narrows both CHECKs back to 3")
        return out

    # Every function that can remove a row from one of those surfaces, found where it lives.
    ALLOWED_TRUNCATE = ('internal/store/store.go', '*Store', 'TruncateAll')
    named = '|'.join(sorted(surfaces))
    paths, truncates = {}, []
    for path, text in sorted(store_sources.items()):
        recv, fn = '', None
        for lineno, line in enumerate(text.split('\n'), 1):
            m = re.match(r'func (?:\((?:\w+ )?([\w*]+)\) )?(\w+)', line)
            if m:
                recv, fn = m.group(1) or '', m.group(2)
            if not fn:
                continue
            hit = re.search(r'DELETE FROM (' + named + r')\b', line)
            if hit:
                paths.setdefault(hit.group(1), set()).add(fn)
            if re.search(r'\bTRUNCATE\b', line):
                for surface in re.findall(r'\b(' + named + r')\b', line):
                    # Repo-relative path equality, NORMALIZED — not endswith. A suffix test
                    # allowlists `other/internal/store/store.go` carrying the same receiver and
                    # function, which is the collision reviewer [401] required closed.
                    # No lstrip: it strips a CHARACTER SET, not a prefix, so `../internal/store/
                    # store.go` became the allowlisted literal and traversed straight past the
                    # check (reviewer [407]). normpath alone already folds `./` away.
                    here = os.path.normpath(path).replace(os.sep, '/')
                    if (here == ALLOWED_TRUNCATE[0] and recv == ALLOWED_TRUNCATE[1]
                            and fn == ALLOWED_TRUNCATE[2]):
                        continue
                    truncates.append((path, lineno, recv, fn, surface))

    for path, lineno, recv, fn, surface in truncates:
        shown = f'({recv}).{fn}' if recv else fn
        out.append(f'{path}:{lineno} — {shown} TRUNCATEs `{surface}`, an effective generation-4 '
                   f'surface, and is not the one allowlisted test helper '
                   f'(*{ALLOWED_TRUNCATE[1].lstrip("*")}).{ALLOWED_TRUNCATE[2]} in '
                   f'{ALLOWED_TRUNCATE[0]}. Either it is a terminal path §13.0 must describe, or it '
                   f'is operational code discarding V4 work')

    if '**Rollback and drain.**' not in spec_body:
        out.append(f'{spec} §13.0 no longer carries a "Rollback and drain." paragraph, so the rule '
                   f'this guard checks has no place to be wrong in')
        return out
    sec = spec_body.split('**Rollback and drain.**', 1)[1].split('### 13.1', 1)[0]

    for surface in sorted(surfaces):
        if f'`{surface}`' not in sec:
            out.append(f"{spec} §13.0's drain rule never names `{surface}`, whose effective CHECK "
                       f'ADMITS generation 4 ({", ".join(sorted(admits[surface]))}) and which can '
                       f'therefore hold a V4 row — a rule that names one surface is how the '
                       f'job-shaped sentence stood for all V4 work')
            continue
        for fn in sorted(paths.get(surface, ())):
            if f'`{fn}`' not in sec:
                out.append(f"{spec} §13.0's drain rule does not cite `{fn}`, a terminal path for "
                           f'`{surface}` — leaving expiry to read as the only route')
    for surface in sorted(surfaces):
        if not paths.get(surface):
            out.append(f'{spec} §13.0: no store function deletes from `{surface}`, so its terminal '
                       f'paths cannot be checked; the guard would pass on any wording')
    return out


def check_fr032_transport_matrix(body, spec='docs/specs/func-expected-run-ledger.md'):
    """§16.1's half-deployment rows must say WHICH transports they apply to, and why.

    The matrix presented "core at B1, executor at B2" and its reverse as transport-general. A
    half-deployed cluster needs a WIRE between core and executor, so those rows cannot arise for a
    pure in-process `role=all` — where the safety proof is the FLAG (invariant 10k), not a withheld
    announcement there is nobody to withhold. Stating it generally promises a protection that does
    not exist in the most common deployment, which is exactly how §13.0's drain sentence went wrong
    (reviewer [415], confirming [413]).

    Parses the applicability TABLE rather than grepping prose: the four contexts as a SET, each with
    a verdict, and the in-process row obliged to name both the flag and 10k.
    """
    out = []
    if '### 16.1' not in body:
        return out
    sec = body.split('### 16.1', 1)[1].split('## 17', 1)[0]

    rows, seen_header, duplicates = {}, False, []
    for line in sec.split('\n'):
        if not line.startswith('|'):
            continue
        cells = [c.strip() for c in line.strip('|').split('|')]
        if len(cells) < 3:
            continue
        first = cells[0].lower()
        if first == 'transport':
            seen_header = True
            continue
        if not seen_header or set(cells[0]) <= set('- '):
            continue
        if 'pull.regions' in first:
            key = 'role=all with pull regions'
        elif 'in-process' in first:
            key = 'in-process'
        elif 'amqp' in first:
            key = 'AMQP'
        elif 'pull' in first:
            key = 'pull'
        else:
            continue
        if key in rows:
            duplicates.append(key)
        rows[key] = (cells[1], cells[2])

    if not seen_header:
        out.append(f'{spec} §16.1 has no transport-applicability table (a `| Transport | … |` '
                   f'header), so its half-deployment rows read as transport-general — the defect '
                   f'§13.0 already had')
        return out

    for key in sorted(set(duplicates)):
        out.append(f'{spec} §16.1\'s transport table lists "{key}" more than once; the last row '
                   f'silently wins, so one of them is unread')

    want = {'AMQP', 'pull', 'in-process', 'role=all with pull regions'}
    for missing in sorted(want - set(rows)):
        out.append(f'{spec} §16.1\'s transport table does not cover "{missing}"; every execution '
                   f'context must state whether a half-deployed cluster can arise in it')
    for extra in sorted(set(rows) - want):
        out.append(f'{spec} §16.1\'s transport table has an unrecognised context "{extra}"')

    for key, (verdict, why) in sorted(rows.items()):
        if not verdict.strip('* '):
            out.append(f'{spec} §16.1: context "{key}" states no applicability verdict')
        if not why.strip('* '):
            out.append(f'{spec} §16.1: context "{key}" names no safety mechanism')

    if 'in-process' in rows:
        verdict, why = rows['in-process']
        if 'impossible' not in verdict.lower():
            out.append(f'{spec} §16.1 says the half-deploy rows are "{verdict}" in-process. They '
                       f'require a wire between core and executor, and in-process there is none')
        for token, plain in (('ledger.carrier_enabled', 'the flag'), ('10k', 'invariant 10k')):
            if token not in why:
                out.append(f'{spec} §16.1\'s in-process row does not cite {plain} ({token}); with '
                           f'no announcement to withhold, that is the whole safety proof there')
    # Every WIRE context carries the same mechanism — announcement AND the gate — and a nonempty
    # cell is not that claim. `role=all` with pull regions is a wire context too: it may explain
    # why the local executor is excluded, but only after stating the mechanism (reviewer [417]).
    for wire in ('AMQP', 'pull', 'role=all with pull regions'):
        if wire not in rows:
            continue
        verdict, why = rows[wire]
        if 'appl' not in verdict.lower():
            out.append(f'{spec} §16.1 says the half-deploy rows are "{verdict}" for {wire}; '
                       f'that transport has a wire, so they apply')
        # The ACT, not the label. Checking for the word "announcement" passed on the lead-in
        # phrase "announcement plus the gate:" while the mechanism beside it said something else —
        # a label standing in for a mechanism, inside the guard that exists to stop exactly that.
        if not re.search(r'announce[sd]?\s+V4', why, re.I):
            out.append(f'{spec} §16.1: the {wire} row does not say WHO announces V4. On a wire '
                       f'context a region is promoted only when its executors announce the '
                       f'generation, and the phrase "announcement" in a label is not that claim')
        if 'ledger.carrier_enabled' not in why:
            out.append(f'{spec} §16.1: the {wire} row does not name `ledger.carrier_enabled`. '
                       f'Announcement alone cannot promote a region while the gate is off')
    return out


def check_fr032_carrier_gate_contract(body, spec='docs/specs/func-expected-run-ledger.md'):
    """The `ledger.carrier_enabled` contract must say what a B1 binary does with `true`.

    "Defaults false" describes only ABSENCE. An operator can supply the key, and both plausible
    readings of silence are wrong: accepting it lets B1 publish V4 before `DueAt` exists (10h), and
    coercing it to false silently is the self-healing AGENTS.md forbids (reviewer [423]). So the
    guard requires the refusal clause, the B2 ownership clause, and NAMED owners — the validating
    function and the scheduler construction path — rather than the word "default".
    """
    out = []
    # The scan STOPS at the end of the contract table. Letting `seen` stay true ran on to §16.1's
    # mixed-version matrix, whose rows also begin `| B1 |` and `| **B1** |`, and those silently
    # overwrote the contract's — the guard then reported the real spec as contractless. Caught by
    # running it against the tree rather than against fixtures.
    # Citations are checked in the contract's OWN section, not the whole document: scanning the
    # body let `internal/config/config.go` satisfy the check from an unrelated mention elsewhere.
    anchor = body.find('| Phase | `ledger.carrier_enabled`')
    section = body[anchor:anchor + 2500] if anchor >= 0 else ''
    rows, seen = {}, False
    for line in body.split('\n'):
        if seen and not line.startswith('|'):
            break
        if not line.startswith('|'):
            continue
        cells = [c.strip() for c in line.strip('|').split('|')]
        if len(cells) < 3:
            continue
        if cells[0].lower() == 'phase' and 'carrier_enabled' in line:
            seen = True
            continue
        if not seen:
            continue
        key = cells[0].strip('*` ')
        if key in ('B1', 'B2'):
            rows[key] = cells[2]
    if not seen:
        out.append(f'{spec} has no `ledger.carrier_enabled` phase contract table (a `| Phase | … |` '
                   f'header naming the key), so "defaults false" is the whole specification and it '
                   f'says nothing about an operator supplying true')
        return out
    for phase in ('B1', 'B2'):
        if phase not in rows:
            out.append(f'{spec}: the carrier-gate contract has no {phase} row; each phase must say '
                       f'what it does when the key is supplied as true')
    if 'B1' in rows:
        cell = rows['B1']
        if not re.search(r'refus|reject', cell, re.I):
            out.append(f'{spec}: the carrier-gate contract does not say B1 REFUSES '
                       f'`ledger.carrier_enabled: true`. Accepting it publishes V4 before its '
                       f'payload exists; coercing it to false silently is a self-healing runtime')
        if '(*Config).Validate' not in cell and 'Validate' not in cell:
            out.append(f'{spec}: the B1 refusal names no validating owner. A refusal nobody owns is '
                       f'a sentence, not a startup failure')
    if 'B2' in rows and 'atomic' not in rows['B2'].lower():
        out.append(f'{spec}: the B2 row does not tie the gate to the ATOMIC payload change; '
                   f'enabling selection apart from it is the ordering 10h forbids')
    for cited, why in (('internal/config/config.go', 'the config owner'),
                       ('internal/cli/cli.go', 'the scheduler construction path')):
        if cited not in section:
            out.append(f'{spec}: the carrier-gate contract cites no path for {why} ({cited}); '
                       f'unowned, it drifts into a role-local branch or an environment read')
    return out


def check_enumerations():
    bad = []

    # 1. README's check-type highlight vs the constants — the COUNT and the SET.
    doc = read('README.md')
    wire = set(monitor_type_values().values())
    m = re.search(r'\*\*(\d+) check types\*\*(.*?)All behind an SSRF guard', flatten(doc))
    if not m:
        bad.append(('README.md', 1, 'enum',
                    'the "N check types ... All behind an SSRF guard" highlight is gone; the guard '
                    'cannot find the list to check'))
    else:
        line = doc[:doc.find('check types')].count('\n') + 1
        for msg in check_type_list_findings(int(m.group(1)), m.group(2), wire):
            bad.append(('README.md', line, 'enum', msg))

    # 2. the ON DELETE SET NULL enumeration, in BOTH documents that state it.
    mig = setnull_migrations()
    word = NUMBER_WORDS.get(len(mig), str(len(mig)))
    m = re.search(r'(\w+)\s+migrations use the column-list `ON DELETE SET NULL \(col\)` form', flatten(doc))
    if not m:
        bad.append(('README.md', 1, 'enum', 'the ON DELETE SET NULL sentence is gone; the guard cannot check it'))
    elif m.group(1) != word:
        bad.append(('README.md', 1, 'enum',
                    f'says "{m.group(1)} migrations" use the column-list ON DELETE SET NULL form; '
                    f'{len(mig)} do ({", ".join(mig)})'))

    go = flatten(read('internal/store/migrate.go'))
    m = re.search(r"(\w+) migrations use PostgreSQL 15's column-list "
                  r"`ON DELETE SET NULL \(col\)` form \(([0-9, ]+)\)", go)
    if not m:
        bad.append(('internal/store/migrate.go', 1, 'enum',
                    'the version-check comment no longer states the count and the file list'))
    else:
        listed = sorted(x.strip() for x in m.group(2).split(','))
        if m.group(1) != word or listed != mig:
            bad.append(('internal/store/migrate.go', 1, 'enum',
                        f'comment says "{m.group(1)}" ({", ".join(listed)}); the tree has '
                        f'{len(mig)} ({", ".join(mig)})'))

    # 3. the spec index against the spec directory, as a SET in both directions.
    idx = read('docs/specs/README.md')
    listed = set(re.findall(r'\| `([a-z0-9-]+\.md)`', idx))
    on_disk = {os.path.basename(p) for p in glob.glob('docs/specs/*.md')} - {'README.md'}
    for f in sorted(on_disk - listed):
        bad.append(('docs/specs/README.md', 1, 'enum', f'{f} exists but the index does not list it'))
    for f in sorted(listed - on_disk):
        bad.append(('docs/specs/README.md', 1, 'enum', f'the index lists {f}, which is not in docs/specs/'))

    # 4. FR-032 §17.3's discharge citation and counts, and §17.2's own totals against its table.
    # Both live in named functions so the fixture tests invoke the guards instead of modelling them.
    spec = 'docs/specs/func-expected-run-ledger.md'
    testfile = 'internal/store/revisiontimeline_internal_test.go'
    for tf, section, ends, row in (
            (testfile, '### 17.3', '### 17.4', '13a'),
            ('internal/store/pullcarrier4_internal_test.go', '### 17.5', '### 17.6', '10j')):
        if os.path.exists(spec) and os.path.exists(tf):
            for msg in check_fr032_discharge(read(spec), read(tf), spec, tf, section, ends, row):
                bad.append((spec, 1, 'enum', msg))
    if os.path.exists(spec):
        for msg in check_fr032_audit_totals(read(spec), spec):
            bad.append((spec, 1, 'enum', msg))
    for msg in check_iteration_finding_counts():
        bad.append(('docs/iterations/iter-0177.md', 1, 'enum', msg))
    for msg in check_iteration_finding_counts('docs/iterations/iter-0178.md'):
        bad.append(('docs/iterations/iter-0178.md', 1, 'enum', msg))
    for msg in check_iter0178_findings():
        bad.append(('docs/iterations/iter-0178.md', 1, 'enum', msg))
    # THESE THREE WERE DEAD. They were indented into the loop above, so they ran only when the
    # finding-count guard FAILED — that is, never, because it was green. The success line of this
    # script named all three by name throughout. Found while fixing a drifted count in `iter-0178`
    # (2026-09-06), which is the same class one level up: a summary claiming what the mechanism
    # beside it does not do. `check_every_guard_is_called` below now makes the omission structural
    # rather than a matter of reading the indentation.
    if os.path.exists(spec):
        migs = {p: read(p) for p in sorted(glob.glob('internal/store/migrations/*.sql'))}
        stores = {p: read(p) for p in sorted(glob.glob('internal/store/*.go'))
                  if not p.endswith('_test.go')}
        for msg in check_fr032_drain_surfaces(read(spec), migs, stores, spec):
            bad.append((spec, 1, 'enum', msg))
        for msg in check_fr032_transport_matrix(read(spec), spec):
            bad.append((spec, 1, 'enum', msg))
        for msg in check_fr032_carrier_gate_contract(read(spec), spec):
            bad.append((spec, 1, 'enum', msg))

    # 5. the Monitoring-as-Code bundle README against fileSupportedTypes.
    mac = mac_supported_types()
    mac_doc = 'docker/monitoring.d/README.md'
    if mac is None:
        bad.append(('internal/fileprovider/bundle.go', 1, 'enum',
                    'fileSupportedTypes could not be parsed; the bundle README is unguarded'))
    else:
        text = flatten(read(mac_doc))
        m = re.search(r'\*\*Supported types \((\d+)\):\*\*(.*?)(?:Credentialed|$)', text)
        if not m:
            bad.append((mac_doc, 1, 'enum', 'the "Supported types (N)" bullet is gone; the guard cannot check it'))
        else:
            named = sorted(set(re.findall(r'`([a-z_]+)`', m.group(2))) & set(monitor_type_values().values()))
            # Reported separately: a wrong COUNT beside an intact list, printed as one line, reads
            # as a contradiction and sends the reader looking for a missing entry that is present.
            if int(m.group(1)) != len(mac):
                bad.append((mac_doc, 1, 'enum',
                            f'the bullet says {int(m.group(1))} supported types; '
                            f'fileSupportedTypes admits {len(mac)}'))
            if named != mac:
                bad.append((mac_doc, 1, 'enum',
                            f'the bullet names {named}; fileSupportedTypes admits {mac} '
                            f'(missing: {sorted(set(mac) - set(named)) or "none"}, '
                            f'extra: {sorted(set(named) - set(mac)) or "none"})'))
    return bad


# A `path.go:NNN` citation, the form a living document uses when it wants a reader to open one
# exact line. The `:NNN-MMM` span form is accepted too; the first number is the one checked.
LINE_CITE_RE = re.compile(
    r'((?:internal|cmd|frontend|scripts|e2e|docker|deploy)/[A-Za-z0-9_./\-]+'
    r'\.(?:go|ts|vue|sql|py|sh)):(\d+)(?:-\d+)?')

# A line that carries no claim of its own. A citation landing on one of these is a citation whose
# code has MOVED — it is the signature drift leaves, because a shifted anchor usually falls into
# whitespace or a closing delimiter.
_EMPTY_ANCHOR = {'', '}', ')', '{', '})', '},', '),', ');', '`', '`,', '],', ']'}


def check_line_citations():
    """A `file.go:NNN` citation must land on a line that says something.

    This guard exists because FR-032's spec cited thirty-three exact lines and, by the time its
    last phase landed, thirteen of them pointed at whitespace, a closing brace or an unrelated
    statement — while this checker passed, because it validated that the PATH existed and never
    that the LINE did. Section 7.2's whole advance-rule audit was keyed to line numbers that had
    all moved.

    It is a WEAK guard and says so rather than implying more: it cannot tell that a citation landed
    on the wrong statement, only that it landed on nothing. The strong form is not to cite a line at
    all — name the function — which is what FR-032's spec now does almost everywhere. A citation
    past end-of-file is caught too, which no amount of drift makes acceptable.
    """
    bad = []
    for doc in LIVING:
        if not os.path.exists(doc):
            continue
        for n, line in enumerate(read(doc).splitlines(), 1):
            for path, num in LINE_CITE_RE.findall(line):
                if not os.path.exists(path):
                    continue  # the path guard above already reports it
                lines = read(path).splitlines()
                num = int(num)
                if num < 1 or num > len(lines):
                    bad.append((doc, n, 'line-cite',
                                '%s:%d is past end of file (%d lines)' % (path, num, len(lines))))
                    continue
                if lines[num - 1].strip() in _EMPTY_ANCHOR:
                    bad.append((doc, n, 'line-cite',
                                '%s:%d is blank or a bare delimiter — the code it named has moved'
                                % (path, num)))
    return bad


def main():
    src = source_text()
    bad = []
    for doc in LIVING:
        if not os.path.exists(doc):
            continue
        for n, line in enumerate(read(doc).splitlines(), 1):
            for raw in PATH_RE.findall(line) + LINK_RE.findall(line):
                for tok in expand(raw):
                    if tok in ALLOWED or raw in ALLOWED or resolves(tok):
                        continue
                    if excused(line, line.find(raw) + len(raw)):
                        continue
                    bad.append((doc, n, 'path', tok))
            for name in TEST_RE.findall(line):
                if name in ALLOWED:
                    continue
                # Go test, or a TS/Vue test title
                if name in test_tokens(src):
                    continue
                if excused(line, line.find(name) + len(name)):
                    continue
                bad.append((doc, n, 'test', name))
    bad += check_discharge(src)
    bad += check_row_statuses()
    bad += check_spec_banners()
    bad += check_gate_stale_spellings()
    bad += check_change_stale_spellings()
    bad += check_customer_names()
    bad += check_enumerations()
    bad += check_partial_claims()
    bad += check_line_citations()
    if not bad:
        print('docs references: OK — every path and Test* name in the living documents resolves, '
              'and every acceptance map is complete (FR-021 invariants compared as a SET against '
              'the spec, plus 24 scenarios; FR-022: 16+16, FR-023: 16+19; FR-025: §6 as a SET + 9 '
              'scenario groups, its retired spellings refused; FR-026 and FR-029: §6 as a SET); '
              'no real customer name appears in example data; '
              'every requirement row states one of the three statuses, and no spec calls itself '
              'unbuilt while its requirement is DONE; and every hand-written enumeration about the '
              'tree agrees with it — the check-type count, the column-list ON DELETE SET NULL '
              'migrations in both places that name them, the spec index as a SET, and the '
              'Monitoring-as-Code supported types, and FR-032 §17.3\'s discharge citation against '
              'the test file it discharges from, BOTH of its counts against the artefacts they '
              'summarise, §17.2\'s own totals DERIVED from its discharge table in both '
              'places that state them, and §13.0\'s rollback-and-drain rule against every '
              'V4-capable pull surface and every store function that removes one of its rows, and '
              '§16.1\'s half-deployment rows against the transports they can actually arise in, and the '
              '`ledger.carrier_enabled` phase contract naming what B1 does with `true` '
              '— the check types and the bundle types as SETS '
              'through an asserted label map, not by count alone; and no document announces a '
              '`PARTIAL` residual its own discharge map does not have; and every file.go:NNN '
              'citation lands on a line that exists and is not bare whitespace or a '
              'closing delimiter — the signature drift leaves when the code it named moved)')
        return 0
    print(f'docs references: {len(bad)} unresolved citation(s) in living documents\n')
    for doc, n, kind, tok in bad:
        print(f'  {doc}:{n}  [{kind}]  {tok}')
    print('\nEach is a claim a reader cannot follow: fix the citation, or add it to ALLOWED with a reason.')
    return 1

if __name__ == '__main__':
    sys.exit(main())
