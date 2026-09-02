package agentautomation

import (
	"testing"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
)

type fakeSessionController struct {
	enabled  bool
	requests []remoteprobe.CreateAutomaticRequest
	views    map[string]remoteprobe.SessionView
	err      error
}

func (f *fakeSessionController) Enabled() bool { return f.enabled }

func (f *fakeSessionController) CreateAutomatic(request remoteprobe.CreateAutomaticRequest) (remoteprobe.SessionView, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return remoteprobe.SessionView{}, f.err
	}
	view := remoteprobe.SessionView{Session: diagnostics.DiagnosticSession{
		SchemaVersion: diagnostics.SessionSchemaVersion,
		SessionID:     "diag-one", StableID: request.StableID, Trigger: request.Trigger,
		AutomationContext: request.AutomationContext,
		RequestedAgents:   []string{"agent-one"}, State: diagnostics.SessionStateRequested,
	}}
	if f.views == nil {
		f.views = make(map[string]remoteprobe.SessionView)
	}
	f.views[view.Session.SessionID] = view
	return view, nil
}

func (f *fakeSessionController) Session(sessionID string) (remoteprobe.SessionView, bool) {
	view, ok := f.views[sessionID]
	return view, ok
}

// expire drives a session to a terminal state with whatever the agent managed to
// return, which is nothing when it never claimed the job.
func (f *fakeSessionController) expire(sessionID string, observations []diagnostics.AcceptedObservation) {
	view, ok := f.views[sessionID]
	if !ok {
		return
	}
	view.Session.State = diagnostics.SessionStateExpired
	view.Session.AgentObservations = observations
	f.views[sessionID] = view
}

type fakeAgentSource struct{}

func (fakeAgentSource) Agent(agentID string) (probeagent.AgentSnapshot, bool) {
	return probeagent.AgentSnapshot{AgentID: agentID, DisplayName: "EU probe", Region: "DE", Provider: "example"}, true
}

func TestSpeedFallbackAutomationRequiresAnAttemptedUnresolvedFallbackAndDeduplicates(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
		{StableID: "node-two", Error: "context deadline exceeded"},
	}}
	first := coordinator.StartSpeedDiagnostics(report, 10)
	second := coordinator.StartSpeedDiagnostics(report, 10)
	if len(first) != 1 || first["node-one"].SessionID != "diag-one" || second["node-one"].SessionID != "diag-one" {
		t.Fatalf("handles = first:%+v second:%+v", first, second)
	}
	if len(controller.requests) != 1 {
		t.Fatalf("automatic creates = %d, want one", len(controller.requests))
	}
	request := controller.requests[0]
	if request.Trigger != diagnostics.TriggerAutoSpeedFallback || request.ProfileID != diagnostics.ProfileDownload ||
		request.AutomationContext.Outcome != diagnostics.AutomationOutcomeTechnical || request.AutomationContext.FallbackAttempts != 2 {
		t.Fatalf("automatic request = %+v", request)
	}
}

func TestSpeedDiagnosticAnnotationUsesReliableRemoteEvidenceWithoutChangingTheResult(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-one", Mbps: 2, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true,
	}}}
	handles := coordinator.StartSpeedDiagnostics(report, 10)
	view := controller.views["diag-one"]
	view.Session.State = diagnostics.SessionStateCompleted
	view.Session.AgentObservations = []diagnostics.AcceptedObservation{{
		Reliable: true,
		Observation: diagnostics.Observation{
			Status: diagnostics.ProbeStatusOnline, CheckedAt: time.Now(),
			DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
			Throughput:         &diagnostics.ThroughputEvidence{Mbps: 3},
		},
	}}
	controller.views["diag-one"] = view
	annotations := coordinator.Annotations(handles)
	annotation := annotations["node-one"]
	if annotation.State != speedtest.AgentDiagnosticReproduced || annotation.Mbps != 3 || annotation.AgentName != "EU probe" {
		t.Fatalf("annotation = %+v", annotation)
	}
	if report.Results[0].AgentDiagnostic != nil || report.Results[0].Mbps != 2 {
		t.Fatalf("operational report was mutated: %+v", report.Results[0])
	}
}

