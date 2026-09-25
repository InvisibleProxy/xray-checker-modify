package remoteprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
	// nodeHostLookupTimeout bounds the lookup of a node published under a name.
	// The automation coordinator holds its own lock across CreateAutomatic, so a
	// resolver that stops answering has to cost a short wait, not the platform's
	// retry schedule.
	nodeHostLookupTimeout = 3 * time.Second
)

var (
	ErrUnavailableAgent   = errors.New("diagnostic agent is not connected")
	ErrActiveSession      = errors.New("diagnostic session is already active for this node and agent")
	ErrNoPendingJob       = errors.New("no pending diagnostic job")
	ErrUnknownProfile     = errors.New("unknown diagnostic profile")
	ErrUnsupportedByAgent = errors.New("diagnostic agent does not support this profile")
	ErrAutomaticPaused    = errors.New("automatic remote diagnostics are paused")
	// ErrOnlyNodeHostAgent refuses an automatic diagnostic when every agent free
	// to take it runs on the node's own host; see CreateAutomatic for why such
	// an agent is never asked.
	ErrOnlyNodeHostAgent = errors.New("every idle diagnostic agent runs on the node's own host")
	// ErrNodeAddressUnresolved refuses an automatic diagnostic for a node whose
	// name did not resolve: without its addresses there is no telling whether an
	// agent runs on its host.
	ErrNodeAddressUnresolved = errors.New("node address could not be resolved")
)

type Config struct {
	Enabled     bool
	CheckMethod string
	JobTTL      time.Duration
	SocksPort   int
	// AgentPreference ranks the vantage points an automatic diagnostic may use.
	// It is optional; without it every node falls back to liveness order.
	AgentPreference AgentPreference
	// Resolver looks up a node published under a name, so an automatic
	// diagnostic can tell an agent on the node's own host from one elsewhere.
	// It is optional; nil uses the system resolver. A node published as an
	// address never reaches it.
	Resolver HostResolver
}

// HostResolver is the one lookup the controller makes itself. *net.Resolver
// satisfies it; it is an interface so tests do not depend on real DNS.
type HostResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// AgentPreference orders the vantage points an automatic diagnostic should try
// for one node, best first.
//
// It exists because the two questions differ: liveness order answers "who is
// free", and an automatic diagnostic needs "whose answer will mean something".
// The implementation lives outside this package — the reachability matrix is
// the natural source, and it already depends on this one — so the contract is
// deliberately forgiving: unknown ids are ignored, ids left out keep their
// place in the controller's own order, and returning nothing is not an error.
type AgentPreference interface {
	PreferAgents(stableID string, agentIDs []string) []string
}

type CreateManualRequest struct {
	StableID string `json:"stableId"`
	AgentID  string `json:"agentId"`
	// ProfileID selects the diagnostic. Empty keeps the profile matching the
	// controller's configured availability check method, which is what the
	// workflow used before profiles became selectable.
	ProfileID string `json:"profileId,omitempty"`
}

type CreateAutomaticRequest struct {
	StableID          string
	Trigger           diagnostics.Trigger
	ProfileID         string
	AutomationContext diagnostics.AutomationContext
}

