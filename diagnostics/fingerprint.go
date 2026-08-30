package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
)

// ConfigFingerprint hashes the caller's canonical effective configuration.
// Only the digest is retained in diagnostic state; the configuration and its
// credentials remain outside the session manager.
func ConfigFingerprint(canonicalEffectiveConfig []byte) string {
	digest := sha256.Sum256(canonicalEffectiveConfig)
	return "sha256:" + hex.EncodeToString(digest[:])
}
