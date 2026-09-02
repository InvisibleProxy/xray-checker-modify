package reachability

import (
	"strings"
	"time"

	"xray-checker/diagnostics"
)

// Fixed explanations for an unknown verdict. They are chosen from this list and
// never built from a transport error, so nothing unbounded or credential-shaped
// reaches the state file, the API or the admin UI.
const (
	detailNoObservation  = "agent returned no observation"
	detailAgentOffline   = "agent lost its own connectivity during the probe"
	detailLocalUnknown   = "controller had no recent result to compare"
	detailSessionExpired = "diagnostic job expired before the agent answered"
	detailCancelled      = "diagnostic session was cancelled"
)

// CellFor turns a finished session into one cell.
//
// The comparison is deliberately between the agent's fresh result and the
// controller's last availability result, not a fresh local check: making the
// sweep trigger local checks would let a diagnostic workflow drive the
// availability loop, which is the one coupling this design refuses. The cost is
// that the local side can be up to one check interval stale, so a node that
// died moments before the sweep briefly looks like a divergence. That
// resolves itself: the next sweep compares against a current local result and
// the verdict changes, so a stale-local artefact never reaches a confirmed
// streak. This is why Confirmed requires two consecutive sweeps.
func CellFor(session diagnostics.DiagnosticSession, agentID string, now time.Time) Cell {
	agentID = strings.TrimSpace(agentID)
	cell := Cell{
		AgentID:        agentID,
		Verdict:        VerdictUnknown,
		LocalStatus:    session.LocalResultSnapshot.Status,
		LocalCheckedAt: session.LocalResultSnapshot.CheckedAt.UTC(),
		CheckedAt:      now.UTC(),
	}

	observation, found := observationFor(session, agentID)
	if !found {
		cell.Detail = detailForMissing(session.State)
		return cell
	}

	cell.AgentStatus = observation.Observation.Status
	cell.CheckedAt = observation.Observation.CheckedAt.UTC()
	cell.LatencyMillis = observation.Observation.LatencyMillis
	cell.TCPReached = observation.Observation.TCP.Checked && observation.Observation.TCP.Online
	cell.FailureCode = strings.TrimSpace(observation.Observation.Failure.Code)
	cell.FailureStage = observation.Observation.Failure.Stage

	// An agent that could not reach the internet at all reports every node as
	// unreachable. Accepting that would turn one agent's outage into a matrix
	// full of findings, so the controller's own reliability flag gates the
	// comparison before any verdict is derived.
	if !observation.Reliable {
		cell.Verdict = VerdictUnknown
		cell.Detail = detailAgentOffline
		return cell
	}
	if cell.LocalStatus == "" || cell.LocalStatus == diagnostics.ProbeStatusUnknown {
		cell.Verdict = VerdictUnknown
		cell.Detail = detailLocalUnknown
		return cell
	}

	localUp := cell.LocalStatus == diagnostics.ProbeStatusOnline
	agentUp := cell.AgentStatus == diagnostics.ProbeStatusOnline
	switch {
	case localUp && agentUp:
		cell.Verdict = VerdictAgreedUp
	case !localUp && !agentUp:
		cell.Verdict = VerdictAgreedDown
	case localUp && !agentUp:
		cell.Verdict = VerdictAgentOnlyFailure
	default:
		cell.Verdict = VerdictLocalOnlyFailure
	}
	return cell
}

func observationFor(session diagnostics.DiagnosticSession, agentID string) (diagnostics.AcceptedObservation, bool) {
	// Sessions created by the sweep carry exactly one job, but scanning in
	// reverse keeps the newest answer if that ever stops being true.
	for i := len(session.AgentObservations) - 1; i >= 0; i-- {
		if session.AgentObservations[i].Observation.AgentID == agentID {
			return session.AgentObservations[i], true
		}
	}
	return diagnostics.AcceptedObservation{}, false
}

func detailForMissing(state diagnostics.SessionState) string {
	switch state {
	case diagnostics.SessionStateExpired:
		return detailSessionExpired
	case diagnostics.SessionStateCancelled:
		return detailCancelled
	default:
		return detailNoObservation
	}
}

// Confirmed reports whether a divergence has survived a second sweep. One
// disagreement is a sample; two consecutive ones are a finding. Everything that
// notifies or counts should read this rather than Verdict.Divergent, which
// answers only "did the two sides disagree this time".
func (c Cell) Confirmed() bool {
	return c.Verdict.Divergent() && c.Streak >= confirmSweeps
}

const confirmSweeps = 2
