package probeagent

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"xray-checker/diagnostics"
)

func TestClientIdentitySurvivesContainerStyleRestart(t *testing.T) {
	identityDir := t.TempDir()
	config := ClientConfig{
		AgentID: "agent_test", ControllerURL: "https://checker.example.com",
		ControllerIP: "198.51.100.10", IdentityDir: identityDir,
		AgentVersion: "test", Capabilities: []string{"control-v1"},
	}
	first, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity := first.identity
	second, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if second.identity.IdentityPrivateKey != firstIdentity.IdentityPrivateKey || second.identity.ObservationPrivateKey != firstIdentity.ObservationPrivateKey {
		t.Fatal("client generated new keys instead of reusing the persistent identity")
	}
	info, err := os.Stat(filepath.Join(identityDir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("identity file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestPinnedHTTPClientUsesPinnedAddressAndRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/target" {
			targetHits.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, request, "/target", http.StatusFound)
	}))
	defer server.Close()

	certificate := server.Certificate()
	caPath := filepath.Join(t.TempDir(), "controller-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(certificate.Raw)
	if err != nil || parsed == nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	client, err := newPinnedHTTPClient(server.URL, "127.0.0.1", caPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound || targetHits.Load() != 0 {
		t.Fatalf("redirect status = %d, target hits = %d", response.StatusCode, targetHits.Load())
	}
}

func TestControllerRejectionSeparatesJobFailuresFromAccessFailures(t *testing.T) {
	for _, testCase := range []struct {
		status    int
		jobScoped bool
	}{
		{status: http.StatusConflict, jobScoped: true},
		{status: http.StatusBadRequest, jobScoped: true},
		{status: http.StatusNotFound, jobScoped: true},
		{status: http.StatusUnauthorized, jobScoped: false},
		{status: http.StatusForbidden, jobScoped: false},
		{status: http.StatusInternalServerError, jobScoped: false},
	} {
		rejection := &ControllerRejection{StatusCode: testCase.status, Message: "rejected"}
		if rejection.JobScoped() != testCase.jobScoped {
			t.Errorf("status %d JobScoped = %v, want %v", testCase.status, rejection.JobScoped(), testCase.jobScoped)
		}
	}
}

// A single refused observation used to end Run, which stopped the heartbeat for
// the whole reconnect backoff and reported a live agent as disconnected.
func TestRefusedObservationKeepsTheControlConnectionAndIsNotRetriedForever(t *testing.T) {
	var polls, observations, executions atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case JobPollPath:
			polls.Add(1)
			// The controller redelivers the same job until it expires.
			_, _ = w.Write([]byte(`{"success":true,"data":{"job":{"job":{"schemaVersion":1,"jobId":"job-1","sessionId":"s","agentId":"agent_test","nonce":"n","stableId":"node-1","configFingerprint":"sha256:x","profile":{"id":"default-status","method":"status"},"expiresAt":"2099-01-01T00:00:00Z","state":"running"},"xrayConfig":{"ok":true},"socksPort":18080,"targetHost":"node.example.com","targetPort":443}}}`))
		case ObservationPath:
			observations.Add(1)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"success":false,"error":"Observation rejected"}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
		}
	}))
	defer server.Close()

	client := newRejectionTestClient(t, server, &executions)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	err := client.runJobLoop(ctx)

	// The loop may end on the context deadline; what it must never do is end
	// because the controller refused a job.
	var rejection *ControllerRejection
	if errors.As(err, &rejection) {
		t.Fatalf("a refused job ended the control loop: %v", err)
	}
	if polls.Load() < 2 {
		t.Fatalf("loop polled %d times, want it to keep polling after the refusal", polls.Load())
	}
	// The job is remembered as refused, so it is neither re-executed nor
	// re-submitted while the controller keeps redelivering it.
	if observations.Load() != 1 {
		t.Errorf("submitted %d observations, want exactly one", observations.Load())
	}
	if executions.Load() != 1 {
		t.Errorf("executed the job %d times, want exactly one", executions.Load())
	}
}

