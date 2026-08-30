package diagnostics

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
)

type VerifyObservationFunc func(agentID string, payload, signature []byte) error

// ObservationSigningPayload returns the deterministic JSON payload covered by
// the agent signature. The protocol schema contains no maps, and encoding/json
// preserves struct field order.
func ObservationSigningPayload(observation Observation) ([]byte, error) {
	unsigned := observation
	unsigned.Signature = nil
	return json.Marshal(unsigned)
}

// NewEd25519Verifier builds a fail-closed verifier without coupling the
// session manager to enrollment or persisted agent state.
func NewEd25519Verifier(publicKey func(agentID string) (ed25519.PublicKey, bool)) VerifyObservationFunc {
	return func(agentID string, payload, signature []byte) error {
		if publicKey == nil {
			return fmt.Errorf("public key provider is not configured")
		}
		key, ok := publicKey(agentID)
		if !ok || len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("public key is unavailable")
		}
		if len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, payload, signature) {
			return fmt.Errorf("ed25519 signature verification failed")
		}
		return nil
	}
}
