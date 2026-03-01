package id

// Package id provides ID generation utilities.

import (
	"crypto/rand"
	"encoding/hex"
)

// Generate returns a random 16-character hex string.
// Used for envelope IDs, tool call IDs, todo item IDs, and other non-DB identifiers.
func Generate() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
