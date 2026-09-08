package agentautomation

import (
	"context"
	"fmt"
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
	created  int
}

func (f *fakeSessionController) Enabled() bool { return f.enabled }

func (f *fakeSessionController) CreateAutomatic(request remoteprobe.CreateAutomaticRequest) (remoteprobe.SessionView, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return remoteprobe.SessionView{}, f.err
	}
	view := remoteprobe.SessionView{Session: diagnostics.DiagnosticSession{
		SchemaVersion: diagnostics.SessionSchemaVersion,
		SessionID:     f.nextSessionID(), StableID: request.StableID, Trigger: request.Trigger,
		AutomationContext: request.AutomationContext,
		RequestedAgents:   []string{"agent-one"}, State: diagnostics.SessionStateRequested,
	}}
	if f.views == nil {
		f.views = make(map[string]remoteprobe.SessionView)
	}
	f.views[view.Session.SessionID] = view
	return view, nil
}

// nextSessionID keeps the first session named as every single-session test
// expects it, and names the ones after it apart so a test can hold two.
func (f *fakeSessionController) nextSessionID() string {
	f.created++
	if f.created == 1 {
		return "diag-one"
	}
	return fmt.Sprintf("diag-%d", f.created)
}

// complete drives a session to a terminal state with an answer, which is what
// releases its concurrency slot.
func (f *fakeSessionController) complete(sessionID string) {
	view, ok := f.views[sessionID]
	if !ok {
		return
	}
	view.Session.State = diagnostics.SessionStateCompleted
	view.Session.AgentObservations = []diagnostics.AcceptedObservation{{
		Reliable: true,
		Observation: diagnostics.Observation{
			Status: diagnostics.ProbeStatusOnline, CheckedAt: time.Now(),
			DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
			Throughput:         &diagnostics.ThroughputEvidence{Mbps: 500},
		},
	}}
	f.views[sessionID] = view
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

// A country fallback is not a precondition. Requiring one withheld the second
// vantage point from the failures that need it most: a node whose country has
// no fallback endpoint configured never attempts one, and so was never
// diagnosed at all.
func TestSpeedAutomationDiagnosesAnUnresolvedMeasurementWithOrWithoutAFallbackAndDeduplicates(t *testing.T) {
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
	if len(first) != 2 || first["node-one"].SessionID == "" || first["node-two"].SessionID == "" {
		t.Fatalf("handles = %+v, want a session for both unresolved nodes", first)
	}
	if second["node-one"].SessionID != first["node-one"].SessionID ||
		second["node-two"].SessionID != first["node-two"].SessionID {
		t.Fatalf("second pass handles = %+v, want the sessions the first pass created", second)
	}
	if len(controller.requests) != 2 {
		t.Fatalf("automatic creates = %d, want one per node", len(controller.requests))
	}
	request := controller.requests[0]
	if request.Trigger != diagnostics.TriggerAutoSpeedFallback || request.ProfileID != diagnostics.ProfileDownload ||
		request.AutomationContext.Outcome != diagnostics.AutomationOutcomeTechnical || request.AutomationContext.FallbackAttempts != 2 {
		t.Fatalf("automatic request = %+v", request)
	}
	if got := controller.requests[1].AutomationContext.FallbackAttempts; got != 0 {
		t.Fatalf("fallback attempts for the node that never tried one = %d, want zero", got)
	}
}

// A healthy measurement is not diagnosed, and neither is an offline node: its
// speed test never ran, so there is no measurement for an agent to reproduce.
func TestSpeedAutomationSkipsHealthyOfflineAndMaintenanceResults(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{
		Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 4,
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-fast", Mbps: 500},
		{StableID: "node-offline", Offline: true},
		{StableID: "node-paused", Error: "context deadline exceeded", MaintenanceProbe: true},
		{StableID: "node-project", Mbps: 2, ProjectMaintenanceProbe: true},
	}}
	if handles := coordinator.StartSpeedDiagnostics(report, 100); len(handles) != 0 {
		t.Fatalf("handles = %+v, want none", handles)
	}
	if len(controller.requests) != 0 {
		t.Fatalf("automatic creates = %d, want none", len(controller.requests))
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

// The last free slot used to go to whoever the run happened to measure first.
// A report that carries a timeout and a measurable slowdown must spend it on
// the slowdown: that is the only one where the agent answers with a number the
// run's own number can be held against.
func TestTheLastSlotGoesToTheDeepestSlowdownRatherThanTheFirstResultListed(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	coordinator, err := New(Config{
		Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 1,
		Now: func() time.Time { return now },
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-timeout", Error: "context deadline exceeded", FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true},
		{StableID: "node-mild", Mbps: 80, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true},
		{StableID: "node-severe", Mbps: 10, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true},
	}}

	handles := coordinator.StartSpeedDiagnostics(report, 100)
	if got := handles["node-severe"].SessionID; got == "" {
		t.Fatalf("the deepest slowdown got no session: %+v", handles["node-severe"])
	}
	for _, stableID := range []string{"node-timeout", "node-mild"} {
		if got := handles[stableID].SessionID; got != "" {
			t.Errorf("%s took the only slot with session %q", stableID, got)
		}
		if got := handles[stableID].State; got != speedtest.AgentDiagnosticUnavailable {
			t.Errorf("%s state = %q, want unavailable", stableID, got)
		}
	}
	if len(controller.requests) != 1 || controller.requests[0].StableID != "node-severe" {
		t.Fatalf("automatic creates = %+v, want one for node-severe", controller.requests)
	}
}

// Capacity frees up inside the alert wait far more often than not, and the node
// refused at second zero used to stay refused until the next run — which is how
// the one node with a measurable slowdown became the one the alert said nothing
// about.
func TestANodeRefusedForCapacityIsRetriedWhenASlotFreesUpDuringTheWait(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{
		Enabled: true, Cooldown: time.Minute, AlertWait: 2 * time.Second, MaxConcurrent: 1,
		PollInterval: time.Millisecond,
	}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-severe", Mbps: 10, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true},
		{StableID: "node-mild", Mbps: 80, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true},
	}}

	handles := coordinator.StartSpeedDiagnostics(report, 100)
	if handles["node-severe"].SessionID != "diag-one" || handles["node-mild"].SessionID != "" {
		t.Fatalf("first pass = %+v", handles)
	}

	// The first session answers, exactly as it does well inside a real wait.
	controller.complete("diag-one")

	// The retry lands about ten poll intervals in; the wait only has to outlast
	// that, and then runs out because the second session never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	annotations := coordinator.Await(ctx, handles)

	if got := annotations["node-mild"].State; got == speedtest.AgentDiagnosticUnavailable {
		t.Fatalf("node-mild was never retried: %+v", annotations["node-mild"])
	}
	if len(controller.requests) != 2 {
		t.Fatalf("automatic creates = %d, want the freed slot to be reused", len(controller.requests))
	}
	// The caller's own map is untouched: Await works on a copy, so a handle it
	// replaced cannot leak back into the caller's bookkeeping.
	if handles["node-mild"].SessionID != "" {
		t.Errorf("Await mutated the caller's handles: %+v", handles["node-mild"])
	}
}

