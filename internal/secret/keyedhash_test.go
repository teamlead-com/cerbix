package secret

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestKeyedHashDeterministicAndSeparated(t *testing.T) {
	c1, _ := New(key(1))
	c1b, _ := New(key(1))
	c2, _ := New(key(2))

	a, err := c1.KeyedHash("phone-number", []byte("+905321116869"))
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Fatalf("want 64 hex chars, got %d", len(a))
	}
	if b, _ := c1b.KeyedHash("phone-number", []byte("+905321116869")); a != b {
		t.Fatal("not deterministic per key")
	}
	if b, _ := c1.KeyedHash("phone-verification", []byte("+905321116869")); a == b {
		t.Fatal("label does not separate")
	}
	if b, _ := c2.KeyedHash("phone-number", []byte("+905321116869")); a == b {
		t.Fatal("key does not separate")
	}
	if b, _ := c1.KeyedHash("phone-number", []byte("+905321116860")); a == b {
		t.Fatal("value does not separate")
	}
}

func TestKeyedHashUsesDerivedSubkeyNotRawKey(t *testing.T) {
	raw := key(7)
	c, _ := New(raw)
	got, _ := c.KeyedHash("l", []byte("v"))

	m := hmac.New(sha256.New, raw)
	m.Write([]byte("l\x00v"))
	if got == hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("hash was computed with the raw AEAD key")
	}
	sk := hmac.New(sha256.New, raw)
	sk.Write([]byte("cerbix/keyed-hash/v1"))
	m2 := hmac.New(sha256.New, sk.Sum(nil))
	m2.Write([]byte("l\x00v"))
	if got != hex.EncodeToString(m2.Sum(nil)) {
		t.Fatal("hash is not HMAC(subkey, label||0x00||value)")
	}
}

func TestKeyedHashesRotation(t *testing.T) {
	old, _ := New(key(1))
	rot, _ := New(key(2), key(1))
	oldH, _ := old.KeyedHash("phone-number", []byte("+12025550147"))
	hs, err := rot.KeyedHashes("phone-number", []byte("+12025550147"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 {
		t.Fatalf("want 2 hashes, got %d", len(hs))
	}
	if primary, _ := rot.KeyedHash("phone-number", []byte("+12025550147")); hs[0] != primary {
		t.Fatal("primary hash must come first")
	}
	if hs[1] != oldH {
		t.Fatal("rotated keyring lost the old key's hash")
	}
}

func TestKeyedHashNilCipherFailsClosed(t *testing.T) {
	var c *Cipher
	if _, err := c.KeyedHash("l", []byte("v")); err == nil {
		t.Fatal("nil cipher must error")
	}
	if _, err := c.KeyedHashes("l", []byte("v")); err == nil {
		t.Fatal("nil cipher must error")
	}
}
