package probeagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/models"
	"xray-checker/xray"
)

func TestExecutorUsesTemporaryXrayConfigAndReturnsIsolatedEvidence(t *testing.T) {
	directServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer directServer.Close()
	parsed, _ := url.Parse(directServer.URL)
	port, _ := strconv.Atoi(parsed.Port())
	proxy := &models.ProxyConfig{
		StableID: "node-one", Name: "Node One", Protocol: "vless",
		Server: parsed.Hostname(), Port: port, UUID: "11111111-1111-1111-1111-111111111111",
	}
	const socksPort = 18080
	configJSON, err := xray.NewConfigGenerator().GenerateConfig([]*models.ProxyConfig{proxy}, socksPort, "none")
	if err != nil {
		t.Fatalf("generate Xray config: %v", err)
	}
	runtimeDirectory := t.TempDir()
	executor, err := NewExecutor(ExecutorConfig{
		RuntimeDir: runtimeDirectory, StatusCheckURL: directServer.URL,
		DirectCheckURL: directServer.URL, ProxyTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	now := time.Now().UTC()
	assignment := JobAssignment{
		Job: diagnostics.DiagnosticJob{
			SchemaVersion: diagnostics.JobSchemaVersion, JobID: "job-one", SessionID: "session-one",
			AgentID: "agent-one", Nonce: "nonce-one", StableID: "node-one", ConfigGeneration: 1,
			ConfigFingerprint: diagnostics.ConfigFingerprint(configJSON),
			Profile:           diagnostics.TestProfile{ID: "default-status", Method: diagnostics.ProbeMethodStatus},
			CreatedAt:         now, ExpiresAt: now.Add(time.Minute), State: diagnostics.JobStateRunning,
		},
		XrayConfig: configJSON, SocksPort: socksPort, TargetHost: parsed.Hostname(), TargetPort: port,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observation := executor.Execute(ctx, assignment)
	if !observation.DirectConnectivity.Checked || !observation.DirectConnectivity.Online {
		t.Fatalf("direct connectivity = %+v", observation.DirectConnectivity)
	}
	if observation.Status == diagnostics.ProbeStatusOnline || !observation.TCP.Checked || !observation.TCP.Online {
		t.Fatalf("unexpected proxy/host evidence: status=%s tcp=%+v failure=%+v", observation.Status, observation.TCP, observation.Failure)
	}
	entries, err := os.ReadDir(runtimeDirectory)
	if err != nil {
		t.Fatalf("read runtime directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary Xray config was not removed: %v", entries)
	}
}

func TestExecutorRejectsFingerprintMismatchBeforeStartingXray(t *testing.T) {
	executor, err := NewExecutor(ExecutorConfig{RuntimeDir: t.TempDir(), DirectCheckURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	observation := executor.Execute(context.Background(), JobAssignment{
		Job: diagnostics.DiagnosticJob{
			SchemaVersion: diagnostics.JobSchemaVersion, JobID: "job-one", SessionID: "session-one",
			AgentID: "agent-one", Nonce: "nonce-one", StableID: "node-one",
			ConfigFingerprint: diagnostics.ConfigFingerprint([]byte(`{"different":true}`)),
			Profile:           diagnostics.TestProfile{ID: "default-status", Method: diagnostics.ProbeMethodStatus},
			CreatedAt:         now, ExpiresAt: now.Add(time.Minute), State: diagnostics.JobStateRunning,
		},
		XrayConfig: []byte(`{"inbounds":[]}`), SocksPort: 18080, TargetHost: "127.0.0.1", TargetPort: 443,
	})
	if observation.Failure.Code != "configuration" || observation.Failure.Stage != diagnostics.FailureStageConfiguration {
		t.Fatalf("fingerprint mismatch result = %+v", observation)
	}
}
