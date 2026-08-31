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

func newProbeTestExecutor(t *testing.T, config ExecutorConfig) *Executor {
	t.Helper()
	config.RuntimeDir = "/tmp"
	executor, err := NewExecutor(config)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor
}

// A download that stalls short of the minimum is still worth measuring: the rate
// it managed distinguishes a throttled path from a refused one.
func TestDownloadProbeReportsThroughputForCompleteAndTruncatedTransfers(t *testing.T) {
	payload := make([]byte, 64*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	executor := newProbeTestExecutor(t, ExecutorConfig{DownloadURL: server.URL, DownloadMinSize: int64(len(payload))})
	result := executor.proxyCheck(context.Background(), diagnostics.TestProfile{ID: diagnostics.ProfileDownload, Method: diagnostics.ProbeMethodDownload}, 0, "")
	if result.status != diagnostics.ProbeStatusOnline {
		t.Fatalf("complete transfer status = %q, want online", result.status)
	}
	if result.throughput == nil || result.throughput.Bytes != int64(len(payload)) {
		t.Fatalf("throughput evidence = %+v, want %d bytes", result.throughput, len(payload))
	}

	truncating := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload[:1024])
	}))
	defer truncating.Close()
	executor = newProbeTestExecutor(t, ExecutorConfig{DownloadURL: truncating.URL, DownloadMinSize: int64(len(payload))})
	result = executor.proxyCheck(context.Background(), diagnostics.TestProfile{ID: diagnostics.ProfileDownload, Method: diagnostics.ProbeMethodDownload}, 0, "")
	if result.status != diagnostics.ProbeStatusProxyFailure || result.failure.Code != "download_incomplete" {
		t.Fatalf("truncated transfer = %q/%q, want proxy_failure/download_incomplete", result.status, result.failure.Code)
	}
	if result.throughput == nil || result.throughput.Bytes != 1024 {
		t.Fatalf("truncated throughput = %+v, want the 1024 bytes that did arrive", result.throughput)
	}
}

func TestLatencyProfileAggregatesEverySampleAndFailsOnPartialSuccess(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		// The third request fails, so the series is incomplete.
		if requests == 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	executor := newProbeTestExecutor(t, ExecutorConfig{StatusCheckURL: server.URL, LatencySamples: 4})
	result := executor.latencyProfile(context.Background(), 0)
	if result.latencySeries == nil {
		t.Fatal("latency profile returned no series")
	}
	if result.latencySeries.Samples != 4 || result.latencySeries.Succeeded != 3 {
		t.Fatalf("series = %+v, want 3 of 4 samples succeeding", result.latencySeries)
	}
	// An intermittent tunnel is not a healthy one; reporting online would hide it.
	if result.status != diagnostics.ProbeStatusProxyFailure {
		t.Fatalf("partial series status = %q, want proxy_failure", result.status)
	}
	if requests != 4 {
		t.Fatalf("issued %d requests, want one per configured sample", requests)
	}
}

func TestDNSProbeReportsLiteralAddressesWithoutClaimingALookup(t *testing.T) {
	executor := newProbeTestExecutor(t, ExecutorConfig{})
	evidence := executor.dnsProbe(context.Background(), "203.0.113.10", time.Second)
	if !evidence.Checked || !evidence.Literal {
		t.Fatalf("literal address evidence = %+v, want Checked and Literal", evidence)
	}
	if len(evidence.Resolvers) != 0 {
		t.Fatalf("literal address consulted %d resolvers, want none", len(evidence.Resolvers))
	}
}

func TestTLSProbeReportsHandshakeAndPresentsTheConfiguredSNI(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(parsed.Port())

	executor := newProbeTestExecutor(t, ExecutorConfig{})
	evidence := executor.tlsProbe(context.Background(), JobAssignment{
		TargetHost: parsed.Hostname(), TargetPort: port, TargetSNI: "node.example.com",
	}, 5*time.Second)
	if !evidence.Checked || !evidence.Handshake {
		t.Fatalf("TLS evidence = %+v, want a completed handshake", evidence)
	}
	if evidence.ServerName != "node.example.com" {
		t.Fatalf("presented SNI = %q, want the node's configured name", evidence.ServerName)
	}
	if evidence.NegotiatedVersion == "" {
		t.Error("handshake reported no negotiated version")
	}
}

func TestTLSProbeReportsRefusedPortWithoutClaimingAHandshake(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	parsed, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(parsed.Port())
	server.Close()

	executor := newProbeTestExecutor(t, ExecutorConfig{})
	evidence := executor.tlsProbe(context.Background(), JobAssignment{
		TargetHost: parsed.Hostname(), TargetPort: port, TargetSNI: "node.example.com",
	}, 2*time.Second)
	if evidence.Handshake {
		t.Fatal("closed port reported a completed handshake")
	}
	if evidence.FailureCode == "" {
		t.Error("closed port produced no failure code")
	}
}
