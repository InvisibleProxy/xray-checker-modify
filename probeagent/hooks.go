package probeagent

import (
	"net"
	"strconv"
	"time"

	"xray-checker/diagnostics"
)

// Hooks report what the agent is doing so the binary can log it.
//
// The package stays logger-free on purpose: the controller links it too, and a
// package-level logger would make one binary's log level govern the other's.
// Every field is optional; a nil hook drops its event.
//
// What reaches a hook is bounded the same way an observation is — profile IDs,
// failure codes, hosts and durations. Enrollment tokens, private keys, signing
// payloads and Xray execution config never appear here, because the agent log
// is as readable as any other file on that host.
type Hooks struct {
	// OnEnrolled fires once per process, after the controller accepts this
	// agent. Resumed reports the recovery path where the token was already
	// consumed and a signed heartbeat re-established the identity instead.
	OnEnrolled func(heartbeatIntervalSeconds int, resumed bool)
	// OnConnected fires after the first successful heartbeat, which is the
	// moment the controller starts showing this agent as connected.
	OnConnected func(heartbeatIntervalSeconds int)
	// OnHeartbeatFailed fires for the failure that ends the control connection.
	// The caller retries with backoff, so this is not a fatal condition.
	OnHeartbeatFailed func(err error)
	OnJobStarted      func(JobStarted)
	OnJobFinished     func(JobFinished)
	// OnObservationAccepted fires when the controller has taken the result. A
	// job that gets this far cost the node a real probe, which is worth seeing
	// in the log next to what it found.
	OnObservationAccepted func(JobFinished)
	// OnJobRejected reports a refused observation. The control connection stays
	// up: one rejected result must not stop the heartbeat.
	OnJobRejected func(err error)
}

// JobStarted is the work the controller handed over.
type JobStarted struct {
	JobID     string
	SessionID string
	StableID  string
	ProfileID string
	Method    diagnostics.ProbeMethod
	// Target is the node's host and port, which is what makes a log line
	// actionable without cross-referencing the controller.
	Target    string
	ExpiresIn time.Duration
}

// JobFinished is what the probe saw, in the same bounded terms the signed
// observation uses.
type JobFinished struct {
	JobID        string
	StableID     string
	ProfileID    string
	Status       diagnostics.ProbeStatus
	Latency      time.Duration
	Elapsed      time.Duration
	FailureCode  string
	FailureStage diagnostics.FailureStage
	TCP          diagnostics.CheckEvidence
	Ping         diagnostics.CheckEvidence
	// Direct is the agent's control on its own connectivity. A failed control
	// makes every other result on this line unusable, and the controller
	// refuses to derive a verdict from it, so it belongs in the log.
	Direct         diagnostics.CheckEvidence
	ThroughputMbps int64
	// Alternative reports that the tunnelled probe failed and was retried
	// against the fallback endpoint, which is what separates a dead endpoint
	// from a dead node.
	Alternative bool
}

func jobStartedFrom(assignment JobAssignment, now time.Time) JobStarted {
	target := assignment.TargetHost
	if target != "" && assignment.TargetPort > 0 {
		target = net.JoinHostPort(assignment.TargetHost, strconv.Itoa(assignment.TargetPort))
	}
	return JobStarted{
		JobID:     assignment.Job.JobID,
		SessionID: assignment.Job.SessionID,
		StableID:  assignment.Job.StableID,
		ProfileID: assignment.Job.Profile.ID,
		Method:    assignment.Job.Profile.Method,
		Target:    target,
		ExpiresIn: assignment.Job.ExpiresAt.Sub(now),
	}
}

func jobFinishedFrom(observation diagnostics.Observation, elapsed time.Duration) JobFinished {
	finished := JobFinished{
		JobID:        observation.JobID,
		StableID:     observation.StableID,
		ProfileID:    observation.EndpointProfile,
		Status:       observation.Status,
		Latency:      time.Duration(observation.LatencyMillis) * time.Millisecond,
		Elapsed:      elapsed,
		FailureCode:  observation.Failure.Code,
		FailureStage: observation.Failure.Stage,
		TCP:          observation.TCP,
		Ping:         observation.Ping,
		Direct:       observation.DirectConnectivity,
		Alternative:  observation.AlternativeEndpoint != nil,
	}
	if observation.Throughput != nil {
		finished.ThroughputMbps = observation.Throughput.Mbps
	}
	return finished
}
