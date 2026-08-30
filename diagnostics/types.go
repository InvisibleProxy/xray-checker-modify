package diagnostics

import "time"

const (
	SessionSchemaVersion     = 1
	JobSchemaVersion         = 1
	ObservationSchemaVersion = 1
)

type Trigger string

const (
	TriggerManual               Trigger = "manual"
	TriggerAutoProxyFailure     Trigger = "auto_proxy_failure"
	TriggerAutoCheckEndpoint    Trigger = "auto_check_endpoint"
	TriggerAutoAmbiguousFailure Trigger = "auto_ambiguous_failure"
)

func (t Trigger) Automatic() bool {
	return t != TriggerManual
}

type SessionState string

const (
	SessionStateRequested   SessionState = "requested"
	SessionStateDispatching SessionState = "dispatching"
	SessionStateRunning     SessionState = "running"
	SessionStateCompleted   SessionState = "completed"
	SessionStatePartial     SessionState = "partial"
	SessionStateExpired     SessionState = "expired"
	SessionStateCancelled   SessionState = "cancelled"
)

func (s SessionState) Terminal() bool {
	switch s {
	case SessionStateCompleted, SessionStatePartial, SessionStateExpired, SessionStateCancelled:
		return true
	default:
		return false
	}
}

type JobState string

const (
	JobStatePending    JobState = "pending"
	JobStateDispatched JobState = "dispatched"
	JobStateRunning    JobState = "running"
	JobStateCompleted  JobState = "completed"
	JobStateExpired    JobState = "expired"
	JobStateCancelled  JobState = "cancelled"
)

func (s JobState) Terminal() bool {
	return s == JobStateCompleted || s == JobStateExpired || s == JobStateCancelled
}

type ProbeStatus string

const (
	ProbeStatusUnknown      ProbeStatus = "unknown"
	ProbeStatusOnline       ProbeStatus = "online"
	ProbeStatusProxyFailure ProbeStatus = "proxy_failure"
	ProbeStatusOffline      ProbeStatus = "offline"
)

type ProbeMethod string

const (
	ProbeMethodIP       ProbeMethod = "ip"
	ProbeMethodStatus   ProbeMethod = "status"
	ProbeMethodDownload ProbeMethod = "download"
)

type FailureStage string

const (
	FailureStageConfiguration FailureStage = "configuration"
	FailureStageXrayStart     FailureStage = "xray_start"
	FailureStageProxy         FailureStage = "proxy"
	FailureStageTCP           FailureStage = "tcp"
	FailureStagePing          FailureStage = "ping"
	FailureStageDirect        FailureStage = "direct"
	FailureStageEndpoint      FailureStage = "endpoint"
)

// FailureEvidence intentionally carries only bounded classification values.
// Raw transport errors may contain credentials or endpoint details and are not
// part of the diagnostic session schema.
type FailureEvidence struct {
	Code  string       `json:"code,omitempty"`
	Stage FailureStage `json:"stage,omitempty"`
}

type CheckEvidence struct {
	Checked       bool   `json:"checked"`
	Online        bool   `json:"online"`
	LatencyMillis int64  `json:"latencyMillis,omitempty"`
	FailureCode   string `json:"failureCode,omitempty"`
}

type LocalResultSnapshot struct {
	Status        ProbeStatus     `json:"status"`
	CheckedAt     time.Time       `json:"checkedAt,omitempty"`
	LatencyMillis int64           `json:"latencyMillis,omitempty"`
	Failure       FailureEvidence `json:"failure,omitempty"`
	TCP           CheckEvidence   `json:"tcp"`
	Ping          CheckEvidence   `json:"ping"`
}

type TestProfile struct {
	// ID resolves to controller/agent-owned endpoint configuration. It is not a
	// URL and therefore cannot turn a diagnostic job into an arbitrary fetch.
	ID                   string      `json:"id"`
	Method               ProbeMethod `json:"method"`
	AlternativeProfileID string      `json:"alternativeProfileId,omitempty"`
}

