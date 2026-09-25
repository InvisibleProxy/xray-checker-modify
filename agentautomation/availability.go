package agentautomation

import (
	"context"
	"sort"
	"strings"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/logger"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
	"xray-checker/verdictlog"
)

// AvailabilityFailure is one node the availability check currently reports as
// failing. Kind says how: diagnostics.AutomationKindProxyFailure when the host
// answers TCP or ping but traffic does not get through the tunnel, and
// diagnostics.AutomationKindOffline when nothing answers at all. Since is when
// the current episode began, which is what tells a node that is still failing
// apart from one that failed, recovered and failed again.
type AvailabilityFailure struct {
	StableID string
	Kind     string
	Since    time.Time
}

// availabilityEntry is the probe that answers for one node's failure.
type availabilityEntry struct {
	// kind and since are the episode the probe was started for. A node that
	// moves from proxy_failure to offline is asked again: the first answer was
	// about the tunnel, and the new question is whether the node is there at all.
	kind   string
	since  time.Time
	handle Handle
	// final is the verdict of a session that answered. It is kept because the
	// session is not: the manager evicts old sessions, and an episode that
	// lasts a night outlives the one probe it gets. A reminder sent in the
	// morning should still say what the agent found, not that the result is
	// gone.
	final *speedtest.AgentDiagnostic
	// journaled records that the verdict journal has this entry's answer, so a
	// verdict read many times is written once.
	journaled bool
}

// ProxyFailureEnabled reports whether the proxy-failure trigger is switched on.
func (c *Coordinator) ProxyFailureEnabled() bool {
	return c.Enabled() && c.config.ProxyFailureEnabled
}

// OfflineEnabled reports whether the offline trigger is switched on.
func (c *Coordinator) OfflineEnabled() bool {
	return c.Enabled() && c.config.OfflineEnabled
}

// AvailabilityEnabled reports whether any availability trigger is on, which is
// what an alert needs to know before it waits for an answer.
func (c *Coordinator) AvailabilityEnabled() bool {
	return c.ProxyFailureEnabled() || c.OfflineEnabled()
}

func (c *Coordinator) availabilityKindEnabled(kind string) bool {
	switch kind {
	case diagnostics.AutomationKindProxyFailure:
		return c.ProxyFailureEnabled()
	case diagnostics.AutomationKindOffline:
		return c.OfflineEnabled()
	default:
		return false
	}
}

// StartAvailabilityDiagnostics is called after every completed availability
// check with every node that check left failing — the whole list, not only the
// nodes that changed. A node missing from it has recovered, and its entry is
// released once nothing depends on it.
//
// Each episode gets one probe. A node that stays down for hours is not probed
// again: the first answer is what separates "the node is broken" from "the
// checker's path is broken", and repeating it every cooldown would hold agents
// a speed run may need for nothing new. A start that never produced an
// observation — no free agent, a job nobody claimed — is not an answer, and the
// next check asks again.
//
// The call is where the slot is taken, not where the answer is awaited: the
// first down alert goes out no earlier than the second failed check, one full
// check interval later, and an agent answers well inside that.
func (c *Coordinator) StartAvailabilityDiagnostics(failures []AvailabilityFailure) {
	if !c.AvailabilityEnabled() {
		return
	}
	candidates, current := availabilityCandidates(failures, c.config.EnvironmentSourced)
	now := c.config.Now().UTC()
	c.mu.Lock()
	records := c.harvestAvailabilityLocked()
	records = append(records, c.pruneAvailabilityLocked(now, current)...)
	c.mu.Unlock()
	c.journal(records)
	for _, candidate := range candidates {
		if c.availabilityKindEnabled(candidate.Kind) {
			c.startAvailability(candidate, now)
		}
	}
}

// availabilityCandidates orders the episodes worth an agent, the oldest first,
// and lists every node still failing so entries of recovered nodes can go. A
// node from a panel-added subscription is failing all the same but is never a
// candidate: see Config.EnvironmentSourced.
func availabilityCandidates(failures []AvailabilityFailure, environmentSourced func(string) bool) ([]AvailabilityFailure, map[string]bool) {
	candidates := make([]AvailabilityFailure, 0, len(failures))
	current := make(map[string]bool, len(failures))
	for _, failure := range failures {
		failure.StableID = strings.TrimSpace(failure.StableID)
		if failure.StableID == "" || current[failure.StableID] {
			continue
		}
		current[failure.StableID] = true
		if environmentSourced != nil && !environmentSourced(failure.StableID) {
			continue
		}
		if failure.Kind != diagnostics.AutomationKindOffline {
			failure.Kind = diagnostics.AutomationKindProxyFailure
		}
		failure.Since = failure.Since.UTC()
		candidates = append(candidates, failure)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].Since.Equal(candidates[j].Since) {
			return candidates[i].Since.Before(candidates[j].Since)
		}
		return candidates[i].StableID < candidates[j].StableID
	})
	return candidates, current
}

