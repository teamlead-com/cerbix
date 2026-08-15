package totp

import (
	"encoding/base32"
	"testing"
	"time"
)

func TestRFC6238Vector(t *testing.T) {
	// RFC 6238 test secret "12345678901234567890" (SHA1). At T=59 → 287082.
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	got, err := Code(secret, time.Unix(59, 0))
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if got != "287082" {
		t.Fatalf("code = %q, want 287082", got)
	}
}

func TestValidateSkewAndGenerate(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	now := time.Now()
	cur, _ := Code(secret, now)
	if !ValidateAt(secret, cur, now) {
		t.Fatal("current code should validate")
	}
	// Previous step (clock skew) still accepted.
	prev, _ := Code(secret, now.Add(-30*time.Second))
	if !ValidateAt(secret, prev, now) {
		t.Fatal("previous-step code should validate (skew)")
	}
	// Two steps away is rejected.
	old, _ := Code(secret, now.Add(-90*time.Second))
	if old != cur && ValidateAt(secret, old, now) {
		t.Fatal("code two steps away should not validate")
	}
	if ValidateAt(secret, "12345", now) {
		t.Fatal("short input should not validate")
	}
}

func TestURI(t *testing.T) {
	u := URI("ABCD", "user@x", "cerbix")
	if u == "" || u[:12] != "otpauth://to" {
		t.Fatalf("uri = %q", u)
	}
}