func TestAlternativeTunnelledEndpointMeansTheNodeFailureWasNotReproduced(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	handles := coordinator.StartSpeedDiagnostics(speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-one", Error: "timeout", FallbackAttempted: true, FallbackAttempts: 1, FallbackExhausted: true,
	}}}, 10)
	view := controller.views["diag-one"]
	view.Session.State = diagnostics.SessionStateCompleted
	view.Session.AgentObservations = []diagnostics.AcceptedObservation{{Reliable: true, Observation: diagnostics.Observation{
		Status:              diagnostics.ProbeStatusProxyFailure,
		AlternativeEndpoint: &diagnostics.AlternativeEndpointObservation{ProfileID: diagnostics.ProfileStatus, Status: diagnostics.ProbeStatusOnline},
		DirectConnectivity:  diagnostics.CheckEvidence{Checked: true, Online: true},
	}}}
	controller.views["diag-one"] = view
	annotation := coordinator.Annotations(handles)["node-one"]
	if annotation.State != speedtest.AgentDiagnosticNotReproduced || annotation.AlternativeStatus != "online" {
		t.Fatalf("annotation = %+v", annotation)
	}
}

func TestLowSpeedObservationWithoutThroughputIsInsufficient(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	handles := coordinator.StartSpeedDiagnostics(speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-one", Mbps: 2, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true,
	}}}, 10)
	view := controller.views["diag-one"]
	view.Session.State = diagnostics.SessionStateCompleted
	view.Session.AgentObservations = []diagnostics.AcceptedObservation{{Reliable: true, Observation: diagnostics.Observation{
		Status: diagnostics.ProbeStatusOnline, DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
	}}}
	controller.views["diag-one"] = view
	annotation := coordinator.Annotations(handles)["node-one"]
	if annotation.State != speedtest.AgentDiagnosticUnreliable || annotation.Detail != "agent download observation has no throughput evidence" {
		t.Fatalf("annotation = %+v", annotation)
	}
}

// A refusal that never started a session must not occupy the cooldown: an agent
// that reconnects a moment later should be usable on the next run.
func TestTransientRefusalDoesNotOccupyTheCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true, err: remoteprobe.ErrUnavailableAgent}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: 30 * time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
	}}

	if got := coordinator.StartSpeedDiagnostics(report, 10)["node-one"].State; got != speedtest.AgentDiagnosticUnavailable {
		t.Fatalf("first attempt state = %q, want unavailable", got)
	}

	// The agent is back before the cooldown would have elapsed.
	controller.err = nil
	if got := coordinator.StartSpeedDiagnostics(report, 10)["node-one"].SessionID; got == "" {
		t.Fatal("second attempt was suppressed by a cooldown no session ever earned")
	}
	if len(controller.requests) != 2 {
		t.Fatalf("automatic creates = %d, want the refused attempt to be retried", len(controller.requests))
	}
}