// ProfileView is a catalogue entry as presented to the admin UI.
type ProfileView struct {
	diagnostics.ProfileDescriptor
	Default bool `json:"default"`
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
	if config.Resolver == nil {
		config.Resolver = net.DefaultResolver
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
	return c.createTargeted(targetedRequest{
		StableID:  request.StableID,
		AgentID:   request.AgentID,
		ProfileID: request.ProfileID,
		Trigger:   diagnostics.TriggerManual,
	})
}

// CreateSweepRequest asks one named agent whether it can reach one node. It is
// the reachability sweep's only entry into the diagnostics manager and carries
// no operational authority: the session it produces is evidence like any other.
type CreateSweepRequest struct {
	StableID  string
	AgentID   string
	ProfileID string
}

// CreateSweep runs the same bounded, generation-bound job as the manual
// workflow, against an agent the sweeper names rather than one the controller
// picks. Naming the agent is the whole point: a matrix needs every cell, not
// one answer from whichever agent happened to be idle.
func (c *Controller) CreateSweep(request CreateSweepRequest) (SessionView, error) {
	return c.createTargeted(targetedRequest{
		StableID:  request.StableID,
		AgentID:   request.AgentID,
		ProfileID: request.ProfileID,
		Trigger:   diagnostics.TriggerReachabilitySweep,
	})
}

// targetedRequest is the shared shape of the workflows that name their own
// agent, as opposed to CreateAutomatic which selects one.
type targetedRequest struct {
	StableID  string
	AgentID   string
	ProfileID string
	Trigger   diagnostics.Trigger
}

func (c *Controller) createTargeted(request targetedRequest) (SessionView, error) {
	if !c.Enabled() {
		return SessionView{}, probeagent.ErrDisabled
	}
	if request.Trigger.Automatic() && c.checker.ProjectMaintenanceEnabled() {
		return SessionView{}, ErrAutomaticPaused
	}
	request.StableID = strings.TrimSpace(request.StableID)
	request.AgentID = strings.TrimSpace(request.AgentID)
	agent, ok := c.registry.Agent(request.AgentID)
	if !ok {
		return SessionView{}, probeagent.ErrAgentNotFound
	}
	if !agent.Enabled || !agent.Connected || !contains(agent.Capabilities, diagnostics.CapabilityControlV1) || !contains(agent.Capabilities, diagnostics.CapabilityDiagnosticV1) {
		return SessionView{}, ErrUnavailableAgent
	}
	descriptor, err := c.resolveProfile(request.ProfileID, agent.Capabilities)
	if err != nil {
		return SessionView{}, err
	}
	snapshot, configJSON, fingerprint, err := c.executionSnapshot(request.StableID)
	if err != nil {
		return SessionView{}, err
	}
	// Only an operator may diagnose a paused node. An automatic trigger has
	// nothing to learn from one: its result cannot be compared against a local
	// status the checker stopped maintaining. The manager rejects the pairing
	// outright, so refusing here keeps the automatic workflows from producing a
	// validation error instead of an explanation.
	if snapshot.Maintenance && request.Trigger.Automatic() {
		return SessionView{}, ErrAutomaticPaused
	}
	// A fallback endpoint is only offered when the agent could actually run it,
	// otherwise the retry would come back as a configuration failure.
	alternativeID := ""
	if candidate, ok := diagnostics.AlternativeFor(descriptor.ID); ok {
		if alternative, known := diagnostics.ProfileByID(candidate); known && contains(agent.Capabilities, alternative.Capability) {
			alternativeID = alternative.ID
		}
	}
	profile := descriptor.TestProfileFor(alternativeID)
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
		Trigger:               request.Trigger,
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
		TargetSNI:  strings.TrimSpace(snapshot.Proxy.SNI),
	}
	c.assignments[job.JobID] = &queuedAssignment{assignment: assignment}
	c.signalAgentLocked(request.AgentID)
	updated, _ := c.manager.Session(session.SessionID)
	return view(updated), nil
}

// automaticProfile asks the agent to repeat the run's measurement as closely as
// the agent can: against the same server, for the same amount.
//
// The server is asked for whatever the outcome, when the run's test URL is in the
// catalogue and the agent can download from it. A technical failure against a
// server is a question about that server as much as a slowdown is, and an agent
// that answers from another one answers a different question.
//
// Only a low-speed outcome carries a size worth copying. A technical failure
// transferred whatever it managed before it gave up, and asking the agent to
// stop at that many bytes would measure the checker's timeout rather than the
// node — so that case keeps the agent's own configured amount.
func automaticProfile(descriptor diagnostics.ProfileDescriptor, alternativeID string, context diagnostics.AutomationContext, capabilities []string) diagnostics.TestProfile {
	profile := descriptor.TestProfileFor(alternativeID)
	if profile.Method != diagnostics.ProbeMethodDownload {
		return profile
	}
	if context.SpeedServerID != "" && contains(capabilities, diagnostics.CapabilitySpeedServersV1) {
		if _, known := diagnostics.SpeedServerByID(context.SpeedServerID); known {
			profile.ServerID = context.SpeedServerID
		}
	}
	if context.Outcome != diagnostics.AutomationOutcomeLowSpeed {
		return profile
	}
	if bytes, ok := diagnostics.ProfileDownloadBytes(context.MeasuredBytes); ok {
		profile.DownloadBytes = bytes
	}
	return profile
}

