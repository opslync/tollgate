package trace

import (
	"crypto/rand"
	"encoding/hex"
)

// newTraceID and newSpanID return lowercase-hex OTLP identifiers (32 and 16
// hex chars respectively — 16 and 8 random bytes). Verified against a real
// otel-collector: hex, not base64, is what its OTLP/HTTP JSON receiver
// expects for the traceId/spanId bytes fields.
func newTraceID() (string, error) { return randomHex(16) }
func newSpanID() (string, error)  { return randomHex(8) }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
