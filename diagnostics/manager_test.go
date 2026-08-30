package diagnostics

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type managerFixture struct {
	manager    *DiagnosticSessionManager
	now        *time.Time
	privateKey ed25519.PrivateKey
	session    DiagnosticSession
	job        DiagnosticJob
}

func newManagerFixture(t *testing.T, requestedAgents ...string) managerFixture {
	t.Helper()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	nextID := 0
	manager, err := NewDiagnosticSessionManager(ManagerConfig{
		Now: func() time.Time { return now },
		NewID: func(prefix string) (string, error) {
			nextID++
			return fmt.Sprintf("%s_%d", prefix, nextID), nil
		},
		VerifyObservation: NewEd25519Verifier(func(agentID string) (ed25519.PublicKey, bool) {
			if agentID != "agent-eu" {
				return nil, false
			}
			return publicKey, true
		}),
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	if len(requestedAgents) == 0 {
		requestedAgents = []string{"agent-eu"}
	}
	session, err := manager.CreateSession(CreateSessionRequest{
		StableID:          "stable-node-1",
		Trigger:           TriggerManual,
		ConfigGeneration:  7,
		ConfigFingerprint: ConfigFingerprint([]byte(`{"effective":"config-v7"}`)),
		LocalResultSnapshot: LocalResultSnapshot{
			Status:    ProbeStatusProxyFailure,
			CheckedAt: now.Add(-time.Second),
			Failure: FailureEvidence{
				Code:  "proxy_handshake",
				Stage: FailureStageProxy,
			},
		},
		RequestedAgents: requestedAgents,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	job, err := manager.RegisterJob(RegisterJobRequest{
		SessionID: session.SessionID,
		AgentID:   "agent-eu",
		Profile: TestProfile{
			ID:                   "primary-ip",
			Method:               ProbeMethodIP,
			AlternativeProfileID: "alternative-ip",
		},
	})
	if err != nil {
		t.Fatalf("register job: %v", err)
	}
	return managerFixture{manager: manager, now: &now, privateKey: privateKey, session: session, job: job}
}

func (fixture managerFixture) observation(t *testing.T) Observation {
	t.Helper()
	observation := Observation{
		SchemaVersion:      ObservationSchemaVersion,
		AgentID:            fixture.job.AgentID,
		SessionID:          fixture.job.SessionID,
		JobID:              fixture.job.JobID,
		Nonce:              fixture.job.Nonce,
		StableID:           fixture.job.StableID,
		ConfigGeneration:   fixture.job.ConfigGeneration,
		ConfigFingerprint:  fixture.job.ConfigFingerprint,
		CheckedAt:          *fixture.now,
		DurationMillis:     125,
		EndpointProfile:    fixture.job.Profile.ID,
		Status:             ProbeStatusOnline,
		LatencyMillis:      42,
		DirectConnectivity: CheckEvidence{Checked: true, Online: true, LatencyMillis: 12},
		AgentVersion:       "1.0.0-linux-amd64",
	}
	return signObservation(t, observation, fixture.privateKey)
}

func signObservation(t *testing.T, observation Observation, privateKey ed25519.PrivateKey) Observation {
	t.Helper()
	payload, err := ObservationSigningPayload(observation)
	if err != nil {
		t.Fatalf("build signing payload: %v", err)
	}
	observation.Signature = ed25519.Sign(privateKey, payload)
	return observation
}

func TestManagerAcceptsBoundObservationWithoutOperationalState(t *testing.T) {
	fixture := newManagerFixture(t)
	observation := fixture.observation(t)

	record, err := fixture.manager.AcceptObservation(observation)
	if err != nil {
		t.Fatalf("accept observation: %v", err)
	}
	if !record.Reliable {
		t.Fatal("observation with healthy direct connectivity is unreliable")
	}
	stored, ok := fixture.manager.Session(fixture.session.SessionID)
	if !ok {
		t.Fatal("session disappeared")
	}
	if stored.State != SessionStateCompleted {
		t.Fatalf("session state = %q, want %q", stored.State, SessionStateCompleted)
	}
	if stored.LocalResultSnapshot.Status != ProbeStatusProxyFailure {
		t.Fatalf("local snapshot status changed to %q", stored.LocalResultSnapshot.Status)
	}
	if len(stored.AgentObservations) != 1 || stored.AgentObservations[0].Observation.Status != ProbeStatusOnline {
		t.Fatalf("stored observations = %+v", stored.AgentObservations)
	}

	// Returned snapshots are detached from manager-owned state.
	stored.RequestedAgents[0] = "mutated"
	stored.AgentObservations[0].Observation.Signature[0] ^= 0xff
	again, _ := fixture.manager.Session(fixture.session.SessionID)
	if again.RequestedAgents[0] != "agent-eu" || again.AgentObservations[0].Observation.Signature[0] != observation.Signature[0] {
		t.Fatal("caller mutated manager-owned diagnostic state")
	}
}

func TestManagerRejectsInvalidSignatureWithoutMutation(t *testing.T) {
	fixture := newManagerFixture(t)
	observation := fixture.observation(t)
	observation.Signature[0] ^= 0xff

	if _, err := fixture.manager.AcceptObservation(observation); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v, want invalid signature", err)
	}
	stored, _ := fixture.manager.Session(fixture.session.SessionID)
	if len(stored.AgentObservations) != 0 || stored.Jobs[0].State == JobStateCompleted {
		t.Fatalf("invalid observation mutated session: %+v", stored)
	}
}

func TestManagerRejectsIncoherentOrUnclassifiedConnectivityEvidence(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Observation)
	}{
		{
			name: "unchecked evidence carries a failure",
			change: func(observation *Observation) {
				observation.TCP = CheckEvidence{FailureCode: "tcp_timeout"}
			},
		},
		{
			name: "unchecked evidence claims online",
			change: func(observation *Observation) {
				observation.Ping = CheckEvidence{Online: true}
			},
		},
		{
			name: "online evidence carries a failure",
			change: func(observation *Observation) {
				observation.TCP = CheckEvidence{Checked: true, Online: true, FailureCode: "dns"}
			},
		},
		{
			name: "failed evidence carries an unclassified value",
			change: func(observation *Observation) {
				observation.TCP = CheckEvidence{Checked: true, FailureCode: "203.0.113.40:443"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			observation := fixture.observation(t)
			test.change(&observation)
			observation = signObservation(t, observation, fixture.privateKey)
			if _, err := fixture.manager.AcceptObservation(observation); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want invalid request", err)
			}
		})
	}
}

func TestManagerRejectsIncoherentLocalConnectivityEvidence(t *testing.T) {
	fixture := newManagerFixture(t)
	_, err := fixture.manager.CreateSession(CreateSessionRequest{
		StableID:            "stable-node-2",
		Trigger:             TriggerManual,
		ConfigGeneration:    8,
		ConfigFingerprint:   ConfigFingerprint([]byte(`{"effective":"config-v8"}`)),
		RequestedAgents:     []string{"agent-eu"},
		LocalResultSnapshot: LocalResultSnapshot{Status: ProbeStatusUnknown, TCP: CheckEvidence{FailureCode: "tcp_timeout"}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want invalid request", err)
	}
}

func TestManagerRejectsStaleUnknownAndMismatchedObservations(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Observation)
		want   error
	}{
		{
			name: "stale timestamp",
			change: func(observation *Observation) {
				observation.CheckedAt = observation.CheckedAt.Add(-DefaultObservationMaxAge - time.Second)
			},
			want: ErrStaleObservation,
		},
		{
			name: "unknown job",
			change: func(observation *Observation) {
				observation.JobID = "job_unknown"
			},
			want: ErrUnknownJob,
		},
		{
			name: "old generation",
			change: func(observation *Observation) {
				observation.ConfigGeneration--
			},
			want: ErrStaleGeneration,
		},
		{
			name: "different fingerprint",
			change: func(observation *Observation) {
				observation.ConfigFingerprint = ConfigFingerprint([]byte("different"))
			},
			want: ErrBindingMismatch,
		},
		{
			name: "old schema",
			change: func(observation *Observation) {
				observation.SchemaVersion = 0
			},
			want: ErrInvalidSchema,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			observation := fixture.observation(t)
			test.change(&observation)
			observation = signObservation(t, observation, fixture.privateKey)
			if _, err := fixture.manager.AcceptObservation(observation); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestManagerRejectsDuplicateAndReplayedNonce(t *testing.T) {
	fixture := newManagerFixture(t)
	first := fixture.observation(t)
	if _, err := fixture.manager.AcceptObservation(first); err != nil {
		t.Fatalf("accept first observation: %v", err)
	}
	if _, err := fixture.manager.AcceptObservation(first); !errors.Is(err, ErrDuplicateObservation) {
		t.Fatalf("duplicate error = %v, want duplicate observation", err)
	}

	secondSession, err := fixture.manager.CreateSession(CreateSessionRequest{
		StableID:            fixture.session.StableID,
		Trigger:             TriggerManual,
		ConfigGeneration:    fixture.session.ConfigGeneration,
		ConfigFingerprint:   fixture.session.ConfigFingerprint,
		LocalResultSnapshot: fixture.session.LocalResultSnapshot,
		RequestedAgents:     []string{"agent-eu"},
	})
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	secondJob, err := fixture.manager.RegisterJob(RegisterJobRequest{
		SessionID: secondSession.SessionID,
		AgentID:   "agent-eu",
		Profile:   fixture.job.Profile,
	})
	if err != nil {
		t.Fatalf("register second job: %v", err)
	}
	replay := first
	replay.SessionID = secondJob.SessionID
	replay.JobID = secondJob.JobID
	replay.Nonce = first.Nonce
	replay = signObservation(t, replay, fixture.privateKey)
	if _, err := fixture.manager.AcceptObservation(replay); !errors.Is(err, ErrReplayObservation) {
		t.Fatalf("replay error = %v, want replay observation", err)
	}
}

func TestManagerMarksFailedDirectConnectivityUnreliable(t *testing.T) {
	fixture := newManagerFixture(t)
	observation := fixture.observation(t)
	observation.Status = ProbeStatusProxyFailure
	observation.LatencyMillis = 0
	observation.Failure = FailureEvidence{Code: "proxy_timeout", Stage: FailureStageProxy}
	observation.DirectConnectivity = CheckEvidence{
		Checked:     true,
		Online:      false,
		FailureCode: "network_unreachable",
	}
	observation = signObservation(t, observation, fixture.privateKey)

	record, err := fixture.manager.AcceptObservation(observation)
	if err != nil {
		t.Fatalf("accept observation: %v", err)
	}
	if record.Reliable {
		t.Fatal("observation with failed direct connectivity is reliable")
	}
}

func TestManagerExpiresPartialSession(t *testing.T) {
	fixture := newManagerFixture(t, "agent-eu", "agent-us")
	observation := fixture.observation(t)
	if _, err := fixture.manager.AcceptObservation(observation); err != nil {
		t.Fatalf("accept observation: %v", err)
	}
	*fixture.now = fixture.session.ExpiresAt.Add(time.Second)

	stored, ok := fixture.manager.Session(fixture.session.SessionID)
	if !ok {
		t.Fatal("session disappeared")
	}
	if stored.State != SessionStatePartial {
		t.Fatalf("session state = %q, want %q", stored.State, SessionStatePartial)
	}
}

func TestAutomaticMaintenanceSessionIsRejected(t *testing.T) {
	fixture := newManagerFixture(t)
	_, err := fixture.manager.CreateSession(CreateSessionRequest{
		StableID:              "stable-node-1",
		Trigger:               TriggerAutoProxyFailure,
		ConfigFingerprint:     ConfigFingerprint([]byte("config")),
		RequestedAgents:       []string{"agent-eu"},
		MaintenanceDiagnostic: true,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want invalid request", err)
	}
}

func TestManagerCancellationStopsOutstandingJobs(t *testing.T) {
	fixture := newManagerFixture(t)
	if err := fixture.manager.MarkJobRunning(fixture.job.JobID); err != nil {
		t.Fatalf("mark job running: %v", err)
	}
	if err := fixture.manager.CancelSession(fixture.session.SessionID); err != nil {
		t.Fatalf("cancel session: %v", err)
	}
	stored, _ := fixture.manager.Session(fixture.session.SessionID)
	if stored.State != SessionStateCancelled || stored.Jobs[0].State != JobStateCancelled {
		t.Fatalf("cancelled session = %+v", stored)
	}
	if _, err := fixture.manager.AcceptObservation(fixture.observation(t)); !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("late observation error = %v, want cancelled session", err)
	}
}

func TestManagerEnforcesAgentLimit(t *testing.T) {
	fixture := newManagerFixture(t)
	agents := make([]string, DefaultMaxAgentsPerSession+1)
	for index := range agents {
		agents[index] = fmt.Sprintf("agent-%d", index)
	}
	_, err := fixture.manager.CreateSession(CreateSessionRequest{
		StableID:            "stable-node-1",
		Trigger:             TriggerManual,
		ConfigFingerprint:   ConfigFingerprint([]byte("config")),
		LocalResultSnapshot: LocalResultSnapshot{Status: ProbeStatusUnknown},
		RequestedAgents:     agents,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want invalid request", err)
	}
}

func TestExportContainsNoExecutionConfigurationOrRawErrors(t *testing.T) {
	fixture := newManagerFixture(t)
	exported, err := fixture.manager.ExportSession(fixture.session.SessionID)
	if err != nil {
		t.Fatalf("export session: %v", err)
	}
	for _, forbidden := range []string{"password", "subscriptionUrl", "rawError", "proxyConfig"} {
		if strings.Contains(string(exported), forbidden) {
			t.Fatalf("session export contains forbidden field %q: %s", forbidden, exported)
		}
	}
}
