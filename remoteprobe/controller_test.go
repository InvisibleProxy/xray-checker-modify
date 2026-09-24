package remoteprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
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
	resolver           *fakeResolver
	agentID            string
	observationPrivate ed25519.PrivateKey
	now                time.Time
}

// fakeResolver answers from a fixed table, so the tests that decide where an
// agent runs never depend on real DNS.
type fakeResolver struct {
	hosts   map[string][]string
	lookups int
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.lookups++
	addresses := make([]net.IPAddr, 0, len(f.hosts[host]))
	for _, value := range f.hosts[host] {
		addresses = append(addresses, net.IPAddr{IP: net.ParseIP(value)})
	}
	if len(addresses) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addresses, nil
}

// bringAgentOnline enrolls a created agent and lands one signed heartbeat, so
// the registry reports it connected and healthy. It is separate from the
// fixture because a test that is about choosing between vantage points needs
// more than one of them.
func bringAgentOnline(t *testing.T, registry *probeagent.Registry, created probeagent.CreationResult, sourceIP string, now time.Time) ed25519.PrivateKey {
	t.Helper()
	identityPublic, identityPrivate, _ := ed25519.GenerateKey(rand.Reader)
	observationPublic, observationPrivate, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := registry.Enroll(probeagent.EnrollRequest{
		ProtocolVersion: probeagent.ProtocolVersion, AgentID: created.Agent.AgentID,
		EnrollmentToken: created.EnrollmentToken, IdentityPublicKey: identityPublic,
		ObservationPublicKey: observationPublic, AgentVersion: "test",
		Capabilities: []string{"control-v1", "diagnostic-v1"},
	}, netip.MustParseAddr(sourceIP)); err != nil {
		t.Fatalf("enroll agent: %v", err)
	}
	heartbeat := probeagent.HeartbeatRequest{
		ProtocolVersion: probeagent.ProtocolVersion, AgentID: created.Agent.AgentID,
		AgentVersion: "test", Capabilities: []string{"control-v1", "diagnostic-v1"}, Health: "healthy",
	}
	body, _ := json.Marshal(heartbeat)
	payload, _ := probeagent.ControlSigningPayload(http.MethodPost, probeagent.HeartbeatPath, created.Agent.AgentID, now, 1, body)
	if _, err := registry.AcceptHeartbeat(heartbeat, netip.MustParseAddr(sourceIP), now, 1, payload, ed25519.Sign(identityPrivate, payload)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	return observationPrivate
}

func newControllerFixture(t *testing.T) controllerFixture {
	t.Helper()
	return newControllerFixtureForServer(t, "node.example.com")
}

// newControllerFixtureForServer publishes the node under the given server. The
// name the default fixture uses resolves to an address no agent has, so a test
// only meets an agent on the node's host when it puts one there.
func newControllerFixtureForServer(t *testing.T, server string) controllerFixture {
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
	observationPrivate := bringAgentOnline(t, registry, created, "203.0.113.40", now)
	proxy := &models.ProxyConfig{
		StableID: "node-one", Name: "Node One", Protocol: "vless", Server: server,
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111", Security: "tls", SNI: "node.example.com",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "https://api.ipify.org?format=text", 30, "http://cp.cloudflare.com/generate_204", "https://proof.ovh.net/files/1Mb.dat", 60, 51200, "status")
	resolver := &fakeResolver{hosts: map[string][]string{"node.example.com": {"198.51.100.20"}}}
	controller, err := NewController(Config{Enabled: true, CheckMethod: "status", Resolver: resolver}, registry, proxyChecker)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	return controllerFixture{controller: controller, registry: registry, proxyChecker: proxyChecker, resolver: resolver, agentID: created.Agent.AgentID, observationPrivate: observationPrivate, now: now}
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

func TestCreateAutomaticSelectsAHealthyAgentAndBindsSpeedFallbackContext(t *testing.T) {
	fixture := newControllerFixture(t)
	request := CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoSpeedFallback, ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeTechnical,
			Source: "schedule", ThresholdMbps: 10, FallbackAttempts: 2,
		},
	}
	created, err := fixture.controller.CreateAutomatic(request)
	if err != nil {
		t.Fatalf("create automatic diagnostics: %v", err)
	}
	if created.Session.Trigger != diagnostics.TriggerAutoSpeedFallback || created.Session.AutomationContext != request.AutomationContext {
		t.Fatalf("automatic session = %+v", created.Session)
	}
	if len(created.Session.RequestedAgents) != 1 || created.Session.RequestedAgents[0] != fixture.agentID {
		t.Fatalf("selected agents = %+v", created.Session.RequestedAgents)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim automatic job: %v", err)
	}
	if assignment.Job.Profile.ID != diagnostics.ProfileDownload || assignment.Job.Profile.AlternativeProfileID != diagnostics.ProfileStatus {
		t.Fatalf("automatic profile = %+v", assignment.Job.Profile)
	}
}

func TestCreateAutomaticAcceptsTheProxyFailureTriggerAndRefusesTheUnimplementedOnes(t *testing.T) {
	fixture := newControllerFixture(t)
	created, err := fixture.controller.CreateAutomatic(CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoProxyFailure, ProfileID: diagnostics.ProfileStatus,
		AutomationContext: diagnostics.ProxyFailureAutomationContext(),
	})
	if err != nil {
		t.Fatalf("create proxy failure diagnostics: %v", err)
	}
	if created.Session.Trigger != diagnostics.TriggerAutoProxyFailure {
		t.Fatalf("automatic session = %+v", created.Session)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim automatic job: %v", err)
	}
	if assignment.Job.Profile.ID != diagnostics.ProfileStatus || assignment.Job.Profile.AlternativeProfileID != diagnostics.ProfileIP {
		t.Fatalf("automatic profile = %+v", assignment.Job.Profile)
	}

	for _, trigger := range []diagnostics.Trigger{diagnostics.TriggerManual, diagnostics.TriggerAutoCheckEndpoint, diagnostics.TriggerReachabilitySweep} {
		if _, err := newControllerFixture(t).controller.CreateAutomatic(CreateAutomaticRequest{
			StableID: "node-one", Trigger: trigger, ProfileID: diagnostics.ProfileStatus,
		}); err == nil {
			t.Fatalf("automatic session with trigger %q was accepted", trigger)
		}
	}
}

