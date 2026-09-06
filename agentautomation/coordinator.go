package agentautomation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
)

const (
	DefaultCooldown      = 30 * time.Minute
	DefaultMaxConcurrent = 2
	defaultPollInterval  = 200 * time.Millisecond
)

type Config struct {
	Enabled  bool
	Cooldown time.Duration
	// AlertWait is deliberately allowed to be zero: in that mode the caller
	// attaches the current running state without waiting for an observation.
	AlertWait     time.Duration
	MaxConcurrent int
	PollInterval  time.Duration
	Now           func() time.Time
}

type Snapshot struct {
	Enabled              bool `json:"enabled"`
	SpeedFallbackEnabled bool `json:"speedFallbackEnabled"`
	CooldownSeconds      int  `json:"cooldownSeconds"`
	AlertWaitSeconds     int  `json:"alertWaitSeconds"`
	MaxConcurrent        int  `json:"maxConcurrent"`
	Active               int  `json:"active"`
}

type SessionController interface {
	Enabled() bool
	CreateAutomatic(remoteprobe.CreateAutomaticRequest) (remoteprobe.SessionView, error)
	Session(string) (remoteprobe.SessionView, bool)
}

type AgentSource interface {
	Agent(string) (probeagent.AgentSnapshot, bool)
}

type Handle struct {
	StableID  string
	SessionID string
	State     string
	Detail    string
	Outcome   string
	Threshold float64
	StartedAt time.Time
	// retry is set when the start was refused for a reason that clears on its
	// own, and carries what a second attempt needs. It is unexported because it
	// is bookkeeping rather than evidence: a caller hands the handle back and
	// never builds or reads one.
	retry *startRequest
}

type entry struct {
	handle Handle
}

// Coordinator is the only consumer of speed-test events that may create an
// automatic diagnostic session. It returns sanitized annotations but has no
// callbacks into speedtest, Telegram or any operational state owner.
//
// Its mutex is deliberately held across calls into the session controller:
// checking capacity and creating the session have to be one atomic step, or two
// concurrent reports would both pass the limit. This is safe only while the
// controller never calls back into the coordinator, which is why the dependency
// runs one way and is expressed as the narrow SessionController interface.
type Coordinator struct {
	mu         sync.Mutex
	config     Config
	controller SessionController
	agents     AgentSource
	entries    map[string]entry
}

func New(config Config, controller SessionController, agents AgentSource) (*Coordinator, error) {
	if config.Cooldown == 0 {
		config.Cooldown = DefaultCooldown
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if controller == nil || agents == nil || config.Cooldown < time.Minute || config.AlertWait < 0 ||
		config.MaxConcurrent < 1 || config.PollInterval <= 0 {
		return nil, errors.New("invalid agent automation configuration")
	}
	return &Coordinator{config: config, controller: controller, agents: agents, entries: make(map[string]entry)}, nil
}

func (c *Coordinator) Enabled() bool {
	return c != nil && c.config.Enabled && c.controller.Enabled()
}

func (c *Coordinator) AlertWait() time.Duration {
	if c == nil {
		return 0
	}
	return c.config.AlertWait
}

func (c *Coordinator) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.config.Now().UTC()
	c.pruneLocked(now)
	return Snapshot{
		Enabled:              c.Enabled(),
		SpeedFallbackEnabled: c.Enabled(),
		CooldownSeconds:      int(c.config.Cooldown / time.Second),
		AlertWaitSeconds:     int(c.config.AlertWait / time.Second),
		MaxConcurrent:        c.config.MaxConcurrent,
		Active:               c.activeLocked(),
	}
}

