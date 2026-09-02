package reachability

import (
	"context"
	"sync"
	"testing"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
	"xray-checker/remoteprobe"
)

type fakeController struct {
	mu sync.Mutex
	// statuses answers per agent, so one agent can disagree with another.
	statuses map[string]diagnostics.ProbeStatus
	local    diagnostics.ProbeStatus
	// failFor makes CreateSweep return an error for one agent.
	failFor  map[string]error
	pending  bool
	created  []remoteprobe.CreateSweepRequest
	deleted  []string
	canceled []string
	sessions map[string]diagnostics.DiagnosticSession
	seq      int
}

func newFakeController() *fakeController {
	return &fakeController{
		statuses: make(map[string]diagnostics.ProbeStatus),
		local:    diagnostics.ProbeStatusOnline,
		failFor:  make(map[string]error),
		sessions: make(map[string]diagnostics.DiagnosticSession),
	}
}

func (f *fakeController) Enabled() bool { return true }

func (f *fakeController) CreateSweep(request remoteprobe.CreateSweepRequest) (remoteprobe.SessionView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failFor[request.AgentID]; ok {
		return remoteprobe.SessionView{}, err
	}
	f.created = append(f.created, request)
	f.seq++
	sessionID := request.StableID + "/" + request.AgentID
	status, ok := f.statuses[request.AgentID]
	if !ok {
		status = diagnostics.ProbeStatusOnline
	}
	state := diagnostics.SessionStateCompleted
	var observations []diagnostics.AcceptedObservation
	if f.pending {
		state = diagnostics.SessionStateRunning
	} else {
		observations = []diagnostics.AcceptedObservation{{
			Observation: diagnostics.Observation{
				AgentID:   request.AgentID,
				Status:    status,
				CheckedAt: time.Unix(2000, 0).UTC(),
				TCP:       diagnostics.CheckEvidence{Checked: true, Online: status != diagnostics.ProbeStatusOffline},
			},
			Reliable: true,
		}}
	}
	session := diagnostics.DiagnosticSession{
		SessionID:           sessionID,
		StableID:            request.StableID,
		Trigger:             diagnostics.TriggerReachabilitySweep,
		LocalResultSnapshot: diagnostics.LocalResultSnapshot{Status: f.local, CheckedAt: time.Unix(1000, 0).UTC()},
		AgentObservations:   observations,
		State:               state,
	}
	f.sessions[sessionID] = session
	return remoteprobe.SessionView{Session: session}, nil
}

func (f *fakeController) Session(sessionID string) (remoteprobe.SessionView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[sessionID]
	return remoteprobe.SessionView{Session: session}, ok
}

func (f *fakeController) Cancel(sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, sessionID)
	return nil
}

func (f *fakeController) Delete(sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sessionID)
	delete(f.sessions, sessionID)
	return nil
}

func (f *fakeController) counts() (created, deleted, canceled int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.deleted), len(f.canceled)
}

type fakeAgents struct{ agents []probeagent.AgentSnapshot }

func (f fakeAgents) Snapshot() []probeagent.AgentSnapshot { return f.agents }

func healthyAgent(id string) probeagent.AgentSnapshot {
	return probeagent.AgentSnapshot{
		AgentID:      id,
		Enabled:      true,
		Connected:    true,
		Health:       "healthy",
		Capabilities: []string{diagnostics.CapabilityControlV1, diagnostics.CapabilityDiagnosticV1},
	}
}

func newTestSweeper(t *testing.T, controller SessionController, agents AgentSource, targets []Target, matrix *Matrix) *Sweeper {
	t.Helper()
	sweeper, err := NewSweeper(Config{
		Enabled:      true,
		Interval:     time.Hour,
		ProbeTimeout: time.Minute,
		PollInterval: time.Millisecond,
		Now:          func() time.Time { return time.Unix(3000, 0).UTC() },
	}, controller, agents, func() []Target { return targets }, matrix)
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	return sweeper
}

