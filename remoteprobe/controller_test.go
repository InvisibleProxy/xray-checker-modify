package remoteprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/diagnostics"
	"xray-checker/models"
	"xray-checker/probeagent"
)

type controllerFixture struct {
	controller         *Controller
	registry           *probeagent.Registry
	proxyChecker       *checker.ProxyChecker
	agentID            string
	observationPrivate ed25519.PrivateKey
	now                time.Time
}

func newControllerFixture(t *testing.T) controllerFixture {
	t.Helper()
	now := time.Now().UTC()
	registry, err := probeagent.NewRegistry(probeagent.RegistryConfig{
		Path: filepath.Join(t.TempDir(), "diagnostic_agents.json"), Enabled: true,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	created, err := registry.Create(probeagent.CreateAgentRequest{
		DisplayName: "EU probe", ExpectedSourceIP: "203.0.113.40",
		ControllerIP: "198.51.100.10", ControllerURL: "https://checker.example.com",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	identityPublic, identityPrivate, _ := ed25519.GenerateKey(rand.Reader)
	observationPublic, observationPrivate, _ := ed25519.GenerateKey(rand.Reader)
	_, err = registry.Enroll(probeagent.EnrollRequest{
		ProtocolVersion: probeagent.ProtocolVersion, AgentID: created.Agent.AgentID,
		EnrollmentToken: created.EnrollmentToken, IdentityPublicKey: identityPublic,
		ObservationPublicKey: observationPublic, AgentVersion: "test",
		Capabilities: []string{"control-v1", "diagnostic-v1"},
	}, netip.MustParseAddr("203.0.113.40"))
	if err != nil {
		t.Fatalf("enroll agent: %v", err)
	}
	heartbeat := probeagent.HeartbeatRequest{
		ProtocolVersion: probeagent.ProtocolVersion, AgentID: created.Agent.AgentID,
		AgentVersion: "test", Capabilities: []string{"control-v1", "diagnostic-v1"}, Health: "healthy",
	}
	body, _ := json.Marshal(heartbeat)
	payload, _ := probeagent.ControlSigningPayload(http.MethodPost, probeagent.HeartbeatPath, created.Agent.AgentID, now, 1, body)
	if _, err := registry.AcceptHeartbeat(heartbeat, netip.MustParseAddr("203.0.113.40"), now, 1, payload, ed25519.Sign(identityPrivate, payload)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	proxy := &models.ProxyConfig{
		StableID: "node-one", Name: "Node One", Protocol: "vless", Server: "node.example.com",
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111", Security: "tls", SNI: "node.example.com",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "https://api.ipify.org?format=text", 30, "http://cp.cloudflare.com/generate_204", "https://proof.ovh.net/files/1Mb.dat", 60, 51200, "status")
	controller, err := NewController(Config{Enabled: true, CheckMethod: "status"}, registry, proxyChecker)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	return controllerFixture{controller: controller, registry: registry, proxyChecker: proxyChecker, agentID: created.Agent.AgentID, observationPrivate: observationPrivate, now: now}
}

func TestManualJobCompletesWithoutOperationalSideEffectsOrCredentialExport(t *testing.T) {
	fixture := newControllerFixture(t)
	created, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID})
	if err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	exported, err := fixture.controller.Export(created.Session.SessionID)
	if err != nil {
		t.Fatalf("export session: %v", err)
	}
	for _, secret := range []string{"11111111-1111-1111-1111-111111111111", "node.example.com", "xrayConfig"} {
		if strings.Contains(string(exported), secret) {
			t.Fatalf("session export leaked execution material %q: %s", secret, exported)
		}
	}
	observation := diagnostics.Observation{
		SchemaVersion: diagnostics.ObservationSchemaVersion, AgentID: fixture.agentID,
		SessionID: assignment.Job.SessionID, JobID: assignment.Job.JobID, Nonce: assignment.Job.Nonce,
		StableID: assignment.Job.StableID, ConfigGeneration: assignment.Job.ConfigGeneration,
		ConfigFingerprint: assignment.Job.ConfigFingerprint, CheckedAt: fixture.now.Add(time.Second),
		DurationMillis: 100, EndpointProfile: assignment.Job.Profile.ID, Status: diagnostics.ProbeStatusOnline,
		LatencyMillis: 42, DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true, LatencyMillis: 10},
		AgentVersion: "test",
	}
	payload, _ := diagnostics.ObservationSigningPayload(observation)
	observation.Signature = ed25519.Sign(fixture.observationPrivate, payload)
	if _, err := fixture.controller.AcceptObservation(observation); err != nil {
		t.Fatalf("accept observation: %v", err)
	}
	completed, ok := fixture.controller.Session(created.Session.SessionID)
	if !ok || completed.Session.State != diagnostics.SessionStateCompleted || len(completed.Session.AgentObservations) != 1 {
		t.Fatalf("completed session = %+v, ok=%v", completed, ok)
	}
	if _, err := fixture.proxyChecker.GetProxyStatusDetailsIncludingMaintenance("node-one"); err == nil {
		t.Fatal("remote observation unexpectedly created authoritative proxy status")
	}
}

func TestCreateManualRejectsConcurrentDuplicateForNodeAndAgent(t *testing.T) {
	fixture := newControllerFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID})
			results <- err
		}()
	}
	close(start)

	created := 0
	rejected := 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrActiveSession):
			rejected++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if created != 1 || rejected != 1 {
		t.Fatalf("created = %d, rejected = %d, want one of each", created, rejected)
	}
}

