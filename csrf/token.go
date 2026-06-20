package csrf

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// tokenLen is the size of the canonical token in bytes.
const tokenLen = 32

// enc encodes tokens for cookies and form values. URL-safe + no padding
// keeps the value clean in a Set-Cookie header and a form field.
var enc = base64.URLEncoding.WithPadding(base64.NoPadding)

// newToken returns a fresh canonical token.
func newToken() ([]byte, error) {
	b := make([]byte, tokenLen)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// signCookie encodes the canonical token with an appended HMAC, so a
// tampered or attacker-set cookie fails verification. Signing is what
// makes the double-submit resistant to cookie injection.
func (p *Protector) signCookie(t []byte) string {
	raw := make([]byte, 0, tokenLen+sha256.Size)
	raw = append(raw, t...)
	raw = append(raw, hmacSum(p.secret, t)...)
	return enc.EncodeToString(raw)
}

// parseCookie decodes and verifies a cookie value, recovering the
// canonical token. ok is false for any malformed or unauthenticated
// value (including the empty string when no cookie was sent).
func (p *Protector) parseCookie(v string) (t []byte, ok bool) {
	raw, err := enc.DecodeString(v)
	if err != nil || len(raw) != tokenLen+sha256.Size {
		return nil, false
	}
	tok, mac := raw[:tokenLen], raw[tokenLen:]
	if !hmac.Equal(mac, hmacSum(p.secret, tok)) {
		return nil, false
	}
	return tok, true
}

// maskToken returns a per-render masking of t: a random pad followed by
// pad XOR t. The submitted value differs every render (BREACH
// resistance) yet unmasks back to t.
func maskToken(t []byte) (string, error) {
	pad := make([]byte, tokenLen)
	if _, err := rand.Read(pad); err != nil {
		return "", err
	}
	raw := make([]byte, 0, tokenLen*2)
	raw = append(raw, pad...)
	raw = append(raw, xor(t, pad)...)
	return enc.EncodeToString(raw), nil
}

// unmaskToken reverses maskToken. ok is false for a malformed value.
func unmaskToken(v string) (t []byte, ok bool) {
	raw, err := enc.DecodeString(v)
	if err != nil || len(raw) != tokenLen*2 {
		return nil, false
	}
	pad, xored := raw[:tokenLen], raw[tokenLen:]
	return xor(pad, xored), true
}

// tokensEqual is a constant-time comparison of two canonical tokens.
func tokensEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func hmacSum(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func xor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}
