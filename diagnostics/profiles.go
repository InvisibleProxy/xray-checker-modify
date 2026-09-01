package diagnostics

import "strings"

// Agent capabilities gating profile dispatch. An agent that predates a profile
// must never be handed it: the job would be rejected at the configuration stage
// with no useful evidence, which reads to an operator as a network fault.
const (
	CapabilityControlV1    = "control-v1"
	CapabilityDiagnosticV1 = "diagnostic-v1"
	CapabilityDiagnosticV2 = "diagnostic-v2"
)

// ProfileDescriptor is the single catalogue shared by the controller, the agent
// and the admin UI. Profiles are addressed by ID and never by URL, so selecting
// one cannot turn a diagnostic job into an arbitrary fetch; the endpoints behind
// these IDs belong to the agent's own configuration.
type ProfileDescriptor struct {
	ID     string      `json:"id"`
	Method ProbeMethod `json:"method"`
	// Capability the agent must advertise before this profile may be dispatched.
	Capability string `json:"capability"`
	Label      string `json:"label"`
	Summary    string `json:"summary"`
	// Tunnelled probes answer "does traffic flow through this node"; transport
	// probes answer "is the node reachable at all", which is what separates a
	// broken tunnel from a broken path.
	Tunnelled bool `json:"tunnelled"`
}

const (
	ProfileIP        = "default-ip"
	ProfileStatus    = "default-status"
	ProfileDownload  = "default-download"
	ProfileLatency   = "default-latency"
	ProfileStability = "default-stability"
	ProfileTLS       = "default-tls"
	ProfileDNS       = "default-dns"
)

var profileCatalogue = []ProfileDescriptor{
	{
		ID: ProfileStatus, Method: ProbeMethodStatus, Capability: CapabilityDiagnosticV1,
		Label:     "Endpoint status",
		Summary:   "Fetches the agent's status endpoint through the tunnel and checks the response code.",
		Tunnelled: true,
	},
	{
		ID: ProfileIP, Method: ProbeMethodIP, Capability: CapabilityDiagnosticV1,
		Label:     "Exit IP",
		Summary:   "Compares the tunnelled exit IP with the agent's direct IP, so traffic bypassing the tunnel is caught.",
		Tunnelled: true,
	},
	{
		ID: ProfileDownload, Method: ProbeMethodDownload, Capability: CapabilityDiagnosticV1,
		Label:     "Download throughput",
		Summary:   "Transfers a fixed amount through the tunnel and reports the achieved rate.",
		Tunnelled: true,
	},
	{
		ID: ProfileLatency, Method: ProbeMethodLatency, Capability: CapabilityDiagnosticV2,
		Label:     "Latency profile",
		Summary:   "Repeats a short tunnelled request and reports median, p95 and jitter instead of a single sample.",
		Tunnelled: true,
	},
	{
		ID: ProfileStability, Method: ProbeMethodStability, Capability: CapabilityDiagnosticV2,
		Label:     "Connection stability",
		Summary:   "Holds one tunnelled transfer open to catch filtering that drops a session after a delay.",
		Tunnelled: true,
	},
	{
		ID: ProfileTLS, Method: ProbeMethodTLS, Capability: CapabilityDiagnosticV2,
		Label:     "TLS handshake",
		Summary:   "Completes a direct TLS handshake with the node's SNI, without the tunnel, to locate SNI-based interference.",
		Tunnelled: false,
	},
	{
		ID: ProfileDNS, Method: ProbeMethodDNS, Capability: CapabilityDiagnosticV2,
		Label:     "DNS resolution",
		Summary:   "Resolves the node hostname through several resolvers and reports disagreement between them.",
		Tunnelled: false,
	},
}

// Profiles returns the catalogue in presentation order.
func Profiles() []ProfileDescriptor {
	return append([]ProfileDescriptor(nil), profileCatalogue...)
}

// ProfileByID resolves a requested profile. An unknown ID is rejected here
// rather than reaching the agent, which cannot report anything more useful than
// a configuration failure.
func ProfileByID(id string) (ProfileDescriptor, bool) {
	id = strings.TrimSpace(id)
	for _, descriptor := range profileCatalogue {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return ProfileDescriptor{}, false
}

// ProfileForCheckMethod maps the controller's configured availability check
// method onto the equivalent profile, which stays the default selection.
func ProfileForCheckMethod(method string) (ProfileDescriptor, bool) {
	switch strings.TrimSpace(method) {
	case "ip":
		return ProfileByID(ProfileIP)
	case "status":
		return ProfileByID(ProfileStatus)
	case "download":
		return ProfileByID(ProfileDownload)
	default:
		return ProfileDescriptor{}, false
	}
}

// TestProfileFor builds the wire profile, optionally pairing it with a fallback
// endpoint so a single unreachable endpoint is not reported as a node failure.
func (d ProfileDescriptor) TestProfileFor(alternativeID string) TestProfile {
	return TestProfile{ID: d.ID, Method: d.Method, AlternativeProfileID: strings.TrimSpace(alternativeID)}
}

// AlternativeFor returns the endpoint a failed tunnelled probe should retry
// against. Only tunnelled HTTP probes have a meaningful alternative: a TLS or
// DNS failure is about the node itself, not about the endpoint being fetched.
func AlternativeFor(id string) (string, bool) {
	switch id {
	case ProfileStatus:
		return ProfileIP, true
	case ProfileIP:
		return ProfileStatus, true
	case ProfileDownload:
		return ProfileStatus, true
	case ProfileLatency:
		return ProfileStatus, true
	default:
		return "", false
	}
}
