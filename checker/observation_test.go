package checker

import (
	"testing"

	"xray-checker/models"
	"xray-checker/observation"
)

func sourcedProxy(stableID string, sourceID string) *models.ProxyConfig {
	proxy := testProxy(stableID, stableID)
	proxy.SourceID = sourceID
	return proxy
}

// A node inherits how it is watched from the source that produced it, and a
// node from the environment — it carries no source id — is watched in full.
func TestPolicyReachesNodesThroughTheirSource(t *testing.T) {
	own := sourcedProxy("own", "")
	quiet := sourcedProxy("quiet", "src-quiet")
	paused := sourcedProxy("paused", "src-paused")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{own, quiet, paused})
	proxyChecker.SetSourcePolicies(map[string]observation.Policy{
		"src-quiet":  observation.PolicyFor(observation.ModeAvailability, true, true),
		"src-paused": observation.PolicyFor(observation.ModePaused, false, false),
	})

	if got := proxyChecker.ObservationPolicyFor(own.StableID); got != observation.Full() {
		t.Fatalf("environment node policy = %+v, want the full one", got)
	}
	if !proxyChecker.SpeedTestEnabled(own.StableID) || !proxyChecker.AlertsEnabled(own.StableID) || !proxyChecker.ListedPublicly(own.StableID) {
		t.Fatal("environment node lost part of the full policy")
	}

	if proxyChecker.SpeedTestEnabled(quiet.StableID) {
		t.Fatal("an availability-only source was selected for a scheduled speed test")
	}
	if !proxyChecker.AvailabilityAccounted(quiet.StableID) {
		t.Fatal("an availability-only source stopped being accounted for")
	}
	if proxyChecker.AlertsEnabled(quiet.StableID) || proxyChecker.ListedPublicly(quiet.StableID) {
		t.Fatal("a silent, unlisted source still alerts or publishes")
	}

	if proxyChecker.AvailabilityAccounted(paused.StableID) || proxyChecker.SpeedTestEnabled(paused.StableID) {
		t.Fatal("a paused source is still measured")
	}
	if !proxyChecker.AlertsEnabled(paused.StableID) || !proxyChecker.ListedPublicly(paused.StableID) {
		t.Fatal("pausing a source silently changed its unrelated switches")
	}
}

// The operator's own decision about one node outranks the preset its source
// carries: pausing a node has to hold even inside a fully watched source.
func TestNodeMaintenanceOutranksTheSourcePolicy(t *testing.T) {
	proxy := sourcedProxy("node-1", "src-full")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyChecker.SetSourcePolicies(map[string]observation.Policy{"src-full": observation.Full()})
	if err := proxyChecker.SetMaintenanceMode(proxy.StableID, true); err != nil {
		t.Fatal(err)
	}

	if proxyChecker.SpeedTestEnabled(proxy.StableID) || proxyChecker.AvailabilityAccounted(proxy.StableID) {
		t.Fatal("a paused node was measured because its source allows it")
	}
}

// A refresh replaces the node set, so the map from node to source has to be
// rebuilt with it — otherwise a node keeps the policy of a source it left.
func TestSourceIndexFollowsARefresh(t *testing.T) {
	before := sourcedProxy("node-1", "src-quiet")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{before})
	proxyChecker.SetSourcePolicies(map[string]observation.Policy{
		"src-quiet": observation.PolicyFor(observation.ModeAvailability, false, false),
	})
	if proxyChecker.SpeedTestEnabled(before.StableID) {
		t.Fatal("the availability-only policy did not reach the node")
	}

	proxyChecker.UpdateProxies([]*models.ProxyConfig{sourcedProxy("node-1", "")})
	if !proxyChecker.SpeedTestEnabled("node-1") {
		t.Fatal("the node kept the policy of a source it no longer belongs to")
	}
}
