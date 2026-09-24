package agentautomation

import (
	"context"
	"sort"
	"strings"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/remoteprobe"
	"xray-checker/speedtest"
)

// ProxyFailure is one node the availability check currently reports as a proxy
// failure: its host answers TCP or ping, but traffic does not get through the
// tunnel. Since is when the current episode began, which is what tells a node
// that is still failing apart from one that failed, recovered and failed again.
type ProxyFailure struct {
	StableID string
	Since    time.Time
}

// proxyFailureEntry is the probe that answers for one node's proxy failure.
type proxyFailureEntry struct {
	// since is the episode the probe was started for.
	since  time.Time
	handle Handle
	// final is the verdict of a session that answered. It is kept because the
	// session is not: the manager evicts old sessions, and an episode that
	// lasts a night outlives the one probe it gets. A reminder sent in the
	// morning should still say what the agent found, not that the result is
	// gone.
	final *speedtest.AgentDiagnostic
}

// ProxyFailureEnabled reports whether the availability trigger is switched on.
func (c *Coordinator) ProxyFailureEnabled() bool {
	return c.Enabled() && c.config.ProxyFailureEnabled
}

// StartProxyFailureDiagnostics is called after every completed availability
// check with every node that check left in proxy_failure — the whole list, not
// only the nodes that changed. A node missing from it has recovered or gone
// fully offline, and its entry is released once nothing depends on it.
//
// Each episode gets one probe. A node that stays in proxy_failure for hours is
// not probed again: the first answer is what separates "the node is broken"
// from "the checker's route is broken", and repeating it every cooldown would
// hold agents a speed run may need for nothing new. A start that never produced
// an observation — no free agent, a job nobody claimed — is not an answer, and
// the next check asks again.
//
// The call is where the slot is taken, not where the answer is awaited: the
// first down alert goes out no earlier than the second failed check, one full
// check interval later, and an agent answers well inside that.
func (c *Coordinator) StartProxyFailureDiagnostics(failures []ProxyFailure) {
	if !c.ProxyFailureEnabled() {
		return
	}
	candidates, current := proxyFailureCandidates(failures, c.config.EnvironmentSourced)
	now := c.config.Now().UTC()
	c.mu.Lock()
	c.pruneProxyFailuresLocked(now, current)
	c.mu.Unlock()
	for _, candidate := range candidates {
		c.startProxyFailure(candidate, now)
	}
}

// proxyFailureCandidates orders the episodes worth an agent, the oldest first,
// and lists every node still failing so entries of recovered nodes can go. A
// node from a panel-added subscription is failing all the same but is never a
// candidate: see Config.EnvironmentSourced.
func proxyFailureCandidates(failures []ProxyFailure, environmentSourced func(string) bool) ([]ProxyFailure, map[string]bool) {
	candidates := make([]ProxyFailure, 0, len(failures))
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

func (c *Coordinator) startProxyFailure(failure ProxyFailure, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.proxyFailures[failure.StableID]; ok && !c.proxyFailureNeedsProbeLocked(existing, failure.Since, now) {
		return
	}
	request := startRequest{
		stableID: failure.StableID, kind: diagnostics.AutomationKindProxyFailure,
		profileID: c.config.ProxyFailureProfileID, source: diagnostics.AutomationSourceAvailability,
	}
	if c.activeLocked() >= c.config.MaxConcurrent {
		c.proxyFailures[failure.StableID] = proxyFailureEntry{since: failure.Since, handle: Handle{
			StableID: failure.StableID, State: speedtest.AgentDiagnosticUnavailable,
			Detail: "automation capacity is busy", StartedAt: now, request: request,
		}}
		return
	}
	view, err := c.controller.CreateAutomatic(remoteprobe.CreateAutomaticRequest{
		StableID:          failure.StableID,
		Trigger:           diagnostics.TriggerAutoProxyFailure,
		ProfileID:         c.config.ProxyFailureProfileID,
		AutomationContext: diagnostics.ProxyFailureAutomationContext(),
	})
	if err != nil {
		// Every refusal is retried at the next check, transient or not, and none
		// of them occupies the episode: no session started, so there is no
		// evidence to protect from being repeated.
		detail, _ := refusalDetail(err)
		c.proxyFailures[failure.StableID] = proxyFailureEntry{since: failure.Since, handle: Handle{
			StableID: failure.StableID, State: speedtest.AgentDiagnosticUnavailable,
			Detail: detail, StartedAt: now, request: request,
		}}
		return
	}
	c.proxyFailures[failure.StableID] = proxyFailureEntry{since: failure.Since, handle: Handle{
		StableID: failure.StableID, SessionID: view.Session.SessionID, State: speedtest.AgentDiagnosticRunning,
		StartedAt: now, request: request,
	}}
}

// proxyFailureNeedsProbeLocked decides whether the entry already answers for
// this episode.
//
// A probe in flight answers for any episode: asking again while the first is
// still out would only spend a second agent on the same question. An answered
// probe answers for its own episode, and for a later one while it is younger
// than the cooldown — a node flapping between online and proxy_failure every
// few minutes would otherwise take an agent on every flap for an answer that
// has not had time to change.
func (c *Coordinator) proxyFailureNeedsProbeLocked(existing proxyFailureEntry, since time.Time, now time.Time) bool {
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
	if existing.since.Equal(since) {
		return false
	}
	return now.Sub(existing.handle.StartedAt) >= c.config.Cooldown
}

// pruneProxyFailuresLocked releases the entries of nodes that are no longer
// failing. An entry is kept while its session is still out, so a speed run
// keeps waiting for it, and while it is younger than the cooldown, so a node
// that comes straight back into proxy_failure reuses the answer instead of
// asking for a new one.
func (c *Coordinator) pruneProxyFailuresLocked(now time.Time, current map[string]bool) {
	for stableID, existing := range c.proxyFailures {
		if current[stableID] {
			continue
		}
		if existing.handle.SessionID != "" &&
			(c.inFlightLocked(existing.handle) || now.Sub(existing.handle.StartedAt) < c.config.Cooldown) {
			continue
		}
		delete(c.proxyFailures, stableID)
	}
}

// ProxyFailureAnnotations reports what the agents found for the given nodes. A
// node with no entry is left out, which is how an alert tells "nobody was
// asked" apart from "the agent is still working".
func (c *Coordinator) ProxyFailureAnnotations(stableIDs []string) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(stableIDs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]speedtest.AgentDiagnostic, len(stableIDs))
	for _, stableID := range stableIDs {
		stableID = strings.TrimSpace(stableID)
		existing, ok := c.proxyFailures[stableID]
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
			c.proxyFailures[stableID] = existing
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// AwaitProxyFailure holds a down alert until every probe for the given nodes
// has answered or ctx runs out. Normally there is nothing to wait for: the
// probe started a full check interval before the alert became due. It matters
// when the alert threshold is one check away from the probe's start, or an
// agent was slow to claim the job.
func (c *Coordinator) AwaitProxyFailure(ctx context.Context, stableIDs []string) map[string]speedtest.AgentDiagnostic {
	if c == nil || len(stableIDs) == 0 {
		return nil
	}
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		annotations := c.ProxyFailureAnnotations(stableIDs)
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