// A tunnel that fails for the agent at another stage than for the checker still
// fails. The generic comparison, which wants equal failure codes, would call
// that "no stable pattern" and hide the one answer this trigger exists for.
func TestProxyFailureSummaryReadsAnyRemoteTunnelFailureAsReproduced(t *testing.T) {
	session := diagnostics.DiagnosticSession{
		Trigger:             diagnostics.TriggerAutoProxyFailure,
		LocalResultSnapshot: diagnostics.LocalResultSnapshot{Status: diagnostics.ProbeStatusProxyFailure, Failure: diagnostics.FailureEvidence{Code: "proxy_timeout"}},
		AgentObservations: []diagnostics.AcceptedObservation{{Reliable: true, Observation: diagnostics.Observation{
			Status: diagnostics.ProbeStatusProxyFailure, Failure: diagnostics.FailureEvidence{Code: "http_status"},
			DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
		}}},
	}
	if summary := summarize(session); !strings.Contains(summary, "proxy failure was reproduced") {
		t.Fatalf("summary = %q, want the failure reproduced", summary)
	}

	session.AgentObservations[0].Observation.AlternativeEndpoint = &diagnostics.AlternativeEndpointObservation{Status: diagnostics.ProbeStatusOnline}
	if summary := summarize(session); !strings.Contains(summary, "endpoint-specific") {
		t.Fatalf("summary = %q, want an endpoint-specific reading", summary)
	}

	session.AgentObservations[0].Observation = diagnostics.Observation{
		Status: diagnostics.ProbeStatusOnline, DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
	}
	if summary := summarize(session); !strings.Contains(summary, "not reproduced") {
		t.Fatalf("summary = %q, want the failure not reproduced", summary)
	}
}

