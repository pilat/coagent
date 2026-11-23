package localapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// NewToken mints a credential whose plaintext is disclosed only to its caller.
func NewToken() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("mint API token: %w", err)
	}

	return "coagent_" + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

// TokenHash is the config-safe SHA-256 of the whole credential.
func TokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))

	return hex.EncodeToString(hash[:])
}
