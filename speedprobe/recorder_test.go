package speedprobe

import (
	"context"
	"sync"
	"testing"
	"time"

	"xray-checker/agentautomation"
	"xray-checker/speedtest"
)

type fakeCoordinator struct {
	enabled     bool
	started     []speedtest.RunReport
	handles     map[string]agentautomation.Handle
	annotations map[string]speedtest.AgentDiagnostic
	awaited     map[string]speedtest.AgentDiagnostic
	awaitCalls  int
}

func (f *fakeCoordinator) Enabled() bool { return f.enabled }

func (f *fakeCoordinator) StartSpeedDiagnostics(report speedtest.RunReport, threshold float64) map[string]agentautomation.Handle {
	f.started = append(f.started, report)
	return f.handles
}

func (f *fakeCoordinator) Annotations(map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic {
	return f.annotations
}

func (f *fakeCoordinator) Await(context.Context, map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic {
	f.awaitCalls++
	if f.awaited != nil {
		return f.awaited
	}
	return f.annotations
}

type recordedProbe struct {
	stableID  string
	checkedAt time.Time
	probe     speedtest.AgentDiagnostic
}

type fakeHistory struct {
	mu      sync.Mutex
	written []recordedProbe
	missing bool
}

func (f *fakeHistory) RecordAgentProbe(stableID string, checkedAt time.Time, probe speedtest.AgentDiagnostic) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, recordedProbe{stableID: stableID, checkedAt: checkedAt, probe: probe})
	return !f.missing, nil
}

func (f *fakeHistory) writes() []recordedProbe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedProbe(nil), f.written...)
}

// The probe is written as soon as it starts and again when it answers. A node
// opened while the agent is still working has to show the question it was
// asked, not an empty column that reads as "nothing was tried".
func TestRecorderStoresTheStartedProbeAndThenItsAnswer(t *testing.T) {
	checkedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	coordinator := &fakeCoordinator{
		enabled: true,
		handles: map[string]agentautomation.Handle{"node-1": {StableID: "node-1", SessionID: "diag-one"}},
		annotations: map[string]speedtest.AgentDiagnostic{
			"node-1": {State: speedtest.AgentDiagnosticRunning, SessionID: "diag-one"},
		},
		awaited: map[string]speedtest.AgentDiagnostic{
			"node-1": {
				State: speedtest.AgentDiagnosticReproduced, SessionID: "diag-one",
				Observation: &speedtest.AgentProbeObservation{Status: "offline", Reliable: true},
			},
		},
	}
	history := &fakeHistory{}
	recorder := New(Config{Threshold: func() float64 { return 100 }, Wait: time.Second}, coordinator, history)

	recorder.RunSpeedProbes(speedtest.RunReport{Results: []speedtest.Result{
		{StableID: "node-1", Error: "context deadline exceeded", CheckedAt: checkedAt},
	}})

	writes := history.writes()
	if len(writes) != 2 {
		t.Fatalf("writes = %d (%+v), want the start and the answer", len(writes), writes)
	}
	if writes[0].probe.State != speedtest.AgentDiagnosticRunning {
		t.Errorf("first write = %q, want the running probe", writes[0].probe.State)
	}
	if writes[1].probe.State != speedtest.AgentDiagnosticReproduced || writes[1].probe.Observation == nil {
		t.Errorf("second write = %+v, want the answered probe", writes[1].probe)
	}
	for _, write := range writes {
		if write.stableID != "node-1" || !write.checkedAt.Equal(checkedAt) {
			t.Errorf("write = %+v, want it filed against the measurement that triggered it", write)
		}
	}
}

// Rewriting an unchanged probe persists the whole result file to say nothing
// new, which is the common case: most probes are already terminal when the
// alert wait ends.
func TestRecorderDoesNotRewriteAnUnchangedProbe(t *testing.T) {
	coordinator := &fakeCoordinator{
		enabled: true,
		handles: map[string]agentautomation.Handle{"node-1": {StableID: "node-1"}},
		annotations: map[string]speedtest.AgentDiagnostic{
			"node-1": {State: speedtest.AgentDiagnosticUnavailable, Detail: "no healthy idle diagnostic agent is connected"},
		},
	}
	history := &fakeHistory{}
	recorder := New(Config{Wait: time.Second}, coordinator, history)

	recorder.RunSpeedProbes(speedtest.RunReport{Results: []speedtest.Result{
		{StableID: "node-1", Error: "context deadline exceeded", CheckedAt: time.Now().UTC()},
	}})

	if writes := history.writes(); len(writes) != 1 {
		t.Fatalf("writes = %d, want only the first one", len(writes))
	}
	if coordinator.awaitCalls != 1 {
		t.Fatalf("await calls = %d, want the recorder to still wait for a change", coordinator.awaitCalls)
	}
}

func TestRecorderIgnoresARunWithNothingToDiagnose(t *testing.T) {
	coordinator := &fakeCoordinator{enabled: true}
	history := &fakeHistory{}
	recorder := New(Config{Wait: time.Second}, coordinator, history)

	recorder.RunSpeedProbes(speedtest.RunReport{Results: []speedtest.Result{
		{StableID: "node-1", Mbps: 500, CheckedAt: time.Now().UTC()},
	}})

	if writes := history.writes(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
}

// A disabled coordinator must not even be asked: automation is opt-in, and a
// deployment that left it off gets the behaviour it had before.
func TestRecorderStaysOutOfTheWayWhenAutomationIsDisabled(t *testing.T) {
	coordinator := &fakeCoordinator{}
	history := &fakeHistory{}
	recorder := New(Config{Wait: time.Second}, coordinator, history)

	recorder.RunSpeedProbes(speedtest.RunReport{Results: []speedtest.Result{
		{StableID: "node-1", Error: "context deadline exceeded", CheckedAt: time.Now().UTC()},
	}})

	if len(coordinator.started) != 0 {
		t.Fatalf("start calls = %d, want none", len(coordinator.started))
	}
	if writes := history.writes(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
}

// A measurement with no timestamp cannot be found again, and a probe filed
// against the wrong one is worse than no probe at all.
func TestRecorderSkipsAMeasurementItCannotIdentify(t *testing.T) {
	coordinator := &fakeCoordinator{
		enabled: true,
		handles: map[string]agentautomation.Handle{"node-1": {StableID: "node-1"}},
		annotations: map[string]speedtest.AgentDiagnostic{
			"node-1": {State: speedtest.AgentDiagnosticRunning},
		},
	}
	history := &fakeHistory{}
	recorder := New(Config{Wait: time.Second}, coordinator, history)

	recorder.RunSpeedProbes(speedtest.RunReport{Results: []speedtest.Result{
		{StableID: "node-1", Error: "context deadline exceeded"},
	}})

	if writes := history.writes(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
}
