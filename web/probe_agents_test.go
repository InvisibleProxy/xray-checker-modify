package web

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"xray-checker/probeagent"
)

func TestProbeAgentEnrollmentAndSignedHeartbeatHandlers(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	registry := newWebTestAgentRegistry(t, now)
	created, err := registry.Create(probeagent.CreateAgentRequest{
		DisplayName: "Local probe", ExpectedSourceIP: "203.0.113.40",
		ControllerIP: "198.51.100.10", ControllerURL: "https://checker.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	identityPublic, identityPrivate, _ := ed25519.GenerateKey(rand.Reader)
	observationPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	enroll := probeagent.EnrollRequest{
		ProtocolVersion: probeagent.ProtocolVersion, AgentID: created.Agent.AgentID,
		EnrollmentToken: created.EnrollmentToken, IdentityPublicKey: identityPublic,
		ObservationPublicKey: observationPublic, AgentVersion: "test", Capabilities: []string{"control-v1"},
	}
	enrollBody, _ := json.Marshal(enroll)
	enrollRequest := httptest.NewRequest(http.MethodPost, probeagent.EnrollPath, bytes.NewReader(enrollBody))
	enrollRequest.RemoteAddr = "172.18.0.4:12345"
	enrollRequest.Header.Set(probeagent.ProxySecretHeader, "proxy-secret")
	enrollRequest.Header.Set(probeagent.ForwardedIPHeader, "203.0.113.40")
	enrollRecorder := httptest.NewRecorder()
	ProbeAgentEnrollHandler(registry, "proxy-secret").ServeHTTP(enrollRecorder, enrollRequest)
	if enrollRecorder.Code != http.StatusOK {
		t.Fatalf("enrollment status = %d, body = %s", enrollRecorder.Code, enrollRecorder.Body.String())
	}

	heartbeat := probeagent.HeartbeatRequest{ProtocolVersion: probeagent.ProtocolVersion, AgentID: created.Agent.AgentID, AgentVersion: "test", Capabilities: []string{"control-v1"}, Health: "healthy"}
	heartbeatBody, _ := json.Marshal(heartbeat)
	payload, _ := probeagent.ControlSigningPayload(http.MethodPost, probeagent.HeartbeatPath, heartbeat.AgentID, now, 1, heartbeatBody)
	heartbeatRequest := httptest.NewRequest(http.MethodPost, probeagent.HeartbeatPath, bytes.NewReader(heartbeatBody))
	heartbeatRequest.RemoteAddr = "172.18.0.4:12345"
	heartbeatRequest.Header.Set(probeagent.ProxySecretHeader, "proxy-secret")
	heartbeatRequest.Header.Set(probeagent.ForwardedIPHeader, "203.0.113.40")
	heartbeatRequest.Header.Set("X-Probe-Agent-ID", heartbeat.AgentID)
	heartbeatRequest.Header.Set("X-Probe-Timestamp", now.Format(time.RFC3339Nano))
	heartbeatRequest.Header.Set("X-Probe-Sequence", strconv.FormatUint(1, 10))
	heartbeatRequest.Header.Set("X-Probe-Signature", base64.RawStdEncoding.EncodeToString(ed25519.Sign(identityPrivate, payload)))
	heartbeatRecorder := httptest.NewRecorder()
	ProbeAgentHeartbeatHandler(registry, "proxy-secret").ServeHTTP(heartbeatRecorder, heartbeatRequest)
	if heartbeatRecorder.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, body = %s", heartbeatRecorder.Code, heartbeatRecorder.Body.String())
	}
	if got := registry.Snapshot()[0]; got.Health != "healthy" || got.LastSeenAt.IsZero() || !got.Connected {
		t.Fatalf("heartbeat did not update registry: %+v", got)
	}
}

func TestProbeAgentHandlerRejectsSpoofedForwardedIP(t *testing.T) {
	registry := newWebTestAgentRegistry(t, time.Now().UTC())
	request := httptest.NewRequest(http.MethodPost, probeagent.EnrollPath, bytes.NewReader([]byte(`{}`)))
	request.RemoteAddr = "172.18.0.4:12345"
	request.Header.Set(probeagent.ProxySecretHeader, "wrong")
	request.Header.Set(probeagent.ForwardedIPHeader, netip.MustParseAddr("203.0.113.40").String())
	recorder := httptest.NewRecorder()
	ProbeAgentEnrollHandler(registry, "right").ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || recorder.Body.String() == "" {
		t.Fatalf("spoofed forwarded IP status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestAdminDiagnosticAgentCreationReturnsComposeOnce(t *testing.T) {
	registry := newWebTestAgentRegistry(t, time.Now().UTC())
	body := []byte(`{"displayName":"EU probe","expectedSourceIp":"203.0.113.40","controllerIp":"198.51.100.10","controllerUrl":"https://checker.example.com"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/diagnostic-agents", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	AdminDiagnosticAgentsHandler(registry).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status = %d, headers = %v, body = %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var response APIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(response.Data)
	if !bytes.Contains(encoded, []byte("enrollmentToken")) || !bytes.Contains(encoded, []byte("compose")) {
		t.Fatalf("creation response is incomplete: %s", encoded)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/admin/diagnostic-agents", nil)
	listRecorder := httptest.NewRecorder()
	AdminDiagnosticAgentsHandler(registry).ServeHTTP(listRecorder, listRequest)
	if bytes.Contains(listRecorder.Body.Bytes(), []byte("enroll_")) || bytes.Contains(listRecorder.Body.Bytes(), []byte("compose")) {
		t.Fatalf("list response leaked one-time material: %s", listRecorder.Body.String())
	}
}

func newWebTestAgentRegistry(t *testing.T, now time.Time) *probeagent.Registry {
	t.Helper()
	registry, err := probeagent.NewRegistry(probeagent.RegistryConfig{
		Path: filepath.Join(t.TempDir(), "diagnostic_agents.json"), Enabled: true,
		AgentImage: "agent:test", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