// StartSpeedDiagnostics creates at most one automatic job per unresolved
// StableID. A repeated confirmation run reuses the same session during the
// cooldown, which is how its completed evidence reaches the final alert.
//
// Slots are handed out in candidate order, not report order. A report lists
// nodes in whatever order the run measured them, so with more unresolved nodes
// than slots the last free one used to go to whoever happened to be listed
// first. That is how an alert ends up explaining two timeouts and saying
// nothing about the one node that came back with a measurable slowdown.
func (c *Coordinator) StartSpeedDiagnostics(report speedtest.RunReport, threshold float64) map[string]Handle {
	if !c.Enabled() {
		return nil
	}
	candidates := speedAutomationCandidates(report.Results, report.Source, threshold)
	if len(candidates) == 0 {
		return nil
	}
	handles := make(map[string]Handle, len(candidates))
	for _, candidate := range candidates {
		handles[candidate.stableID] = c.startSpeed(candidate)
	}
	return handles
}

// startRequest is one node's claim on a diagnostic slot. It holds everything a
// start needs, so a start refused for a transient reason can be attempted again
// from the handle alone, without the report it came from.
type startRequest struct {
	stableID         string
	source           string
	outcome          string
	threshold        float64
	observedMbps     float64
	measuredBytes    int64
	fallbackAttempts int
	// shortfall is how far below the threshold the measurement fell, as a
	// fraction of it. Zero means there is no number to compare, which is the
	// case for every technical failure; see speedAutomationCandidates.
	shortfall float64
	// notBefore paces the retries of a deferred start.
	notBefore time.Time
}

// speedAutomationCandidates selects the nodes worth an agent's time and orders
// them by what a remote observation can settle.
//
// A low-speed result goes first, deepest shortfall first. It is the only
// outcome where the agent answers with a number that can be held against the
// one the run produced, and that comparison only means anything while both
// describe the same moment: a rate that drifts is not settled by measuring it
// again half an hour later. A technical failure gets a yes/no answer instead,
// and the confirmation retry re-measures it anyway.
func speedAutomationCandidates(results []speedtest.Result, source string, threshold float64) []startRequest {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	candidates := make([]startRequest, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		effective := effectiveThreshold(result, threshold)
		outcome, ok := speedAutomationOutcome(result, effective)
		if !ok {
			continue
		}
		stableID := strings.TrimSpace(result.StableID)
		if seen[stableID] {
			continue
		}
		seen[stableID] = true
		candidate := startRequest{
			stableID: stableID, source: source, outcome: outcome, threshold: effective,
			observedMbps: result.Mbps, measuredBytes: result.DownloadedBytes,
			fallbackAttempts: result.FallbackAttempts,
		}
		if outcome == diagnostics.AutomationOutcomeLowSpeed && effective > 0 {
			candidate.shortfall = (effective - result.Mbps) / effective
		}
		candidates = append(candidates, candidate)
	}
	sortStartRequests(candidates)
	return candidates
}

// sortStartRequests is the single ordering both the first pass and a deferred
// retry use. Map iteration is random, and a random winner for the last free
// slot is exactly what this ordering exists to remove.
func sortStartRequests(requests []startRequest) {
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].shortfall != requests[j].shortfall {
			return requests[i].shortfall > requests[j].shortfall
		}
		return requests[i].stableID < requests[j].stableID
	})
}

// effectiveThreshold is the threshold the run judged this measurement against:
// the node's own override when it has one, exactly as the report reads it.
// Reading the global setting here instead would classify one node two ways — a
// node overridden to 50 Mbps and measured at 60 reads as healthy in the alert
// and as a slowdown worth diagnosing in this package.
func effectiveThreshold(result speedtest.Result, threshold float64) float64 {
	if result.LowSpeedThresholdMbps > 0 {
		return result.LowSpeedThresholdMbps
	}
	return threshold
}

func speedAutomationOutcome(result speedtest.Result, threshold float64) (string, bool) {
	if strings.TrimSpace(result.StableID) == "" || result.MaintenanceProbe || result.ProjectMaintenanceProbe ||
		result.Offline || !result.FallbackAttempted || result.FallbackAttempts < 1 {
		return "", false
	}
	if result.FallbackExhausted && result.Error != "" {
		return diagnostics.AutomationOutcomeTechnical, true
	}
	if threshold > 0 && result.Error == "" && result.Mbps < threshold && (result.FallbackExhausted || result.FallbackUsed) {
		return diagnostics.AutomationOutcomeLowSpeed, true
	}
	return "", false
}

