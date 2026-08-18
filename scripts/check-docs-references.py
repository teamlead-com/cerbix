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
import os, re, sys, glob, itertools

LIVING = ['docs/status.md', 'docs/traceability.md', 'docs/overview.md', 'docs/runbook.md',
          'docs/project-description.md', 'README.md', 'CLAUDE.md'] + sorted(glob.glob('docs/specs/*.md'))

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


def check_discharge(src):
    """FR-021 states 91 acceptance invariants and 24 required scenarios; FR-022 states 16. Every one
    must have a row,
    and every row must name a test that exists or an INSPECTION: reason — that is what makes "done"
    a checkable claim instead of a memory of thirty iteration reports."""
    bad = []
    text = open(DISCHARGE_DOC, encoding='utf-8').read()
    for heading, count, label in ((INV_HEADING, 91, 'invariant'), (MATRIX_HEADING, 24, 'scenario'),
                                  (FR022_HEADING, 16, 'FR-022 invariant'),
                                  (FR022_MATRIX_HEADING, 16, 'FR-022 scenario')):
        rows = discharge_rows(text, heading)
        if rows is None:
            bad.append((DISCHARGE_DOC, 0, 'discharge', f'the {label} table is missing entirely'))
            continue
        for n in range(1, count + 1):
            cell = rows.get(n)
            if cell is None:
                bad.append((DISCHARGE_DOC, 0, 'discharge', f'{label} {n} has no row'))
                continue
            names = re.findall(r'`(Test[A-Za-z0-9_]+)`', cell)
            if names:
                for name in names:
                    if not (re.search(r'\bfunc\s+' + re.escape(name) + r'\b', src) or name in src):
                        bad.append((DISCHARGE_DOC, 0, 'discharge', f'{label} {n} cites missing {name}'))
            elif 'INSPECTION:' not in cell and 'spec.ts' not in cell:
                bad.append((DISCHARGE_DOC, 0, 'discharge',
                            f'{label} {n} names neither a test nor an INSPECTION: reason'))
    return bad


def main():
    src = source_text()
    bad = []
    for doc in LIVING:
        if not os.path.exists(doc):
            continue
        for n, line in enumerate(open(doc, encoding='utf-8'), 1):
            for raw in PATH_RE.findall(line):
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
    if not bad:
        print('docs references: OK — every path and Test* name in the living documents resolves, '
              'and all 91 FR-021 invariants + 24 scenarios + 16 FR-022 invariants + 16 FR-022 scenarios are discharged; '
              'every requirement row states one of the three statuses')
        return 0
    print(f'docs references: {len(bad)} unresolved citation(s) in living documents\n')
    for doc, n, kind, tok in bad:
        print(f'  {doc}:{n}  [{kind}]  {tok}')
    print('\nEach is a claim a reader cannot follow: fix the citation, or add it to ALLOWED with a reason.')
    return 1

if __name__ == '__main__':
    sys.exit(main())
