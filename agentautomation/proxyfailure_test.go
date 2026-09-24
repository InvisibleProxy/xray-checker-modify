package agentautomation

import (
	"context"
	"testing"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
)

func newProxyFailureCoordinator(t *testing.T, controller *fakeSessionController, now *time.Time, configure ...func(*Config)) *Coordinator {
	t.Helper()
	config := Config{
		Enabled: true, ProxyFailureEnabled: true, Cooldown: 15 * time.Minute, AlertWait: time.Second, MaxConcurrent: 2,
		PollInterval: time.Millisecond,
		Now:          func() time.Time { return *now },
	}
	for _, apply := range configure {
		apply(&config)
	}
	coordinator, err := New(config, controller, fakeAgentSource{})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func answer(controller *fakeSessionController, sessionID string, status diagnostics.ProbeStatus) {
	view := controller.views[sessionID]
	view.Session.State = diagnostics.SessionStateCompleted
	view.Session.AgentObservations = []diagnostics.AcceptedObservation{{
		Reliable: true,
		Observation: diagnostics.Observation{
			AgentID: "agent-one", Status: status, CheckedAt: time.Date(2026, 9, 1, 1, 3, 0, 0, time.UTC),
			DirectConnectivity: diagnostics.CheckEvidence{Checked: true, Online: true},
		},
	}}
	controller.views[sessionID] = view
}

// One episode, one probe: a node that stays in proxy_failure check after check
// is not asked about again, and what it is asked is the availability question
// under the trigger reserved for it.
func TestProxyFailureStartsOneProbePerEpisode(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	since := now.Add(-time.Minute)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) {
		config.ProxyFailureProfileID = diagnostics.ProfileIP
	})

	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: since}})
	now = now.Add(5 * time.Minute)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: since}})
	answer(controller, "diag-one", diagnostics.ProbeStatusProxyFailure)
	now = now.Add(time.Hour)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: since}})

	if len(controller.requests) != 1 {
		t.Fatalf("automatic creates = %d, want one for the whole episode", len(controller.requests))
	}
	request := controller.requests[0]
	if request.Trigger != diagnostics.TriggerAutoProxyFailure || request.ProfileID != diagnostics.ProfileIP ||
		request.AutomationContext != diagnostics.ProxyFailureAutomationContext() {
		t.Fatalf("automatic request = %+v", request)
	}
	annotation := coordinator.ProxyFailureAnnotations([]string{"node-one"})["node-one"]
	if annotation.State != speedtest.AgentDiagnosticReproduced || annotation.AgentName != "EU probe" {
		t.Fatalf("annotation = %+v, want the agent's reproduced failure", annotation)
	}
	if annotation.Task == nil || annotation.Task.Kind != diagnostics.AutomationKindProxyFailure {
		t.Fatalf("task = %+v, want the proxy-failure kind", annotation.Task)
	}
}

func TestProxyFailureTunnelThatWorksForTheAgentIsNotReproduced(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	answer(controller, "diag-one", diagnostics.ProbeStatusOnline)

	annotation := coordinator.ProxyFailureAnnotations([]string{"node-one"})["node-one"]
	if annotation.State != speedtest.AgentDiagnosticNotReproduced {
		t.Fatalf("annotation state = %q, want not reproduced", annotation.State)
	}
}

// The trigger is opt-in, and a panel-added node never spends an agent slot on
// its own, whichever automation would have asked.
func TestProxyFailureSkipsWhenDisabledAndForPanelSourcedNodes(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	disabled := &fakeSessionController{enabled: true}
	newProxyFailureCoordinator(t, disabled, &now, func(config *Config) {
		config.ProxyFailureEnabled = false
	}).StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	if len(disabled.requests) != 0 {
		t.Fatalf("creates with the trigger off = %d, want none", len(disabled.requests))
	}

	foreign := &fakeSessionController{enabled: true}
	coordinator := newProxyFailureCoordinator(t, foreign, &now, func(config *Config) {
		config.EnvironmentSourced = func(stableID string) bool { return stableID != "node-foreign" }
	})
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{
		{StableID: "node-foreign", Since: now},
		{StableID: "node-own", Since: now},
	})
	if len(foreign.requests) != 1 || foreign.requests[0].StableID != "node-own" {
		t.Fatalf("requests = %+v, want only the environment's node", foreign.requests)
	}
	if annotations := coordinator.ProxyFailureAnnotations([]string{"node-foreign"}); len(annotations) != 0 {
		t.Fatalf("annotations for the panel-added node = %+v, want none", annotations)
	}
}

// A refused start is not an answer. It is reported for the alert that reads it
// now and asked again at the next check, instead of occupying the episode.
func TestProxyFailureRetriesARefusedStartAtTheNextCheck(t *testing.T) {
	controller := &fakeSessionController{enabled: true, err: remoteprobe.ErrUnavailableAgent}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)
	failures := []ProxyFailure{{StableID: "node-one", Since: now}}

	coordinator.StartProxyFailureDiagnostics(failures)
	annotation := coordinator.ProxyFailureAnnotations([]string{"node-one"})["node-one"]
	if annotation.State != speedtest.AgentDiagnosticUnavailable || annotation.Detail != "no healthy idle diagnostic agent is connected" {
		t.Fatalf("annotation = %+v, want the refusal", annotation)
	}

	controller.err = nil
	now = now.Add(5 * time.Minute)
	coordinator.StartProxyFailureDiagnostics(failures)
	if len(controller.requests) != 2 {
		t.Fatalf("creates = %d, want the refused start asked again", len(controller.requests))
	}
	if annotation := coordinator.ProxyFailureAnnotations([]string{"node-one"})["node-one"]; annotation.State != speedtest.AgentDiagnosticRunning {
		t.Fatalf("annotation state = %q, want running", annotation.State)
	}
}

