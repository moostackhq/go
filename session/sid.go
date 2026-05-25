package session

import (
	"crypto/rand"
	"encoding/base64"
)

// sidByteLen is the entropy budget for a session ID. 32 bytes (256 bits)
// is well past the birthday-bound for any realistic concurrent session
// count, and the resulting base64url string is 43 characters — short
// enough to fit comfortably in a cookie value.
const sidByteLen = 32

// generateSID returns a fresh, cryptographically random session ID
// encoded as unpadded base64url. The returned string is safe to use in
// cookies, URLs, and headers without escaping.
func generateSID() (string, error) {
	buf := make([]byte, sidByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
