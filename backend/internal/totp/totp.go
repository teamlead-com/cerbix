// Package totp implements time-based one-time passwords (RFC 6238, HMAC-SHA1,
// 6 digits, 30s period) with no external dependency.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 / authenticator apps use HMAC-SHA1
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const period = 30 * time.Second

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a random 160-bit secret, base32-encoded (the form
// authenticator apps expect).
func GenerateSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return enc.EncodeToString(b), nil
}

// URI builds an otpauth:// provisioning URI for authenticator apps / QR codes.
func URI(secret, account, issuer string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// Code computes the 6-digit TOTP for the given time.
func Code(secret string, t time.Time) (string, error) {
	key, err := enc.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("totp: bad secret: %w", err)
	}
	counter := uint64(t.Unix()) / uint64(period.Seconds())
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	h := hmac.New(sha1.New, key)
	h.Write(buf[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	val := uint32(sum[off]&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", val%1_000_000), nil
}

// ValidateAt reports whether input matches the secret within ±1 step of t
// (tolerating clock skew). Comparison is constant-time.
func ValidateAt(secret, input string, t time.Time) bool {
	input = strings.TrimSpace(input)
	if len(input) != 6 {
		return false
	}
	for _, skew := range []time.Duration{0, -period, period} {
		if c, err := Code(secret, t.Add(skew)); err == nil && subtle.ConstantTimeCompare([]byte(c), []byte(input)) == 1 {
			return true
		}
	}
	return false
}

// Validate checks input against the secret at the current time.
func Validate(secret, input string) bool { return ValidateAt(secret, input, time.Now()) }