func TestCreateAutomaticSkipsProjectAndNodeMaintenance(t *testing.T) {
	request := CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoSpeedFallback, ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeLowSpeed,
			Source: "schedule", ThresholdMbps: 10, ObservedMbps: 2, FallbackAttempts: 1,
		},
	}

	projectFixture := newControllerFixture(t)
	projectFixture.proxyChecker.SetProjectMaintenance(true)
	if _, err := projectFixture.controller.CreateAutomatic(request); !errors.Is(err, ErrAutomaticPaused) {
		t.Fatalf("project maintenance error = %v, want ErrAutomaticPaused", err)
	}

	nodeFixture := newControllerFixture(t)
	if err := nodeFixture.proxyChecker.SetMaintenanceMode("node-one", true); err != nil {
		t.Fatalf("enable node maintenance: %v", err)
	}
	if _, err := nodeFixture.controller.CreateAutomatic(request); !errors.Is(err, ErrAutomaticPaused) {
		t.Fatalf("node maintenance error = %v, want ErrAutomaticPaused", err)
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

type fakeAgentPreference struct {
	order    []string
	asked    []string
	stableID string
}

func (f *fakeAgentPreference) PreferAgents(stableID string, agentIDs []string) []string {
	f.stableID = stableID
	f.asked = append([]string(nil), agentIDs...)
	return f.order
}

// Liveness order answers "who is free", which is the wrong question for an
// automatic diagnostic: an agent that has never reached this node cannot settle
// anything about it. When a preference is configured its ranking wins over the
// order the registry happened to return.
func TestCreateAutomaticAsksTheVantagePointThePreferenceRanksFirst(t *testing.T) {
	fixture := newControllerFixture(t)
	second, err := fixture.registry.Create(probeagent.CreateAgentRequest{
		DisplayName: "US probe", ExpectedSourceIP: "203.0.113.41",
		ControllerIP: "198.51.100.10", ControllerURL: "https://checker.example.com",
	})
	if err != nil {
		t.Fatalf("create second agent: %v", err)
	}
	bringAgentOnline(t, fixture.registry, second, "203.0.113.41", fixture.now)

	// Both agents last checked in at the same instant, so liveness order falls
	// through to its agent-id tie-break. Rank the other one first.
	livenessWinner := fixture.agentID
	if second.Agent.AgentID < livenessWinner {
		livenessWinner = second.Agent.AgentID
	}
	preferred := fixture.agentID
	if preferred == livenessWinner {
		preferred = second.Agent.AgentID
	}

	preference := &fakeAgentPreference{order: []string{preferred, livenessWinner}}
	controller, err := NewController(Config{Enabled: true, CheckMethod: "status", AgentPreference: preference, Resolver: fixture.resolver}, fixture.registry, fixture.proxyChecker)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	created, err := controller.CreateAutomatic(CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoSpeedFallback, ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeLowSpeed,
			Source: "schedule", ThresholdMbps: 100, ObservedMbps: 37, FallbackAttempts: 1,
		},
	})
	if err != nil {
		t.Fatalf("create automatic diagnostics: %v", err)
	}
	if len(created.Session.RequestedAgents) != 1 || created.Session.RequestedAgents[0] != preferred {
		t.Fatalf("selected agents = %+v, want the preferred %q", created.Session.RequestedAgents, preferred)
	}
	if preference.stableID != "node-one" {
		t.Errorf("preference was asked about %q, want the node being diagnosed", preference.stableID)
	}
	if len(preference.asked) != 2 {
		t.Errorf("preference saw %d candidates, want both free agents", len(preference.asked))
	}
}