func TestSweepOnceFillsEveryCellAndDiscardsTheSessions(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1"), healthyAgent("agent-2")}}
	targets := []Target{{StableID: "node-1", Name: "One"}, {StableID: "node-2", Name: "Two"}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, targets, matrix)

	summary := sweeper.SweepOnce(context.Background())
	if summary.Recorded != 4 {
		t.Fatalf("recorded = %d, want one cell per node and agent", summary.Recorded)
	}
	created, deleted, _ := controller.counts()
	if created != 4 || deleted != 4 {
		t.Fatalf("created = %d deleted = %d, want every session created and then discarded", created, deleted)
	}
	rows := matrix.Rows()
	if len(rows) != 2 || len(rows[0].Cells) != 2 || len(rows[1].Cells) != 2 {
		t.Fatalf("rows = %+v, want a full two by two matrix", rows)
	}
}

func TestSweepOnceRecordsTheDisagreeingAgentOnly(t *testing.T) {
	controller := newFakeController()
	controller.local = diagnostics.ProbeStatusOnline
	controller.statuses["agent-2"] = diagnostics.ProbeStatusOffline
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1"), healthyAgent("agent-2")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, []Target{{StableID: "node-1"}}, matrix)

	summary := sweeper.SweepOnce(context.Background())
	if summary.Divergent != 1 {
		t.Fatalf("divergent = %d, want only the disagreeing agent", summary.Divergent)
	}
	if summary.Confirmed != 0 {
		t.Fatalf("confirmed = %d, want nothing confirmed on the first pass", summary.Confirmed)
	}
	// The second pass repeats the disagreement, which is what makes it a finding.
	second := sweeper.SweepOnce(context.Background())
	if second.Confirmed != 1 {
		t.Fatalf("confirmed = %d after a repeat, want 1", second.Confirmed)
	}
	if view := sweeper.Snapshot(); view.Divergent != 1 {
		t.Fatalf("view divergent = %d, want the confirmed finding", view.Divergent)
	}
}

// An agent that disconnects mid-pass must not poison its column with "unknown"
// cells: the previous answers stay, with their own timestamps showing they are
// stale.
func TestSweepOnceLeavesExistingCellsWhenAnAgentGoesAway(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	targets := []Target{{StableID: "node-1"}, {StableID: "node-2"}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, targets, matrix)
	sweeper.SweepOnce(context.Background())

	controller.mu.Lock()
	controller.failFor["agent-1"] = remoteprobe.ErrUnavailableAgent
	controller.mu.Unlock()
	summary := sweeper.SweepOnce(context.Background())

	if summary.Recorded != 0 {
		t.Fatalf("recorded = %d, want nothing from an unavailable agent", summary.Recorded)
	}
	rows := matrix.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the earlier answers kept", len(rows))
	}
	for _, row := range rows {
		if len(row.Cells) != 1 || row.Cells[0].Verdict != VerdictAgreedUp {
			t.Fatalf("row %q = %+v, want the earlier verdict untouched", row.StableID, row.Cells)
		}
	}
}

func TestSweepOnceStopsAnAgentPassAfterOneRefusal(t *testing.T) {
	controller := newFakeController()
	controller.failFor["agent-1"] = remoteprobe.ErrUnavailableAgent
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	targets := []Target{{StableID: "node-1"}, {StableID: "node-2"}, {StableID: "node-3"}}
	sweeper := newTestSweeper(t, controller, agents, targets, NewMatrix(""))

	sweeper.SweepOnce(context.Background())
	if created, _, _ := controller.counts(); created != 0 {
		t.Fatalf("created = %d, want the pass abandoned instead of one refusal per node", created)
	}
}

func TestSweepOnceReportsMaintenanceAsSkipped(t *testing.T) {
	controller := newFakeController()
	controller.failFor["agent-1"] = remoteprobe.ErrAutomaticPaused
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	sweeper := newTestSweeper(t, controller, agents, []Target{{StableID: "node-1"}}, NewMatrix(""))

	summary := sweeper.SweepOnce(context.Background())
	if !summary.Skipped || summary.Recorded != 0 {
		t.Fatalf("summary = %+v, want a skipped pass with nothing recorded", summary)
	}
}