func (c *Coordinator) startSpeed(request startRequest) Handle {
	now := c.config.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	if existing, ok := c.entries[request.stableID]; ok {
		return existing.handle
	}
	if c.activeLocked() >= c.config.MaxConcurrent {
		return c.deferredHandle(request, now, "automation capacity is busy")
	}
	view, err := c.controller.CreateAutomatic(remoteprobe.CreateAutomaticRequest{
		StableID:  request.stableID,
		Trigger:   diagnostics.TriggerAutoSpeedFallback,
		ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind:             diagnostics.AutomationKindSpeedFallback,
			Outcome:          request.outcome,
			Source:           request.source,
			ThresholdMbps:    request.threshold,
			ObservedMbps:     request.observedMbps,
			MeasuredBytes:    request.measuredBytes,
			FallbackAttempts: request.fallbackAttempts,
		},
	})
	if err != nil {
		detail := "automatic diagnostic could not be started"
		transient := false
		switch {
		case errors.Is(err, remoteprobe.ErrUnavailableAgent):
			detail = "no healthy idle diagnostic agent is connected"
			// Occupied is far more common than absent: the periodic reachability
			// sweep holds every agent for the length of a pass, and a manual
			// session holds one. Both clear well inside a single alert wait.
			transient = true
		case errors.Is(err, remoteprobe.ErrAutomaticPaused):
			detail = "automatic diagnostics are paused by maintenance"
		case errors.Is(err, probeagent.ErrDisabled):
			detail = "remote diagnostics are disabled"
		}
		// Not recorded: the cooldown exists to stop a session from being repeated,
		// and no session started here. Burning it on a transient refusal - no idle
		// agent right now - would silence the node for the whole cooldown even
		// after an agent reconnects a second later. This also keeps the two
		// transient refusals, here and the capacity one above, behaving alike.
		if transient {
			return c.deferredHandle(request, now, detail)
		}
		return Handle{StableID: request.stableID, State: speedtest.AgentDiagnosticUnavailable, Detail: detail, Outcome: request.outcome, Threshold: request.threshold, StartedAt: now}
	}
	handle := Handle{
		StableID: request.stableID, SessionID: view.Session.SessionID, State: speedtest.AgentDiagnosticRunning,
		Outcome: request.outcome, Threshold: request.threshold, StartedAt: now,
	}
	c.entries[request.stableID] = entry{handle: handle}
	return handle
}

// deferredHandle records a refusal the wait window may outlive. The handle
// still reads as unavailable, so an alert sent right now says exactly what it
// said before; the difference is that this one can be retried.
func (c *Coordinator) deferredHandle(request startRequest, now time.Time, detail string) Handle {
	request.notBefore = now.Add(c.deferredRetryInterval())
	return Handle{
		StableID: request.stableID, State: speedtest.AgentDiagnosticUnavailable, Detail: detail,
		Outcome: request.outcome, Threshold: request.threshold, StartedAt: now, retry: &request,
	}
}

// deferredRetryInterval paces retries of a deferred start. Asking at the
// annotation poll rate would put hundreds of creation attempts into one alert
// wait, for an answer that changes on the scale of a whole diagnostic session,
// so a deferred node re-asks an order of magnitude less often than the alert
// re-reads the sessions it already has.
func (c *Coordinator) deferredRetryInterval() time.Duration {
	return c.config.PollInterval * 10
}

func (c *Coordinator) Annotations(handles map[string]Handle) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(handles) == 0 {
		return nil
	}
	result := make(map[string]speedtest.AgentDiagnostic, len(handles))
	for stableID, handle := range handles {
		result[stableID] = c.annotation(handle)
	}
	return result
}

