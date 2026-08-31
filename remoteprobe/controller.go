package remoteprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/checker"
	"xray-checker/diagnostics"
	"xray-checker/models"
	"xray-checker/probeagent"
	"xray-checker/xray"
)

const (
	DefaultAgentSocksPort = 18080
	DefaultManualJobTTL   = 5 * time.Minute
	DefaultLongPollWait   = 15 * time.Second
)

var (
	ErrUnavailableAgent = errors.New("diagnostic agent is not connected")
	ErrActiveSession    = errors.New("diagnostic session is already active for this node and agent")
	ErrNoPendingJob     = errors.New("no pending diagnostic job")
)

type Config struct {
	Enabled     bool
	CheckMethod string
	JobTTL      time.Duration
	SocksPort   int
}

type CreateManualRequest struct {
	StableID string `json:"stableId"`
	AgentID  string `json:"agentId"`
}

type SessionView struct {
	Session diagnostics.DiagnosticSession `json:"session"`
	Summary string                        `json:"summary"`
}

type queuedAssignment struct {
	assignment probeagent.JobAssignment
	claimed    bool
}

// Controller is the only adapter between operational proxy configuration and
// the isolated diagnostics manager. Credential-bearing Xray config lives only
// in assignments and is never copied into DiagnosticSession.
type Controller struct {
	mu          sync.Mutex
	config      Config
	registry    *probeagent.Registry
	checker     *checker.ProxyChecker
	manager     *diagnostics.DiagnosticSessionManager
	assignments map[string]*queuedAssignment
	waiters     map[string]chan struct{}
}

func NewController(config Config, registry *probeagent.Registry, proxyChecker *checker.ProxyChecker) (*Controller, error) {
	config.CheckMethod = strings.TrimSpace(config.CheckMethod)
	if config.JobTTL == 0 {
		config.JobTTL = DefaultManualJobTTL
	}
	if config.SocksPort == 0 {
		config.SocksPort = DefaultAgentSocksPort
	}
	if registry == nil || proxyChecker == nil || config.JobTTL < 30*time.Second || config.SocksPort < 1024 || config.SocksPort > 65535 {
		return nil, fmt.Errorf("invalid remote diagnostic controller configuration")
	}
	// The proxy check method is only a diagnostics concern. Rejecting an
	// unsupported value while remote diagnostics is disabled would refuse to
	// start over a setting the checker itself tolerates.
	if config.Enabled {
		if _, err := profileForMethod(config.CheckMethod); err != nil {
			return nil, err
		}
	}
	manager, err := diagnostics.NewDiagnosticSessionManager(diagnostics.ManagerConfig{
		VerifyObservation: diagnostics.NewEd25519Verifier(registry.ObservationPublicKey),
	})
	if err != nil {
		return nil, err
	}
	return &Controller{
		config:      config,
		registry:    registry,
		checker:     proxyChecker,
		manager:     manager,
		assignments: make(map[string]*queuedAssignment),
		waiters:     make(map[string]chan struct{}),
	}, nil
}

func (c *Controller) Enabled() bool {
	return c != nil && c.config.Enabled && c.registry.Enabled()
}

