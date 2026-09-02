package probeagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryEnrollmentIsIPBoundOneUseAndPersistsOnlyTokenHash(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	registry, path := newTestRegistry(t, now)
	created, err := registry.Create(testCreateRequest())
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if strings.Contains(string(data), created.EnrollmentToken) {
		t.Fatal("one-time enrollment token was persisted in plaintext")
	}
	if !strings.Contains(created.Compose, created.EnrollmentToken) {
		t.Fatal("generated Compose does not contain the one-time enrollment token")
	}

	identityPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	observationPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	request := EnrollRequest{
		ProtocolVersion: ProtocolVersion, AgentID: created.Agent.AgentID,
		EnrollmentToken: created.EnrollmentToken, IdentityPublicKey: identityPublic,
		ObservationPublicKey: observationPublic, AgentVersion: "test", Capabilities: []string{"control-v1"},
	}
	if _, err := registry.Enroll(request, netip.MustParseAddr("203.0.113.41")); !errors.Is(err, ErrSourceIPMismatch) {
		t.Fatalf("wrong source IP error = %v, want ErrSourceIPMismatch", err)
	}
	if _, err := registry.Enroll(request, netip.MustParseAddr("203.0.113.40")); err != nil {
		t.Fatalf("enroll from expected IP: %v", err)
	}
	if _, err := registry.Enroll(request, netip.MustParseAddr("203.0.113.40")); !errors.Is(err, ErrEnrollmentUsed) {
		t.Fatalf("reused token error = %v, want ErrEnrollmentUsed", err)
	}
	snapshot := registry.Snapshot()[0]
	if !snapshot.IdentityConfigured || !snapshot.ObservationKeySet || snapshot.EnrolledAt.IsZero() {
		t.Fatalf("enrolled snapshot is incomplete: %+v", snapshot)
	}
}

