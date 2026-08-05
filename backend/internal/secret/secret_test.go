package secret

import (
	"crypto/rand"
	"strings"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	c, err := New(newKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, plain := range []string{"topsecret", "bot:12345:abcDEF", "", "unicode ✓ päss wörd ✓"} {
		enc, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plain, err)
		}
		if plain != "" && !strings.HasPrefix(enc, prefix) {
			t.Fatalf("ciphertext missing prefix: %q", enc)
		}
		if plain != "" && enc == plain {
			t.Fatalf("value was not encrypted: %q", enc)
		}
		got, err := c.Decrypt(enc)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plain {
			t.Fatalf("round-trip = %q, want %q", got, plain)
		}
	}
}

func TestDecryptPassesLegacyPlaintextThrough(t *testing.T) {
	c, _ := New(newKey(t))
	got, err := c.Decrypt("plain-legacy-secret")
	if err != nil || got != "plain-legacy-secret" {
		t.Fatalf("legacy passthrough = %q err=%v", got, err)
	}
}

func TestNilCipherIsIdentity(t *testing.T) {
	var c *Cipher // encryption disabled
	enc, err := c.Encrypt("s")
	if err != nil || enc != "s" {
		t.Fatalf("nil Encrypt = %q err=%v, want identity", enc, err)
	}
	got, err := c.Decrypt("plain")
	if err != nil || got != "plain" {
		t.Fatalf("nil Decrypt plain = %q err=%v", got, err)
	}
	// An encrypted value with no key is an error, not silent ciphertext.
	if _, err := c.Decrypt(prefix + "abc"); err == nil {
		t.Fatal("nil Decrypt of an encrypted value should error")
	}
}

func TestWrongKeyFails(t *testing.T) {
	a, _ := New(newKey(t))
	b, _ := New(newKey(t))
	enc, _ := a.Encrypt("secret")
	if _, err := b.Decrypt(enc); err == nil {
		t.Fatal("decrypt with the wrong key should fail (GCM auth)")
	}
}

func TestKeyLengthValidated(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key should be rejected")
	}
	if _, err := New(); err == nil {
		t.Fatal("no key should be rejected")
	}
}

// TestKeyringRotation covers the rotation flow: a value encrypted under the old
// key is still readable via a keyring [new, old], new writes use the new primary,
// and the old key alone can no longer read new writes.
func TestKeyringRotation(t *testing.T) {
	keyA, keyB := newKey(t), newKey(t)
	cA, _ := New(keyA)
	encUnderA, _ := cA.Encrypt("secret")

	// Rotated keyring: B primary, A previous.
	rotated, _ := New(keyB, keyA)
	if got, err := rotated.Decrypt(encUnderA); err != nil || got != "secret" {
		t.Fatalf("rotated keyring should read old value: got %q err=%v", got, err)
	}
	encUnderB, _ := rotated.Encrypt("secret") // uses primary (B)

	cB, _ := New(keyB)
	if got, err := cB.Decrypt(encUnderB); err != nil || got != "secret" {
		t.Fatalf("new key should read new value: got %q err=%v", got, err)
	}
	if _, err := cA.Decrypt(encUnderB); err == nil {
		t.Fatal("old key alone must not read a value encrypted under the new key")
	}
}

func TestDecryptFailsWhenNoKeyMatches(t *testing.T) {
	cA, _ := New(newKey(t))
	cB, _ := New(newKey(t))
	enc, _ := cB.Encrypt("x")
	if _, err := cA.Decrypt(enc); err == nil {
		t.Fatal("decrypt should fail when no configured key matches")
	}
}
