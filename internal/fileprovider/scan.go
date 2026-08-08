package fileprovider

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teamlead-com/cerbix/internal/config"
)

// Candidate is one eligible bundle file read from a provider directory.
type Candidate struct {
	Path    string // absolute
	RelPath string // tenant-safe path relative to the provider root (never absolute)
	Data    []byte
}

// ScanError is a bounded per-file rejection (over-size, bad symlink, decode/scope failure).
// RelPath is tenant-safe; Err carries the bounded reason (no raw YAML, no secrets).
type ScanError struct {
	RelPath string
	Err     *BundleError
}

// ScanDirectory enumerates the eligible bundle files that are regular immediate children of
// dir ending in .yaml/.yml (spec §11): non-recursive; dotfiles, subdirectories, and special
// files are ignored; a symlink is followed only when its canonical target is a regular file
// INSIDE the canonical root (escape rejects). All resource bounds are enforced
// (max_files, max_file_bytes, max_total_bytes); a violation rejects that file with a bounded
// diagnostic and never partially accepts it. A returned top-level error means the directory
// itself is unreadable — the caller keeps last-known-good and marks the provider degraded,
// it is NOT desired deletion.
func ScanDirectory(dir string, limits config.ProviderLimits) ([]Candidate, []ScanError, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	// Deterministic order so scans are reproducible across replicas.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var (
		cands   []Candidate
		errs    []ScanError
		total   int64
		fileCnt int
	)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // dotfiles / editor temporaries
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".yaml" && ext != ".yml" {
			continue
		}
		full := filepath.Join(dir, name)
		// Resolve symlinks and reject anything that is not a regular file inside the root.
		realPath, rerr := filepath.EvalSymlinks(full)
		if rerr != nil {
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "unreadable path"}})
			continue
		}
		if !withinRoot(root, realPath) {
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "symlink target escapes the provider root"}})
			continue
		}
		info, ierr := os.Stat(realPath)
		if ierr != nil || !info.Mode().IsRegular() {
			// subdirectories (non-recursive), sockets, devices, FIFOs
			continue
		}
		fileCnt++
		if fileCnt > limits.MaxFiles {
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "provider file count exceeds max_files"}})
			continue
		}
		if info.Size() > limits.MaxFileBytes {
			// Cheap early reject on the stat size; the bounded read below is authoritative.
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "file exceeds max_file_bytes"}})
			continue
		}
		// Read through a bounded reader so an oversized file (grown between stat and read, TOCTOU)
		// is NEVER fully loaded into memory: cap = min(max_file_bytes, remaining total budget),
		// read one extra byte to detect overflow.
		capBytes := limits.MaxFileBytes
		if rem := limits.MaxTotalBytes - total; rem < capBytes {
			capBytes = rem
		}
		if capBytes < 0 {
			capBytes = 0
		}
		f, oerr := os.Open(realPath)
		if oerr != nil {
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "unreadable file"}})
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(f, capBytes+1))
		_ = f.Close()
		if rerr != nil {
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "unreadable file"}})
			continue
		}
		if int64(len(data)) > capBytes {
			// Hit the cap+1 → the file exceeds its per-file limit or the remaining total budget.
			msg := "file exceeds max_file_bytes"
			if capBytes < limits.MaxFileBytes {
				msg = "provider total bytes exceeds max_total_bytes"
			}
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: msg}})
			continue
		}
		total += int64(len(data))
		cands = append(cands, Candidate{Path: full, RelPath: name, Data: data})
	}
	return cands, errs, nil
}

// GroupResult is the outcome of decoding + grouping a provider snapshot.
type GroupResult struct {
	// Valid maps "org/project" → the decoded bundle for projects with exactly one candidate.
	Valid map[string]*DesiredProject
	// Paths maps "org/project" → the tenant-safe relative source path (for provenance).
	Paths map[string]string
	// Frozen holds tenant keys that resolved to a tenant but were REJECTED (a duplicate-target
	// project). Such a project keeps its last-known-good and must NOT be orphaned this scan —
	// it is neither applied (out of Valid) nor treated as absent (spec §6/§9.1). Distinct from
	// SuspendOrphan, which is provider-wide.
	Frozen map[string]bool
	// Errors are bounded per-file rejections (decode/scope/duplicate).
	Errors []ScanError
	// SuspendOrphan is set when a file could not be bound to a tenant (unbound_error): a
	// broken/half-written file must not make a previously managed project look absent, so the
	// caller applies non-destructive plans only and skips ALL orphan processing this scan
	// (spec §9.1).
	SuspendOrphan bool
}

// GroupBundles decodes each candidate under the provider scope and groups them by resolved
// tenant. A project declared by more than one file rejects ALL its candidates and freezes
// that project's last-known-good (spec §6). A file that cannot be bound to a tenant
// (decode/scope failure) is an unbound_error that suspends orphaning provider-wide (§9.1);
// independently valid bundles still group for a non-destructive apply.
func GroupBundles(cands []Candidate, scope config.ProviderScopeConfig) GroupResult {
	res := GroupResult{Valid: map[string]*DesiredProject{}, Paths: map[string]string{}, Frozen: map[string]bool{}}
	seen := map[string]int{}                // tenant key → candidate count
	firstPath := map[string]string{}        // tenant key → first path (for dup diagnostics)
	decoded := map[string]*DesiredProject{} // tenant key → decoded bundle

	for _, c := range cands {
		dp, err := Decode(c.Data, scope)
		if err != nil {
			be, ok := err.(*BundleError)
			if !ok {
				be = &BundleError{Reason: ReasonInvalidFormat, Msg: "invalid bundle"}
			}
			res.Errors = append(res.Errors, ScanError{RelPath: c.RelPath, Err: be})
			// A file we cannot bind to a tenant freezes orphaning (ambiguous absence).
			res.SuspendOrphan = true
			continue
		}
		key := dp.Organization + "/" + dp.Project
		seen[key]++
		if seen[key] == 1 {
			decoded[key] = dp
			firstPath[key] = c.RelPath
			res.Paths[key] = c.RelPath
		} else {
			res.Errors = append(res.Errors, ScanError{RelPath: c.RelPath, Err: &BundleError{Reason: ReasonDuplicateProject, Msg: "project " + key + " is declared by more than one file"}})
		}
	}
	for key, n := range seen {
		if n == 1 {
			res.Valid[key] = decoded[key]
		} else {
			// Duplicate target: reject BOTH competitors and FREEZE this project — drop it from
			// Valid AND mark it Frozen so the reconcile loop keeps its last-known-good instead
			// of reading it as absent and orphaning it (spec §6/§9.1). This is a per-project
			// freeze, not a provider-wide orphan suspension.
			delete(res.Valid, key)
			res.Frozen[key] = true
			res.Errors = append(res.Errors, ScanError{RelPath: firstPath[key], Err: &BundleError{Reason: ReasonDuplicateProject, Msg: "project " + key + " is declared by more than one file"}})
		}
	}
	return res
}

// withinRoot reports whether target is root itself or a descendant of it (both canonical).
func withinRoot(root, target string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
