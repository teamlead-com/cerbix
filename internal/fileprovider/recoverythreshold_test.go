package fileprovider

import (
	"errors"
	"strings"
	"testing"
)

// FR-043 in Monitoring-as-Code (func-recovery-threshold.md §4.5, I10, I15).
//
// The file provider's raw monitor carries `recovery_threshold` as an optional integer, and
// `applyCommonMonitorFields` is its transport decoder: absent is 1, an explicit value — `0`
// included — is copied verbatim and left to `Monitor.Validate`, which is the one owner of the rule.
// The canonical form carries the field only above 1, so no bundle written before the field existed
// changes its spec_hash at upgrade.

// rtBundle is one http monitor in the given bundle format, with `extra` appended to its block.
func rtBundle(format, extra string) string {
	return "format: " + format + "\norganization: acme\nproject: payments\nmonitors:\n  api:\n" +
		"    name: Payments API\n    type: http\n    target: https://payments.internal/health\n" +
		"    interval: 30s\n    timeout: 5s\n" + extra
}

// rtPushBundle is one push monitor in the given bundle format, with `extra` appended to its block.
func rtPushBundle(format, extra string) string {
	return "format: " + format + "\norganization: acme\nproject: payments\nmonitors:\n  api:\n" +
		"    name: Payments API\n    type: push\n    interval: 5m\n    grace: 1m\n" + extra
}

// The spec_hash these exact bundles had BEFORE FR-043. Captured by decoding them on a tree exported
// from commit b823aca (the last commit without any FR-043 code; `internal/fileprovider` is unchanged
// between it and the FR-043 scaffold) with a throwaway test that printed `Monitors["api"].Hash`.
// They are literals on purpose: a hash recomputed by the code under test would agree with itself
// whatever the canonical form did, and I15 is a promise about the bytes already stored in
// `spec_hash` columns. The format does not enter the hash, so format 1 and 2 share each value.
const (
	preChangeHTTPHash = "55c6e718f89d04243c2891c19fc9c9f771617f9d73b75c69ed3d615ffce424ac"
	preChangePushHash = "20539fd949c4168a4472b6ce7d8a1d35d382e20c08460c806ed2690173ecf0fc"
)

func TestRecoveryThresholdIsDecodedInEveryBundleFormat(t *testing.T) {
	for _, format := range []string{"1", "2"} {
		t.Run("format "+format, func(t *testing.T) {
			m := mustDecode(t, rtBundle(format, "    recovery_threshold: 3\n")).Monitors["api"].Monitor
			if m.RecoveryThreshold != 3 {
				t.Fatalf("recovery_threshold = %d, want 3 — the bundle declared it", m.RecoveryThreshold)
			}
			m = mustDecode(t, rtBundle(format, "    recovery_threshold: 10\n")).Monitors["api"].Monitor
			if m.RecoveryThreshold != 10 {
				t.Fatalf("recovery_threshold = %d, want 10 (the top of the range)", m.RecoveryThreshold)
			}
		})
	}
}

func TestAbsentRecoveryThresholdDecodesToOne(t *testing.T) {
	for _, format := range []string{"1", "2"} {
		t.Run("format "+format, func(t *testing.T) {
			m := mustDecode(t, rtBundle(format, "")).Monitors["api"].Monitor
			if m.RecoveryThreshold != 1 {
				t.Fatalf("recovery_threshold = %d, want 1 — an absent key is the contract default", m.RecoveryThreshold)
			}
			p := mustDecode(t, rtPushBundle(format, "")).Monitors["api"].Monitor
			if p.RecoveryThreshold != 1 {
				t.Fatalf("push recovery_threshold = %d, want 1", p.RecoveryThreshold)
			}
		})
	}
}

// I10: out of range is a bundle validation error, and an explicit 0 is refused, not defaulted — the
// decoder copies it and Validate refuses it, so nothing between the YAML and the rule clamps.
func TestRecoveryThresholdOutsideTheRangeIsABundleError(t *testing.T) {
	for _, v := range []string{"0", "11", "-1"} {
		t.Run(v, func(t *testing.T) {
			_, err := Decode([]byte(rtBundle("2", "    recovery_threshold: "+v+"\n")), instanceScope())
			assertRecoveryThresholdRefused(t, err)
		})
	}
}

