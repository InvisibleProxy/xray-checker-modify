package remoteprobe

import (
	"context"
	"strings"
	"testing"

	"xray-checker/diagnostics"
)

// The agent is asked for the server the run measured whenever it can download
// from a catalogue server, whatever the outcome; an agent that cannot is left to
// its own URL and the verdict then treats its rate as not comparable.
func TestAutomaticDownloadProfileAsksCapableAgentsForTheRunServer(t *testing.T) {
	descriptor, _ := diagnostics.ProfileByID(diagnostics.ProfileDownload)
	capable := []string{diagnostics.CapabilityControlV1, diagnostics.CapabilityDiagnosticV1, diagnostics.CapabilitySpeedServersV1}
	for _, test := range []struct {
		name         string
		context      diagnostics.AutomationContext
		capabilities []string
		want         string
	}{
		{
			name:         "a slowdown against a catalogue server",
			context:      diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeLowSpeed, SpeedServerID: "edis-new-york"},
			capabilities: capable, want: "edis-new-york",
		},
		{
			name:         "a technical failure against a catalogue server",
			context:      diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeTechnical, SpeedServerID: "edis-new-york"},
			capabilities: capable, want: "edis-new-york",
		},
		{
			name:         "an agent that predates the catalogue",
			context:      diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeLowSpeed, SpeedServerID: "edis-new-york"},
			capabilities: []string{diagnostics.CapabilityControlV1, diagnostics.CapabilityDiagnosticV1},
		},
		{
			name:         "a run whose server is not in the catalogue",
			context:      diagnostics.AutomationContext{Outcome: diagnostics.AutomationOutcomeLowSpeed},
			capabilities: capable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := automaticProfile(descriptor, diagnostics.ProfileStatus, test.context, test.capabilities)
			if profile.ServerID != test.want {
				t.Fatalf("server = %q, want %q", profile.ServerID, test.want)
			}
		})
	}

	status, _ := diagnostics.ProfileByID(diagnostics.ProfileStatus)
	if profile := automaticProfile(status, diagnostics.ProfileIP, diagnostics.AutomationContext{SpeedServerID: "edis-new-york"}, capable); profile.ServerID != "" {
		t.Fatalf("a status probe was given a speed-test server: %+v", profile)
	}
}

// The fixture's agent advertises only the v1 capabilities, which is what every
// agent deployed before the catalogue sends: its job must not name a server.
func TestCreateAutomaticLeavesTheServerOutForAnAgentWithoutTheCatalogue(t *testing.T) {
	fixture := newControllerFixture(t)
	_, err := fixture.controller.CreateAutomatic(CreateAutomaticRequest{
		StableID: "node-one", Trigger: diagnostics.TriggerAutoSpeedFallback, ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeLowSpeed,
			Source: "schedule", ThresholdMbps: 100, ObservedMbps: 3, SpeedServerID: "leaseweb-amsterdam-ams1",
		},
	})
	if err != nil {
		t.Fatalf("create automatic diagnostics: %v", err)
	}
	assignment, err := fixture.controller.Claim(context.Background(), fixture.agentID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if assignment.Job.Profile.ServerID != "" {
		t.Fatalf("job profile = %+v, want no server for an agent without the catalogue", assignment.Job.Profile)
	}
}

func TestSpeedSummaryFollowsTheVerdict(t *testing.T) {
	session := func(observed float64, agent int64, runServer, agentServer string) diagnostics.DiagnosticSession {
		return diagnostics.DiagnosticSession{
			Trigger: diagnostics.TriggerAutoSpeedFallback,
			AutomationContext: diagnostics.AutomationContext{
				Kind: diagnostics.AutomationKindSpeedFallback, Outcome: diagnostics.AutomationOutcomeLowSpeed,
				ThresholdMbps: 50, ObservedMbps: observed, SpeedServerID: runServer,
			},
			AgentObservations: []diagnostics.AcceptedObservation{{Reliable: true, Observation: diagnostics.Observation{
				Status: diagnostics.ProbeStatusOnline, DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
				Throughput: &diagnostics.ThroughputEvidence{Mbps: agent}, SpeedServerID: agentServer,
			}}},
		}
	}
	for _, test := range []struct {
		name    string
		session diagnostics.DiagnosticSession
		want    string
	}{
		{"reproduced on the same server", session(20, 30, "edis-new-york", "edis-new-york"), "against the same speed-test server"},
		{"many times faster but below the threshold", session(2, 40, "edis-new-york", "ovh-proof"), "not above suspicion"},
		{"many times faster above the threshold", session(2, 400, "edis-new-york", "edis-new-york"), "not in the node"},
		{"the same range on another server", session(20, 30, "edis-new-york", "ovh-proof"), "different speed-test server"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if summary := summarize(test.session); !strings.Contains(summary, test.want) {
				t.Fatalf("summary = %q, want it to mention %q", summary, test.want)
			}
		})
	}
}

// An unreachable node that answers from elsewhere is a block on the checker's
// path; one that answers nowhere is down. The summary is what tells the operator
// which of the two to act on.
func TestOfflineSummarySeparatesABlockedPathFromADeadHost(t *testing.T) {
	session := diagnostics.DiagnosticSession{
		Trigger:             diagnostics.TriggerAutoOffline,
		LocalResultSnapshot: diagnostics.LocalResultSnapshot{Status: diagnostics.ProbeStatusOffline, Failure: diagnostics.FailureEvidence{Code: "host_unreachable"}},
		AgentObservations: []diagnostics.AcceptedObservation{{Reliable: true, Observation: diagnostics.Observation{
			Status: diagnostics.ProbeStatusOnline, DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
		}}},
	}
	if summary := summarize(session); !strings.Contains(summary, "block") {
		t.Fatalf("summary = %q, want a blocked path", summary)
	}
	session.AgentObservations[0].Observation.Status = diagnostics.ProbeStatusOffline
	if summary := summarize(session); !strings.Contains(summary, "unreachable from another network as well") {
		t.Fatalf("summary = %q, want a dead host", summary)
	}
	session.AgentObservations[0].Observation.Status = diagnostics.ProbeStatusProxyFailure
	if summary := summarize(session); !strings.Contains(summary, "proxy service") {
		t.Fatalf("summary = %q, want a dead proxy service", summary)
	}
}