// preferredAgent picks which of the free agents to ask.
//
// Liveness order alone hands a node in one region to whichever agent last
// checked in, and an observation taken from an unrelated continent cannot
// settle whether that node is slow: the agent's path to it is a different path
// from the checker's, so a healthy number there says nothing about the number
// here. When a preference is configured it answers from recorded evidence about
// this specific node, and liveness order stays the tie-break it always was —
// including when the preference ranks nothing, which is the normal state before
// the first reachability sweep has run.
func (c *Controller) preferredAgent(stableID string, eligible []probeagent.AgentSnapshot) probeagent.AgentSnapshot {
	if c.config.AgentPreference == nil || len(eligible) < 2 {
		return eligible[0]
	}
	agentIDs := make([]string, 0, len(eligible))
	index := make(map[string]probeagent.AgentSnapshot, len(eligible))
	for _, candidate := range eligible {
		agentIDs = append(agentIDs, candidate.AgentID)
		index[candidate.AgentID] = candidate
	}
	for _, agentID := range c.config.AgentPreference.PreferAgents(stableID, agentIDs) {
		if candidate, ok := index[agentID]; ok {
			return candidate
		}
	}
	return eligible[0]
}

// nodeHostAddresses lists the addresses a node's server stands for. A published
// address is taken as it is; a name is resolved, because subscriptions publish
// addresses today and nothing guarantees they will tomorrow.
//
// A name that does not resolve is an error rather than an empty list. An empty
// list matches no agent, which would let the node's own one through — the one
// outcome the comparison exists to prevent.
func (c *Controller) nodeHostAddresses(server string) ([]netip.Addr, error) {
	server = strings.TrimSpace(server)
	if address, err := netip.ParseAddr(server); err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeHostLookupTimeout)
	defer cancel()
	resolved, err := c.config.Resolver.LookupIPAddr(ctx, server)
	if err != nil {
		// The lookup error names the server, which is execution material, so it
		// stops here instead of travelling up with the refusal.
		return nil, ErrNodeAddressUnresolved
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	for _, entry := range resolved {
		// A resolver may hand an IPv4 answer back in its 16-byte form, which
		// would never equal the 4-byte source IP the registry keeps.
		if address, ok := netip.AddrFromSlice(entry.IP); ok {
			addresses = append(addresses, address.Unmap())
		}
	}
	if len(addresses) == 0 {
		return nil, ErrNodeAddressUnresolved
	}
	return addresses, nil
}

// runsOnNodeHost reports whether an agent's expected source IP is one of the
// node's own addresses.
func runsOnNodeHost(agent probeagent.AgentSnapshot, nodeHost []netip.Addr) bool {
	source, err := netip.ParseAddr(strings.TrimSpace(agent.ExpectedSourceIP))
	if err != nil {
		return false
	}
	source = source.Unmap()
	for _, address := range nodeHost {
		if address == source {
			return true
		}
	}
	return false
}

