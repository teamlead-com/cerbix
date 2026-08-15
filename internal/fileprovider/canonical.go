package fileprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// The monitor SLUG is deliberately absent from this projection. It is immutable, so it
// cannot move without the apply first refusing the change outright — and including it would
// make the first bundle that merely spells out an already-derived `slug:` line look like a
// semantic update, calling the update path for a file edit that changed nothing.
//
// canonicalMonitor is the stable projection hashed for create/update/no-op decisions
// (spec §7). It excludes YAML comments/order/path/mtime, server-owned runtime fields, push
// tokens, and generation timestamps. Set-like values (tags, depends_on) are sorted+deduped;
// order-sensitive conditions retain order. The UID is included because identity binds the
// hash to a specific managed resource.
type canonicalMonitor struct {
	UID              string            `json:"uid"`
	Name             string            `json:"name"`
	Type             string            `json:"type"`
	Target           string            `json:"target"`
	Method           string            `json:"method"`
	IntervalSeconds  int               `json:"interval_seconds"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	Retries          int               `json:"retries"`
	FailureThreshold int               `json:"failure_threshold"`
	ConfirmInterval  int               `json:"confirm_interval_seconds"`
	Renotify         int               `json:"renotify_seconds"`
	Grace            int               `json:"grace_seconds"`
	Region           string            `json:"region"`
	Enabled          bool              `json:"enabled"`
	AutoIncident     bool              `json:"auto_incident"`
	Conditions       []string          `json:"conditions"` // order-preserved
	Tags             []string          `json:"tags"`       // sorted+deduped
	Config           map[string]string `json:"config,omitempty"`
	// NOTE: depends_on is deliberately EXCLUDED from the semantic hash. Dependency edges are
	// a separate reconcile axis (dependency_update) that does NOT bump execution_revision
	// (D-0142); the planner compares dependency sets on their own. Folding them into the hash
	// would misclassify a dep-only change as a semantic update.
}

// canonicalHash computes the semantic hash of a normalized monitor. Because
// json.Marshal emits map keys in sorted order and the set-like slices are pre-normalized,
// the encoding is deterministic across parses of semantically-equal YAML.
func canonicalHash(uid string, m domain.Monitor) string {
	cm := canonicalMonitor{
		UID:              uid,
		Name:             m.Name,
		Type:             string(m.Type),
		Target:           m.Target,
		Method:           m.Method,
		IntervalSeconds:  m.IntervalSeconds,
		TimeoutSeconds:   m.TimeoutSeconds,
		Retries:          m.Retries,
		FailureThreshold: m.FailureThreshold,
		ConfirmInterval:  m.ConfirmIntervalSeconds,
		Renotify:         m.RenotifySeconds,
		Grace:            m.GraceSeconds,
		Region:           m.Region,
		Enabled:          m.Enabled,
		AutoIncident:     m.AutoIncident,
		Conditions:       append([]string(nil), m.Conditions...),
		Tags:             normStringSet(m.Tags),
		Config:           m.Config,
	}
	b, err := json.Marshal(cm)
	if err != nil {
		// A normalized monitor always marshals; fall back to a stable non-empty value so a
		// marshal bug can never make two monitors hash-equal.
		sum := sha256.Sum256([]byte("canonical-marshal-error:" + uid))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sortedUIDs returns the bundle's monitor UIDs in deterministic order (plan/lock ordering).
func sortedUIDs(dp *DesiredProject) []string {
	uids := make([]string, 0, len(dp.Monitors))
	for uid := range dp.Monitors {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
