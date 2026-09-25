package agentautomation

import (
	"sync"
	"testing"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
	"xray-checker/verdictlog"
)

type memoryJournal struct {
	mu      sync.Mutex
	entries []verdictlog.Entry
}

func (m *memoryJournal) Record(entry verdictlog.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
	return nil
}

func (m *memoryJournal) all() []verdictlog.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]verdictlog.Entry(nil), m.entries...)
}

func offline(stableID string, since time.Time) AvailabilityFailure {
	return AvailabilityFailure{StableID: stableID, Kind: diagnostics.AutomationKindOffline, Since: since}
}

// An unreachable node gets its own probe under its own trigger. From a checker
// inside a filtered network a blocked IP and a dead host look the same, and the
// agent's answer is what tells them apart.
func TestOfflineStartsOneProbePerEpisodeUnderItsOwnTrigger(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) {
		config.ProxyFailureEnabled = false
		config.OfflineEnabled = true
	})

	coordinator.StartAvailabilityDiagnostics([]AvailabilityFailure{offline("node-one", now)})
	now = now.Add(5 * time.Minute)
	coordinator.StartAvailabilityDiagnostics([]AvailabilityFailure{offline("node-one", now.Add(-5*time.Minute))})

	if len(controller.requests) != 1 {
		t.Fatalf("automatic creates = %d, want one for the episode", len(controller.requests))
	}
	request := controller.requests[0]
	if request.Trigger != diagnostics.TriggerAutoOffline || request.AutomationContext != diagnostics.OfflineAutomationContext() ||
		request.ProfileID != diagnostics.ProfileStatus {
		t.Fatalf("automatic request = %+v, want the offline trigger with the tunnelled status probe", request)
	}
	answer(controller, "diag-one", diagnostics.ProbeStatusOnline)
	annotation := coordinator.AvailabilityAnnotations([]string{"node-one"})["node-one"]
	if annotation.State != speedtest.AgentDiagnosticNotReproduced || annotation.Task == nil || annotation.Task.Kind != diagnostics.AutomationKindOffline {
		t.Fatalf("annotation = %+v, want the node working from the agent under the offline kind", annotation)
	}
}

// Each availability trigger is its own opt-in: enabling one must not start the
// other.
func TestAvailabilityTriggersAreSeparateOptIns(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	controller := &fakeSessionController{enabled: true}
	coordinator := newProxyFailureCoordinator(t, controller, &now) // proxy failure on, offline off
	coordinator.StartAvailabilityDiagnostics([]AvailabilityFailure{
		offline("node-down", now),
		{StableID: "node-tunnel", Kind: diagnostics.AutomationKindProxyFailure, Since: now},
	})
	if len(controller.requests) != 1 || controller.requests[0].StableID != "node-tunnel" {
		t.Fatalf("requests = %+v, want only the proxy failure", controller.requests)
	}
	if snapshot := coordinator.Snapshot(); !snapshot.ProxyFailureEnabled || snapshot.OfflineEnabled {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

// "The tunnel works from elsewhere" says nothing about a node that has since
// stopped answering TCP, so a change of kind is a new question even inside the
// cooldown.
func TestAChangeOfFailureKindAsksAgain(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) {
		config.OfflineEnabled = true
	})
	coordinator.StartAvailabilityDiagnostics([]AvailabilityFailure{{StableID: "node-one", Kind: diagnostics.AutomationKindProxyFailure, Since: now}})
	answer(controller, "diag-one", diagnostics.ProbeStatusOnline)
	now = now.Add(5 * time.Minute)
	coordinator.StartAvailabilityDiagnostics([]AvailabilityFailure{offline("node-one", now)})

	if len(controller.requests) != 2 || controller.requests[1].Trigger != diagnostics.TriggerAutoOffline {
		t.Fatalf("requests = %+v, want a second probe for the offline episode", controller.requests)
	}
}

