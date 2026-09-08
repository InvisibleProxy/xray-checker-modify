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
	idleWaits   [][]string
	idleBlocks  bool
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

func (f *fakeCoordinator) AwaitIdle(ctx context.Context, stableIDs []string) {
	f.idleWaits = append(f.idleWaits, append([]string(nil), stableIDs...))
	if f.idleBlocks {
		<-ctx.Done()
	}
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

// A run waits for the agents to stop measuring the nodes it is about to
// measure. Two transfers over one uplink produce two rates and neither
// describes the node.
func TestRecorderHoldsARunUntilTheNodesItMeasuresAreIdle(t *testing.T) {
	coordinator := &fakeCoordinator{enabled: true}
	recorder := New(Config{Wait: time.Second}, coordinator, &fakeHistory{})

	recorder.AwaitIdleNodes([]string{"node-1", "node-2"})

	if len(coordinator.idleWaits) != 1 {
		t.Fatalf("idle waits = %+v, want one", coordinator.idleWaits)
	}
	if got := coordinator.idleWaits[0]; len(got) != 2 || got[0] != "node-1" || got[1] != "node-2" {
		t.Fatalf("waited for %+v, want the nodes the run selected", got)
	}
}

// The wait is bounded: past the job deadline the agent stops on its own, and a
// run that never starts is worse than one measured beside a probe.
func TestRecorderStopsWaitingWhenTheBoundRunsOut(t *testing.T) {
	coordinator := &fakeCoordinator{enabled: true, idleBlocks: true}
	recorder := New(Config{Wait: 50 * time.Millisecond}, coordinator, &fakeHistory{})

	startedAt := time.Now()
	recorder.AwaitIdleNodes(nil)

	if waited := time.Since(startedAt); waited > 2*time.Second {
		t.Fatalf("waited %s, want the bound to release the run", waited)
	}
}

func TestRecorderDoesNotWaitWhenAutomationIsDisabled(t *testing.T) {
	coordinator := &fakeCoordinator{idleBlocks: true}
	recorder := New(Config{Wait: time.Minute}, coordinator, &fakeHistory{})

	recorder.AwaitIdleNodes([]string{"node-1"})

	if len(coordinator.idleWaits) != 0 {
		t.Fatalf("idle waits = %+v, want none", coordinator.idleWaits)
	}
}
