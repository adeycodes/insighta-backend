package auth

import (
	"crypto/sha256"
	"encoding/base64"
)

// VerifyPKCE checks that the given code verifier matches the stored challenge.
// The challenge must be BASE64URL(SHA256(verifier)) with no padding.
func VerifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(h[:])
	return computed == challenge
}