// CreateAutomatic selects one healthy idle agent and creates the same bounded,
// generation-bound assignment as the manual workflow. The returned session is
// diagnostic evidence only; this method has no operational callbacks.
//
// An agent on the node's own host is never selected. It reaches the node
// without its traffic leaving the machine, so the provider's network, the route
// and whatever filters traffic in front of the node — the part a client depends
// on, and the part that usually breaks — never enter its measurement. Its answer
// comes back "not reproduced" almost regardless of the fault, which sends the
// operator to the checker's route while the node is the one in trouble, and
// keeps the alert waiting for its confirmation retry — only a reproduced
// problem lets it go out at once. No ranking can be trusted to avoid it either:
// such an agent reaches its own node faster than anyone, so evidence-based
// preference would pick it first.
//
// Placement is read from the agent's expected source IP. The registry refuses
// every control request from any other address, so that is where the agent
// actually is, and it is compared against every address the node's server
// stands for.
//
// Manual sessions and the reachability sweep name their agent themselves and
// are deliberately left alone: an operator may want the view from inside the
// host, and the sweep asks every agent about every node by design.
func (c *Controller) CreateAutomatic(request CreateAutomaticRequest) (SessionView, error) {
	if !c.Enabled() {
		return SessionView{}, probeagent.ErrDisabled
	}
	request.StableID = strings.TrimSpace(request.StableID)
	request.ProfileID = strings.TrimSpace(request.ProfileID)
	switch request.Trigger {
	case diagnostics.TriggerAutoSpeedFallback, diagnostics.TriggerAutoProxyFailure, diagnostics.TriggerAutoOffline:
	default:
		return SessionView{}, fmt.Errorf("unsupported automatic diagnostic trigger %q", request.Trigger)
	}
	if c.checker.ProjectMaintenanceEnabled() {
		return SessionView{}, ErrAutomaticPaused
	}
	descriptor, ok := diagnostics.ProfileByID(request.ProfileID)
	if !ok {
		return SessionView{}, ErrUnknownProfile
	}
	snapshot, configJSON, fingerprint, err := c.executionSnapshot(request.StableID)
	if err != nil {
		return SessionView{}, err
	}
	if snapshot.Maintenance || c.checker.ProjectMaintenanceEnabled() {
		return SessionView{}, ErrAutomaticPaused
	}
	// Resolved before the lock is taken: a lookup can take seconds, and the same
	// lock serves every agent's job poll.
	nodeHost, err := c.nodeHostAddresses(snapshot.Proxy.Server)
	if err != nil {
		return SessionView{}, err
	}

	agents := c.registry.Snapshot()
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].LastSeenAt.Equal(agents[j].LastSeenAt) {
			return agents[i].AgentID < agents[j].AgentID
		}
		return agents[i].LastSeenAt.After(agents[j].LastSeenAt)
	})
	now := time.Now().UTC()
	expiresAt := now.Add(c.config.JobTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeExpiredAssignmentsLocked(now)
	busyAgents := make(map[string]bool, len(c.assignments))
	for _, queued := range c.assignments {
		busyAgents[queued.assignment.Job.AgentID] = true
	}
	eligible := make([]probeagent.AgentSnapshot, 0, len(agents))
	onNodeHost := false
	for _, candidate := range agents {
		if !candidate.Enabled || !candidate.Connected || candidate.Health != "healthy" || busyAgents[candidate.AgentID] ||
			!contains(candidate.Capabilities, diagnostics.CapabilityControlV1) ||
			!contains(candidate.Capabilities, descriptor.Capability) {
			continue
		}
		// Checked after every other condition, so the refusal below names the
		// node's host only when an agent that could otherwise have taken the job
		// was turned away for it.
		if runsOnNodeHost(candidate, nodeHost) {
			onNodeHost = true
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		if onNodeHost {
			return SessionView{}, ErrOnlyNodeHostAgent
		}
		return SessionView{}, ErrUnavailableAgent
	}
	agent := c.preferredAgent(snapshot.Proxy.StableID, eligible)
	alternativeID := ""
	if candidate, ok := diagnostics.AlternativeFor(descriptor.ID); ok {
		if alternative, known := diagnostics.ProfileByID(candidate); known && contains(agent.Capabilities, alternative.Capability) {
			alternativeID = alternative.ID
		}
	}
	session, err := c.manager.CreateSession(diagnostics.CreateSessionRequest{
		StableID:            snapshot.Proxy.StableID,
		Trigger:             request.Trigger,
		ConfigGeneration:    snapshot.Generation,
		ConfigFingerprint:   fingerprint,
		LocalResultSnapshot: localResult(snapshot),
		AutomationContext:   request.AutomationContext,
		RequestedAgents:     []string{agent.AgentID},
		ExpiresAt:           expiresAt,
	})
	if err != nil {
		return SessionView{}, err
	}
	job, err := c.manager.RegisterJob(diagnostics.RegisterJobRequest{
		SessionID: session.SessionID,
		AgentID:   agent.AgentID,
		Profile:   automaticProfile(descriptor, alternativeID, request.AutomationContext, agent.Capabilities),
		ExpiresAt: expiresAt,
	})
	if err != nil {
		_ = c.manager.CancelSession(session.SessionID)
		return SessionView{}, err
	}
	c.assignments[job.JobID] = &queuedAssignment{assignment: probeagent.JobAssignment{
		Job:        job,
		XrayConfig: append(json.RawMessage(nil), configJSON...),
		SocksPort:  c.config.SocksPort,
		TargetHost: snapshot.Proxy.Server,
		TargetPort: snapshot.Proxy.Port,
		TargetSNI:  strings.TrimSpace(snapshot.Proxy.SNI),
	}}
	c.signalAgentLocked(agent.AgentID)
	updated, _ := c.manager.Session(session.SessionID)
	return view(updated), nil
}

// Profiles exposes the catalogue with the controller's own default marked, so
// the admin UI does not have to duplicate either the list or the default rule.
func (c *Controller) Profiles() []ProfileView {
	defaultID := ""
	if descriptor, ok := diagnostics.ProfileForCheckMethod(c.config.CheckMethod); ok {
		defaultID = descriptor.ID
	}
	catalogue := diagnostics.Profiles()
	result := make([]ProfileView, 0, len(catalogue))
	for _, descriptor := range catalogue {
		result = append(result, ProfileView{ProfileDescriptor: descriptor, Default: descriptor.ID == defaultID})
	}
	return result
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

// Delete removes one session together with any queued assignment. Dropping the
// assignment matters: an agent holding a job for a session that no longer exists
// would run a probe whose result can never be accepted.
func (c *Controller) Delete(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if err := c.manager.DeleteSession(sessionID); err != nil {
		return err
	}
	c.dropAssignmentsFor(func(assignment probeagent.JobAssignment) bool {
		return assignment.Job.SessionID == sessionID
	})
	return nil
}

// Clear removes every session for one node, or all of them when stableID is
// empty, and reports how many were removed.
func (c *Controller) Clear(stableID string) int {
	stableID = strings.TrimSpace(stableID)
	removed := c.manager.DeleteSessions(stableID)
	c.dropAssignmentsFor(func(assignment probeagent.JobAssignment) bool {
		return stableID == "" || assignment.Job.StableID == stableID
	})
	return removed
}

func (c *Controller) dropAssignmentsFor(match func(probeagent.JobAssignment) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for jobID, queued := range c.assignments {
		if match(queued.assignment) {
			delete(c.assignments, jobID)
		}
	}
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

// resolveProfile turns the requested profile into a descriptor, refusing one the
// agent cannot execute. Rejecting here produces an actionable error instead of
// the opaque configuration failure the agent would otherwise return.
func (c *Controller) resolveProfile(requested string, capabilities []string) (diagnostics.ProfileDescriptor, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		descriptor, ok := diagnostics.ProfileForCheckMethod(c.config.CheckMethod)
		if !ok {
			return diagnostics.ProfileDescriptor{}, fmt.Errorf("%w: unsupported diagnostic check method %q", ErrUnknownProfile, c.config.CheckMethod)
		}
		return descriptor, nil
	}
	descriptor, ok := diagnostics.ProfileByID(requested)
	if !ok {
		return diagnostics.ProfileDescriptor{}, fmt.Errorf("%w: %q", ErrUnknownProfile, requested)
	}
	if !contains(capabilities, descriptor.Capability) {
		return diagnostics.ProfileDescriptor{}, fmt.Errorf("%w: %q requires %s", ErrUnsupportedByAgent, descriptor.ID, descriptor.Capability)
	}
	return descriptor, nil
}

func profileForMethod(method string) (diagnostics.TestProfile, error) {
	descriptor, ok := diagnostics.ProfileForCheckMethod(method)
	if !ok {
		return diagnostics.TestProfile{}, fmt.Errorf("unsupported diagnostic check method %q", method)
	}
	return descriptor.TestProfileFor(""), nil
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
	if session.Trigger == diagnostics.TriggerAutoSpeedFallback {
		// The same judgement the automation hands to alerts and the verdict
		// journal, so the session list never tells a different story.
		context := session.AutomationContext
		verdict, reason := diagnostics.JudgeSpeed(diagnostics.SpeedEvidence{
			Outcome: context.Outcome, ThresholdMbps: context.ThresholdMbps,
			ObservedMbps: context.ObservedMbps, ServerID: context.SpeedServerID,
		}, remote, observation.Reliable)
		return speedSummary(verdict, reason, remote)
	}
	if session.Trigger == diagnostics.TriggerAutoOffline {
		return offlineSummary(remote)
	}
	if session.Trigger == diagnostics.TriggerAutoProxyFailure && remote.Status != diagnostics.ProbeStatusOnline {
		// Read before the generic comparison below, which only calls a failure
		// reproduced when both sides report the same code. A tunnel that fails
		// for the agent at a different stage still fails: that is the answer to
		// the question this trigger asks, and "no stable pattern" would bury it.
		if alternative := remote.AlternativeEndpoint; alternative != nil && alternative.Status == diagnostics.ProbeStatusOnline {
			return "The agent's primary endpoint failed but its alternative tunnelled endpoint worked; the tunnel carries traffic from another network, so an endpoint-specific problem is likely."
		}
		return "The proxy failure was reproduced from another network; the node's proxy service, its configuration or the hosting network is likely involved."
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

// speedSummary words a speed verdict. It names where the evidence points and
// stops there: "the path" is the stretch between the checker and the node, which
// is where a checker inside a filtering or congested network loses rate its
// clients may lose too — it is not a claim that the checker is at fault.
func speedSummary(verdict diagnostics.Verdict, reason string, remote diagnostics.Observation) string {
	switch verdict {
	case diagnostics.VerdictReproduced:
		if remote.Status == diagnostics.ProbeStatusOnline {
			return "Low throughput was reproduced from another network against the same speed-test server; the node, its uplink or its hosting network is likely involved."
		}
		return "The speed-test problem was reproduced from another network; a shared node, server or configuration issue is likely."
	case diagnostics.VerdictPathLimited:
		if reason == diagnostics.ReasonAgentAlsoBelowThreshold {
			return "The agent got many times the checker's rate through the same node but stayed below the threshold; most of the loss is on the path between the checker and the node, and the node is not above suspicion."
		}
		return "The agent got many times the checker's rate through the same node; the loss is on the path between the checker and the node, not in the node."
	case diagnostics.VerdictInconclusive:
		if reason == diagnostics.ReasonNearThreshold {
			return "The agent's rate is too close to the threshold to confirm or clear the slowdown."
		}
		return "The agent measured a different speed-test server, so its rate can neither confirm nor clear the slowdown."
	case diagnostics.VerdictUnreliable:
		return "The agent reached the endpoint but returned no throughput evidence; there is not enough data to compare the low-speed result."
	default:
		if reason == diagnostics.ReasonAlternativeWorked {
			return "The agent reproduced a failure only against its download endpoint; the alternative tunnelled endpoint worked, so an endpoint-specific problem is likely."
		}
		return "The node delivered the threshold to another network; the problem is on the path between the checker and the node, or at the checker's test server."
	}
}

// offlineSummary words the answer to "the checker cannot reach this node at
// all". From a checker inside a filtered network a blocked IP and a dead host
// look the same; which of the two it is decides what the operator does next.
func offlineSummary(remote diagnostics.Observation) string {
	switch {
	case remote.Status == diagnostics.ProbeStatusOnline:
		return "The node works from another network; it is unreachable only on the checker's path, so an IP or route block on that side is likely."
	case remote.AlternativeEndpoint != nil && remote.AlternativeEndpoint.Status == diagnostics.ProbeStatusOnline:
		return "The node's tunnel works from another network against the alternative endpoint; it is unreachable only on the checker's path, so an IP or route block on that side is likely."
	case remote.Status == diagnostics.ProbeStatusProxyFailure:
		return "The host answers from another network but its tunnel does not carry traffic; the proxy service on the node is likely down, and the checker's path does not reach the host at all."
	default:
		return "The node is unreachable from another network as well; the host, its port or the hosting network is likely down."
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
