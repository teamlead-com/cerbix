package secret

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// randNonce returns a fresh random nonce of the given size.
func randNonce(size int) ([]byte, error) {
	nonce := make([]byte, size)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return nonce, nil
}

// aadPrefix tags AAD-bound ciphertexts (spec func-secret-inventory §4.7). The format is
// versioned SEPARATELY from the legacy "enc:v1:" format: an AAD-bound value can never be
// opened by the AAD-less path and vice versa, and an authentication failure NEVER falls
// back to a legacy decrypt (reviewer guardrail: no silent downgrade of context binding).
const aadPrefix = "enc:v2a:"

// CanonicalAAD builds the unambiguous additional-authenticated-data encoding used for both
// the at-rest inventory binding (project_id, secret_id) and the dispatch envelope binding
// (v, region, key_id, monitor_id, execution_revision, field_name, job_id). Each part is
// length-prefixed (uvarint) so no concatenation of different part lists can collide:
// ["ab","c"] and ["a","bc"] encode differently by construction.
func CanonicalAAD(parts ...string) []byte {
	var out []byte
	var lenBuf [binary.MaxVarintLen64]byte
	out = binary.AppendUvarint(out, uint64(len(parts)))
	for _, p := range parts {
		n := binary.PutUvarint(lenBuf[:], uint64(len(p)))
		out = append(out, lenBuf[:n]...)
		out = append(out, p...)
	}
	return out
}

// EncryptBytes seals plaintext under the primary key, cryptographically bound to aad, and
// returns an "enc:v2a:" token. This path is fail-closed by design (it protects the secret
// inventory and dispatch envelopes): a nil Cipher and an empty plaintext are errors — there
// is no plaintext passthrough and no "encryption off" mode here, unlike the legacy string
// API. The caller owns pt and may zero it after the call.
func (c *Cipher) EncryptBytes(pt, aad []byte) (string, error) {
	if c == nil {
		return "", errors.New("secret: AAD encryption requires a configured key")
	}
	if len(pt) == 0 {
		return "", errors.New("secret: empty plaintext")
	}
	if len(aad) == 0 {
		return "", errors.New("secret: empty AAD")
	}
	aead := c.aeads[0]
	nonce, err := randNonce(aead.NonceSize())
	if err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, pt, aad)
	return aadPrefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

// DecryptBytes opens an "enc:v2a:" token bound to aad, trying each keyring key (GCM's tag
// distinguishes a wrong key). Fail-closed: a token without the v2a prefix — legacy "enc:v1:"
// or raw plaintext — is an error, never passed through; an authentication failure (wrong
// key, wrong AAD, tampered ciphertext) is an error with no fallback. The returned buffer is
// caller-owned and should be zeroed after use (best-effort; Go guarantees no wiping).
func (c *Cipher) DecryptBytes(token string, aad []byte) ([]byte, error) {
	if !strings.HasPrefix(token, aadPrefix) {
		return nil, errors.New("secret: value is not an AAD-bound ciphertext")
	}
	if c == nil {
		return nil, errors.New("secret: AAD-bound value found but no encryption key is configured")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(token, aadPrefix))
	if err != nil {
		return nil, fmt.Errorf("secret: decode: %w", err)
	}
	for _, aead := range c.aeads {
		ns := aead.NonceSize()
		if len(raw) < ns {
			continue
		}
		if pt, err := aead.Open(nil, raw[:ns], raw[ns:], aad); err == nil {
			return pt, nil
		}
	}
	return nil, errors.New("secret: authentication failed for this value and context")
}

// IsAADEncrypted reports whether s carries the AAD-bound ciphertext prefix.
func IsAADEncrypted(s string) bool { return strings.HasPrefix(s, aadPrefix) }
