package diagnostics

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSpeedServerCatalogueIsConsistent(t *testing.T) {
	ids := make(map[string]bool)
	hosts := make(map[string]string)
	for _, server := range SpeedServers() {
		if !validProfileID(server.ID) {
			t.Errorf("server ID %q is not a valid bounded identifier", server.ID)
		}
		if ids[server.ID] {
			t.Errorf("server ID %q is listed twice", server.ID)
		}
		ids[server.ID] = true
		parsed, err := url.Parse(server.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			t.Errorf("server %q has an unusable URL %q", server.ID, server.URL)
			continue
		}
		// Matching is by host, so two entries on one host would make the name a
		// URL resolves to depend on the order of this list.
		host := speedServerHost(server.URL)
		if other, taken := hosts[host]; taken {
			t.Errorf("servers %q and %q share the host %q", other, server.ID, host)
		}
		hosts[host] = server.ID
		if len(server.CountryCode) != 2 || server.Provider == "" {
			t.Errorf("server %q is missing its country or provider", server.ID)
		}
	}
}

// The run's test URL is matched by host: a different file on the same server
// crosses the same uplink, and scheme, port or case do not change the server.
func TestSpeedServerForURLMatchesTheHostOnly(t *testing.T) {
	for _, test := range []struct {
		url    string
		wantID string
	}{
		{"https://fsn1-speed.hetzner.com/100MB.bin", "hetzner-falkenstein-fsn1"},
		{"https://FSN1-SPEED.hetzner.com/1GB.bin?x=1", "hetzner-falkenstein-fsn1"},
		{"http://speedtest.ams1.nl.leaseweb.net/1000mb.bin", "leaseweb-amsterdam-ams1"},
		{"https://speedtest.ams1.nl.leaseweb.net:443/100mb.bin", "leaseweb-amsterdam-ams1"},
		{"https://us.edisglobal.com/100MB.test", "edis-new-york"},
		{"https://proof.ovh.net/files/10Mb.dat", "ovh-proof"},
		{"https://speed.example.com/100MB.bin", ""},
		{"not a url", ""},
		{"", ""},
	} {
		server, ok := SpeedServerForURL(test.url)
		if test.wantID == "" {
			if ok {
				t.Errorf("SpeedServerForURL(%q) = %q, want no server", test.url, server.ID)
			}
			continue
		}
		if !ok || server.ID != test.wantID {
			t.Errorf("SpeedServerForURL(%q) = %q, %v, want %q", test.url, server.ID, ok, test.wantID)
		}
	}
	if _, ok := SpeedServerByID("hetzner-falkenstein-fsn1"); !ok {
		t.Fatal("a catalogue ID does not resolve")
	}
	if _, ok := SpeedServerByID("https://fsn1-speed.hetzner.com/100MB.bin"); ok {
		t.Fatal("a URL resolved as if it were a catalogue ID")
	}
}

// A job may name a catalogue server only for a download probe, and only one the
// catalogue knows: anything else would be a URL by another name.
func TestJobValidationAcceptsOnlyCatalogueServersOnADownloadProbe(t *testing.T) {
	fixture := newManagerFixture(t)
	for _, test := range []struct {
		name    string
		profile TestProfile
		wantErr bool
	}{
		{"a catalogue server", TestProfile{ID: ProfileDownload, Method: ProbeMethodDownload, ServerID: "edis-new-york"}, false},
		{"an unknown server", TestProfile{ID: ProfileDownload, Method: ProbeMethodDownload, ServerID: "somewhere-else"}, true},
		{"a probe that downloads nothing", TestProfile{ID: ProfileStatus, Method: ProbeMethodStatus, ServerID: "edis-new-york"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.manager.RegisterJob(RegisterJobRequest{
				SessionID: fixture.session.SessionID, AgentID: "agent-eu",
				Profile: test.profile, ExpiresAt: fixture.now.Add(time.Minute),
			})
			if test.wantErr != errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("register job error = %v, want invalid request: %t", err, test.wantErr)
			}
		})
	}
}

// The server an observation names is part of what the agent signed, and it has
// to be the one the job asked for. Naming none is allowed: the agent measured its
// own URL, which the verdict then treats as not comparable.
func TestObservationMayNameOnlyTheServerItsJobAskedFor(t *testing.T) {
	newFixture := func(t *testing.T) (managerFixture, DiagnosticJob) {
		fixture := newManagerFixture(t)
		session, err := fixture.manager.CreateSession(CreateSessionRequest{
			StableID: "stable-node-2", Trigger: TriggerManual, ConfigGeneration: 7,
			ConfigFingerprint:   ConfigFingerprint([]byte("config-2")),
			LocalResultSnapshot: LocalResultSnapshot{Status: ProbeStatusUnknown},
			RequestedAgents:     []string{"agent-eu"},
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		job, err := fixture.manager.RegisterJob(RegisterJobRequest{
			SessionID: session.SessionID, AgentID: "agent-eu",
			Profile:   TestProfile{ID: ProfileDownload, Method: ProbeMethodDownload, ServerID: "edis-new-york"},
			ExpiresAt: fixture.now.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("register job: %v", err)
		}
		return fixture, job
	}
	observe := func(t *testing.T, fixture managerFixture, job DiagnosticJob, serverID string) error {
		observation := Observation{
			SchemaVersion: ObservationSchemaVersion, AgentID: job.AgentID, SessionID: job.SessionID,
			JobID: job.JobID, Nonce: job.Nonce, StableID: job.StableID,
			ConfigGeneration: job.ConfigGeneration, ConfigFingerprint: job.ConfigFingerprint,
			CheckedAt: *fixture.now, DurationMillis: 1, EndpointProfile: job.Profile.ID,
			Status:             ProbeStatusOnline,
			DirectConnectivity: CheckEvidence{Checked: true, Online: true},
			Throughput:         &ThroughputEvidence{Bytes: 1, DurationMillis: 1, Mbps: 100},
			AgentVersion:       "test", SpeedServerID: serverID,
		}
		_, err := fixture.manager.AcceptObservation(signObservation(t, observation, fixture.privateKey))
		return err
	}

	t.Run("the requested server", func(t *testing.T) {
		fixture, job := newFixture(t)
		if err := observe(t, fixture, job, "edis-new-york"); err != nil {
			t.Fatalf("accept: %v", err)
		}
	})
	t.Run("no server", func(t *testing.T) {
		fixture, job := newFixture(t)
		if err := observe(t, fixture, job, ""); err != nil {
			t.Fatalf("accept: %v", err)
		}
	})
	t.Run("another catalogue server", func(t *testing.T) {
		fixture, job := newFixture(t)
		if err := observe(t, fixture, job, "ovh-proof"); !errors.Is(err, ErrBindingMismatch) {
			t.Fatalf("error = %v, want a binding mismatch", err)
		}
	})
	t.Run("an unknown server", func(t *testing.T) {
		fixture, job := newFixture(t)
		if err := observe(t, fixture, job, "made-up"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("error = %v, want an invalid request", err)
		}
	})
}

// An agent that predates the field signs a payload without it. The controller
// re-encodes what it received before verifying, so an empty server must vanish
// from that encoding exactly as it never existed.
func TestAnObservationWithoutAServerSignsTheSameAsBeforeTheField(t *testing.T) {
	payload, err := ObservationSigningPayload(Observation{SchemaVersion: ObservationSchemaVersion, AgentVersion: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "speedServerId") {
		t.Fatalf("signing payload = %s, want no speedServerId key", payload)
	}
}