func TestHeartbeatSignatureSequenceAndReplaySurviveRegistryRestart(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	registry, path := newTestRegistry(t, now)
	created, identityPrivate := enrollTestAgent(t, registry)
	request := HeartbeatRequest{ProtocolVersion: ProtocolVersion, AgentID: created.Agent.AgentID, AgentVersion: "test", Capabilities: []string{"control-v1"}, Health: "healthy"}
	body, _ := json.Marshal(request)
	payload, _ := ControlSigningPayload("POST", HeartbeatPath, request.AgentID, now, 1, body)
	signature := ed25519.Sign(identityPrivate, payload)
	if _, err := registry.AcceptHeartbeat(request, netip.MustParseAddr("203.0.113.40"), now, 1, payload, signature); err != nil {
		t.Fatalf("accept heartbeat: %v", err)
	}

	restarted, err := NewRegistry(RegistryConfig{Path: path, Enabled: true, AgentImage: "agent:test", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if _, err := restarted.AcceptHeartbeat(request, netip.MustParseAddr("203.0.113.40"), now, 1, payload, signature); !errors.Is(err, ErrControlReplay) {
		t.Fatalf("replayed heartbeat error = %v, want ErrControlReplay", err)
	}
	payload2, _ := ControlSigningPayload("POST", HeartbeatPath, request.AgentID, now, 2, body)
	if _, err := restarted.AcceptHeartbeat(request, netip.MustParseAddr("203.0.113.41"), now, 2, payload2, ed25519.Sign(identityPrivate, payload2)); !errors.Is(err, ErrSourceIPMismatch) {
		t.Fatalf("wrong heartbeat source error = %v, want ErrSourceIPMismatch", err)
	}
}

func TestReissueClearsOldIdentityAndRevocationFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	registry, _ := newTestRegistry(t, now)
	created, _ := enrollTestAgent(t, registry)
	reissued, err := registry.Reissue(created.Agent.AgentID)
	if err != nil {
		t.Fatalf("reissue enrollment: %v", err)
	}
	if reissued.EnrollmentToken == created.EnrollmentToken || reissued.Agent.IdentityConfigured || !reissued.Agent.EnrolledAt.IsZero() {
		t.Fatalf("reissued agent retained old identity: %+v", reissued.Agent)
	}
	if strings.SplitN(reissued.Compose, "\n", 2)[0] == strings.SplitN(created.Compose, "\n", 2)[0] {
		t.Fatal("reissued Compose reused the old project and identity volume")
	}
	revoked, err := registry.Revoke(created.Agent.AgentID)
	if err != nil {
		t.Fatalf("revoke agent: %v", err)
	}
	if revoked.Enabled || revoked.RevokedAt.IsZero() || !revoked.Revoked {
		t.Fatalf("revoked snapshot is invalid: %+v", revoked)
	}
}

func TestDeleteRequiresRevocationAndPersistsRemoval(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	registry, path := newTestRegistry(t, now)
	created, err := registry.Create(testCreateRequest())
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := registry.Delete(created.Agent.AgentID); !errors.Is(err, ErrAgentNotRevoked) {
		t.Fatalf("delete live agent error = %v, want ErrAgentNotRevoked", err)
	}
	if _, ok := registry.Agent(created.Agent.AgentID); !ok {
		t.Fatal("failed live-agent deletion removed the registry record")
	}
	if _, err := registry.Revoke(created.Agent.AgentID); err != nil {
		t.Fatalf("revoke agent: %v", err)
	}
	if err := registry.Delete(created.Agent.AgentID); err != nil {
		t.Fatalf("delete revoked agent: %v", err)
	}
	if _, ok := registry.Agent(created.Agent.AgentID); ok {
		t.Fatal("deleted agent remains in the in-memory registry")
	}

	restarted, err := NewRegistry(RegistryConfig{Path: path, Enabled: true, AgentImage: "agent:test", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new restarted registry: %v", err)
	}
	if err := restarted.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if len(restarted.Snapshot()) != 0 {
		t.Fatalf("deleted agent returned after restart: %+v", restarted.Snapshot())
	}
	if err := restarted.Delete(created.Agent.AgentID); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("delete missing agent error = %v, want ErrAgentNotFound", err)
	}
}

// encoding/json never omits a struct, so revokedAt is encoded as the Go zero
// time even for a live agent. Clients must be able to rely on revoked instead.
func TestSnapshotJSONReportsRevokedIndependentlyOfZeroRevokedAt(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	registry, _ := newTestRegistry(t, now)
	created, err := registry.Create(testCreateRequest())
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	decoded := decodeSnapshotJSON(t, created.Agent)
	if decoded["revoked"] != false {
		t.Fatalf("live agent reported revoked = %v, want false", decoded["revoked"])
	}
	if decoded["revokedAt"] == nil {
		t.Fatal("revokedAt is omitted for a live agent; the revoked flag is no longer needed")
	}
	revoked, err := registry.Revoke(created.Agent.AgentID)
	if err != nil {
		t.Fatalf("revoke agent: %v", err)
	}
	if decodeSnapshotJSON(t, revoked)["revoked"] != true {
		t.Fatal("revoked agent did not report revoked = true")
	}
}

func decodeSnapshotJSON(t *testing.T, snapshot AgentSnapshot) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode agent snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode agent snapshot: %v", err)
	}
	return decoded
}

func TestDecodeRegistryMigratesVersionZero(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	data := []byte(`{"version":0,"updatedAt":"2026-08-30T01:02:03Z","agents":{"agent_legacy":{"displayName":"Legacy","expectedSourceIp":"203.0.113.40","controllerIp":"198.51.100.10","controllerUrl":"https://checker.example.com","enabled":true,"createdAt":"2026-08-30T01:02:03Z","updatedAt":"2026-08-30T01:02:03Z"}}}`)
	state, err := DecodeRegistry(data)
	if err != nil {
		t.Fatalf("decode v0 registry: %v", err)
	}
	if state.Version != RegistryVersion || state.UpdatedAt != now || state.Agents["agent_legacy"].AgentID != "agent_legacy" {
		t.Fatalf("unexpected migrated state: %+v", state)
	}
}