// Await holds the alert until every diagnostic has either answered or run out
// of wait. It works on its own copy of the handles, because a start that was
// refused when the report arrived may succeed part way through the wait and the
// handle it produces has to replace the refused one.
func (c *Coordinator) Await(ctx context.Context, handles map[string]Handle) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(handles) == 0 {
		return nil
	}
	current := make(map[string]Handle, len(handles))
	for stableID, handle := range handles {
		current[stableID] = handle
	}
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		c.resumeDeferred(current)
		annotations := c.Annotations(current)
		pending := hasDeferred(current)
		for _, annotation := range annotations {
			if annotation.State == speedtest.AgentDiagnosticRunning {
				pending = true
				break
			}
		}
		if !pending {
			return annotations
		}
		select {
		case <-ctx.Done():
			return annotations
		case <-ticker.C:
		}
	}
}

// resumeDeferred gives a node refused for a self-clearing reason another chance
// while the alert is still waiting.
//
// Capacity comes back inside the wait far more often than not: a session that
// answers in ten seconds releases its slot with most of the wait still to run.
// Without this the node refused at second zero stays refused until the next
// run, which is how the only node with a measurable slowdown ends up being the
// one the alert has nothing to say about.
func (c *Coordinator) resumeDeferred(handles map[string]Handle) {
	now := c.config.Now().UTC()
	deferred := make([]startRequest, 0, len(handles))
	for _, handle := range handles {
		if handle.retry == nil || now.Before(handle.retry.notBefore) {
			continue
		}
		deferred = append(deferred, *handle.retry)
	}
	if len(deferred) == 0 {
		return
	}
	sortStartRequests(deferred)
	for _, request := range deferred {
		handles[request.stableID] = c.startSpeed(request)
	}
}

func hasDeferred(handles map[string]Handle) bool {
	for _, handle := range handles {
		if handle.retry != nil {
			return true
		}
	}
	return false
}

func (c *Coordinator) annotation(handle Handle) speedtest.AgentDiagnostic {
	annotation := speedtest.AgentDiagnostic{State: handle.State, SessionID: handle.SessionID, Detail: handle.Detail}
	if handle.SessionID == "" {
		return annotation
	}
	view, ok := c.controller.Session(handle.SessionID)
	if !ok {
		annotation.State = speedtest.AgentDiagnosticUnavailable
		annotation.Detail = "diagnostic session is no longer available"
		return annotation
	}
	// Prefer the agent that actually signed the observation over the first one
	// requested; with more than one requested agent they are not the same, and an
	// alert naming the wrong region is worse than naming none.
	if count := len(view.Session.AgentObservations); count > 0 {
		annotation.AgentID = view.Session.AgentObservations[count-1].Observation.AgentID
	}
	if annotation.AgentID == "" && len(view.Session.RequestedAgents) > 0 {
		annotation.AgentID = view.Session.RequestedAgents[0]
	}
	if annotation.AgentID != "" {
		if agent, found := c.agents.Agent(annotation.AgentID); found {
			annotation.AgentName = agent.DisplayName
			annotation.Region = agent.Region
			annotation.Provider = agent.Provider
		}
	}
	if len(view.Session.AgentObservations) == 0 {
		if view.Session.State.Terminal() {
			annotation.State = speedtest.AgentDiagnosticUnavailable
			annotation.Detail = "no signed remote observation was received"
		} else {
			annotation.State = speedtest.AgentDiagnosticRunning
		}
		return annotation
	}
	record := view.Session.AgentObservations[len(view.Session.AgentObservations)-1]
	observation := record.Observation
	annotation.RemoteStatus = string(observation.Status)
	annotation.FailureCode = observation.Failure.Code
	annotation.FailureStage = string(observation.Failure.Stage)
	annotation.DirectConnectivityChecked = observation.DirectConnectivity.Checked
	annotation.DirectConnectivityOnline = observation.DirectConnectivity.Online
	annotation.CheckedAt = observation.CheckedAt
	if observation.Throughput != nil {
		annotation.Mbps = observation.Throughput.Mbps
	}
	if observation.AlternativeEndpoint != nil {
		annotation.AlternativeProfile = observation.AlternativeEndpoint.ProfileID
		annotation.AlternativeStatus = string(observation.AlternativeEndpoint.Status)
	}
	if !record.Reliable {
		annotation.State = speedtest.AgentDiagnosticUnreliable
		annotation.Detail = "agent direct connectivity control failed"
		return annotation
	}
	if observation.AlternativeEndpoint != nil && observation.AlternativeEndpoint.Status == diagnostics.ProbeStatusOnline {
		annotation.State = speedtest.AgentDiagnosticNotReproduced
		annotation.Detail = "the agent alternative tunnelled endpoint worked"
		return annotation
	}
	if observation.Status == diagnostics.ProbeStatusOnline {
		// The agent's own rate decides, whichever outcome sent it. A run that
		// timed out and an agent that gets through at half the threshold is not
		// a node the agent found healthy: calling that "not reproduced" sends an
		// operator to look at the checker while the node is the thing that is
		// slow. It was the shape of the very first alerts this handled — a node
		// reported as fine at 42 Mbps against a threshold of 100.
		if handle.Threshold > 0 && observation.Throughput != nil {
			// The agent reports whole Mbps, so its true rate lies in
			// [Mbps, Mbps+1). Only claim the slowdown was reproduced when the
			// whole interval is below the threshold; near the boundary the
			// evidence cannot tell, and a false "reproduced" is the costly one.
			if float64(observation.Throughput.Mbps)+1 <= handle.Threshold {
				annotation.State = speedtest.AgentDiagnosticReproduced
				return annotation
			}
		}
		// A slowdown answered with no rate at all settles nothing, and saying so
		// is the honest reading. A technical failure is different: the agent got
		// through, which is an answer on its own terms.
		if handle.Outcome == diagnostics.AutomationOutcomeLowSpeed && handle.Threshold > 0 && observation.Throughput == nil {
			annotation.State = speedtest.AgentDiagnosticUnreliable
			annotation.Detail = "agent download observation has no throughput evidence"
			return annotation
		}
		annotation.State = speedtest.AgentDiagnosticNotReproduced
		return annotation
	}
	annotation.State = speedtest.AgentDiagnosticReproduced
	return annotation
}

