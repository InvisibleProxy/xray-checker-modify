package reachability

import (
	"testing"
	"time"

	"xray-checker/diagnostics"
)

func sessionWith(local diagnostics.ProbeStatus, observation *diagnostics.AcceptedObservation, state diagnostics.SessionState) diagnostics.DiagnosticSession {
	session := diagnostics.DiagnosticSession{
		SessionID:           "sess-1",
		StableID:            "node-1",
		Trigger:             diagnostics.TriggerReachabilitySweep,
		LocalResultSnapshot: diagnostics.LocalResultSnapshot{Status: local, CheckedAt: time.Unix(1000, 0).UTC()},
		State:               state,
	}
	if observation != nil {
		session.AgentObservations = []diagnostics.AcceptedObservation{*observation}
	}
	return session
}

func observation(agentID string, status diagnostics.ProbeStatus, reliable bool) *diagnostics.AcceptedObservation {
	return &diagnostics.AcceptedObservation{
		Observation: diagnostics.Observation{
			AgentID:   agentID,
			Status:    status,
			CheckedAt: time.Unix(2000, 0).UTC(),
			TCP:       diagnostics.CheckEvidence{Checked: true, Online: status != diagnostics.ProbeStatusOffline},
			Failure:   diagnostics.FailureEvidence{Code: "tcp_timeout", Stage: diagnostics.FailureStageTCP},
		},
		Reliable: reliable,
	}
}

func TestCellForComparesBothVantagePoints(t *testing.T) {
	now := time.Unix(3000, 0).UTC()
	tests := []struct {
		name    string
		local   diagnostics.ProbeStatus
		agent   diagnostics.ProbeStatus
		want    Verdict
		diverge bool
	}{
		{"both online", diagnostics.ProbeStatusOnline, diagnostics.ProbeStatusOnline, VerdictAgreedUp, false},
		{"both down", diagnostics.ProbeStatusOffline, diagnostics.ProbeStatusOffline, VerdictAgreedDown, false},
		{"local proxy failure and agent offline still agree", diagnostics.ProbeStatusProxyFailure, diagnostics.ProbeStatusOffline, VerdictAgreedDown, false},
		{"reachable here only", diagnostics.ProbeStatusOnline, diagnostics.ProbeStatusOffline, VerdictAgentOnlyFailure, true},
		{"tunnel fails from the agent only", diagnostics.ProbeStatusOnline, diagnostics.ProbeStatusProxyFailure, VerdictAgentOnlyFailure, true},
		{"reachable from the agent only", diagnostics.ProbeStatusOffline, diagnostics.ProbeStatusOnline, VerdictLocalOnlyFailure, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := sessionWith(test.local, observation("agent-1", test.agent, true), diagnostics.SessionStateCompleted)
			cell := CellFor(session, "agent-1", now)
			if cell.Verdict != test.want {
				t.Fatalf("verdict = %q, want %q", cell.Verdict, test.want)
			}
			if cell.Verdict.Divergent() != test.diverge {
				t.Fatalf("Divergent() = %v, want %v", cell.Verdict.Divergent(), test.diverge)
			}
			if !cell.CheckedAt.Equal(time.Unix(2000, 0).UTC()) {
				t.Fatalf("CheckedAt = %v, want the agent's own timestamp", cell.CheckedAt)
			}
			if !cell.LocalCheckedAt.Equal(time.Unix(1000, 0).UTC()) {
				t.Fatalf("LocalCheckedAt = %v, want the local sample time", cell.LocalCheckedAt)
			}
		})
	}
}

// An agent that lost its own uplink reports every node as unreachable. Deriving
// a verdict from that would turn one agent's outage into a matrix full of
// findings, so the reliability control has to win over the comparison.
func TestCellForRefusesToJudgeWhenTheAgentLostConnectivity(t *testing.T) {
	session := sessionWith(diagnostics.ProbeStatusOnline, observation("agent-1", diagnostics.ProbeStatusOffline, false), diagnostics.SessionStateCompleted)
	cell := CellFor(session, "agent-1", time.Unix(3000, 0).UTC())
	if cell.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want %q", cell.Verdict, VerdictUnknown)
	}
	if cell.Detail != detailAgentOffline {
		t.Fatalf("detail = %q, want %q", cell.Detail, detailAgentOffline)
	}
	if cell.Verdict.Divergent() {
		t.Fatal("an unreliable observation must not count as a divergence")
	}
}

func TestCellForRefusesToJudgeWithoutALocalResult(t *testing.T) {
	session := sessionWith(diagnostics.ProbeStatusUnknown, observation("agent-1", diagnostics.ProbeStatusOnline, true), diagnostics.SessionStateCompleted)
	cell := CellFor(session, "agent-1", time.Unix(3000, 0).UTC())
	if cell.Verdict != VerdictUnknown || cell.Detail != detailLocalUnknown {
		t.Fatalf("verdict = %q detail = %q, want unknown/%q", cell.Verdict, cell.Detail, detailLocalUnknown)
	}
}

func TestCellForExplainsAMissingObservation(t *testing.T) {
	tests := []struct {
		state diagnostics.SessionState
		want  string
	}{
		{diagnostics.SessionStateExpired, detailSessionExpired},
		{diagnostics.SessionStateCancelled, detailCancelled},
		{diagnostics.SessionStatePartial, detailNoObservation},
	}
	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			session := sessionWith(diagnostics.ProbeStatusOnline, nil, test.state)
			cell := CellFor(session, "agent-1", time.Unix(3000, 0).UTC())
			if cell.Verdict != VerdictUnknown {
				t.Fatalf("verdict = %q, want unknown", cell.Verdict)
			}
			if cell.Detail != test.want {
				t.Fatalf("detail = %q, want %q", cell.Detail, test.want)
			}
		})
	}
}

// An observation from a different agent must not be borrowed for this cell.
func TestCellForIgnoresAnotherAgentsObservation(t *testing.T) {
	session := sessionWith(diagnostics.ProbeStatusOnline, observation("agent-2", diagnostics.ProbeStatusOnline, true), diagnostics.SessionStateCompleted)
	cell := CellFor(session, "agent-1", time.Unix(3000, 0).UTC())
	if cell.Verdict != VerdictUnknown || cell.Detail != detailNoObservation {
		t.Fatalf("verdict = %q detail = %q, want unknown/%q", cell.Verdict, cell.Detail, detailNoObservation)
	}
}

func TestConfirmedRequiresASecondSweep(t *testing.T) {
	cell := Cell{Verdict: VerdictAgentOnlyFailure, Streak: 1}
	if cell.Confirmed() {
		t.Fatal("a single disagreement must not be confirmed")
	}
	cell.Streak = 2
	if !cell.Confirmed() {
		t.Fatal("a repeated disagreement must be confirmed")
	}
	agreed := Cell{Verdict: VerdictAgreedUp, Streak: 9}
	if agreed.Confirmed() {
		t.Fatal("agreement is never a confirmed finding")
	}
}
