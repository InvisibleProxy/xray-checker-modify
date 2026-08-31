package diagnostics

import "time"

const (
	SessionSchemaVersion = 1
	JobSchemaVersion     = 1
	// Bumped to 2 for the selectable diagnostic profiles: throughput, latency
	// series, stability, TLS and DNS evidence changed the signed payload.
	ObservationSchemaVersion = 2
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
	ProbeMethodIP        ProbeMethod = "ip"
	ProbeMethodStatus    ProbeMethod = "status"
	ProbeMethodDownload  ProbeMethod = "download"
	ProbeMethodLatency   ProbeMethod = "latency"
	ProbeMethodStability ProbeMethod = "stability"
	ProbeMethodTLS       ProbeMethod = "tls"
	ProbeMethodDNS       ProbeMethod = "dns"
)

// TunnelledMethods run their probe through the agent's ephemeral Xray instance.
// The transport methods reach the node directly instead, which is what lets
// them separate a broken tunnel from a broken path to the server.
func (m ProbeMethod) Tunnelled() bool {
	switch m {
	case ProbeMethodIP, ProbeMethodStatus, ProbeMethodDownload, ProbeMethodLatency, ProbeMethodStability:
		return true
	default:
		return false
	}
}

type FailureStage string

const (
	FailureStageConfiguration FailureStage = "configuration"
	FailureStageXrayStart     FailureStage = "xray_start"
	FailureStageProxy         FailureStage = "proxy"
	FailureStageTCP           FailureStage = "tcp"
	FailureStagePing          FailureStage = "ping"
	FailureStageDirect        FailureStage = "direct"
	FailureStageEndpoint      FailureStage = "endpoint"
	FailureStageDNS           FailureStage = "dns"
	FailureStageTLS           FailureStage = "tls"
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

// ThroughputEvidence reports what a download probe actually achieved. Bytes and
// duration are kept alongside the derived rate so a short transfer cannot be
// mistaken for a sustained measurement.
type ThroughputEvidence struct {
	Bytes          int64 `json:"bytes"`
	DurationMillis int64 `json:"durationMillis"`
	Mbps           int64 `json:"mbps"`
	TTFBMillis     int64 `json:"ttfbMillis,omitempty"`
}

// LatencySeriesEvidence summarises repeated requests. A single measurement
// cannot distinguish a consistently slow node from one with a heavy tail, which
// is exactly the distinction an operator needs before blaming the route.
type LatencySeriesEvidence struct {
	Samples      int    `json:"samples"`
	Succeeded    int    `json:"succeeded"`
	MinMillis    int64  `json:"minMillis,omitempty"`
	MedianMillis int64  `json:"medianMillis,omitempty"`
	P95Millis    int64  `json:"p95Millis,omitempty"`
	MaxMillis    int64  `json:"maxMillis,omitempty"`
	JitterMillis int64  `json:"jitterMillis,omitempty"`
	FailureCode  string `json:"failureCode,omitempty"`
}

// StabilityEvidence records how long a tunnelled transfer survived. Filtering
// that drops a session after a delay looks healthy to every short probe.
type StabilityEvidence struct {
	PlannedMillis int64  `json:"plannedMillis"`
	HeldMillis    int64  `json:"heldMillis"`
	Bytes         int64  `json:"bytes"`
	Interrupted   bool   `json:"interrupted"`
	FailureCode   string `json:"failureCode,omitempty"`
}

// TLSEvidence describes a direct handshake with the node, without the tunnel.
// It separates "the port answers" from "the TLS session the node needs can
// actually be established with this SNI".
type TLSEvidence struct {
	Checked            bool   `json:"checked"`
	Handshake          bool   `json:"handshake"`
	ServerName         string `json:"serverName,omitempty"`
	NegotiatedVersion  string `json:"negotiatedVersion,omitempty"`
	NegotiatedProtocol string `json:"negotiatedProtocol,omitempty"`
	CertificateIssuer  string `json:"certificateIssuer,omitempty"`
	CertificateExpiry  string `json:"certificateExpiry,omitempty"`
	LatencyMillis      int64  `json:"latencyMillis,omitempty"`
	FailureCode        string `json:"failureCode,omitempty"`
}

// DNSResolverEvidence is one resolver's answer. Addresses are node addresses the
// controller already knows; no other hostname is ever resolved on its behalf.
type DNSResolverEvidence struct {
	Resolver      string   `json:"resolver"`
	Addresses     []string `json:"addresses,omitempty"`
	LatencyMillis int64    `json:"latencyMillis,omitempty"`
	FailureCode   string   `json:"failureCode,omitempty"`
}

// DNSEvidence compares resolvers. Disagreement is the signal: it means the
// answer depends on who is asked, which a single lookup cannot reveal.
type DNSEvidence struct {
	Checked   bool                  `json:"checked"`
	Hostname  string                `json:"hostname,omitempty"`
	Literal   bool                  `json:"literal,omitempty"`
	Resolvers []DNSResolverEvidence `json:"resolvers,omitempty"`
	Mismatch  bool                  `json:"mismatch,omitempty"`
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
	// Method-specific evidence. Each probe fills only its own field, so an
	// observation stays as small as the question it answers.
	Throughput   *ThroughputEvidence    `json:"throughput,omitempty"`
	Latency      *LatencySeriesEvidence `json:"latencySeries,omitempty"`
	Stability    *StabilityEvidence     `json:"stability,omitempty"`
	TLS          *TLSEvidence           `json:"tls,omitempty"`
	DNS          *DNSEvidence           `json:"dns,omitempty"`
	AgentVersion string                 `json:"agentVersion"`
	Signature    []byte                 `json:"signature"`
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
