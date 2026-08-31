package probeagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"xray-checker/diagnostics"
)

const (
	ProtocolVersion = 1
	EnrollPath      = "/api/v1/agent/enroll"
	HeartbeatPath   = "/api/v1/agent/heartbeat"
	JobPollPath     = "/api/v1/agent/jobs/next"
	ObservationPath = "/api/v1/agent/observations"
)

const MaxExecutionConfigBytes = 512 * 1024

type EnrollRequest struct {
	ProtocolVersion      int      `json:"protocolVersion"`
	AgentID              string   `json:"agentId"`
	EnrollmentToken      string   `json:"enrollmentToken"`
	IdentityPublicKey    []byte   `json:"identityPublicKey"`
	ObservationPublicKey []byte   `json:"observationPublicKey"`
	AgentVersion         string   `json:"agentVersion"`
	Capabilities         []string `json:"capabilities"`
}

type EnrollResponse struct {
	AgentID           string    `json:"agentId"`
	EnrolledAt        time.Time `json:"enrolledAt"`
	HeartbeatInterval int       `json:"heartbeatIntervalSec"`
}

type HeartbeatRequest struct {
	ProtocolVersion int      `json:"protocolVersion"`
	AgentID         string   `json:"agentId"`
	AgentVersion    string   `json:"agentVersion"`
	Capabilities    []string `json:"capabilities"`
	Health          string   `json:"health"`
}

type HeartbeatResponse struct {
	AgentID    string    `json:"agentId"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

// ControlRequest is the minimal signed body used by an enrolled agent when it
// asks for work. Authentication remains bound to the HTTP method, path, body,
// timestamp and monotonic sequence through ControlSigningPayload.
type ControlRequest struct {
	ProtocolVersion int    `json:"protocolVersion"`
	AgentID         string `json:"agentId"`
}

// JobAssignment deliberately keeps credential-bearing execution material out
// of diagnostics.DiagnosticJob and therefore out of session exports. The
// controller retains it only in the ephemeral delivery queue.
type JobAssignment struct {
	Job        diagnostics.DiagnosticJob `json:"job"`
	XrayConfig json.RawMessage           `json:"xrayConfig"`
	SocksPort  int                       `json:"socksPort"`
	TargetHost string                    `json:"targetHost"`
	TargetPort int                       `json:"targetPort"`
	// TargetSNI is the name the TLS probe presents. It already travels in
	// plaintext in every real handshake with this node, so carrying it here
	// exposes nothing the network cannot already observe.
	TargetSNI string `json:"targetSni,omitempty"`
}

type JobPollResponse struct {
	Job *JobAssignment `json:"job,omitempty"`
}

type ObservationResponse struct {
	JobID      string    `json:"jobId"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

type controlSignatureEnvelope struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	AgentID    string `json:"agentId"`
	Timestamp  string `json:"timestamp"`
	Sequence   uint64 `json:"sequence"`
	BodySHA256 string `json:"bodySha256"`
}

func ControlSigningPayload(method, path, agentID string, timestamp time.Time, sequence uint64, body []byte) ([]byte, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	agentID = strings.TrimSpace(agentID)
	if method == "" || path == "" || agentID == "" || timestamp.IsZero() || sequence == 0 {
		return nil, fmt.Errorf("control signature binding is incomplete")
	}
	digest := sha256.Sum256(body)
	return json.Marshal(controlSignatureEnvelope{
		Method:     method,
		Path:       path,
		AgentID:    agentID,
		Timestamp:  timestamp.UTC().Format(time.RFC3339Nano),
		Sequence:   sequence,
		BodySHA256: hex.EncodeToString(digest[:]),
	})
}