func (c *Coordinator) startAvailability(failure AvailabilityFailure, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.availability[failure.StableID]; ok && !c.availabilityNeedsProbeLocked(existing, failure.Kind, failure.Since, now) {
		return
	}
	trigger, profileID, context := diagnostics.TriggerAutoProxyFailure, c.config.ProxyFailureProfileID, diagnostics.ProxyFailureAutomationContext()
	if failure.Kind == diagnostics.AutomationKindOffline {
		trigger, profileID, context = diagnostics.TriggerAutoOffline, c.config.OfflineProfileID, diagnostics.OfflineAutomationContext()
	}
	request := startRequest{
		stableID: failure.StableID, kind: failure.Kind,
		profileID: profileID, source: diagnostics.AutomationSourceAvailability,
	}
	if c.activeLocked() >= c.config.MaxConcurrent {
		c.availability[failure.StableID] = availabilityEntry{kind: failure.Kind, since: failure.Since, handle: Handle{
			StableID: failure.StableID, State: speedtest.AgentDiagnosticUnavailable,
			Detail: "automation capacity is busy", StartedAt: now, request: request,
		}}
		return
	}
	view, err := c.controller.CreateAutomatic(remoteprobe.CreateAutomaticRequest{
		StableID:          failure.StableID,
		Trigger:           trigger,
		ProfileID:         profileID,
		AutomationContext: context,
	})
	if err != nil {
		// Every refusal is retried at the next check, transient or not, and none
		// of them occupies the episode: no session started, so there is no
		// evidence to protect from being repeated.
		detail, _ := refusalDetail(err)
		c.availability[failure.StableID] = availabilityEntry{kind: failure.Kind, since: failure.Since, handle: Handle{
			StableID: failure.StableID, State: speedtest.AgentDiagnosticUnavailable,
			Detail: detail, StartedAt: now, request: request,
		}}
		return
	}
	c.availability[failure.StableID] = availabilityEntry{kind: failure.Kind, since: failure.Since, handle: Handle{
		StableID: failure.StableID, SessionID: view.Session.SessionID, State: speedtest.AgentDiagnosticRunning,
		StartedAt: now, request: request,
	}}
}

// availabilityNeedsProbeLocked decides whether the entry already answers for
// this episode.
//
// A probe in flight answers for any episode: asking again while the first is
// still out would only spend a second agent on the same node. An answered probe
// answers for its own episode, and for a later episode of the same kind while it
// is younger than the cooldown — a node flapping between online and failing
// every few minutes would otherwise take an agent on every flap for an answer
// that has not had time to change. It does not answer for an episode of another
// kind: "the tunnel works from elsewhere" says nothing about a node that has
// since stopped answering TCP.
func (c *Coordinator) availabilityNeedsProbeLocked(existing availabilityEntry, kind string, since time.Time, now time.Time) bool {
	if existing.handle.SessionID == "" {
		return true
	}
	answered := existing.final != nil
	if view, ok := c.controller.Session(existing.handle.SessionID); ok {
		if !view.Session.State.Terminal() {
			return false
		}
		answered = len(view.Session.AgentObservations) > 0
	}
	if !answered {
		return true
	}
	if existing.kind != kind {
		return true
	}
	if existing.since.Equal(since) {
		return false
	}
	return now.Sub(existing.handle.StartedAt) >= c.config.Cooldown
}

// pruneAvailabilityLocked releases the entries of nodes that are no longer
// failing. An entry is kept while its session is still out, so a speed run
// keeps waiting for it, and while it is younger than the cooldown, so a node
// that comes straight back reuses the answer instead of asking for a new one.
//
// An episode that ends without any probe having answered is journaled as such:
// "nobody could look" is a fact about the fleet of agents worth counting.
func (c *Coordinator) pruneAvailabilityLocked(now time.Time, current map[string]bool) []verdictlog.Entry {
	var records []verdictlog.Entry
	for stableID, existing := range c.availability {
		if current[stableID] {
			continue
		}
		if existing.handle.SessionID != "" &&
			(c.inFlightLocked(existing.handle) || now.Sub(existing.handle.StartedAt) < c.config.Cooldown) {
			continue
		}
		if existing.handle.SessionID == "" && !existing.journaled {
			records = append(records, c.availabilityRecord(existing, speedtest.AgentDiagnostic{
				State: speedtest.AgentDiagnosticUnavailable, Detail: existing.handle.Detail,
			}, nil, now))
		}
		delete(c.availability, stableID)
	}
	return records
}

// harvestAvailabilityLocked journals every availability probe that has
// answered and has not been journaled yet. It runs after every check, so a
// verdict reaches the journal whether or not an alert ever asked for it.
func (c *Coordinator) harvestAvailabilityLocked() []verdictlog.Entry {
	var records []verdictlog.Entry
	now := c.config.Now().UTC()
	for stableID, existing := range c.availability {
		if existing.journaled || existing.handle.SessionID == "" {
			continue
		}
		annotation, view, final := c.finalAvailabilityLocked(existing)
		if !final {
			continue
		}
		if annotation.Observation != nil && existing.final == nil {
			copyValue := annotation
			existing.final = &copyValue
		}
		existing.journaled = true
		c.availability[stableID] = existing
		records = append(records, c.availabilityRecord(existing, annotation, view, now))
	}
	return records
}

