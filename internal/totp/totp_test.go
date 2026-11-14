package totp

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238
	"encoding/base32"
	"encoding/binary"
	"fmt"
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
	if step, ok := ValidateAt(secret, cur, now); !ok || step != Step(now) {
		t.Fatalf("current code: step %d ok %v, want step %d", step, ok, Step(now))
	}
	// Previous step (clock skew) still accepted.
	prev, _ := Code(secret, now.Add(-30*time.Second))
	if step, ok := ValidateAt(secret, prev, now); (!ok || step != Step(now)-1) && prev != cur {
		t.Fatalf("previous-step code: step %d ok %v, want step %d (skew)", step, ok, Step(now)-1)
	}
	// Next step (a fast clock) is accepted too, and reports its own step.
	next, _ := Code(secret, now.Add(30*time.Second))
	if step, ok := ValidateAt(secret, next, now); (!ok || step != Step(now)+1) && next != cur && next != prev {
		t.Fatalf("next-step code: step %d ok %v, want step %d", step, ok, Step(now)+1)
	}
	// Two steps away is rejected.
	old, _ := Code(secret, now.Add(-90*time.Second))
	if _, ok := ValidateAt(secret, old, now); old != cur && old != prev && ok {
		t.Fatal("code two steps away should not validate")
	}
	if _, ok := ValidateAt(secret, "12345", now); ok {
		t.Fatal("short input should not validate")
	}
}

func TestURI(t *testing.T) {
	u := URI("ABCD", "user@x", "cerbix")
	if u == "" || u[:12] != "otpauth://to" {
		t.Fatalf("uri = %q", u)
	}
}

// Review of iter-0212, P1-1: an empty secret is not a key. HMAC accepts an empty key, so without
// this refusal an account left enabled with an empty secret would accept codes anyone can compute.
func TestAnEmptySecretIsNotAKey(t *testing.T) {
	for _, secret := range []string{"", "   ", "\t"} {
		if c, err := Code(secret, time.Now()); err == nil {
			t.Errorf("Code(%q) = %q, nil — want an error", secret, c)
		}
	}
	emptyKeyCode := "000000"
	{
		// What HMAC-SHA1 with an empty key yields now: the code an attacker would send.
		h := hmac.New(sha1.New, nil)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(time.Now().Unix())/30)
		h.Write(buf[:])
		sum := h.Sum(nil)
		off := sum[len(sum)-1] & 0x0f
		val := uint32(sum[off]&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
		emptyKeyCode = fmt.Sprintf("%06d", val%1_000_000)
	}
	if _, ok := ValidateAt("", emptyKeyCode, time.Now()); ok {
		t.Errorf("an empty secret accepted the empty-key code %s", emptyKeyCode)
	}
}
