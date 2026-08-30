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
