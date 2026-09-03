package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// D11: the fixture registry is closed, embedded and PINNED. An operator cannot make the canary
// upload a file of their choosing — that is an exfiltration primitive — and rotating a fixture is a
// release, which is the cost this decision accepted.
func TestCanaryFixtureDigestsArePinned(t *testing.T) {
	for _, ref := range CanaryFixtureRefs() {
		f, ok := CanaryFixtureByRef(ref)
		if !ok {
			t.Fatalf("%s is listed and not resolvable", ref)
		}
		if len(f.Bytes) == 0 {
			t.Fatalf("%s carries no embedded bytes", ref)
		}
		if len(f.Bytes) > f.MaxBytes {
			t.Fatalf("%s is %d bytes, past its declared maximum %d", ref, len(f.Bytes), f.MaxBytes)
		}
		if f.MaxBytes > CanaryFixtureRegistryMax {
			t.Fatalf("%s declares a maximum past the registry ceiling", ref)
		}
		sum := sha256.Sum256(f.Bytes)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			t.Fatalf("%s digest = %s, pinned %s — the asset changed without its pin", ref, got, f.SHA256)
		}
		if _, err := CanaryFixtureBytes(ref); err != nil {
			t.Fatalf("%s must be readable: %v", ref, err)
		}
	}
}

func TestCanaryFixtureBytesRefusesAnythingNotInTheRegistry(t *testing.T) {
	for _, bad := range []string{"", "small_wav_v2", "/etc/passwd", "https://example.com/x.wav"} {
		if _, err := CanaryFixtureBytes(bad); err == nil {
			t.Fatalf("CanaryFixtureBytes(%q) must fail", bad)
		}
	}
}
