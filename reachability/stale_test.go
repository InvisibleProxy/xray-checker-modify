package reachability

import (
	"context"
	"testing"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
)

func offlineAgent(id string) probeagent.AgentSnapshot {
	agent := healthyAgent(id)
	agent.Connected = false
	return agent
}

// A vantage point that goes away is exactly when its last observation matters,
// so the cells stay and are marked instead of being deleted.
func TestOfflineAgentKeepsItsLastObservationAsStale(t *testing.T) {
	controller := newFakeController()
	controller.local = diagnostics.ProbeStatusOnline
	controller.statuses["agent-2"] = diagnostics.ProbeStatusOffline
	agents := &mutableAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1"), healthyAgent("agent-2")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, []Target{{StableID: "node-1", Name: "One"}}, matrix)

	sweeper.SweepOnce(context.Background())
	if cells := len(matrix.Rows()[0].Cells); cells != 2 {
		t.Fatalf("cells after the first pass = %d, want one per agent", cells)
	}

	// agent-2 drops off the network and no longer answers.
	agents.set([]probeagent.AgentSnapshot{healthyAgent("agent-1"), offlineAgent("agent-2")})
	sweeper.SweepOnce(context.Background())

	row := matrix.Rows()[0]
	if len(row.Cells) != 2 {
		t.Fatalf("cells after the agent went offline = %d, want the history kept", len(row.Cells))
	}
	var gone, present Cell
	for _, cell := range row.Cells {
		if cell.AgentID == "agent-2" {
			gone = cell
			continue
		}
		present = cell
	}
	if !gone.Stale {
		t.Fatalf("the absent agent's cell must be marked stale: %+v", gone)
	}
	if gone.Verdict != VerdictAgentOnlyFailure {
		t.Fatalf("the stale cell must keep what it last saw, got %q", gone.Verdict)
	}
	if present.Stale {
		t.Fatalf("the answering agent's cell must stay fresh: %+v", present)
	}
}

// A stale observation describes a moment that has passed. Letting it stand as a
// finding would leave an alert for a vantage point nobody is watching from.
func TestStaleCellStopsCountingAsAFinding(t *testing.T) {
	controller := newFakeController()
	controller.local = diagnostics.ProbeStatusOnline
	controller.statuses["agent-2"] = diagnostics.ProbeStatusOffline
	agents := &mutableAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1"), healthyAgent("agent-2")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, []Target{{StableID: "node-1"}}, matrix)

	if summary := sweeper.SweepOnce(context.Background()); summary.Divergent != 1 {
		t.Fatalf("divergent = %d, want the disagreeing agent while it is online", summary.Divergent)
	}

	agents.set([]probeagent.AgentSnapshot{healthyAgent("agent-1"), offlineAgent("agent-2")})
	summary := sweeper.SweepOnce(context.Background())
	if summary.Divergent != 0 {
		t.Fatalf("divergent = %d once the agent stopped answering, want 0", summary.Divergent)
	}
	if len(matrix.Findings()) != 0 {
		t.Fatalf("a stale observation must not stand as a finding: %+v", matrix.Findings())
	}
}

// Revoking an agent removes it as a vantage point, which is different from it
// being temporarily offline.
func TestRevokedAgentLosesItsCells(t *testing.T) {
	controller := newFakeController()
	agents := &mutableAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1"), healthyAgent("agent-2")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, []Target{{StableID: "node-1"}}, matrix)
	sweeper.SweepOnce(context.Background())

	revoked := healthyAgent("agent-2")
	revoked.Revoked = true
	agents.set([]probeagent.AgentSnapshot{healthyAgent("agent-1"), revoked})
	sweeper.SweepOnce(context.Background())

	for _, cell := range matrix.Rows()[0].Cells {
		if cell.AgentID == "agent-2" {
			t.Fatalf("a revoked agent must not keep a column: %+v", cell)
		}
	}
}

// A cell written before this build has no generation. It must not be reported
// as stale until a pass has actually run and left it behind.
func TestCellsAreNotStaleBeforeTheFirstPass(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgreedUp})

	for _, cell := range matrix.Rows()[0].Cells {
		if cell.Stale {
			t.Fatalf("nothing can be stale before a pass has run: %+v", cell)
		}
	}
}

type mutableAgents struct{ agents []probeagent.AgentSnapshot }

func (m *mutableAgents) Snapshot() []probeagent.AgentSnapshot { return m.agents }

func (m *mutableAgents) set(agents []probeagent.AgentSnapshot) { m.agents = agents }
