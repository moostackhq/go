package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// HashLength is the number of hex characters of the SHA-256 digest
// embedded in compiled filenames. 8 chars = 32 bits = ~65k assets
// before a 50% collision probability — comfortably above any project
// size in practice.
const HashLength = 8

// hashContent returns the truncated hex digest used in public
// filenames. The full SHA-256 is shortened to [HashLength] characters.
func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:HashLength]
}

// hashedName splits logical "foo/bar.js" + "abc12345" into
// "foo/bar-abc12345.js". Files without an extension get the hash
// appended after a dash: "Makefile" → "Makefile-abc12345".
func hashedName(logicalPath, hash string) string {
	dir, base := path.Split(logicalPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return dir + stem + "-" + hash + ext
}