func (c *Coordinator) pruneLocked(now time.Time) {
	for stableID, current := range c.entries {
		if c.abandonedLocked(current.handle) {
			delete(c.entries, stableID)
			continue
		}
		if now.Sub(current.handle.StartedAt) < c.config.Cooldown {
			continue
		}
		if current.handle.SessionID != "" {
			if view, ok := c.controller.Session(current.handle.SessionID); ok && !view.Session.State.Terminal() {
				continue
			}
		}
		delete(c.entries, stableID)
	}
}

// abandonedLocked reports a session that reached a terminal state without a
// single observation, and releases its cooldown early.
//
// The cooldown exists to stop a diagnostic being repeated while its evidence is
// still fresh. A session that collected nothing has no evidence, so holding the
// node for the full cooldown buys silence and no information. That happens for
// real: an agent is chosen while it still looks connected — liveness is a
// freshness window, so "connected" always means "was answering a moment ago" —
// and then never claims the job, which expires. This is the same reasoning that
// keeps a transient refusal in startSpeed from occupying the cooldown; without
// it, the two paths disagree about the same situation.
//
// An unreliable observation is deliberately not abandoned. It answered: the
// alert names the agent and says its own connectivity control failed, and
// repeating it would ask the same broken agent the same question.
func (c *Coordinator) abandonedLocked(handle Handle) bool {
	if handle.SessionID == "" {
		return false
	}
	view, ok := c.controller.Session(handle.SessionID)
	if !ok {
		// The session has aged out of the manager, so whether it ever answered
		// is no longer knowable. Fall back to the cooldown rather than guessing
		// in the direction that repeats work.
		return false
	}
	return view.Session.State.Terminal() && len(view.Session.AgentObservations) == 0
}

func (c *Coordinator) activeLocked() int {
	active := 0
	for _, current := range c.entries {
		if current.handle.SessionID == "" {
			continue
		}
		if view, ok := c.controller.Session(current.handle.SessionID); ok && !view.Session.State.Terminal() {
			active++
		}
	}
	return active
}
