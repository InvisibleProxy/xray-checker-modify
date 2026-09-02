package reachability

import (
	"path/filepath"
	"testing"
	"time"

	"xray-checker/diagnostics"
)

func TestRecordKeepsSinceAndCountsTheStreakWhileTheVerdictHolds(t *testing.T) {
	matrix := NewMatrix("")
	first := matrix.Record("node-1", Cell{
		AgentID:   "agent-1",
		Verdict:   VerdictAgentOnlyFailure,
		CheckedAt: time.Unix(1000, 0).UTC(),
	})
	if !first.Since.Equal(time.Unix(1000, 0).UTC()) || first.Streak != 1 {
		t.Fatalf("first record: since = %v streak = %d", first.Since, first.Streak)
	}
	second := matrix.Record("node-1", Cell{
		AgentID:   "agent-1",
		Verdict:   VerdictAgentOnlyFailure,
		CheckedAt: time.Unix(2000, 0).UTC(),
	})
	if !second.Since.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("Since = %v, want the first observation of this verdict", second.Since)
	}
	if second.Streak != 2 || !second.Confirmed() {
		t.Fatalf("streak = %d confirmed = %v, want 2/true", second.Streak, second.Confirmed())
	}
}

func TestRecordResetsSinceAndStreakWhenTheVerdictChanges(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgentOnlyFailure, CheckedAt: time.Unix(1000, 0).UTC()})
	matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgentOnlyFailure, CheckedAt: time.Unix(2000, 0).UTC()})
	recovered := matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgreedUp, CheckedAt: time.Unix(3000, 0).UTC()})
	if !recovered.Since.Equal(time.Unix(3000, 0).UTC()) || recovered.Streak != 1 {
		t.Fatalf("since = %v streak = %d, want a fresh start on the new verdict", recovered.Since, recovered.Streak)
	}
}

// A stale-local artefact appears once and disappears on the next sweep. It must
// never reach a confirmed streak, which is the whole reason Confirmed exists.
func TestASingleDivergenceFollowedByAgreementIsNeverConfirmed(t *testing.T) {
	matrix := NewMatrix("")
	artefact := matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgentOnlyFailure, CheckedAt: time.Unix(1000, 0).UTC()})
	if artefact.Confirmed() {
		t.Fatal("the first divergence must not be confirmed")
	}
	settled := matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgreedDown, CheckedAt: time.Unix(2000, 0).UTC()})
	if settled.Confirmed() || settled.Verdict.Divergent() {
		t.Fatal("agreement on the second sweep must clear the finding")
	}
}

func TestRetainDropsNodesAndAgentsThatAreGone(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgreedUp, CheckedAt: time.Unix(1000, 0).UTC()})
	matrix.Record("node-1", Cell{AgentID: "agent-2", Verdict: VerdictAgreedUp, CheckedAt: time.Unix(1000, 0).UTC()})
	matrix.Record("node-2", Cell{AgentID: "agent-1", Verdict: VerdictAgreedUp, CheckedAt: time.Unix(1000, 0).UTC()})

	matrix.Retain(map[string]string{"node-1": "Node One"}, map[string]bool{"agent-1": true})

	rows := matrix.Rows()
	if len(rows) != 1 || rows[0].StableID != "node-1" {
		t.Fatalf("rows = %+v, want only node-1", rows)
	}
	if rows[0].Name != "Node One" {
		t.Fatalf("name = %q, want the retained display name", rows[0].Name)
	}
	if len(rows[0].Cells) != 1 || rows[0].Cells[0].AgentID != "agent-1" {
		t.Fatalf("cells = %+v, want only agent-1", rows[0].Cells)
	}
}

func TestRetainDropsARowLeftWithNoAgents(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgreedUp, CheckedAt: time.Unix(1000, 0).UTC()})
	matrix.Retain(map[string]string{"node-1": "Node One"}, map[string]bool{})
	if rows := matrix.Rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none once every agent is gone", rows)
	}
}

func TestDivergencesListOnlyDisagreementsNewestFirst(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", Cell{AgentID: "agent-1", Verdict: VerdictAgreedUp, CheckedAt: time.Unix(1000, 0).UTC()})
	matrix.Record("node-2", Cell{AgentID: "agent-1", Verdict: VerdictAgentOnlyFailure, CheckedAt: time.Unix(1000, 0).UTC()})
	matrix.Record("node-3", Cell{AgentID: "agent-2", Verdict: VerdictLocalOnlyFailure, CheckedAt: time.Unix(5000, 0).UTC()})

	found := matrix.Divergences()
	if len(found) != 2 {
		t.Fatalf("divergences = %d, want 2", len(found))
	}
	if found[0].StableID != "node-3" {
		t.Fatalf("first divergence = %q, want the most recent change", found[0].StableID)
	}
}

func TestSaveAndLoadRoundTripsTheMatrix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reachability.json")
	matrix := NewMatrix(path)
	matrix.Record("node-1", Cell{
		AgentID:      "agent-1",
		Verdict:      VerdictAgentOnlyFailure,
		AgentStatus:  diagnostics.ProbeStatusOffline,
		LocalStatus:  diagnostics.ProbeStatusOnline,
		FailureCode:  "tcp_timeout",
		FailureStage: diagnostics.FailureStageTCP,
		CheckedAt:    time.Unix(1000, 0).UTC(),
	})
	matrix.MarkSweep(time.Unix(1100, 0).UTC())
	if err := matrix.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded := NewMatrix(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	rows := reloaded.Rows()
	if len(rows) != 1 || len(rows[0].Cells) != 1 {
		t.Fatalf("rows = %+v, want one cell", rows)
	}
	cell := rows[0].Cells[0]
	if cell.Verdict != VerdictAgentOnlyFailure || cell.FailureCode != "tcp_timeout" || cell.Streak != 1 {
		t.Fatalf("cell = %+v, want the stored verdict, failure and streak", cell)
	}
	if !reloaded.LastSweepAt().Equal(time.Unix(1100, 0).UTC()) {
		t.Fatalf("last sweep = %v, want the persisted value", reloaded.LastSweepAt())
	}
}

// A verdict this build does not recognise is dropped rather than repaired: a
// hole in the matrix is visible, an invented verdict is not.
func TestLoadDropsCellsWithAnUnknownVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reachability.json")
	if err := writeStateFile(path, stateFile{
		Version: StateVersion,
		Nodes: map[string]map[string]Cell{
			"node-1": {
				"agent-1": {AgentID: "agent-1", Verdict: "invented_by_a_future_build"},
				"agent-2": {AgentID: "agent-2", Verdict: VerdictAgreedUp},
			},
		},
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	matrix := NewMatrix(path)
	if err := matrix.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	rows := matrix.Rows()
	if len(rows) != 1 || len(rows[0].Cells) != 1 || rows[0].Cells[0].AgentID != "agent-2" {
		t.Fatalf("rows = %+v, want only the recognised cell", rows)
	}
}

func TestLoadRejectsAnUnsupportedStateVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reachability.json")
	if err := writeStateFile(path, stateFile{Version: StateVersion + 1}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := NewMatrix(path).Load(); err == nil {
		t.Fatal("expected an unsupported version to be rejected")
	}
}

func TestLoadTreatsAMissingFileAsAnEmptyMatrix(t *testing.T) {
	matrix := NewMatrix(filepath.Join(t.TempDir(), "absent.json"))
	if err := matrix.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if rows := matrix.Rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}
