package fileprovider

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teamlead-com/cerbix/internal/config"
)

// maxDirEntries bounds how many directory entries one scan/fingerprint will read, so a
// pathologically huge provider directory cannot exhaust memory/CPU before max_files is even
// applied. The provider directory is operator-managed (a mount/ConfigMap/git-sync checkout), so
// this is a robustness backstop, not a security boundary; hitting it is a misconfiguration and
// makes the scan ambiguous (last-known-good is kept, nothing is orphaned).
const maxDirEntries = 50000

// ReadDirBounded reads up to maxDirEntries directory entries in batches (via os.File.ReadDir),
// returning truncated=true if the directory has more — it stops reading rather than
// materializing the entire listing. Entries are in directory order (callers sort as needed).
func ReadDirBounded(dir string) (entries []os.DirEntry, truncated bool, err error) {
	return readDirBoundedN(dir, maxDirEntries)
}

// readDirBoundedN is ReadDirBounded with an injectable cap (for tests).
func readDirBoundedN(dir string, max int) (entries []os.DirEntry, truncated bool, err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, false, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	for {
		batch, berr := f.ReadDir(512)
		entries = append(entries, batch...)
		if len(entries) > max {
			return entries[:max], true, nil
		}
		if berr == io.EOF {
			return entries, false, nil
		}
		if berr != nil {
			return nil, false, berr
		}
	}
}

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
	entries, truncated, err := ReadDirBounded(dir)
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
	if truncated {
		// A pathologically huge directory: enumeration was bounded before max_files could even
		// be applied. Treat it as an ambiguous scan (a scan-level rejection → SuspendOrphan) so
		// nothing is orphaned on a listing we could not fully read (§9.1/§17).
		errs = append(errs, ScanError{RelPath: "", Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "provider directory entry count exceeds bound"}})
	}
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
		data, over, rerr := readBounded(f, capBytes)
		_ = f.Close()
		if rerr != nil {
			errs = append(errs, ScanError{RelPath: name, Err: &BundleError{Reason: ReasonInvalidFormat, Msg: "unreadable file"}})
			continue
		}
		if over {
			// Exceeded the per-file limit or the remaining total budget (never fully loaded).
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
	// Frozen maps tenant keys that resolved to a tenant but were REJECTED (a duplicate-target
	// project, OR a bundle that bound to a tenant but failed a monitor-level/dependency check)
	// to the reject reason. Such a project keeps its last-known-good and must NOT be orphaned
	// this scan — it is neither applied (out of Valid) nor treated as absent (spec §6/§9.1).
	// Distinct from SuspendOrphan, which is provider-wide. The value is the reason so the
	// per-project diagnostic reports the real cause, not always "duplicate".
	Frozen map[string]Reason
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
	res := GroupResult{Valid: map[string]*DesiredProject{}, Paths: map[string]string{}, Frozen: map[string]Reason{}}

	// A claim is every DECLARATION of a tenant key this scan — from a valid decode OR a
	// bound-invalid decode. A bound-invalid file still CLAIMS its project, so duplicate
	// detection (§6) must count it too: otherwise a bound-invalid file + a valid file both
	// targeting one project would let the valid one silently apply, missing the collision.
	type claim struct {
		valid         int             // # of valid decodes claiming this key
		invalid       int             // # of bound-invalid decodes claiming this key
		decoded       *DesiredProject // the first valid decode (applied only if it wins uniquely)
		invalidReason Reason          // a bound-invalid reason (used only for a lone bound-invalid)
		validPaths    []string        // paths of the VALID decodes (bound-invalid ones self-report)
	}
	claims := map[string]*claim{}
	order := make([]string, 0, len(cands)) // keys in first-seen order → deterministic output

	note := func(key, path string) *claim {
		cl, ok := claims[key]
		if !ok {
			cl = &claim{}
			claims[key] = cl
			order = append(order, key)
		}
		// Provenance is the FIRST path seen for the key (valid or bound-invalid).
		if _, seenPath := res.Paths[key]; !seenPath {
			res.Paths[key] = path
		}
		return cl
	}

	for _, c := range cands {
		dp, err := Decode(c.Data, scope)
		if err != nil {
			be, ok := err.(*BundleError)
			if !ok {
				be = &BundleError{Reason: ReasonInvalidFormat, Msg: "invalid bundle"}
			}
			res.Errors = append(res.Errors, ScanError{RelPath: c.RelPath, Err: be})
			if be.Org != "" && be.Project != "" {
				// The tenant resolved but the bundle is invalid (a monitor-level/dependency
				// error): it still DECLARES this project. Count it as a claim so a competing valid
				// file is detected as a duplicate (§6), and if it stands alone freeze just this
				// project — keep its last-known-good, don't orphan it — instead of suspending
				// orphaning provider-wide (§9.1).
				cl := note(be.Org+"/"+be.Project, c.RelPath)
				cl.invalid++
				if cl.invalidReason == "" {
					cl.invalidReason = be.Reason
				}
			} else {
				// Unbound (format/scope/tenant error): cannot attribute to a project, so absence
				// is ambiguous → suspend orphaning provider-wide.
				res.SuspendOrphan = true
			}
			continue
		}
		cl := note(dp.Organization+"/"+dp.Project, c.RelPath)
		cl.valid++
		cl.validPaths = append(cl.validPaths, c.RelPath)
		if cl.decoded == nil {
			cl.decoded = dp
		}
	}

	for _, key := range order {
		cl := claims[key]
		switch {
		case cl.valid+cl.invalid > 1:
			// Duplicate by claim: MORE THAN ONE declaration for this project (any mix of valid
			// and bound-invalid) rejects them all and FREEZES the project — never partially
			// applied, and kept out of orphaning so the reconcile keeps its last-known-good
			// (spec §6/§9.1). A per-project freeze, not a provider-wide orphan suspension.
			res.Frozen[key] = ReasonDuplicateProject
			// Emit a duplicate error for EVERY valid competing path (not just the first), so each
			// rejected file's bundle row is marked — a bound-invalid competitor already reported
			// its own error above. Safety (path freeze) does not depend on this cardinality; it is
			// for diagnostic completeness (§15). Fall back to the provenance path if a duplicate
			// somehow has no valid path (all competitors bound-invalid).
			dupPaths := cl.validPaths
			if len(dupPaths) == 0 {
				dupPaths = []string{res.Paths[key]}
			}
			for _, pth := range dupPaths {
				res.Errors = append(res.Errors, ScanError{RelPath: pth, Err: &BundleError{Reason: ReasonDuplicateProject, Msg: "project " + key + " is declared by more than one file"}})
			}
		case cl.valid == 1:
			// Exactly one valid declaration, no other claim → applies.
			res.Valid[key] = cl.decoded
		default:
			// Exactly one bound-invalid declaration (0 valid) → freeze with its real reason.
			res.Frozen[key] = cl.invalidReason
		}
	}
	return res
}

// readBounded reads at most capBytes+1 bytes from r and reports whether the source exceeded
// capBytes. It never allocates more than capBytes+1, so an oversized input (e.g. a file that
// grew between stat and read — TOCTOU) is never fully loaded: the memory-safe byte bound.
func readBounded(r io.Reader, capBytes int64) (data []byte, overflow bool, err error) {
	if capBytes < 0 {
		capBytes = 0
	}
	data, err = io.ReadAll(io.LimitReader(r, capBytes+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) > capBytes, nil
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
