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

GONE_RE = re.compile(r'\b(deleted|removed|retired|renamed|replaced|superseded|never existed|dropped)\b', re.I)

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
    if not bad:
        print('docs references: OK — every path and Test* name in the living documents resolves')
        return 0
    print(f'docs references: {len(bad)} unresolved citation(s) in living documents\n')
    for doc, n, kind, tok in bad:
        print(f'  {doc}:{n}  [{kind}]  {tok}')
    print('\nEach is a claim a reader cannot follow: fix the citation, or add it to ALLOWED with a reason.')
    return 1

if __name__ == '__main__':
    sys.exit(main())