// A preference that ranks nothing usable is the normal state before the first
// sweep, and it must not cost the node its diagnostic.
func TestCreateAutomaticFallsBackToLivenessOrderWhenThePreferenceRanksNothing(t *testing.T) {
	fixture := newControllerFixture(t)
	preference := &fakeAgentPreference{order: []string{"agent-that-does-not-exist"}}
	controller, err := NewController(Config{Enabled: true, CheckMethod: "status", AgentPreference: preference, Resolver: fixture.resolver}, fixture.registry, fixture.proxyChecker)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	created, err := controller.CreateAutomatic(CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoSpeedFallback, ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeLowSpeed,
			Source: "schedule", ThresholdMbps: 100, ObservedMbps: 37, FallbackAttempts: 1,
		},
	})
	if err != nil {
		t.Fatalf("create automatic diagnostics: %v", err)
	}
	if len(created.Session.RequestedAgents) != 1 || created.Session.RequestedAgents[0] != fixture.agentID {
		t.Fatalf("selected agents = %+v, want the only free agent", created.Session.RequestedAgents)
	}
}

func speedFallbackRequest() CreateAutomaticRequest {
	return CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoSpeedFallback, ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeLowSpeed,
			Source: "schedule", ThresholdMbps: 100, ObservedMbps: 37, FallbackAttempts: 1,
		},
	}
}

// An agent on the node's own host reaches the node without leaving the machine,
// so its answer says nothing about the path a client takes. It is also the
// fastest vantage point that node has, which is why a ranking would pick it: here
// the preference puts it first and is overruled.
func TestCreateAutomaticNeverAsksTheAgentOnTheNodeOwnHost(t *testing.T) {
	fixture := newControllerFixture(t)
	onHost, err := fixture.registry.Create(probeagent.CreateAgentRequest{
		DisplayName: "Node host probe", ExpectedSourceIP: "198.51.100.20",
		ControllerIP: "198.51.100.10", ControllerURL: "https://checker.example.com",
	})
	if err != nil {
		t.Fatalf("create agent on the node host: %v", err)
	}
	bringAgentOnline(t, fixture.registry, onHost, "198.51.100.20", fixture.now)

	preference := &fakeAgentPreference{order: []string{onHost.Agent.AgentID, fixture.agentID}}
	controller, err := NewController(Config{Enabled: true, CheckMethod: "status", AgentPreference: preference, Resolver: fixture.resolver}, fixture.registry, fixture.proxyChecker)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	created, err := controller.CreateAutomatic(speedFallbackRequest())
	if err != nil {
		t.Fatalf("create automatic diagnostics: %v", err)
	}
	if got := created.Session.RequestedAgents; len(got) != 1 || got[0] != fixture.agentID {
		t.Fatalf("selected agents = %+v, want %q rather than the agent on the node host", got, fixture.agentID)
	}
}

// With only the node's own host free, the automatic diagnostic is refused rather
// than run from there, and refused by name, so an alert does not claim that no
// agent was connected. Manual diagnostics and the sweep name their agent
// themselves and keep working with that very agent.
func TestCreateAutomaticRefusesWhenOnlyTheNodeHostAgentIsIdle(t *testing.T) {
	fixture := newControllerFixture(t)
	// Several addresses, the agent's among them in its IPv4-mapped form.
	fixture.resolver.hosts["node.example.com"] = []string{"2001:db8::20", "::ffff:203.0.113.40"}

	if _, err := fixture.controller.CreateAutomatic(speedFallbackRequest()); !errors.Is(err, ErrOnlyNodeHostAgent) {
		t.Fatalf("automatic diagnostics error = %v, want ErrOnlyNodeHostAgent", err)
	}
	if sessions := fixture.controller.Sessions(""); len(sessions) != 0 {
		t.Fatalf("a refused automatic diagnostic left sessions behind: %+v", sessions)
	}

	manual, err := fixture.controller.CreateManual(CreateManualRequest{StableID: "node-one", AgentID: fixture.agentID})
	if err != nil {
		t.Fatalf("manual diagnostics from the node host agent: %v", err)
	}
	// The manual job would otherwise hold the node and agent pair.
	if err := fixture.controller.Delete(manual.Session.SessionID); err != nil {
		t.Fatalf("delete manual session: %v", err)
	}
	if _, err := fixture.controller.CreateSweep(CreateSweepRequest{StableID: "node-one", AgentID: fixture.agentID}); err != nil {
		t.Fatalf("sweep from the node host agent: %v", err)
	}
}