func TestObservationRejectedAfterConfigurationGenerationChanges(t *testing.T) {
	fixture := newControllerFixture(t)
	created, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID})
	if err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	fixture.proxyChecker.UpdateProxies(fixture.proxyChecker.GetProxies())
	observation := diagnostics.Observation{
		SchemaVersion: diagnostics.ObservationSchemaVersion, AgentID: fixture.agentID,
		SessionID: created.Session.SessionID, JobID: assignment.Job.JobID, Nonce: assignment.Job.Nonce,
		StableID: assignment.Job.StableID, ConfigGeneration: assignment.Job.ConfigGeneration,
		ConfigFingerprint: assignment.Job.ConfigFingerprint, CheckedAt: fixture.now.Add(time.Second),
		DurationMillis: 100, EndpointProfile: assignment.Job.Profile.ID, Status: diagnostics.ProbeStatusOnline,
		DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true}, AgentVersion: "test",
	}
	payload, _ := diagnostics.ObservationSigningPayload(observation)
	observation.Signature = ed25519.Sign(fixture.observationPrivate, payload)
	if _, err := fixture.controller.AcceptObservation(observation); !errors.Is(err, diagnostics.ErrStaleGeneration) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestClaimRedeliversSameBoundJobAfterLostResponse(t *testing.T) {
	fixture := newControllerFixture(t)
	if _, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID}); err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	first, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("redelivery claim: %v", err)
	}
	if second.Job.JobID != first.Job.JobID || second.Job.Nonce != first.Job.Nonce || second.Job.ConfigFingerprint != first.Job.ConfigFingerprint {
		t.Fatalf("redelivered job binding changed: first=%+v second=%+v", first.Job, second.Job)
	}
}

// The agent recomputes the fingerprint from the bytes it received, so the
// controller must hash exactly what the transport delivers. encoding/json
// compacts a json.RawMessage, which silently invalidated every job while the
// controller hashed the indented generator output instead.
func TestJobFingerprintSurvivesTheAgentTransport(t *testing.T) {
	fixture := newControllerFixture(t)
	if _, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID}); err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	wire, err := json.Marshal(probeagent.JobPollResponse{Job: assignment})
	if err != nil {
		t.Fatalf("encode job response: %v", err)
	}
	var delivered probeagent.JobPollResponse
	if err := json.Unmarshal(wire, &delivered); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	received := diagnostics.ConfigFingerprint(delivered.Job.XrayConfig)
	if received != assignment.Job.ConfigFingerprint {
		t.Fatalf("fingerprint changed in transport: job=%s agent-side=%s (%d bytes sent, %d received)",
			assignment.Job.ConfigFingerprint, received, len(assignment.XrayConfig), len(delivered.Job.XrayConfig))
	}
}

// A profile the agent cannot run must be refused by the controller. Dispatching
// it anyway would come back as a bare configuration failure, which reads to an
// operator as a fault on the node rather than a version gap in the fleet.
func TestCreateManualRejectsProfilesTheAgentCannotRun(t *testing.T) {
	fixture := newControllerFixture(t)
	if _, err := fixture.controller.CreateManual(CreateManualRequest{
		StableID: "node-one", AgentID: fixture.agentID, ProfileID: diagnostics.ProfileTLS,
	}); !errors.Is(err, ErrUnsupportedByAgent) {
		t.Fatalf("TLS profile on a v1 agent = %v, want ErrUnsupportedByAgent", err)
	}
	if _, err := fixture.controller.CreateManual(CreateManualRequest{
		StableID: "node-one", AgentID: fixture.agentID, ProfileID: "default-nonsense",
	}); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("unknown profile = %v, want ErrUnknownProfile", err)
	}
}

