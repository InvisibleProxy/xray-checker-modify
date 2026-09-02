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
		AgentID:        "agent-1",
		Verdict:        VerdictAgentOnlyFailure,
		CheckedAt:      time.Unix(1000, 0).UTC(),
		LocalCheckedAt: time.Unix(900, 0).UTC(),
	})
	if !first.Since.Equal(time.Unix(1000, 0).UTC()) || first.Streak != 1 {
		t.Fatalf("first record: since = %v streak = %d", first.Since, first.Streak)
	}
	second := matrix.Record("node-1", Cell{
		AgentID:        "agent-1",
		Verdict:        VerdictAgentOnlyFailure,
		CheckedAt:      time.Unix(2000, 0).UTC(),
		LocalCheckedAt: time.Unix(1900, 0).UTC(),
	})
	if !second.Since.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("Since = %v, want the first observation of this verdict", second.Since)
	}
	if second.Streak != 2 {
		t.Fatalf("streak = %d, want 2", second.Streak)
	}
	// Confirmed is deliberately false here: a cell straight out of Record has
	// not been read as part of a row yet, so nothing has established that any
	// vantage point reached this node.
	if second.Confirmed() {
		t.Fatal("a cell must not confirm itself before the row is derived")
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

func up(agentID string) Cell {
	return Cell{
		AgentID: agentID, Verdict: VerdictAgreedUp,
		AgentStatus: diagnostics.ProbeStatusOnline, LocalStatus: diagnostics.ProbeStatusOnline,
		CheckedAt: time.Unix(1000, 0).UTC(),
	}
}

// agentBlind is the cell for an agent that cannot reach a node the checker can.
func agentBlind(agentID string) Cell {
	return Cell{
		AgentID: agentID, Verdict: VerdictAgentOnlyFailure,
		AgentStatus: diagnostics.ProbeStatusOffline, LocalStatus: diagnostics.ProbeStatusOnline,
		FailureCode: "tcp_timeout", CheckedAt: time.Unix(1000, 0).UTC(),
		LocalCheckedAt: time.Unix(900, 0).UTC(),
	}
}

// The streak is meant to outlast a stale local result, so it may only advance
// when the checker's own sample has moved on. Otherwise a targeted recheck,
// pressed twice in a row, would confirm an artefact the periodic sweep would
// have thrown away.
func TestTheStreakOnlyAdvancesWhenTheLocalSampleDoes(t *testing.T) {
	matrix := NewMatrix("")
	first := agentBlind("agent-1")
	matrix.Record("node-1", first)

	// A recheck moments later: fresh agent result, same local sample.
	repeat := first
	repeat.CheckedAt = time.Unix(1010, 0).UTC()
	held := matrix.Record("node-1", repeat)
	if held.Streak != 1 || held.Confirmed() {
		t.Fatalf("streak = %d confirmed = %v, want the streak held at 1", held.Streak, held.Confirmed())
	}

	// The checker re-checks, and now the observation is independent.
	independent := first
	independent.CheckedAt = time.Unix(2000, 0).UTC()
	independent.LocalCheckedAt = time.Unix(1900, 0).UTC()
	advanced := matrix.Record("node-1", independent)
	if advanced.Streak != 2 {
		t.Fatalf("streak = %d, want 2 once the local sample advanced", advanced.Streak)
	}
	if !advanced.Since.Equal(time.Unix(1000, 0).UTC()) {
		t.Fatalf("since = %v, want the first observation of this verdict", advanced.Since)
	}
}

// checkerBlind is the cell for an agent that reaches a node the checker cannot.
func checkerBlind(agentID string) Cell {
	return Cell{
		AgentID: agentID, Verdict: VerdictLocalOnlyFailure,
		AgentStatus: diagnostics.ProbeStatusOnline, LocalStatus: diagnostics.ProbeStatusProxyFailure,
		CheckedAt: time.Unix(1000, 0).UTC(),
	}
}

// bothBlind is the cell for an agent that agrees with a checker which cannot
// reach the node. On its own it means "the node is down"; in a row where
// another agent reached the node it means "this agent cannot reach it either".
func bothBlind(agentID string) Cell {
	return Cell{
		AgentID: agentID, Verdict: VerdictAgreedDown,
		AgentStatus: diagnostics.ProbeStatusOffline, LocalStatus: diagnostics.ProbeStatusProxyFailure,
		FailureCode: "tcp_refused", CheckedAt: time.Unix(1000, 0).UTC(),
	}
}

// The case that exposed the pairwise comparison: the checker cannot reach the
// node, one agent can, and a second agent cannot. Comparing each agent against
// the checker calls the second agent "agreed_down" and hides the very finding
// it is reporting.
func TestAnAgentAgreeingWithACutOffCheckerIsStillAFinding(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", checkerBlind("agent-de"))
	matrix.Record("node-1", bothBlind("agent-ru"))

	rows := matrix.Rows()
	if len(rows) != 1 || !rows[0].Alive {
		t.Fatalf("row = %+v, want the node recognised as alive from agent-de", rows)
	}
	byAgent := map[string]Cell{}
	for _, cell := range rows[0].Cells {
		byAgent[cell.AgentID] = cell
	}
	if byAgent["agent-de"].Unreachable {
		t.Fatal("the agent that reached the node must not be a finding")
	}
	if !byAgent["agent-ru"].Unreachable {
		t.Fatal("the agent that could not reach a live node must be a finding, even though it agreed with the checker")
	}
	if !rows[0].LocalUnreachable {
		t.Fatal("the checker itself could not reach a live node and must be reported")
	}
}

func TestFindingsReportTheCheckerOnceRegardlessOfAgentCount(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", checkerBlind("agent-de"))
	matrix.Record("node-1", checkerBlind("agent-nl"))
	matrix.Record("node-1", checkerBlind("agent-fi"))

	findings := matrix.Findings()
	local := 0
	for _, finding := range findings {
		if finding.Local {
			local++
		}
	}
	if local != 1 {
		t.Fatalf("local findings = %d, want exactly one however many agents can reach the node", local)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want only the checker's own failure", findings)
	}
}

// A node nobody can reach is an ordinary outage. It is already visible in the
// availability view and must not be repeated here as a reachability finding.
func TestANodeDownEverywhereProducesNoFindings(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", bothBlind("agent-de"))
	matrix.Record("node-1", bothBlind("agent-ru"))

	rows := matrix.Rows()
	if rows[0].Alive || rows[0].LocalUnreachable {
		t.Fatalf("row = %+v, want nothing claimed about a node no vantage point reached", rows[0])
	}
	if findings := matrix.Findings(); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

// An agent that could not vouch for its own connectivity must not be counted as
// evidence that a node is unreachable, nor as evidence that it is alive.
func TestUnknownCellsAreNotEvidenceInEitherDirection(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", Cell{
		AgentID: "agent-blind", Verdict: VerdictUnknown, Detail: detailAgentOffline,
		AgentStatus: diagnostics.ProbeStatusOffline, LocalStatus: diagnostics.ProbeStatusProxyFailure,
		CheckedAt: time.Unix(1000, 0).UTC(),
	})
	matrix.Record("node-1", bothBlind("agent-ru"))

	rows := matrix.Rows()
	if rows[0].Alive {
		t.Fatal("an unknown cell must not make a node look alive")
	}
	for _, cell := range rows[0].Cells {
		if cell.Unreachable {
			t.Fatalf("cell %q must not be a finding when nothing proved the node alive", cell.AgentID)
		}
	}
}

