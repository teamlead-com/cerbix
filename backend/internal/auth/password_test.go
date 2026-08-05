package auth

import "testing"

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("s3cret-passw0rd")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "s3cret-passw0rd" {
		t.Fatal("hash must not equal the plaintext")
	}
	ok, err := VerifyPassword(hash, "s3cret-passw0rd")
	if err != nil || !ok {
		t.Fatalf("verify correct: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(hash, "wrong")
	if err != nil || ok {
		t.Fatalf("verify wrong: ok=%v err=%v", ok, err)
	}
}

func TestVerifyPasswordRejectsBadFormat(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2id$only", "$bcrypt$v=1$m=1,t=1,p=1$a$b"} {
		if _, err := VerifyPassword(bad, "x"); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestHashesAreSalted(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Fatal("identical passwords should produce different hashes (random salt)")
	}
}