func (c *Controller) CreateManual(request CreateManualRequest) (SessionView, error) {
	if !c.Enabled() {
		return SessionView{}, probeagent.ErrDisabled
	}
	request.StableID = strings.TrimSpace(request.StableID)
	request.AgentID = strings.TrimSpace(request.AgentID)
	agent, ok := c.registry.Agent(request.AgentID)
	if !ok {
		return SessionView{}, probeagent.ErrAgentNotFound
	}
	if !agent.Enabled || !agent.Connected || !contains(agent.Capabilities, "control-v1") || !contains(agent.Capabilities, "diagnostic-v1") {
		return SessionView{}, ErrUnavailableAgent
	}
	snapshot, configJSON, fingerprint, err := c.executionSnapshot(request.StableID)
	if err != nil {
		return SessionView{}, err
	}
	profile, err := profileForMethod(c.config.CheckMethod)
	if err != nil {
		return SessionView{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(c.config.JobTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeExpiredAssignmentsLocked(now)
	for _, queued := range c.assignments {
		job := queued.assignment.Job
		if job.StableID == snapshot.Proxy.StableID && job.AgentID == request.AgentID {
			return SessionView{}, ErrActiveSession
		}
	}
	session, err := c.manager.CreateSession(diagnostics.CreateSessionRequest{
		StableID:              snapshot.Proxy.StableID,
		Trigger:               diagnostics.TriggerManual,
		ConfigGeneration:      snapshot.Generation,
		ConfigFingerprint:     fingerprint,
		LocalResultSnapshot:   localResult(snapshot),
		RequestedAgents:       []string{request.AgentID},
		MaintenanceDiagnostic: snapshot.Maintenance,
		ExpiresAt:             expiresAt,
	})
	if err != nil {
		return SessionView{}, err
	}
	job, err := c.manager.RegisterJob(diagnostics.RegisterJobRequest{
		SessionID: session.SessionID,
		AgentID:   request.AgentID,
		Profile:   profile,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		_ = c.manager.CancelSession(session.SessionID)
		return SessionView{}, err
	}
	assignment := probeagent.JobAssignment{
		Job:        job,
		XrayConfig: append(json.RawMessage(nil), configJSON...),
		SocksPort:  c.config.SocksPort,
		TargetHost: snapshot.Proxy.Server,
		TargetPort: snapshot.Proxy.Port,
	}
	c.assignments[job.JobID] = &queuedAssignment{assignment: assignment}
	c.signalAgentLocked(request.AgentID)
	updated, _ := c.manager.Session(session.SessionID)
	return view(updated), nil
}

func (c *Controller) Sessions(stableID string) []SessionView {
	stableID = strings.TrimSpace(stableID)
	sessions := c.manager.Sessions()
	c.mu.Lock()
	c.removeExpiredAssignmentsLocked(time.Now().UTC())
	c.mu.Unlock()
	result := make([]SessionView, 0, len(sessions))
	for _, session := range sessions {
		if stableID == "" || session.StableID == stableID {
			result = append(result, view(session))
		}
	}
	return result
}

func (c *Controller) Session(sessionID string) (SessionView, bool) {
	session, ok := c.manager.Session(sessionID)
	if !ok {
		return SessionView{}, false
	}
	return view(session), true
}

func (c *Controller) Cancel(sessionID string) error {
	if err := c.manager.CancelSession(sessionID); err != nil {
		return err
	}
	c.mu.Lock()
	for jobID, assignment := range c.assignments {
		if assignment.assignment.Job.SessionID == sessionID {
			delete(c.assignments, jobID)
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *Controller) Export(sessionID string) ([]byte, error) {
	return c.manager.ExportSession(sessionID)
}

func (c *Controller) Claim(ctx context.Context, agentID string) (*probeagent.JobAssignment, error) {
	agentID = strings.TrimSpace(agentID)
	timer := time.NewTimer(DefaultLongPollWait)
	defer timer.Stop()
	for {
		assignment, waiter, err := c.claimNow(agentID)
		if assignment != nil || err != nil {
			return assignment, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrNoPendingJob
		case <-waiter:
		}
	}
}

func (c *Controller) claimNow(agentID string) (*probeagent.JobAssignment, <-chan struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	jobIDs := make([]string, 0, len(c.assignments))
	for jobID := range c.assignments {
		jobIDs = append(jobIDs, jobID)
	}
	sort.Strings(jobIDs)
	for _, jobID := range jobIDs {
		queued := c.assignments[jobID]
		if !queued.assignment.Job.ExpiresAt.After(time.Now().UTC()) {
			delete(c.assignments, jobID)
			continue
		}
		if queued.assignment.Job.AgentID != agentID {
			continue
		}
		if !queued.claimed {
			if err := c.manager.MarkJobDispatched(jobID); err != nil {
				delete(c.assignments, jobID)
				return nil, nil, err
			}
			if err := c.manager.MarkJobRunning(jobID); err != nil {
				delete(c.assignments, jobID)
				return nil, nil, err
			}
			queued.claimed = true
		}
		copyValue := queued.assignment
		copyValue.XrayConfig = append(json.RawMessage(nil), queued.assignment.XrayConfig...)
		return &copyValue, nil, nil
	}
	return nil, c.waiterLocked(agentID), nil
}

func (c *Controller) removeExpiredAssignmentsLocked(now time.Time) {
	for jobID, assignment := range c.assignments {
		if !assignment.assignment.Job.ExpiresAt.After(now) {
			delete(c.assignments, jobID)
		}
	}
}

func (c *Controller) waiterLocked(agentID string) chan struct{} {
	waiter := c.waiters[agentID]
	if waiter == nil {
		waiter = make(chan struct{}, 1)
		c.waiters[agentID] = waiter
	}
	return waiter
}

func (c *Controller) signalAgentLocked(agentID string) {
	waiter := c.waiterLocked(agentID)
	select {
	case waiter <- struct{}{}:
	default:
	}
}

func (c *Controller) AcceptObservation(observation diagnostics.Observation) (diagnostics.AcceptedObservation, error) {
	snapshot, _, fingerprint, err := c.executionSnapshot(observation.StableID)
	if err != nil {
		return diagnostics.AcceptedObservation{}, diagnostics.ErrStaleGeneration
	}
	if snapshot.Generation != observation.ConfigGeneration || fingerprint != observation.ConfigFingerprint {
		return diagnostics.AcceptedObservation{}, diagnostics.ErrStaleGeneration
	}
	accepted, err := c.manager.AcceptObservation(observation)
	if err != nil {
		return diagnostics.AcceptedObservation{}, err
	}
	c.mu.Lock()
	delete(c.assignments, observation.JobID)
	c.mu.Unlock()
	return accepted, nil
}

func (c *Controller) executionSnapshot(stableID string) (checker.DiagnosticProxySnapshot, []byte, string, error) {
	snapshot, err := c.checker.DiagnosticSnapshot(stableID)
	if err != nil {
		return checker.DiagnosticProxySnapshot{}, nil, "", err
	}
	proxyCopy := *snapshot.Proxy
	proxyCopy.Index = 0
	configJSON, err := xray.NewConfigGenerator().GenerateConfig([]*models.ProxyConfig{&proxyCopy}, c.config.SocksPort, "none")
	if err != nil {
		return checker.DiagnosticProxySnapshot{}, nil, "", fmt.Errorf("generate diagnostic Xray config: %w", err)
	}
	// GenerateConfig indents its output, but encoding/json compacts a
	// json.RawMessage on the way to the agent. Hashing the indented bytes would
	// make the agent recompute a different fingerprint and reject every job, so
	// canonicalise through the same encoder the transport uses.
	canonicalConfig, err := json.Marshal(json.RawMessage(configJSON))
	if err != nil {
		return checker.DiagnosticProxySnapshot{}, nil, "", fmt.Errorf("canonicalise diagnostic Xray config: %w", err)
	}
	if len(canonicalConfig) == 0 || len(canonicalConfig) > probeagent.MaxExecutionConfigBytes {
		return checker.DiagnosticProxySnapshot{}, nil, "", fmt.Errorf("diagnostic Xray config exceeds delivery limit")
	}
	return snapshot, canonicalConfig, diagnostics.ConfigFingerprint(canonicalConfig), nil
}

func profileForMethod(method string) (diagnostics.TestProfile, error) {
	switch strings.TrimSpace(method) {
	case "ip":
		return diagnostics.TestProfile{ID: "default-ip", Method: diagnostics.ProbeMethodIP}, nil
	case "status":
		return diagnostics.TestProfile{ID: "default-status", Method: diagnostics.ProbeMethodStatus}, nil
	case "download":
		return diagnostics.TestProfile{ID: "default-download", Method: diagnostics.ProbeMethodDownload}, nil
	default:
		return diagnostics.TestProfile{}, fmt.Errorf("unsupported diagnostic check method %q", method)
	}
}

func localResult(snapshot checker.DiagnosticProxySnapshot) diagnostics.LocalResultSnapshot {
	if !snapshot.HasStatus || snapshot.Status.CheckedAt.IsZero() {
		return diagnostics.LocalResultSnapshot{Status: diagnostics.ProbeStatusUnknown}
	}
	status := diagnostics.ProbeStatus(snapshot.Status.EffectiveStatus())
	result := diagnostics.LocalResultSnapshot{
		Status:        status,
		CheckedAt:     snapshot.Status.CheckedAt.UTC(),
		LatencyMillis: snapshot.Status.Latency.Milliseconds(),
		TCP:           checkEvidence(snapshot.Status.HostCheck.Checked, snapshot.Status.HostCheck.Online, snapshot.Status.HostCheck.Latency, "tcp_timeout"),
		Ping:          checkEvidence(snapshot.Status.PingCheck.Checked, snapshot.Status.PingCheck.Online, snapshot.Status.PingCheck.Latency, "host_unreachable"),
	}
	if status != diagnostics.ProbeStatusOnline {
		code := strings.TrimSpace(snapshot.Status.Failure.Code)
		if code == "" {
			code = "unknown"
		}
		result.Failure = diagnostics.FailureEvidence{Code: code, Stage: failureStage(code)}
	}
	return result
}

func checkEvidence(checked, online bool, latency time.Duration, failureCode string) diagnostics.CheckEvidence {
	evidence := diagnostics.CheckEvidence{Checked: checked, Online: online, LatencyMillis: latency.Milliseconds()}
	if checked && !online {
		evidence.FailureCode = failureCode
	}
	return evidence
}

func failureStage(code string) diagnostics.FailureStage {
	switch code {
	case "configuration":
		return diagnostics.FailureStageConfiguration
	case "tcp_refused", "tcp_timeout", "host_unreachable", "dns":
		return diagnostics.FailureStageTCP
	case "check_endpoint", "http_status", "download_incomplete":
		return diagnostics.FailureStageEndpoint
	default:
		return diagnostics.FailureStageProxy
	}
}

func view(session diagnostics.DiagnosticSession) SessionView {
	return SessionView{Session: session, Summary: summarize(session)}
}

func summarize(session diagnostics.DiagnosticSession) string {
	if len(session.AgentObservations) == 0 {
		if session.State.Terminal() {
			return "No remote observations are available."
		}
		return "Remote diagnostics are running."
	}
	observation := session.AgentObservations[len(session.AgentObservations)-1]
	remote := observation.Observation
	local := session.LocalResultSnapshot
	if !observation.Reliable {
		// An unreliable result has two very different causes: the agent refused
		// the job before probing anything, or it probed and its own network
		// failed the control. Reporting both as a network failure sends
		// troubleshooting to the wrong place.
		if !remote.DirectConnectivity.Checked {
			return "The agent rejected the job before probing; no remote evidence was collected."
		}
		return "The agent network failed direct connectivity control; this result is unreliable."
	}
	if local.Status != diagnostics.ProbeStatusOnline && remote.Status == diagnostics.ProbeStatusOnline {
		return "The problem was not reproduced from another network; a local ISP, route, DNS or DPI issue is likely."
	}
	if local.Failure.Code != "" && local.Failure.Code == remote.Failure.Code {
		return "The error was reproduced from another network; a shared configuration, server or port availability issue is likely."
	}
	if local.Status == diagnostics.ProbeStatusOffline && remote.Status == diagnostics.ProbeStatusOffline {
		return "The outage was reproduced; the server, port, firewall or hosting network may be involved."
	}
	return "The results differ without a stable pattern; there is not enough data."
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
