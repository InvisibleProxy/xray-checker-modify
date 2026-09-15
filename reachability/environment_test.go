package reachability

import (
	"context"
	"testing"

	"xray-checker/probeagent"
)

func environmentTargets() []Target {
	return []Target{
		{StableID: "own", Name: "Own", Subscription: "env", Environment: true},
		{StableID: "added", Name: "Added", Subscription: "panel"},
	}
}

func createdFor(controller *fakeController, stableID string) int {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	count := 0
	for _, request := range controller.created {
		if request.StableID == stableID {
			count++
		}
	}
	return count
}

// The scheduled pass is the deployment's own work. A node an operator added
// from the panel belongs to somebody else's service, so the schedule spends no
// agent time on it and says in the summary how many it left out.
func TestScheduledSweepVisitsEnvironmentNodesOnly(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, environmentTargets(), matrix)

	summary := sweeper.sweepScheduled(context.Background())

	if summary.Nodes != 1 || summary.Foreign != 1 {
		t.Fatalf("nodes = %d foreign = %d, want the environment node swept and the added one counted as left out",
			summary.Nodes, summary.Foreign)
	}
	if got := createdFor(controller, "added"); got != 0 {
		t.Fatalf("a scheduled pass asked an agent about a panel-added node %d time(s)", got)
	}
	if got := createdFor(controller, "own"); got != 1 {
		t.Fatalf("sessions for the environment node = %d, want 1", got)
	}
}

// "Sweep now" is the operator asking, and it covers every monitored node. This
// is the only way a panel-added node is observed at all, so it must not be
// narrowed the way the schedule is.
func TestManualSweepCoversPanelAddedNodes(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, environmentTargets(), matrix)

	summary := sweeper.SweepOnce(context.Background())

	if summary.Nodes != 2 || summary.Foreign != 0 {
		t.Fatalf("nodes = %d foreign = %d, want every node swept and nothing left out", summary.Nodes, summary.Foreign)
	}
	if got := createdFor(controller, "added"); got != 1 {
		t.Fatalf("sessions for the panel-added node = %d, want the manual sweep to cover it", got)
	}
}

// A single-node recheck is the same operator request narrowed to one row, so it
// reaches a panel-added node too.
func TestSweepNodeCoversAPanelAddedNode(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, environmentTargets(), matrix)

	summary := sweeper.SweepNode(context.Background(), "added")

	if summary.Nodes != 1 {
		t.Fatalf("nodes = %d, want the one node asked for", summary.Nodes)
	}
	if got := createdFor(controller, "added"); got != 1 {
		t.Fatalf("sessions for the panel-added node = %d, want 1", got)
	}
}

// The schedule skipping a node is not the node leaving the fleet: what a manual
// sweep observed stays in the matrix, and the row keeps its cells rather than
// being retained away by the next scheduled pass.
func TestScheduledSweepKeepsWhatAManualSweepObserved(t *testing.T) {
	controller := newFakeController()
	agents := fakeAgents{agents: []probeagent.AgentSnapshot{healthyAgent("agent-1")}}
	matrix := NewMatrix("")
	sweeper := newTestSweeper(t, controller, agents, environmentTargets(), matrix)

	sweeper.SweepOnce(context.Background())
	sweeper.sweepScheduled(context.Background())

	rows := matrix.Rows()
	var added *NodeRow
	for index, row := range rows {
		if row.StableID == "added" {
			added = &rows[index]
		}
	}
	if added == nil {
		t.Fatal("the panel-added row is gone after a scheduled pass that skipped it")
	}
	if len(added.Cells) != 1 {
		t.Fatalf("cells = %d, want the observation the manual sweep recorded", len(added.Cells))
	}
	// The cell is last-known rather than current, which is exactly what the
	// matrix marks stale; the operator sees when it was last observed.
	if !added.Cells[0].Stale {
		t.Fatal("a cell the latest pass did not refresh must read as stale")
	}
}