func TestFindingsListConfirmedFirstThenNewest(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", up("agent-1"))
	matrix.Record("node-2", agentBlind("agent-1"))
	matrix.Record("node-3", agentBlind("agent-2"))
	// A second sweep, against a local sample that has moved on, confirms node-3.
	third := agentBlind("agent-2")
	third.CheckedAt = time.Unix(5000, 0).UTC()
	third.LocalCheckedAt = time.Unix(4900, 0).UTC()
	matrix.Record("node-3", third)

	found := matrix.Findings()
	if len(found) != 2 {
		t.Fatalf("findings = %+v, want two", found)
	}
	if !found[0].Confirmed || found[0].StableID != "node-3" {
		t.Fatalf("first finding = %+v, want the confirmed one", found[0])
	}
	if found[1].Confirmed {
		t.Fatalf("second finding = %+v, want the unconfirmed one last", found[1])
	}
}

func TestFindingsCarryTheEvidenceOfTheFailingVantagePoint(t *testing.T) {
	matrix := NewMatrix("")
	matrix.Record("node-1", checkerBlind("agent-de"))
	matrix.Record("node-1", bothBlind("agent-ru"))

	var agentFinding Finding
	for _, finding := range matrix.Findings() {
		if finding.AgentID == "agent-ru" {
			agentFinding = finding
		}
	}
	if agentFinding.FailureCode != "tcp_refused" {
		t.Fatalf("failure code = %q, want the failing agent's own evidence", agentFinding.FailureCode)
	}
	if agentFinding.Status != diagnostics.ProbeStatusOffline {
		t.Fatalf("status = %q, want the agent's status", agentFinding.Status)
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