func TestAccessDeniedStillEndsTheControlLoop(t *testing.T) {
	var executions atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"Agent authentication failed"}`))
	}))
	defer server.Close()

	client := newRejectionTestClient(t, server, &executions)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.runJobLoop(ctx); err == nil {
		t.Fatal("a revoked or unauthenticated agent kept polling instead of reconnecting")
	}
}

type stubExecutor struct{ executions *atomic.Int64 }

func (s stubExecutor) Execute(_ context.Context, assignment JobAssignment) diagnostics.Observation {
	s.executions.Add(1)
	return diagnostics.Observation{SchemaVersion: diagnostics.ObservationSchemaVersion, JobID: assignment.Job.JobID}
}

func newRejectionTestClient(t *testing.T, server *httptest.Server, executions *atomic.Int64) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		AgentID: "agent_test", ControllerURL: server.URL, ControllerIP: "127.0.0.1",
		IdentityDir: t.TempDir(), AgentVersion: "test", Capabilities: []string{"control-v1"},
		Executor: stubExecutor{executions: executions},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	client.identity.Enrolled = true
	client.httpClient = server.Client()
	// Below the constructor floor on purpose: the loop behaviour under test is
	// about repetition, not about timing.
	client.config.JobPollInterval = 50 * time.Millisecond
	return client
}

// The agent used to run a probe and say nothing about it, which left an
// operator watching container logs with no idea what it was doing.
func TestJobHooksReportWhatTheAgentWasAskedAndWhatItSaw(t *testing.T) {
	var executions atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case JobPollPath:
			_, _ = w.Write([]byte(`{"success":true,"data":{"job":{"job":{"schemaVersion":1,"jobId":"job-abcdef012345","sessionId":"s","agentId":"agent_test","nonce":"n","stableId":"node-1","configFingerprint":"sha256:x","profile":{"id":"default-status","method":"status"},"expiresAt":"2099-01-01T00:00:00Z","state":"running"},"xrayConfig":{"ok":true},"socksPort":18080,"targetHost":"node.example.com","targetPort":443}}}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
		}
	}))
	defer server.Close()

	var started []JobStarted
	var finished, accepted []JobFinished
	client := newRejectionTestClient(t, server, &executions)
	client.config.Executor = evidenceExecutor{}
	client.config.Hooks = Hooks{
		OnJobStarted:          func(job JobStarted) { started = append(started, job) },
		OnJobFinished:         func(job JobFinished) { finished = append(finished, job) },
		OnObservationAccepted: func(job JobFinished) { accepted = append(accepted, job) },
	}

	if err := client.pollAndExecute(context.Background()); err != nil {
		t.Fatalf("poll and execute: %v", err)
	}

	if len(started) != 1 {
		t.Fatalf("job start hooks = %d, want one", len(started))
	}
	if started[0].StableID != "node-1" || started[0].ProfileID != "default-status" {
		t.Fatalf("start = %+v, want the node and profile the controller named", started[0])
	}
	// The target is what makes a log line actionable without opening the admin UI.
	if started[0].Target != "node.example.com:443" {
		t.Fatalf("target = %q, want host and port", started[0].Target)
	}
	if len(finished) != 1 || len(accepted) != 1 {
		t.Fatalf("finish hooks = %d, accepted hooks = %d, want one each", len(finished), len(accepted))
	}
	result := finished[0]
	if result.Status != diagnostics.ProbeStatusProxyFailure || result.FailureCode != "proxy_timeout" {
		t.Fatalf("result = %+v, want the observed status and failure", result)
	}
	// The direct control decides whether the controller trusts the result at
	// all, so it has to reach the log alongside it.
	if result.Direct.Online {
		t.Fatal("the failed direct connectivity control must be reported as failed")
	}
	if result.TCP.Online || !result.TCP.Checked {
		t.Fatalf("tcp evidence = %+v, want the checked failure to survive into the hook", result.TCP)
	}
}

type evidenceExecutor struct{}

func (evidenceExecutor) Execute(_ context.Context, assignment JobAssignment) diagnostics.Observation {
	return diagnostics.Observation{
		SchemaVersion:      diagnostics.ObservationSchemaVersion,
		JobID:              assignment.Job.JobID,
		StableID:           assignment.Job.StableID,
		EndpointProfile:    assignment.Job.Profile.ID,
		Status:             diagnostics.ProbeStatusProxyFailure,
		LatencyMillis:      120,
		Failure:            diagnostics.FailureEvidence{Code: "proxy_timeout", Stage: diagnostics.FailureStageProxy},
		TCP:                diagnostics.CheckEvidence{Checked: true, Online: false},
		Ping:               diagnostics.CheckEvidence{Checked: true, Online: true, LatencyMillis: 30},
		DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: false},
	}
}