// D8 in a bundle: push only, since composite is refused as an unsupported type before any field is
// read. Above 1 is refused, not silently reset the way `failure_threshold` is for push.
func TestRecoveryThresholdAboveOneOnPushIsABundleError(t *testing.T) {
	for _, v := range []string{"2", "10"} {
		t.Run(v, func(t *testing.T) {
			_, err := Decode([]byte(rtPushBundle("2", "    recovery_threshold: "+v+"\n")), instanceScope())
			assertRecoveryThresholdRefused(t, err)
		})
	}
	m := mustDecode(t, rtPushBundle("2", "    recovery_threshold: 1\n")).Monitors["api"].Monitor
	if m.RecoveryThreshold != 1 {
		t.Fatalf("push with an explicit 1: recovery_threshold = %d, want 1", m.RecoveryThreshold)
	}
}

func assertRecoveryThresholdRefused(t *testing.T, err error) {
	t.Helper()
	var be *BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want a *BundleError, got %v", err)
	}
	if be.Reason != ReasonDomainInvalid {
		t.Fatalf("reason = %s, want %s (%s)", be.Reason, ReasonDomainInvalid, be.Msg)
	}
	if !strings.Contains(be.Msg, "recovery_threshold") {
		t.Fatalf("message %q does not name recovery_threshold", be.Msg)
	}
}

// I15: a bundle that omits the key, and one that declares 1, keep the spec_hash they had before the
// field existed — byte for byte — so the first reconcile after the upgrade plans no update for a
// monitor whose YAML did not change.
func TestRecoveryThresholdOfOneKeepsThePreChangeSpecHash(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"http, format 1, absent", rtBundle("1", ""), preChangeHTTPHash},
		{"http, format 2, absent", rtBundle("2", ""), preChangeHTTPHash},
		{"http, format 1, declared 1", rtBundle("1", "    recovery_threshold: 1\n"), preChangeHTTPHash},
		{"http, format 2, declared 1", rtBundle("2", "    recovery_threshold: 1\n"), preChangeHTTPHash},
		{"push, format 2, absent", rtPushBundle("2", ""), preChangePushHash},
		{"push, format 2, declared 1", rtPushBundle("2", "    recovery_threshold: 1\n"), preChangePushHash},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustDecode(t, c.yaml).Monitors["api"].Hash; got != c.want {
				t.Fatalf("spec_hash = %s, want the pre-change %s — every file-managed monitor would be "+
					"planned as an update at upgrade without one line of YAML having changed", got, c.want)
			}
		})
	}
}

// I15, the other half: a change between 1 and a value above it is drift, in both directions, and so
// is a change between two values above 1.
func TestRecoveryThresholdChangeIsDriftInBothDirections(t *testing.T) {
	one := rtBundle("2", "    recovery_threshold: 1\n")
	three := rtBundle("2", "    recovery_threshold: 3\n")
	five := rtBundle("2", "    recovery_threshold: 5\n")
	hashOf := func(y string) string { return mustDecode(t, y).Monitors["api"].Hash }

	steps := []struct {
		name, from, to string
	}{
		{"1 to 3", one, three},
		{"3 to 1", three, one},
		{"absent to 3", rtBundle("2", ""), three},
		{"3 to absent", three, rtBundle("2", "")},
		{"3 to 5", three, five},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			cur := []CurrentMonitor{{UID: "api", Type: "http", Hash: hashOf(s.from)}}
			p := planFor(t, s.to, cur)
			if got := actionOf(p, "api"); got != ActionUpdate {
				t.Fatalf("plan action = %s, want %s — the threshold change is invisible to the planner", got, ActionUpdate)
			}
		})
	}
	// And the value that changes nothing plans nothing.
	cur := []CurrentMonitor{{UID: "api", Type: "http", Hash: hashOf(rtBundle("2", ""))}}
	if got := actionOf(planFor(t, one, cur), "api"); got != ActionNoop {
		t.Fatalf("absent to declared 1: plan action = %s, want %s", got, ActionNoop)
	}
}