// A node published as an address is compared as published. Only a name costs a
// lookup, so the addresses subscriptions publish today need no DNS at all.
func TestCreateAutomaticComparesAPublishedAddressWithoutALookup(t *testing.T) {
	fixture := newControllerFixtureForServer(t, "203.0.113.40")
	if _, err := fixture.controller.CreateAutomatic(speedFallbackRequest()); !errors.Is(err, ErrOnlyNodeHostAgent) {
		t.Fatalf("automatic diagnostics error = %v, want ErrOnlyNodeHostAgent", err)
	}
	if fixture.resolver.lookups != 0 {
		t.Fatalf("resolver was asked %d times about a node published as an address", fixture.resolver.lookups)
	}
}

// Without the node's addresses there is no telling whether an agent runs on its
// host, and letting the diagnostic through would guess in the one direction the
// rule exists to prevent. The refusal does not carry the server's name, which is
// execution material.
func TestCreateAutomaticRefusesANodeWhoseNameDoesNotResolve(t *testing.T) {
	fixture := newControllerFixture(t)
	delete(fixture.resolver.hosts, "node.example.com")

	_, err := fixture.controller.CreateAutomatic(speedFallbackRequest())
	if !errors.Is(err, ErrNodeAddressUnresolved) {
		t.Fatalf("automatic diagnostics error = %v, want ErrNodeAddressUnresolved", err)
	}
	if strings.Contains(err.Error(), "node.example.com") {
		t.Fatalf("refusal leaked the node's server: %v", err)
	}
	if sessions := fixture.controller.Sessions(""); len(sessions) != 0 {
		t.Fatalf("a refused automatic diagnostic left sessions behind: %+v", sessions)
	}
}

// The agent measures what the run measured, so the two rates can be compared.
// A technical failure transferred whatever it managed before giving up, and
// copying that would measure the checker's timeout rather than the node.
func TestAutomaticDownloadProfileCopiesTheRunTransferSizeOnlyForASlowdown(t *testing.T) {
	descriptor, _ := diagnostics.ProfileByID(diagnostics.ProfileDownload)
	for _, test := range []struct {
		name    string
		context diagnostics.AutomationContext
		want    int64
	}{
		{
			name:    "a slowdown carries a comparable size",
			context: diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeLowSpeed, MeasuredBytes: 100_000_000},
			want:    100_000_000,
		},
		{
			name:    "a technical failure keeps the agent's own amount",
			context: diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeTechnical, MeasuredBytes: 12_000},
		},
		{
			name:    "a size below the floor is not worth asking for",
			context: diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeLowSpeed, MeasuredBytes: 12_000},
		},
		{
			name:    "a size above the ceiling is refused rather than clamped",
			context: diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeLowSpeed, MeasuredBytes: 900_000_000},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := automaticProfile(descriptor, diagnostics.ProfileStatus, test.context)
			if profile.DownloadBytes != test.want {
				t.Fatalf("download bytes = %d, want %d", profile.DownloadBytes, test.want)
			}
			if profile.ID != diagnostics.ProfileDownload || profile.AlternativeProfileID != diagnostics.ProfileStatus {
				t.Fatalf("profile = %+v, want the download profile with its fallback", profile)
			}
		})
	}
}
