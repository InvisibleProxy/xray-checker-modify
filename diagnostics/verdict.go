package diagnostics

// Verdict is what an automatic diagnostic settled about the problem that
// started it. It describes the comparison, never the node: none of these
// values is a status, and none of them feeds an operational decision beyond
// the one alert-timing rule documented on SpeedVerdictReproduced.
type Verdict string

const (
	// VerdictReproduced means the agent saw the same problem. For a speed
	// problem that requires a rate in the same range as the run's, measured
	// against the same server; it is the only verdict that lets a speed alert
	// skip its confirmation wait.
	VerdictReproduced Verdict = "reproduced"
	// VerdictNotReproduced means the agent got through without the problem.
	VerdictNotReproduced Verdict = "not_reproduced"
	// VerdictPathLimited means the agent was many times faster than the run: the
	// node can carry far more than the checker got through it, so most of what
	// was lost was lost on the checker's side of the path. It does not claim the
	// node is healthy — the agent may be below the threshold too, and the reason
	// says so.
	VerdictPathLimited Verdict = "path_limited"
	// VerdictInconclusive means the two measurements cannot be compared: the
	// agent measured another server, or its rate straddles the threshold.
	VerdictInconclusive Verdict = "inconclusive"
	// VerdictUnreliable means the observation cannot be believed at all, or is
	// missing the one number the question needed.
	VerdictUnreliable Verdict = "unreliable"
)

// Fixed reasons. They are chosen from this list and never built from transport
// errors, so nothing unbounded reaches alerts, the verdict journal or the UI;
// consumers translate them by exact match.
const (
	ReasonDirectControlFailed     = "agent direct connectivity control failed"
	ReasonNoThroughput            = "agent download observation has no throughput evidence"
	ReasonAlternativeWorked       = "the agent alternative tunnelled endpoint worked"
	ReasonDifferentServer         = "the agent measured a different speed-test server"
	ReasonNearThreshold           = "the agent rate is too close to the threshold to tell"
	ReasonAgentAlsoBelowThreshold = "the agent was also below the threshold"
)

// PathRateFactor is how many times faster than the run an agent has to be before
// the gap is read as the path rather than the node. Two single-stream transfers
// through the same node routinely differ by a factor of two on their own; past
// three the numbers no longer describe the same bottleneck. The evidence behind
// the choice: in production, the slowdowns the agents did not reproduce came
// back 40 to 200 times faster, while a node that is slow for everyone stays in
// the same range from every vantage point.
const PathRateFactor = 3.0

// SpeedEvidence is what the run established, in the terms the verdict needs.
type SpeedEvidence struct {
	// Outcome is AutomationOutcomeLowSpeed or AutomationOutcomeTechnical.
	Outcome       string
	ThresholdMbps float64
	// ObservedMbps is the run's own rate. For a technical failure it is whatever
	// the transfer reached before it broke, and is not used as a comparison.
	ObservedMbps float64
	// ServerID is the catalogue server the run measured, empty when its test URL
	// is not in the catalogue.
	ServerID string
}

// JudgeSpeed compares an agent's observation with the measurement that asked for
// it.
//
// The order of the checks is the argument:
//
//   - An observation whose own connectivity control failed proves nothing.
//   - An alternative endpoint that worked means the tunnel carries traffic; what
//     failed was the endpoint, not the node.
//   - An agent that could not complete the probe at all saw the problem too.
//   - A slowdown the agent beat by more than PathRateFactor is the path's: the
//     node carried many times what the checker got through it. The reason notes
//     when the agent was below the threshold as well, because then the node is
//     not above suspicion either.
//   - A rate at or above the threshold did not reproduce the problem.
//   - A rate whose integer rounding straddles the threshold cannot tell.
//   - A rate clearly below the threshold, in the run's range, reproduces the
//     problem only when both measured the same server. Against another server
//     the agreement may be the agent's server being slow, which is exactly how
//     every run of a node in the United States used to come back "reproduced"
//     from an agent downloading from France.
func JudgeSpeed(evidence SpeedEvidence, observation Observation, reliable bool) (Verdict, string) {
	if !reliable {
		return VerdictUnreliable, ReasonDirectControlFailed
	}
	if alternative := observation.AlternativeEndpoint; alternative != nil && alternative.Status == ProbeStatusOnline {
		return VerdictNotReproduced, ReasonAlternativeWorked
	}
	if observation.Status != ProbeStatusOnline {
		return VerdictReproduced, ""
	}
	threshold := evidence.ThresholdMbps
	if threshold <= 0 {
		return VerdictNotReproduced, ""
	}
	if observation.Throughput == nil {
		// A slowdown answered with no rate settles nothing. A technical failure
		// is different: the agent got through, which answers it on its own terms.
		if evidence.Outcome == AutomationOutcomeLowSpeed {
			return VerdictUnreliable, ReasonNoThroughput
		}
		return VerdictNotReproduced, ""
	}
	// The agent reports whole Mbps, so its true rate lies in [Mbps, Mbps+1).
	agent := float64(observation.Throughput.Mbps)
	if evidence.Outcome == AutomationOutcomeLowSpeed && agent > PathRateFactor*maxFloat(evidence.ObservedMbps, 1) {
		if agent < threshold {
			return VerdictPathLimited, ReasonAgentAlsoBelowThreshold
		}
		return VerdictPathLimited, ""
	}
	if agent >= threshold {
		return VerdictNotReproduced, ""
	}
	if agent+1 > threshold {
		return VerdictInconclusive, ReasonNearThreshold
	}
	if evidence.ServerID == "" || observation.SpeedServerID != evidence.ServerID {
		return VerdictInconclusive, ReasonDifferentServer
	}
	return VerdictReproduced, ""
}

// JudgeAvailability compares an agent's observation with an availability
// failure — a tunnel that does not carry traffic, or a node the checker cannot
// reach at all. There is no rate to compare: the question is whether the node
// works from somewhere else.
func JudgeAvailability(observation Observation, reliable bool) (Verdict, string) {
	if !reliable {
		return VerdictUnreliable, ReasonDirectControlFailed
	}
	if observation.Status == ProbeStatusOnline {
		return VerdictNotReproduced, ""
	}
	if alternative := observation.AlternativeEndpoint; alternative != nil && alternative.Status == ProbeStatusOnline {
		return VerdictNotReproduced, ReasonAlternativeWorked
	}
	return VerdictReproduced, ""
}

// SpeedServerComparable reports whether an agent measured the server the run
// did. It is exported for summaries that explain a verdict.
func SpeedServerComparable(evidence SpeedEvidence, observation Observation) bool {
	return evidence.ServerID != "" && observation.SpeedServerID == evidence.ServerID
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