// Every final answer reaches the journal exactly once, whether or not an alert
// ever read it, with both sides of the comparison.
func TestAvailabilityVerdictsAreJournaledOnce(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	journal := &memoryJournal{}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) {
		config.OfflineEnabled = true
		config.Journal = journal
		config.NodeName = func(stableID string) string { return "Name of " + stableID }
	})
	failures := []AvailabilityFailure{offline("node-one", now)}
	coordinator.StartAvailabilityDiagnostics(failures)
	view := controller.views["diag-one"]
	view.Session.LocalResultSnapshot = diagnostics.LocalResultSnapshot{
		Status: diagnostics.ProbeStatusOffline, Failure: diagnostics.FailureEvidence{Code: "host_unreachable"},
	}
	controller.views["diag-one"] = view
	answer(controller, "diag-one", diagnostics.ProbeStatusOnline)

	// Read by an alert, then harvested by the next check: one line.
	coordinator.AvailabilityAnnotations([]string{"node-one"})
	now = now.Add(5 * time.Minute)
	coordinator.StartAvailabilityDiagnostics(failures)
	coordinator.AvailabilityAnnotations([]string{"node-one"})

	entries := journal.all()
	if len(entries) != 1 {
		t.Fatalf("journal = %+v, want one line", entries)
	}
	entry := entries[0]
	if entry.Source != verdictlog.SourceAvailability || entry.Trigger != string(diagnostics.TriggerAutoOffline) ||
		entry.Verdict != speedtest.AgentDiagnosticNotReproduced || entry.StableID != "node-one" || entry.Node != "Name of node-one" ||
		entry.LocalStatus != "offline" || entry.LocalFailureCode != "host_unreachable" || entry.AgentStatus != "online" ||
		entry.AgentName != "EU probe" {
		t.Fatalf("journal entry = %+v", entry)
	}
}

// A verdict nobody read still reaches the journal: the next check harvests it.
func TestAnUnreadVerdictIsHarvestedByTheNextCheck(t *testing.T) {
	controller := &fakeSessionController{enabled: true}
	journal := &memoryJournal{}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) { config.Journal = journal })
	failures := []AvailabilityFailure{{StableID: "node-one", Since: now}}
	coordinator.StartAvailabilityDiagnostics(failures)
	answer(controller, "diag-one", diagnostics.ProbeStatusProxyFailure)
	now = now.Add(5 * time.Minute)
	coordinator.StartAvailabilityDiagnostics(failures)

	entries := journal.all()
	if len(entries) != 1 || entries[0].Verdict != speedtest.AgentDiagnosticReproduced || entries[0].Kind != diagnostics.AutomationKindProxyFailure {
		t.Fatalf("journal = %+v, want the reproduced proxy failure", entries)
	}
}

// An episode that ended with nobody able to look is a fact about the agents,
// and is journaled as such when the node recovers.
func TestAnEpisodeNobodyProbedIsJournaledWhenItEnds(t *testing.T) {
	controller := &fakeSessionController{enabled: true, err: remoteprobe.ErrUnavailableAgent}
	journal := &memoryJournal{}
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	coordinator := newProxyFailureCoordinator(t, controller, &now, func(config *Config) { config.Journal = journal })
	coordinator.StartAvailabilityDiagnostics([]AvailabilityFailure{{StableID: "node-one", Since: now}})
	now = now.Add(5 * time.Minute)
	coordinator.StartAvailabilityDiagnostics(nil)

	entries := journal.all()
	if len(entries) != 1 || entries[0].Verdict != speedtest.AgentDiagnosticUnavailable || entries[0].Detail == "" {
		t.Fatalf("journal = %+v, want one unavailable line with its reason", entries)
	}
}

// The offline profile must be tunnelled, like the proxy-failure one.
func TestOfflineProfileMustBeTunnelled(t *testing.T) {
	_, err := New(Config{Enabled: true, OfflineProfileID: diagnostics.ProfileTLS}, &fakeSessionController{enabled: true}, fakeAgentSource{})
	if err == nil {
		t.Fatal("a transport profile was accepted for the offline trigger")
	}
}