type DiagnosticJob struct {
	SchemaVersion     int         `json:"schemaVersion"`
	JobID             string      `json:"jobId"`
	SessionID         string      `json:"sessionId"`
	AgentID           string      `json:"agentId"`
	Nonce             string      `json:"nonce"`
	StableID          string      `json:"stableId"`
	ConfigGeneration  uint64      `json:"configGeneration"`
	ConfigFingerprint string      `json:"configFingerprint"`
	Profile           TestProfile `json:"profile"`
	CreatedAt         time.Time   `json:"createdAt"`
	ExpiresAt         time.Time   `json:"expiresAt"`
	State             JobState    `json:"state"`
}

type AlternativeEndpointObservation struct {
	ProfileID     string          `json:"profileId"`
	Status        ProbeStatus     `json:"status"`
	LatencyMillis int64           `json:"latencyMillis,omitempty"`
	Failure       FailureEvidence `json:"failure,omitempty"`
}

// Observation is the exact signed agent payload. The signature covers every
// field except Signature itself through ObservationSigningPayload.
type Observation struct {
	SchemaVersion       int                             `json:"schemaVersion"`
	AgentID             string                          `json:"agentId"`
	SessionID           string                          `json:"sessionId"`
	JobID               string                          `json:"jobId"`
	Nonce               string                          `json:"nonce"`
	StableID            string                          `json:"stableId"`
	ConfigGeneration    uint64                          `json:"configGeneration"`
	ConfigFingerprint   string                          `json:"configFingerprint"`
	CheckedAt           time.Time                       `json:"checkedAt"`
	DurationMillis      int64                           `json:"durationMillis"`
	EndpointProfile     string                          `json:"endpointProfile"`
	Status              ProbeStatus                     `json:"status"`
	LatencyMillis       int64                           `json:"latencyMillis,omitempty"`
	Failure             FailureEvidence                 `json:"failure,omitempty"`
	TCP                 CheckEvidence                   `json:"tcp"`
	Ping                CheckEvidence                   `json:"ping"`
	DirectConnectivity  CheckEvidence                   `json:"directConnectivity"`
	AlternativeEndpoint *AlternativeEndpointObservation `json:"alternativeEndpoint,omitempty"`
	AgentVersion        string                          `json:"agentVersion"`
	Signature           []byte                          `json:"signature"`
}

type AcceptedObservation struct {
	Observation Observation `json:"observation"`
	AcceptedAt  time.Time   `json:"acceptedAt"`
	// Reliable is controller-derived. A signed result with a failed direct
	// connectivity control remains evidence, but is not suitable for summary.
	Reliable bool `json:"reliable"`
}

type DiagnosticSession struct {
	SchemaVersion         int                   `json:"schemaVersion"`
	SessionID             string                `json:"sessionId"`
	StableID              string                `json:"stableId"`
	Trigger               Trigger               `json:"trigger"`
	ConfigGeneration      uint64                `json:"configGeneration"`
	ConfigFingerprint     string                `json:"configFingerprint"`
	LocalResultSnapshot   LocalResultSnapshot   `json:"localResultSnapshot"`
	RequestedAgents       []string              `json:"requestedAgents"`
	Jobs                  []DiagnosticJob       `json:"jobs"`
	AgentObservations     []AcceptedObservation `json:"agentObservations"`
	MaintenanceDiagnostic bool                  `json:"maintenanceDiagnostic,omitempty"`
	CreatedAt             time.Time             `json:"createdAt"`
	ExpiresAt             time.Time             `json:"expiresAt"`
	State                 SessionState          `json:"state"`
}

type CreateSessionRequest struct {
	StableID              string
	Trigger               Trigger
	ConfigGeneration      uint64
	ConfigFingerprint     string
	LocalResultSnapshot   LocalResultSnapshot
	RequestedAgents       []string
	MaintenanceDiagnostic bool
	ExpiresAt             time.Time
}

type RegisterJobRequest struct {
	SessionID string
	AgentID   string
	Profile   TestProfile
	ExpiresAt time.Time
}
