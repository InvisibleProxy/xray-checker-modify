package reachability

import (
	"testing"
	"time"

	"xray-checker/diagnostics"
)

// The order the caller passes in is its own liveness order. Anything the matrix
// has nothing current to say about has to come back in that order, or a matrix
// with one recorded cell would reshuffle every agent it has never seen.
func TestPreferAgentsRanksReachedAgentsFirstAndKeepsCallerOrderOtherwise(t *testing.T) {
	matrix := NewMatrix("")
	matrix.BeginSweep()
	now := time.Now().UTC()
	matrix.Record("node-one", Cell{AgentID: "agent-far", AgentStatus: diagnostics.ProbeStatusOnline, LatencyMillis: 320, CheckedAt: now})
	matrix.Record("node-one", Cell{AgentID: "agent-near", AgentStatus: diagnostics.ProbeStatusOnline, LatencyMillis: 40, CheckedAt: now})
	matrix.Record("node-one", Cell{AgentID: "agent-blind", AgentStatus: diagnostics.ProbeStatusProxyFailure, CheckedAt: now})
	matrix.Record("node-one", Cell{AgentID: "agent-unmeasured", AgentStatus: diagnostics.ProbeStatusOnline, CheckedAt: now})
	// Another node's evidence must not rank this one's agents.
	matrix.Record("node-two", Cell{AgentID: "agent-blind", AgentStatus: diagnostics.ProbeStatusOnline, LatencyMillis: 5, CheckedAt: now})

	got := matrix.PreferAgents("node-one", []string{"agent-blind", "agent-far", "agent-new", "agent-near", "agent-unmeasured"})
	want := []string{"agent-near", "agent-far", "agent-unmeasured", "agent-new", "agent-blind"}
	if len(got) != len(want) {
		t.Fatalf("ranking = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ranking = %v, want %v", got, want)
		}
	}
}

// A cell the last full pass did not refresh describes a moment that has passed.
// Ranking on it would keep preferring an agent for what it managed yesterday.
func TestPreferAgentsIgnoresCellsTheLastPassDidNotRefresh(t *testing.T) {
	matrix := NewMatrix("")
	now := time.Now().UTC()
	matrix.BeginSweep()
	matrix.Record("node-one", Cell{AgentID: "agent-stale", AgentStatus: diagnostics.ProbeStatusOnline, LatencyMillis: 5, CheckedAt: now})
	matrix.BeginSweep()
	matrix.Record("node-one", Cell{AgentID: "agent-fresh", AgentStatus: diagnostics.ProbeStatusOnline, LatencyMillis: 400, CheckedAt: now})

	got := matrix.PreferAgents("node-one", []string{"agent-stale", "agent-fresh"})
	if len(got) != 2 || got[0] != "agent-fresh" {
		t.Fatalf("ranking = %v, want the agent the last pass actually reached the node from first", got)
	}
}

// A matrix that has never swept has no opinion, and must hand back exactly what
// it was given: this is the state every deployment starts in.
func TestPreferAgentsWithoutEvidenceReturnsTheCallerOrder(t *testing.T) {
	matrix := NewMatrix("")
	got := matrix.PreferAgents("node-one", []string{"agent-two", "agent-one"})
	if len(got) != 2 || got[0] != "agent-two" || got[1] != "agent-one" {
		t.Fatalf("ranking = %v, want the caller order unchanged", got)
	}
}
