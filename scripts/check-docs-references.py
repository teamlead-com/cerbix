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
Iteration reports (`docs/iterations/*.md`), review snapshots (`docs/checks/*`) and
`docs/decisions.md` are IMMUTABLE or historical by contract: they record what was true when they
were written, and a rename afterwards does not make them wrong. They are deliberately not checked.

WHAT IS CHECKED IN THEM:
  * backticked repo paths (`internal/...go`, `frontend/src/...vue`, `migrations/000NN_*.sql`, ...)
  * backticked `Test*` identifiers, which must exist as a Go `func Test...` or a TS/Vue test name

Brace shorthand is expanded: `store/{users,sessions}.go` checks both files.
Anything intentionally unresolvable lives in ALLOWED below, each with a reason.
"""
import glob
import os, re, sys, glob, itertools

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
            parts.append(open(f, encoding='utf-8', errors='ignore').read())
    return '\n'.join(parts)

DISCHARGE_DOC = 'docs/traceability.md'
INV_HEADING = '### Invariants (§19 for 1–74, §16.8 for 75–91)'
MATRIX_HEADING = '### Required test matrix (§16.10, written before the phase-5 code)'
FR022_HEADING = '### FR-022 invariants (§6 of func-service-incidents.md)'
FR022_MATRIX_HEADING = '### FR-022 required test matrix (§7, written before the code)'
FR023_HEADING = '### FR-023 invariants (§6 of func-service-escalation.md)'
FR023_MATRIX_HEADING = '### FR-023 required test matrix (§7, written before the code)'


def discharge_rows(text, heading):
    """Rows of the numbered table that follows `heading`, as {number: discharge cell}."""
    i = text.find(heading)
    if i < 0:
        return None
    out = {}
    for line in text[i:].split('\n')[1:]:
        if line.startswith('### ') or line.startswith('## '):
            break
        cells = [c.strip() for c in line.split('|')]
        if len(cells) < 5 or not cells[1].isdigit():
            continue
        out[int(cells[1])] = cells[3]
    return out


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
        for n, line in enumerate(open(doc, encoding='utf-8'), 1):
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
            lines = open(path, encoding='utf-8').read().split('\n')
        except FileNotFoundError:
            continue
        bad += gate_stale_findings(path, lines)
    try:
        text = open(GATE_SPEC, encoding='utf-8').read()
    except FileNotFoundError:
        return bad
    for h in gate_duplicate_headers(text):
        bad.append((GATE_SPEC, 0, 'stale', f'schema table {h} declared more than once'))
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
    for line in open(status_path, encoding='utf-8'):
        cells = split_row(line.rstrip('\n'))
        if len(cells) < 5:
            continue
        req = cells[1].strip()
        if REQ_RE.fullmatch(req) and cells[3].strip() == 'DONE':
            done.add(req)

    bad = []
    for spec in sorted(glob.glob('docs/specs/*.md')):
        lines = open(spec, encoding='utf-8').read().splitlines()[:BANNER_LINES]
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
    text = open('docs/specs/func-service-reliability.md', encoding='utf-8').read()
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


def check_invariant_set(src, text, expected):
    """The FR-021 invariant table's keys must EQUAL the spec's numbers — both directions.

    Contiguity is checked too, because these are written as a numbered list and a hole in it is a
    typo rather than a decision. Say which numbers, not merely that the counts differ: the point of
    the map is that a reader can follow it."""
    bad = []
    rows = discharge_rows(text, INV_HEADING)
    if rows is None:
        return [(DISCHARGE_DOC, 0, 'discharge', 'the invariant table is missing entirely')]
    holes = sorted(set(range(1, max(expected) + 1)) - expected)
    if holes:
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'the FR-021 spec skips invariant number(s) {holes} — a numbered list with a '
                    f'hole is a typo, and the gate cannot tell it from a deletion'))
    for n in sorted(expected - set(rows)):
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'invariant {n} is stated in the spec and has no discharge row'))
    for n in sorted(set(rows) - expected):
        bad.append((DISCHARGE_DOC, 0, 'discharge',
                    f'discharge row {n} names an invariant the FR-021 spec does not state'))
    for n in sorted(expected & set(rows)):
        bad += discharge_row_evidence(src, rows[n], n, 'invariant')
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
    text = open(DISCHARGE_DOC, encoding='utf-8').read()
    # The FR-021 invariant count is READ from the spec, not written here. Hard-coding 91 meant a
    # discharge row above that number was neither required nor checked, so twelve invariants existed
    # only in the map — a claim nobody had made in the requirement it was being checked against.
    fr021 = fr021_invariant_numbers()
    # The FR-021 invariants are compared as a SET; the other tables are still contiguous 1..N.
    bad += check_invariant_set(src, text, fr021)
    for heading, count, label in ((MATRIX_HEADING, 24, 'scenario'),
                                  (FR022_HEADING, 16, 'FR-022 invariant'),
                                  (FR022_MATRIX_HEADING, 16, 'FR-022 scenario'),
                                  (FR023_HEADING, 16, 'FR-023 invariant'),
                                  (FR023_MATRIX_HEADING, 19, 'FR-023 scenario')):
        rows = discharge_rows(text, heading)
        if rows is None:
            bad.append((DISCHARGE_DOC, 0, 'discharge', f'the {label} table is missing entirely'))
            continue
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
        for n, line in enumerate(open(doc, encoding='utf-8'), 1):
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
    if not bad:
        print('docs references: OK — every path and Test* name in the living documents resolves, '
              'and every acceptance map is complete (FR-021 invariants compared as a SET against '
              'the spec, plus 24 scenarios; FR-022: 16+16, FR-023: 16+19); '
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
