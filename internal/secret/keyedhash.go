package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// macKeyContext is the derivation label of the keyed-hash sub-key (D35).
const macKeyContext = "cerbix/keyed-hash/v1"

var errNoKeyedHash = errors.New("secret: keyed hash needs an encryption key")

// deriveMACKey derives the MAC sub-key of an at-rest key: HMAC-SHA-256(key, context).
func deriveMACKey(key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(macKeyContext))
	return m.Sum(nil)
}

func keyedHash(macKey []byte, label string, value []byte) string {
	m := hmac.New(sha256.New, macKey)
	m.Write([]byte(label))
	m.Write([]byte{0})
	m.Write(value)
	return hex.EncodeToString(m.Sum(nil))
}

// KeyedHash returns hex(HMAC-SHA-256(subkey, label || 0x00 || value)) under the
// primary key. It is deterministic, so it serves equality lookups of a value that
// must not be stored in the clear. A nil Cipher is an error (fail-closed).
func (c *Cipher) KeyedHash(label string, value []byte) (string, error) {
	if c == nil || len(c.macKeys) == 0 {
		return "", errNoKeyedHash
	}
	return keyedHash(c.macKeys[0], label, value), nil
}

// KeyedHashes returns the keyed hash under every configured key, primary first,
// so a lookup keeps matching rows written under a previous key during a rotation.
func (c *Cipher) KeyedHashes(label string, value []byte) ([]string, error) {
	if c == nil || len(c.macKeys) == 0 {
		return nil, errNoKeyedHash
	}
	out := make([]string, len(c.macKeys))
	for i, k := range c.macKeys {
		out[i] = keyedHash(k, label, value)
	}
	return out, nil
}