func newTestRegistry(t *testing.T, now time.Time) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "diagnostic_agents.json")
	registry, err := NewRegistry(RegistryConfig{
		Path: path, Enabled: true, AgentImage: "agent:test",
		EnrollmentTTL: 15 * time.Minute, HeartbeatMaxSkew: 2 * time.Minute,
		HeartbeatIntervalSec: 30, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, path
}

func testCreateRequest() CreateAgentRequest {
	return CreateAgentRequest{
		DisplayName: "EU probe 1", ExpectedSourceIP: "203.0.113.40",
		ControllerIP: "198.51.100.10", ControllerURL: "https://checker.example.com",
		Region: "DE", Provider: "example", NetworkGroup: "eu",
	}
}

func enrollTestAgent(t *testing.T, registry *Registry) (CreationResult, ed25519.PrivateKey) {
	t.Helper()
	created, err := registry.Create(testCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	identityPublic, identityPrivate, _ := ed25519.GenerateKey(rand.Reader)
	observationPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err = registry.Enroll(EnrollRequest{
		ProtocolVersion: ProtocolVersion, AgentID: created.Agent.AgentID,
		EnrollmentToken: created.EnrollmentToken, IdentityPublicKey: identityPublic,
		ObservationPublicKey: observationPublic, AgentVersion: "test", Capabilities: []string{"control-v1"},
	}, netip.MustParseAddr("203.0.113.40"))
	if err != nil {
		t.Fatal(err)
	}
	return created, identityPrivate
}

// The controller's own address is the same for every agent, so an operator
// should state it once instead of retyping it per probe.
func TestCreateFillsTheControllerAddressFromTheConfiguredDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostic_agents.json")
	registry, err := NewRegistry(RegistryConfig{
		Path: path, Enabled: true, AgentImage: "agent:test",
		DefaultControllerURL: "https://checker.example.com/",
		DefaultControllerIP:  "198.51.100.10",
		EnrollmentTTL:        15 * time.Minute, HeartbeatMaxSkew: 2 * time.Minute,
		HeartbeatIntervalSec: 30,
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	created, err := registry.Create(CreateAgentRequest{DisplayName: "EU probe 1", ExpectedSourceIP: "203.0.113.40"})
	if err != nil {
		t.Fatalf("create without a controller address: %v", err)
	}
	// The trailing slash is normalised the same way a typed value would be.
	if created.Agent.ControllerURL != "https://checker.example.com" || created.Agent.ControllerIP != "198.51.100.10" {
		t.Fatalf("agent = %+v, want the configured controller address", created.Agent)
	}
	url, ip := registry.ControllerDefaults()
	if url != "https://checker.example.com" || ip != "198.51.100.10" {
		t.Fatalf("defaults = %q/%q, want the normalised configured values", url, ip)
	}
}

// A controller reachable at a second address must stay expressible without
// changing the configured default.
func TestCreateKeepsAnExplicitControllerAddressOverTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostic_agents.json")
	registry, err := NewRegistry(RegistryConfig{
		Path: path, Enabled: true, AgentImage: "agent:test",
		DefaultControllerURL: "https://checker.example.com",
		DefaultControllerIP:  "198.51.100.10",
		EnrollmentTTL:        15 * time.Minute, HeartbeatMaxSkew: 2 * time.Minute,
		HeartbeatIntervalSec: 30,
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	created, err := registry.Create(CreateAgentRequest{
		DisplayName: "EU probe 2", ExpectedSourceIP: "203.0.113.41",
		ControllerURL: "https://second.example.com", ControllerIP: "198.51.100.11",
	})
	if err != nil {
		t.Fatalf("create with an explicit controller address: %v", err)
	}
	if created.Agent.ControllerURL != "https://second.example.com" || created.Agent.ControllerIP != "198.51.100.11" {
		t.Fatalf("agent = %+v, want the explicitly requested address", created.Agent)
	}
}

// A typo in the controller's own address must stop startup rather than surface
// weeks later when someone adds a probe.
func TestNewRegistryRejectsAnInvalidControllerDefault(t *testing.T) {
	base := RegistryConfig{
		Path: filepath.Join(t.TempDir(), "diagnostic_agents.json"), Enabled: true, AgentImage: "agent:test",
		EnrollmentTTL: 15 * time.Minute, HeartbeatMaxSkew: 2 * time.Minute, HeartbeatIntervalSec: 30,
	}
	plaintext := base
	plaintext.DefaultControllerURL = "http://checker.example.com"
	plaintext.DefaultControllerIP = "198.51.100.10"
	if _, err := NewRegistry(plaintext); err == nil {
		t.Fatal("expected a non-https controller default to be rejected")
	}
	hostname := base
	hostname.DefaultControllerURL = "https://checker.example.com"
	hostname.DefaultControllerIP = "checker.example.com"
	if _, err := NewRegistry(hostname); err == nil {
		t.Fatal("expected a hostname in place of the controller IP to be rejected")
	}
	half := base
	half.DefaultControllerURL = "https://checker.example.com"
	if _, err := NewRegistry(half); err == nil {
		t.Fatal("expected a default with no controller IP to be rejected")
	}
}
