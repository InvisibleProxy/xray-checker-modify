package agentautomation

import (
	"context"
	"errors"
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
func (c *Coordinator) StartSpeedDiagnostics(report speedtest.RunReport, threshold float64) map[string]Handle {
	if !c.Enabled() {
		return nil
	}
	handles := make(map[string]Handle)
	for _, result := range report.Results {
		outcome, ok := speedAutomationOutcome(result, threshold)
		if !ok {
			continue
		}
		handles[result.StableID] = c.startSpeed(result, report.Source, threshold, outcome)
	}
	if len(handles) == 0 {
		return nil
	}
	return handles
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

func (c *Coordinator) startSpeed(result speedtest.Result, source string, threshold float64, outcome string) Handle {
	now := c.config.Now().UTC()
	stableID := strings.TrimSpace(result.StableID)
	if strings.TrimSpace(source) == "" {
		source = "unknown"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	if existing, ok := c.entries[stableID]; ok {
		return existing.handle
	}
	if c.activeLocked() >= c.config.MaxConcurrent {
		return Handle{StableID: stableID, State: speedtest.AgentDiagnosticUnavailable, Detail: "automation capacity is busy", Outcome: outcome, Threshold: threshold, StartedAt: now}
	}
	view, err := c.controller.CreateAutomatic(remoteprobe.CreateAutomaticRequest{
		StableID:  stableID,
		Trigger:   diagnostics.TriggerAutoSpeedFallback,
		ProfileID: diagnostics.ProfileDownload,
		AutomationContext: diagnostics.AutomationContext{
			Kind:             diagnostics.AutomationKindSpeedFallback,
			Outcome:          outcome,
			Source:           source,
			ThresholdMbps:    threshold,
			ObservedMbps:     result.Mbps,
			FallbackAttempts: result.FallbackAttempts,
		},
	})
	if err != nil {
		detail := "automatic diagnostic could not be started"
		switch {
		case errors.Is(err, remoteprobe.ErrUnavailableAgent):
			detail = "no healthy idle diagnostic agent is connected"
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
		return Handle{StableID: stableID, State: speedtest.AgentDiagnosticUnavailable, Detail: detail, Outcome: outcome, Threshold: threshold, StartedAt: now}
	}
	handle := Handle{
		StableID: stableID, SessionID: view.Session.SessionID, State: speedtest.AgentDiagnosticRunning,
		Outcome: outcome, Threshold: threshold, StartedAt: now,
	}
	c.entries[stableID] = entry{handle: handle}
	return handle
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

func (c *Coordinator) Await(ctx context.Context, handles map[string]Handle) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(handles) == 0 {
		return nil
	}
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		annotations := c.Annotations(handles)
		pending := false
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
		if handle.Outcome == diagnostics.AutomationOutcomeLowSpeed && handle.Threshold > 0 {
			if observation.Throughput == nil {
				annotation.State = speedtest.AgentDiagnosticUnreliable
				annotation.Detail = "agent download observation has no throughput evidence"
				return annotation
			}
			// The agent reports whole Mbps, so its true rate lies in
			// [Mbps, Mbps+1). Only claim the slowdown was reproduced when the
			// whole interval is below the threshold; near the boundary the
			// evidence cannot tell, and a false "reproduced" is the costly one.
			if float64(observation.Throughput.Mbps)+1 <= handle.Threshold {
				annotation.State = speedtest.AgentDiagnosticReproduced
				return annotation
			}
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