func TestCreateManualDispatchesTheSelectedProfileWithItsFallback(t *testing.T) {
	fixture := newControllerFixture(t)
	if _, err := fixture.controller.CreateManual(CreateManualRequest{
		StableID: "node-one", AgentID: fixture.agentID, ProfileID: diagnostics.ProfileIP,
	}); err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if assignment.Job.Profile.ID != diagnostics.ProfileIP || assignment.Job.Profile.Method != diagnostics.ProbeMethodIP {
		t.Fatalf("dispatched profile = %+v, want the requested IP profile", assignment.Job.Profile)
	}
	// The fallback is only useful when the agent can actually run it, so it is
	// populated from the same capability check.
	if assignment.Job.Profile.AlternativeProfileID != diagnostics.ProfileStatus {
		t.Fatalf("alternative profile = %q, want %q", assignment.Job.Profile.AlternativeProfileID, diagnostics.ProfileStatus)
	}
}

// An empty selection must keep behaving like the pre-selection workflow.
func TestCreateManualFallsBackToTheConfiguredCheckMethod(t *testing.T) {
	fixture := newControllerFixture(t)
	if _, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID}); err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if assignment.Job.Profile.ID != diagnostics.ProfileStatus {
		t.Fatalf("default profile = %q, want %q for check method \"status\"", assignment.Job.Profile.ID, diagnostics.ProfileStatus)
	}
}

func TestProfilesMarkTheControllerDefault(t *testing.T) {
	fixture := newControllerFixture(t)
	profiles := fixture.controller.Profiles()
	if len(profiles) != len(diagnostics.Profiles()) {
		t.Fatalf("exposed %d profiles, want the whole catalogue", len(profiles))
	}
	defaults := 0
	for _, profile := range profiles {
		if profile.Default {
			defaults++
			if profile.ID != diagnostics.ProfileStatus {
				t.Errorf("default profile = %q, want %q", profile.ID, diagnostics.ProfileStatus)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("marked %d defaults, want exactly one", defaults)
	}
}

// Deleting a session must also drop its queued assignment: an agent still
// holding that job would run a probe whose result can never be accepted.
func TestDeleteRemovesTheSessionAndItsQueuedAssignment(t *testing.T) {
	fixture := newControllerFixture(t)
	created, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID})
	if err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	if err := fixture.controller.Delete(created.Session.SessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if len(fixture.controller.Sessions("")) != 0 {
		t.Fatalf("session survived deletion: %+v", fixture.controller.Sessions(""))
	}
	// claimNow rather than Claim: the public path long-polls for 15s when the
	// queue is empty, which is exactly the state under test.
	assignment, _, err := fixture.controller.claimNow(fixture.agentID)
	if err != nil || assignment != nil {
		t.Fatalf("claim after deletion returned %+v (%v), want no pending job", assignment, err)
	}
	if err := fixture.controller.Delete(created.Session.SessionID); !errors.Is(err, diagnostics.ErrUnknownSession) {
		t.Fatalf("second delete = %v, want ErrUnknownSession", err)
	}
}

func TestClearRemovesOnlyTheRequestedNode(t *testing.T) {
	fixture := newControllerFixture(t)
	if _, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID}); err != nil {
		t.Fatalf("create manual diagnostics: %v", err)
	}
	if removed := fixture.controller.Clear("node-two"); removed != 0 {
		t.Fatalf("clearing another node removed %d sessions", removed)
	}
	if len(fixture.controller.Sessions("")) != 1 {
		t.Fatal("clearing another node discarded this node's session")
	}
	if removed := fixture.controller.Clear("node-one"); removed != 1 {
		t.Fatalf("cleared %d sessions, want 1", removed)
	}
	if len(fixture.controller.Sessions("")) != 0 {
		t.Fatal("session survived the clear")
	}
	// An empty StableID is the deliberate "everything" case.
	if removed := fixture.controller.Clear(""); removed != 0 {
		t.Fatalf("clearing an empty store removed %d sessions", removed)
	}
}