// finalAvailabilityLocked reads an entry's answer and reports whether it is
// final: an observation, or a session that ended without one.
func (c *Coordinator) finalAvailabilityLocked(existing availabilityEntry) (speedtest.AgentDiagnostic, *remoteprobe.SessionView, bool) {
	if existing.final != nil {
		return *existing.final, nil, true
	}
	view, ok := c.controller.Session(existing.handle.SessionID)
	if !ok {
		return speedtest.AgentDiagnostic{}, nil, false
	}
	annotation := c.annotation(existing.handle)
	if annotation.State == speedtest.AgentDiagnosticRunning {
		return annotation, &view, false
	}
	return annotation, &view, true
}

func (c *Coordinator) availabilityRecord(existing availabilityEntry, annotation speedtest.AgentDiagnostic, view *remoteprobe.SessionView, now time.Time) verdictlog.Entry {
	trigger := string(diagnostics.TriggerAutoProxyFailure)
	if existing.kind == diagnostics.AutomationKindOffline {
		trigger = string(diagnostics.TriggerAutoOffline)
	}
	record := verdictlog.Entry{
		At: now, Source: verdictlog.SourceAvailability, Trigger: trigger, Kind: existing.kind,
		StableID: existing.handle.StableID, SessionID: existing.handle.SessionID,
		AgentID: annotation.AgentID, AgentName: annotation.AgentName, Region: annotation.Region, Provider: annotation.Provider,
		Verdict: annotation.State, Detail: annotation.Detail,
		LocalStatus: existing.kind, LocalAt: existing.since,
	}
	if c.config.NodeName != nil {
		record.Node = c.config.NodeName(existing.handle.StableID)
	}
	if view != nil {
		local := view.Session.LocalResultSnapshot
		if local.Status != "" {
			record.LocalStatus = string(local.Status)
		}
		record.LocalFailureCode = local.Failure.Code
	}
	if observation := annotation.Observation; observation != nil {
		record.AgentStatus = observation.Status
		record.AgentFailureCode = observation.Failure.Code
		record.AgentFailureStage = observation.Failure.Stage
		record.AgentLatencyMs = observation.LatencyMillis
		record.AgentAt = observation.CheckedAt
		if observation.TCP.Checked {
			reached := observation.TCP.Online
			record.AgentTCPReached = &reached
		}
	}
	return record
}

// journal writes outside the coordinator's lock: the append touches the disk,
// and the lock serves every agent's job poll.
func (c *Coordinator) journal(records []verdictlog.Entry) {
	if c.config.Journal == nil {
		return
	}
	for _, record := range records {
		if err := c.config.Journal.Record(record); err != nil {
			logger.Warn("Failed to journal the agent verdict for %s: %v", record.StableID, err)
		}
	}
}

// AvailabilityAnnotations reports what the agents found for the given nodes. A
// node with no entry is left out, which is how an alert tells "nobody was
// asked" apart from "the agent is still working".
func (c *Coordinator) AvailabilityAnnotations(stableIDs []string) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(stableIDs) == 0 {
		return nil
	}
	now := c.config.Now().UTC()
	var records []verdictlog.Entry
	c.mu.Lock()
	result := make(map[string]speedtest.AgentDiagnostic, len(stableIDs))
	for _, stableID := range stableIDs {
		stableID = strings.TrimSpace(stableID)
		existing, ok := c.availability[stableID]
		if !ok {
			continue
		}
		if existing.final != nil {
			result[stableID] = *existing.final
			continue
		}
		// Safe under the lock for the same reason startSpeed is: the controller
		// and the agent source never call back into the coordinator.
		annotation := c.annotation(existing.handle)
		result[stableID] = annotation
		if annotation.Observation != nil {
			copyValue := annotation
			existing.final = &copyValue
			if !existing.journaled {
				existing.journaled = true
				var view *remoteprobe.SessionView
				if session, found := c.controller.Session(existing.handle.SessionID); found {
					view = &session
				}
				records = append(records, c.availabilityRecord(existing, annotation, view, now))
			}
			c.availability[stableID] = existing
		}
	}
	c.mu.Unlock()
	c.journal(records)
	if len(result) == 0 {
		return nil
	}
	return result
}

// AwaitAvailability holds a down alert until every probe for the given nodes has
// answered or ctx runs out. Normally there is nothing to wait for: the probe
// started a full check interval before the alert became due. It matters when
// the alert threshold is one check away from the probe's start, or an agent was
// slow to claim the job.
func (c *Coordinator) AwaitAvailability(ctx context.Context, stableIDs []string) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(stableIDs) == 0 {
		return nil
	}
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		annotations := c.AvailabilityAnnotations(stableIDs)
		running := false
		for _, annotation := range annotations {
			if annotation.State == speedtest.AgentDiagnosticRunning {
				running = true
				break
			}
		}
		if !running {
			return annotations
		}
		select {
		case <-ctx.Done():
			return annotations
		case <-ticker.C:
		}
	}
}
