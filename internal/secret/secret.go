// Package secret provides authenticated symmetric encryption for secrets stored
// at rest (webhook signing secrets, notification-channel credentials). Values are
// encrypted with AES-256-GCM and tagged with an "enc:v1:" prefix. Decryption
// passes un-prefixed values through unchanged, so encryption can be switched on
// for an existing database without migrating legacy plaintext rows.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const prefix = "enc:v1:"

// Cipher is a keyring: it encrypts with the primary (first) key and decrypts by
// trying every key, so a rotation can add a new primary while old keys still read
// existing data. A nil *Cipher means encryption is disabled: Encrypt returns its
// input unchanged and Decrypt passes plaintext through (but fails on an encrypted
// value it cannot read).
type Cipher struct {
	aeads []cipher.AEAD // aeads[0] is the primary (encryption) key
}

// New builds a Cipher from one or more 32-byte (AES-256) keys. The first key is
// the primary used for encryption; the rest are additional decryption candidates
// (previous keys during a rotation).
func New(keys ...[]byte) (*Cipher, error) {
	if len(keys) == 0 {
		return nil, errors.New("secret: at least one key is required")
	}
	c := &Cipher{}
	for i, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("secret: key %d must be 32 bytes, got %d", i, len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("secret: new cipher: %w", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("secret: new gcm: %w", err)
		}
		c.aeads = append(c.aeads, aead)
	}
	return c, nil
}

// Encrypt returns an "enc:v1:" token for s under the primary key. An empty string
// and a nil Cipher are returned unchanged (nothing to protect / encryption off).
func (c *Cipher) Encrypt(s string) (string, error) {
	if c == nil || s == "" {
		return s, nil
	}
	aead := c.aeads[0]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	ct := aead.Seal(nonce, nonce, []byte(s), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

// IsEncrypted reports whether s carries the at-rest ciphertext prefix (i.e. it is an
// Encrypt output, not legacy/seeded plaintext). Used by one-time backfills to skip values
// that are already encrypted.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefix) }

// Decrypt reverses Encrypt, trying each key in turn (GCM's authentication tag
// tells a wrong key from the right one). A value without the "enc:v1:" prefix is
// returned as-is (legacy plaintext). An encrypted value presented to a nil Cipher,
// or that no key can open, is an error — never silently returned as ciphertext.
func (c *Cipher) Decrypt(s string) (string, error) {
	if !strings.HasPrefix(s, prefix) {
		return s, nil
	}
	if c == nil {
		return "", errors.New("secret: encrypted value found but no encryption key is configured")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", fmt.Errorf("secret: decode: %w", err)
	}
	for _, aead := range c.aeads {
		ns := aead.NonceSize()
		if len(raw) < ns {
			continue
		}
		if pt, err := aead.Open(nil, raw[:ns], raw[ns:], nil); err == nil {
			return string(pt), nil
		}
	}
	return "", errors.New("secret: no configured key can decrypt this value")
}