// The run judges a node against its own threshold override when it has one.
// Reading the global setting here instead classified the same node two ways:
// healthy in the alert, worth an agent's time in this package.
func TestAutomationJudgesANodeAgainstTheThresholdTheRunUsed(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    speedtest.Result
		global    float64
		wantStart bool
		wantAgent float64
	}{
		{
			name:   "override clears a node the global setting would have flagged",
			result: speedtest.Result{StableID: "node-one", Mbps: 60, LowSpeedThresholdMbps: 50, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true},
			global: 100,
		},
		{
			name:      "override flags a node the global setting would have cleared",
			result:    speedtest.Result{StableID: "node-one", Mbps: 150, LowSpeedThresholdMbps: 200, FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true},
			global:    100,
			wantStart: true,
			wantAgent: 200,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller := &fakeSessionController{enabled: true}
			now := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
			coordinator, err := New(Config{
				Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
				Now: func() time.Time { return now },
			}, controller, fakeAgentSource{})
			if err != nil {
				t.Fatal(err)
			}
			report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{test.result}}

			handles := coordinator.StartSpeedDiagnostics(report, test.global)
			started := handles["node-one"].SessionID != ""
			if started != test.wantStart {
				t.Fatalf("started = %t, want %t (handles %+v)", started, test.wantStart, handles)
			}
			if !test.wantStart {
				return
			}
			if got := controller.requests[0].AutomationContext.ThresholdMbps; got != test.wantAgent {
				t.Errorf("threshold sent to the agent = %v, want the one the run used (%v)", got, test.wantAgent)
			}
			if got := handles["node-one"].Threshold; got != test.wantAgent {
				t.Errorf("handle threshold = %v, want %v", got, test.wantAgent)
			}
		})
	}
}

// A run that timed out and an agent that gets through at half the threshold is
// not a healthy node. Judging the agent's rate only for a low-speed outcome
// reported exactly that as "not reproduced", which sends an operator to look at
// the checker while the node is the thing that is slow.
func TestAnAgentRateBelowTheThresholdIsReproducedWhateverTheRunFailedWith(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-one", Error: "context deadline exceeded",
		FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true,
	}}}
	handles := coordinator.StartSpeedDiagnostics(report, 100)
	if got := controller.requests[0].AutomationContext.Outcome; got != diagnostics.AutomationOutcomeTechnical {
		t.Fatalf("outcome = %q, want the technical one", got)
	}

	view := controller.views["diag-one"]
	view.Session.State = diagnostics.SessionStateCompleted
	view.Session.AgentObservations = []diagnostics.AcceptedObservation{{
		Reliable: true,
		Observation: diagnostics.Observation{
			Status: diagnostics.ProbeStatusOnline, CheckedAt: time.Now(),
			DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
			Throughput:         &diagnostics.ThroughputEvidence{Mbps: 42},
		},
	}}
	controller.views["diag-one"] = view

	annotation := coordinator.Annotations(handles)["node-one"]
	if annotation.State != speedtest.AgentDiagnosticReproduced {
		t.Fatalf("state = %q for 42 Mbps against a threshold of 100, want reproduced", annotation.State)
	}
	if annotation.RemoteStatus != string(diagnostics.ProbeStatusOnline) {
		t.Errorf("remote status = %q, want the alert to still say the agent got through", annotation.RemoteStatus)
	}
}

// Two rates measured over different amounts are not comparable, and a short
// transfer spends its whole life in TCP slow start. The agent is told how much
// the run moved so it can move the same.
func TestTheAgentIsAskedToTransferWhatTheRunTransferred(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	coordinator, err := New(Config{Enabled: true, Cooldown: time.Minute, AlertWait: time.Second, MaxConcurrent: 2}, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-one", Mbps: 37, DownloadedBytes: 100_000_000,
		FallbackAttempted: true, FallbackAttempts: 1, FallbackUsed: true,
	}}}
	coordinator.StartSpeedDiagnostics(report, 100)
	if got := controller.requests[0].AutomationContext.MeasuredBytes; got != 100_000_000 {
		t.Fatalf("measured bytes sent to the agent = %d, want the amount the run transferred", got)
	}
}
