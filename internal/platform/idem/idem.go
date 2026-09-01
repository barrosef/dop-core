// Package idem implements the idempotency required of every write RPC
// (ADR-0017): with events and retries, repeating a call must not duplicate its
// effect.
package idem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Store keeps the result of an operation under a key, so that a repeat returns
// the recorded response instead of executing again.
type Store interface {
	// Begin tries to reserve the key. If it already exists with the same
	// request hash, it returns the recorded response and done=true.
	Begin(ctx context.Context, key, requestHash string, ttl time.Duration) (response []byte, done bool, err error)
	// Complete records the response of a successful execution.
	Complete(ctx context.Context, key string, response []byte) error
}

// Hash builds the request signature — the same key with a different body is a
// conflict, not a repeat.
func Hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

const DefaultTTL = 24 * time.Hour