// A session that expired without a single observation collected nothing, so
// the episode is still unanswered.
func TestProxyFailureRetriesAnAbandonedSession(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)
	failures := []ProxyFailure{{StableID: "node-one", Since: now}}

	coordinator.StartProxyFailureDiagnostics(failures)
	controller.expire("diag-one", nil)
	now = now.Add(5 * time.Minute)
	coordinator.StartProxyFailureDiagnostics(failures)
	if len(controller.requests) != 2 {
		t.Fatalf("creates = %d, want the abandoned episode asked again", len(controller.requests))
	}
}

// A node flapping between online and proxy_failure starts a new episode on
// every flap. Inside the cooldown the answer it already has stands; after it, a
// new episode gets a probe of its own.
func TestProxyFailureNewEpisodeReusesAFreshAnswerAndProbesAfterTheCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)

	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	answer(controller, "diag-one", diagnostics.ProbeStatusProxyFailure)
	now = now.Add(5 * time.Minute)
	coordinator.StartProxyFailureDiagnostics(nil)
	now = now.Add(5 * time.Minute)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	if len(controller.requests) != 1 {
		t.Fatalf("creates inside the cooldown = %d, want the fresh answer reused", len(controller.requests))
	}

	now = now.Add(10 * time.Minute)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	if len(controller.requests) != 2 {
		t.Fatalf("creates after the cooldown = %d, want a probe for the new episode", len(controller.requests))
	}
}

// Once the node is no longer failing and the cooldown is over, nothing is left
// to read: an alert about some later outage must not show this one's answer.
func TestProxyFailureReleasesARecoveredNodeAfterTheCooldown(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	answer(controller, "diag-one", diagnostics.ProbeStatusProxyFailure)

	now = now.Add(20 * time.Minute)
	coordinator.StartProxyFailureDiagnostics(nil)
	if annotations := coordinator.ProxyFailureAnnotations([]string{"node-one"}); len(annotations) != 0 {
		t.Fatalf("annotations after recovery = %+v, want none", annotations)
	}
}

// The manager evicts old sessions; the verdict of an episode that outlives its
// probe must still reach the reminders.
func TestProxyFailureVerdictOutlivesTheSession(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)
	failures := []ProxyFailure{{StableID: "node-one", Since: now}}
	coordinator.StartProxyFailureDiagnostics(failures)
	answer(controller, "diag-one", diagnostics.ProbeStatusProxyFailure)
	coordinator.ProxyFailureAnnotations([]string{"node-one"})

	delete(controller.views, "diag-one")
	now = now.Add(6 * time.Hour)
	coordinator.StartProxyFailureDiagnostics(failures)
	if len(controller.requests) != 1 {
		t.Fatalf("creates after eviction = %d, want the answered episode left alone", len(controller.requests))
	}
	if annotation := coordinator.ProxyFailureAnnotations([]string{"node-one"})["node-one"]; annotation.State != speedtest.AgentDiagnosticReproduced {
		t.Fatalf("annotation after eviction = %+v, want the kept verdict", annotation)
	}
}

// Both automations spend the same agents, so they share one limit, and a speed
// run waits for a proxy-failure probe on the node it is about to measure.
func TestProxyFailureSharesCapacityAndIdleWaitWithSpeedProbes(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) {
		config.MaxConcurrent = 1
	})
	coordinator.StartSpeedDiagnostics(speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{
		{StableID: "node-speed", Error: "timeout"},
	}}, 10)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	annotation := coordinator.ProxyFailureAnnotations([]string{"node-one"})["node-one"]
	if annotation.State != speedtest.AgentDiagnosticUnavailable || annotation.Detail != "automation capacity is busy" {
		t.Fatalf("annotation = %+v, want the shared limit reached", annotation)
	}

	controller.complete("diag-one")
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})
	if got := coordinator.measuringCount(map[string]bool{"node-one": true}); got != 1 {
		t.Fatalf("probes measuring node-one = %d, want the proxy-failure probe counted", got)
	}
	if snapshot := coordinator.Snapshot(); !snapshot.ProxyFailureEnabled || snapshot.Active != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestAwaitProxyFailureReturnsOnceTheProbeAnswers(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now)
	coordinator.StartProxyFailureDiagnostics([]ProxyFailure{{StableID: "node-one", Since: now}})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if annotation := coordinator.AwaitProxyFailure(ctx, []string{"node-one"})["node-one"]; annotation.State != speedtest.AgentDiagnosticRunning {
		t.Fatalf("annotation at the deadline = %+v, want still running", annotation)
	}
	answer(controller, "diag-one", diagnostics.ProbeStatusProxyFailure)
	if annotation := coordinator.AwaitProxyFailure(context.Background(), []string{"node-one"})["node-one"]; annotation.State != speedtest.AgentDiagnosticReproduced {
		t.Fatalf("annotation = %+v, want the answer", annotation)
	}
}

// A transport probe cannot answer a proxy failure: the host already answers.
func TestProxyFailureProfileMustBeTunnelled(t *testing.T) {
	_, err := New(Config{Enabled: true, ProxyFailureProfileID: diagnostics.ProfileTLS}, &fakeSessionController{enabled: true}, fakeAgentSource{})
	if err == nil {
		t.Fatal("a transport profile was accepted for the proxy-failure trigger")
	}
}
