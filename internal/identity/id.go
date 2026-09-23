// Package identity generates request/response identifiers.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// RandomID returns prefix + "_" + hex(size random bytes).
func RandomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