// A probe that never answers has to release its session, or the pair stays
// blocked by the controller's own "one session per node and agent" rule and the
// cell can never be filled again.
func TestSweepOnceCancelsAndDiscardsASessionThatNeverFinishes(t *testing.T) {
	controller := newFakeController()
	controller.pending = true
	matrix := NewMatrix("")
	sweeper := &Sweeper{
		config: Config{
			Enabled:      true,
			Interval:     time.Hour,
			ProbeTimeout: 20 * time.Millisecond,
			PollInterval: time.Millisecond,
			ProfileID:    diagnostics.ProfileStatus,
			Now:          func() time.Time { return time.Unix(3000, 0).UTC() },
		},
		controller: controller,
		agents:     fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}},
		targets:    func() []Target { return []Target{{StableID: "node-1"}} },
		matrix:     matrix,
	}

	summary := sweeper.SweepOnce(context.Background())
	if summary.Timeouts != 1 || summary.Recorded != 0 {
		t.Fatalf("summary = %+v, want one timeout and no cell", summary)
	}
	_, deleted, canceled := controller.counts()
	if canceled != 1 || deleted != 1 {
		t.Fatalf("canceled = %d deleted = %d, want the stuck session released", canceled, deleted)
	}
	if rows := matrix.Rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want no cell from a probe that never answered", rows)
	}
}

func TestSweepOnceIgnoresAgentsThatCannotAnswer(t *testing.T) {
	controller := newFakeController()
	unhealthy := healthyAgent("agent-2")
	unhealthy.Health = "degraded"
	disconnected := healthyAgent("agent-3")
	disconnected.Connected = false
	revoked := healthyAgent("agent-4")
	revoked.Revoked = true
	legacy := healthyAgent("agent-5")
	legacy.Capabilities = []string{diagnostics.CapabilityControlV1}
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1"), unhealthy, disconnected, revoked, legacy}}
	sweeper := newTestSweeper(t, controller, agents, []Target{{StableID: "node-1"}}, NewMatrix(""))

	summary := sweeper.SweepOnce(context.Background())
	if summary.Agents != 1 || summary.Recorded != 1 {
		t.Fatalf("summary = %+v, want only the one agent that can answer", summary)
	}
}

func TestSweepOnceDropsRowsForNodesThatAreGone(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	matrix := NewMatrix("")
	targets := []Target{{StableID: "node-1"}, {StableID: "node-2"}}
	sweeper, err := NewSweeper(Config{
		Enabled:      true,
		Interval:     time.Hour,
		ProbeTimeout: time.Minute,
		PollInterval: time.Millisecond,
		Now:          func() time.Time { return time.Unix(3000, 0).UTC() },
	}, controller, agents, func() []Target { return targets }, matrix)
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	sweeper.SweepOnce(context.Background())

	targets = []Target{{StableID: "node-1"}}
	sweeper.SweepOnce(context.Background())
	rows := matrix.Rows()
	if len(rows) != 1 || rows[0].StableID != "node-1" {
		t.Fatalf("rows = %+v, want the removed node dropped", rows)
	}
}

func TestNewSweeperRejectsAnUnusableConfiguration(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{}
	targets := func() []Target { return nil }
	if _, err := NewSweeper(Config{Enabled: true, Interval: time.Minute}, controller, agents, targets, NewMatrix("")); err == nil {
		t.Fatal("expected an interval below the floor to be rejected")
	}
	if _, err := NewSweeper(Config{Enabled: true, ProbeTimeout: time.Second}, controller, agents, targets, NewMatrix("")); err == nil {
		t.Fatal("expected a probe timeout below the floor to be rejected")
	}
	if _, err := NewSweeper(Config{}, nil, agents, targets, NewMatrix("")); err == nil {
		t.Fatal("expected a missing controller to be rejected")
	}
}

// A disabled sweep must not be able to stop the checker from starting over the
// shape of a setting nothing reads.
func TestNewSweeperIgnoresPacingWhileDisabled(t *testing.T) {
	sweeper, err := NewSweeper(Config{Interval: time.Second, ProbeTimeout: time.Second}, newFakeController(), fakeAgents{}, func() []Target { return nil }, NewMatrix(""))
	if err != nil {
		t.Fatalf("a disabled sweep must not be rejected for its pacing: %v", err)
	}
	if sweeper.Enabled() {
		t.Fatal("the sweep must stay disabled")
	}
}

func TestNewSweeperDefaultsToTheStatusProfile(t *testing.T) {
	sweeper, err := NewSweeper(Config{}, newFakeController(), fakeAgents{}, func() []Target { return nil }, NewMatrix(""))
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	if sweeper.config.ProfileID != diagnostics.ProfileStatus {
		t.Fatalf("profile = %q, want the status probe every agent supports", sweeper.config.ProfileID)
	}
}