// The agent reports whole Mbps, so its true rate lies in [Mbps, Mbps+1). Only a
// whole interval below the threshold proves a slowdown; an interval straddling it
// must not be announced as reproduced.
func TestAgentThroughputAtTheThresholdBoundaryIsNotCalledReproduced(t *testing.T) {
	for _, test := range []struct {
		name      string
		agentMbps int64
		want      string
	}{
		{"whole interval below the threshold", 9, speedtest.AgentDiagnosticReproduced},
		{"interval straddles the threshold", 10, speedtest.AgentDiagnosticNotReproduced},
		{"clearly above", 40, speedtest.AgentDiagnosticNotReproduced},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller := &fakeSessionController{enabled: true}
			coordinator, err := New(Config{Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2}, controller, fakeAgentSource{})
			if err != nil {
				t.Fatal(err)
			}
			report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
				StableID: "node-one", Mbps: 2, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true,
			}}}
			handles := coordinator.StartSpeedDiagnostics(report, 10.5)

			view := controller.views["diag-one"]
			view.Session.State = diagnostics.SessionStateCompleted
			view.Session.AgentObservations = []diagnostics.AcceptedObservation{{
				Reliable: true,
				Observation: diagnostics.Observation{
					AgentID: "agent-signing", Status: diagnostics.ProbeStatusOnline, CheckedAt: time.Now(),
					DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
					Throughput:         &diagnostics.ThroughputEvidence{Mbps: test.agentMbps},
				},
			}}
			controller.views["diag-one"] = view

			annotation := coordinator.Annotations(handles)["node-one"]
			if annotation.State != test.want {
				t.Fatalf("state for %d Mbps against a threshold of 10.5 = %q, want %q", test.agentMbps, annotation.State, test.want)
			}
			// The alert names whoever signed the evidence, not whoever was asked.
			if annotation.AgentID != "agent-signing" {
				t.Errorf("AgentID = %q, want the agent that signed the observation", annotation.AgentID)
			}
		})
	}
}

// An agent is selected while it still looks connected — liveness is a freshness
// window, so "connected" means "was answering a moment ago". If it has already
// gone away it never claims the job, the session expires with nothing in it, and
// holding the cooldown would silence the node for half an hour over a diagnostic
// that never happened.
func TestASessionThatCollectedNothingReleasesTheCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: 30 * time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
	}}
	if got := coordinator.StartSpeedDiagnostics(report, 10)["node-one"].SessionID; got == "" {
		t.Fatal("the first attempt did not create a session")
	}

	// The agent never claimed the job and it expired without an observation.
	controller.expire("diag-one", nil)

	if got := coordinator.StartSpeedDiagnostics(report, 10)["node-one"].SessionID; got == "" {
		t.Fatal("the retry was suppressed by a cooldown an empty session did not earn")
	}
	if len(controller.requests) != 2 {
		t.Fatalf("automatic creates = %d, want the empty session to be retried", len(controller.requests))
	}
}

// A session that did answer keeps its cooldown: repeating it would ask the same
// question while its evidence is still fresh.
func TestASessionThatAnsweredKeepsItsCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: 30 * time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
	}}
	coordinator.StartSpeedDiagnostics(report, 10)

	controller.expire("diag-one", []diagnostics.AcceptedObservation{{
		Observation: diagnostics.Observation{AgentID: "agent-one", Status: diagnostics.ProbeStatusOffline},
		Reliable:    true,
	}})

	coordinator.StartSpeedDiagnostics(report, 10)
	if len(controller.requests) != 1 {
		t.Fatalf("automatic creates = %d, want the answered session to hold its cooldown", len(controller.requests))
	}
}

// An agent whose own connectivity control failed still answered, and naming it
// in the alert is useful. Repeating it would ask the same broken agent again.
func TestAnUnreliableAnswerStillHoldsTheCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: 30 * time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
	}}
	coordinator.StartSpeedDiagnostics(report, 10)

	controller.expire("diag-one", []diagnostics.AcceptedObservation{{
		Observation: diagnostics.Observation{AgentID: "agent-one", Status: diagnostics.ProbeStatusOffline},
		Reliable:    false,
	}})

	coordinator.StartSpeedDiagnostics(report, 10)
	if len(controller.requests) != 1 {
		t.Fatalf("automatic creates = %d, want an unreliable answer to hold its cooldown", len(controller.requests))
	}
}

// A session the manager has already discarded says nothing about whether it
// answered, so the cooldown stays rather than guessing towards repeating work.
func TestAForgottenSessionKeepsItsCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: 30 * time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-one", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
	}}
	coordinator.StartSpeedDiagnostics(report, 10)

	delete(controller.views, "diag-one")

	coordinator.StartSpeedDiagnostics(report, 10)
	if len(controller.requests) != 1 {
		t.Fatalf("automatic creates = %d, want a forgotten session to hold its cooldown", len(controller.requests))
	}
}
