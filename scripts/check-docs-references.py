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
LIVING = ['docs/status.md', 'docs/traceability.md', 'docs/overview.md', 'docs/runbook.md',
          'docs/project-description.md', 'README.md', 'CLAUDE.md',
          'CHANGELOG.md'] + sorted(glob.glob('docs/specs/*.md'))

ALLOWED = {
    'ActionServiceRead':  'a name the spec says must NOT exist (§4 "Service is not a security boundary")',
    'ActionServiceWrite': 'a name the spec says must NOT exist (§4 "Service is not a security boundary")',
    'docs/alerts.yaml':   'named by AGENTS as a long-lived artifact; not created yet — alert rules live in deploy/alerts/',
    'config.yaml':        'generic filename in prose, not a repo path',
    'docs/iterations/iter-NNNN.md': 'a template name, not a file',
    'docs/iterations/iter-XXXX.md': 'a template name, not a file',
    '000NN_project_secrets.sql':    'a placeholder in a spec written before the number was assigned',
    'schema.sql': 'generic filename in prose',
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

def resolves(tok):
    if os.path.exists(tok):
        return True
    # docs cite paths by suffix ("store/heartbeats.go", "migrations/00039_x.sql")
    return bool(glob.glob('**/' + tok, recursive=True))

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


def fr025_invariant_numbers():
    """The SET of invariant numbers §6 of the FR-025 spec states — FR-021's discipline: a set, not a
    maximum, a renamed section a loud failure, a duplicate number a loud failure."""
    text = read(FR025_SPEC)
    i = text.find('\n## 6.')
    if i < 0:
        raise SystemExit(f'check-docs-references: {FR025_SPEC} has no "## 6." section; the FR-025 '
                         'invariant gate has nothing to compare the discharge map against')
    section = text[i + 1:]
    end = re.search(r'\n## ', section)
    if end:
        section = section[:end.start()]
    seen = [int(m) for m in re.findall(r'^\s{0,4}(\d{1,3})\.\s', section, re.M)]
    if not seen:
        raise SystemExit('check-docs-references: the FR-025 spec states no invariants at all')
    dupes = sorted({n for n in seen if seen.count(n) > 1})
    if dupes:
        raise SystemExit('check-docs-references: the FR-025 spec states invariant number(s) '
                         f'{dupes} more than once')
    return set(seen)


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
            if not (re.search(r'\bfunc\s+' + re.escape(name) + r'\b', src) or name in src):
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
                if re.search(r'\bfunc\s+' + re.escape(name) + r'\b', src) or name in src:
                    continue
                if excused(line, line.find(name) + len(name)):
                    continue
                bad.append((doc, n, 'test', name))
    bad += check_discharge(src)
    bad += check_row_statuses()
    bad += check_spec_banners()
    bad += check_gate_stale_spellings()
    bad += check_change_stale_spellings()
    if not bad:
        print('docs references: OK — every path and Test* name in the living documents resolves, '
              'and every acceptance map is complete (FR-021 invariants compared as a SET against '
              'the spec, plus 24 scenarios; FR-022: 16+16, FR-023: 16+19; FR-025: §6 as a SET + 9 '
              'scenario groups, its retired spellings refused); '
              'every requirement row states one of the three statuses, and no spec calls itself '
              'unbuilt while its requirement is DONE')
        return 0
    print(f'docs references: {len(bad)} unresolved citation(s) in living documents\n')
    for doc, n, kind, tok in bad:
        print(f'  {doc}:{n}  [{kind}]  {tok}')
    print('\nEach is a claim a reader cannot follow: fix the citation, or add it to ALLOWED with a reason.')
    return 1

if __name__ == '__main__':
    sys.exit(main())
