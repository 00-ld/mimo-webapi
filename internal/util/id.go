// Package util holds small helpers shared across the relay.
package util

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewID returns a short random hex identifier used for conversation and
// message ids handed to the MiMo web backend.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; surface it rather than
		// silently emitting a predictable id.
		panic(fmt.Sprintf("util: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}
