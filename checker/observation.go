package checker

import (
	"strings"

	"xray-checker/models"
	"xray-checker/observation"
)

// SetSourcePolicies replaces the observation policy of every panel-added
// source. A source missing from the map observes everything, which is how an
// environment subscription — it carries no source id at all — always behaves.
func (pc *ProxyChecker) SetSourcePolicies(policies map[string]observation.Policy) {
	copied := make(map[string]observation.Policy, len(policies))
	for sourceID, policy := range policies {
		sourceID = strings.TrimSpace(sourceID)
		if sourceID == "" {
			continue
		}
		copied[sourceID] = policy
	}
	pc.sourcePolicyMu.Lock()
	pc.sourcePolicies = copied
	pc.sourcePolicyMu.Unlock()
}

// ObservationPolicyFor resolves the policy of the source a node came from.
func (pc *ProxyChecker) ObservationPolicyFor(stableID string) observation.Policy {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return observation.Full()
	}
	pc.sourcePolicyMu.RLock()
	defer pc.sourcePolicyMu.RUnlock()
	if len(pc.sourcePolicies) == 0 {
		return observation.Full()
	}
	sourceID, ok := pc.sourceByStableID[stableID]
	if !ok || sourceID == "" {
		return observation.Full()
	}
	policy, ok := pc.sourcePolicies[sourceID]
	if !ok {
		return observation.Full()
	}
	return policy
}

// SpeedTestEnabled reports whether a scheduled speed test may select the node.
// A node an operator paused by hand is out whatever its source says: the
// explicit decision outranks the preset.
func (pc *ProxyChecker) SpeedTestEnabled(stableID string) bool {
	if !pc.MonitoringEnabled(stableID) {
		return false
	}
	return pc.ObservationPolicyFor(stableID).SpeedTest
}

// AvailabilityAccounted reports whether a probe of this node becomes a verdict.
// When it does not, the probe still runs and its result is still visible to the
// operator; it just carries no downtime, incident, metric or alert.
func (pc *ProxyChecker) AvailabilityAccounted(stableID string) bool {
	if !pc.MonitoringEnabled(stableID) {
		return false
	}
	return pc.ObservationPolicyFor(stableID).AccountAvailability
}

// AlertsEnabled reports whether Telegram may speak about the node.
func (pc *ProxyChecker) AlertsEnabled(stableID string) bool {
	return pc.ObservationPolicyFor(stableID).Alerts
}

// ListedPublicly reports whether the node belongs on the public dashboard, in
// Prometheus and behind its own /config endpoint.
func (pc *ProxyChecker) ListedPublicly(stableID string) bool {
	return pc.ObservationPolicyFor(stableID).Listed
}

func (pc *ProxyChecker) rebuildSourceIndex(proxies []*models.ProxyConfig) {
	index := make(map[string]string, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		stableID := proxy.StableID
		if stableID == "" {
			stableID = proxy.GenerateStableID()
		}
		if stableID == "" || proxy.SourceID == "" {
			continue
		}
		index[stableID] = proxy.SourceID
	}
	pc.sourcePolicyMu.Lock()
	pc.sourceByStableID = index
	pc.sourcePolicyMu.Unlock()
}
