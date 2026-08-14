package secret

import (
	"bytes"
	"strings"
	"testing"
)

func testCipher(t *testing.T, keys ...string) *Cipher {
	t.Helper()
	var bs [][]byte
	for _, k := range keys {
		bs = append(bs, []byte(k))
	}
	c, err := New(bs...)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return c
}

const (
	keyA = "0123456789abcdef0123456789abcdef" // 32 bytes
	keyB = "fedcba9876543210fedcba9876543210"
)

func TestAADRoundTrip(t *testing.T) {
	c := testCipher(t, keyA)
	aad := CanonicalAAD("proj-1", "sec-1")
	tok, err := c.EncryptBytes([]byte("hunter2"), aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(tok, "enc:v2a:") || !IsAADEncrypted(tok) {
		t.Fatalf("token must carry the v2a prefix, got %q", tok)
	}
	pt, err := c.DecryptBytes(tok, aad)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(pt, []byte("hunter2")) {
		t.Fatalf("round trip = %q", pt)
	}
}

// TestAADTransplantFails is the context-integrity core: the same ciphertext presented under
// a different AAD (another project/secret/monitor/field) must FAIL authentication.
func TestAADTransplantFails(t *testing.T) {
	c := testCipher(t, keyA)
	tok, err := c.EncryptBytes([]byte("hunter2"), CanonicalAAD("proj-A", "sec-1"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := c.DecryptBytes(tok, CanonicalAAD("proj-B", "sec-1")); err == nil {
		t.Fatal("cross-project transplant must fail authentication")
	}
	if _, err := c.DecryptBytes(tok, CanonicalAAD("proj-A", "sec-2")); err == nil {
		t.Fatal("cross-secret transplant must fail authentication")
	}
}

// TestAADNoLegacyFallback: the AAD path never opens or passes through non-v2a values, and
// the legacy path never opens v2a values — no silent context-binding downgrade.
func TestAADNoLegacyFallback(t *testing.T) {
	c := testCipher(t, keyA)
	legacy, err := c.Encrypt("hunter2") // enc:v1:
	if err != nil {
		t.Fatalf("legacy encrypt: %v", err)
	}
	if _, err := c.DecryptBytes(legacy, CanonicalAAD("p", "s")); err == nil {
		t.Fatal("DecryptBytes must reject a legacy enc:v1: token")
	}
	if _, err := c.DecryptBytes("plaintext", CanonicalAAD("p", "s")); err == nil {
		t.Fatal("DecryptBytes must reject raw plaintext (no passthrough)")
	}
	tok, err := c.EncryptBytes([]byte("hunter2"), CanonicalAAD("p", "s"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// The v2a prefix is RESERVED in the legacy API: it must error, never pass the
	// ciphertext through as "plaintext" (a wrong-API caller would otherwise feed
	// ciphertext to a prober as a credential).
	if out, err := c.Decrypt(tok); err == nil {
		t.Fatalf("legacy Decrypt must reject a v2a token, got %q", out)
	}
}

// TestAADMalformedTokens pins the external-boundary negatives: invalid base64 and
// truncated ciphertexts return bounded errors, never panic, never open.
func TestAADMalformedTokens(t *testing.T) {
	c := testCipher(t, keyA)
	aad := CanonicalAAD("p", "s")
	for _, tok := range []string{
		"enc:v2a:!!!not-base64!!!",           // invalid base64
		"enc:v2a:",                           // empty body
		"enc:v2a:AAAA",                       // shorter than a nonce
		"enc:v2a:" + strings.Repeat("A", 20), // nonce-sized but no GCM tag
	} {
		if _, err := c.DecryptBytes(tok, aad); err == nil {
			t.Fatalf("malformed token %q must fail", tok)
		}
	}
	// Valid token with empty AAD at decrypt: the API enforces its own precondition.
	tok, err := c.EncryptBytes([]byte("v"), aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := c.DecryptBytes(tok, nil); err == nil {
		t.Fatal("DecryptBytes must reject empty AAD")
	}
}

func TestAADFailClosed(t *testing.T) {
	var nilC *Cipher
	if _, err := nilC.EncryptBytes([]byte("x"), CanonicalAAD("p", "s")); err == nil {
		t.Fatal("nil cipher must not encrypt (no plaintext fallback)")
	}
	if _, err := nilC.DecryptBytes("enc:v2a:AAAA", CanonicalAAD("p", "s")); err == nil {
		t.Fatal("nil cipher must not decrypt")
	}
	c := testCipher(t, keyA)
	if _, err := c.EncryptBytes(nil, CanonicalAAD("p", "s")); err == nil {
		t.Fatal("empty plaintext must be rejected")
	}
	if _, err := c.EncryptBytes([]byte("x"), nil); err == nil {
		t.Fatal("empty AAD must be rejected")
	}
}

// TestAADKeyringRotation: a previous key still opens old tokens; a foreign key does not.
func TestAADKeyringRotation(t *testing.T) {
	old := testCipher(t, keyA)
	aad := CanonicalAAD("p", "s")
	tok, err := old.EncryptBytes([]byte("v"), aad)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	rotated := testCipher(t, keyB, keyA) // new primary, old key retained
	if pt, err := rotated.DecryptBytes(tok, aad); err != nil || string(pt) != "v" {
		t.Fatalf("previous key must open old tokens, got %q err=%v", pt, err)
	}
	if needs, err := rotated.NeedsReencryptBytes(tok, aad); err != nil || !needs {
		t.Fatalf("old-key token needs=%v err=%v, want true", needs, err)
	}
	primaryToken, err := rotated.EncryptBytes([]byte("v"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if needs, err := rotated.NeedsReencryptBytes(primaryToken, aad); err != nil || needs {
		t.Fatalf("primary token needs=%v err=%v, want false", needs, err)
	}
	foreign := testCipher(t, keyB)
	if _, err := foreign.DecryptBytes(tok, aad); err == nil {
		t.Fatal("a keyring without the original key must fail")
	}
}

// TestCanonicalAADUnambiguous: length-prefixed parts cannot collide across different splits.
func TestCanonicalAADUnambiguous(t *testing.T) {
	if bytes.Equal(CanonicalAAD("ab", "c"), CanonicalAAD("a", "bc")) {
		t.Fatal(`["ab","c"] and ["a","bc"] must encode differently`)
	}
	if bytes.Equal(CanonicalAAD("a", "b"), CanonicalAAD("a", "b", "")) {
		t.Fatal("part count must be part of the encoding")
	}
}
