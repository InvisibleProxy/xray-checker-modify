package checker

import (
	"testing"

	"xray-checker/models"
)

// The label is what people read; the subscription's name is what the checker
// keeps. Both have to stay reachable, and applying a label must not touch the
// shared proxy list every other reader shares.
func TestLabelPrefersTheOperatorNameWithoutRewritingTheSource(t *testing.T) {
	proxy := testProxy("node-1", "proxy-01-nl-edge-host-01")
	plain := testProxy("node-2", "proxy-02-nl-edge-host-02")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy, plain})

	if got := proxyChecker.Label(proxy); got != proxy.Name {
		t.Fatalf("label without an override = %q, want the subscription name", got)
	}

	proxyChecker.ApplyDisplayNames(map[string]string{proxy.StableID: "  Нидерланды · узел 1  "})
	if got := proxyChecker.DisplayName(proxy.StableID); got != "Нидерланды · узел 1" {
		t.Fatalf("display name = %q, want it trimmed", got)
	}
	if got := proxyChecker.Label(proxy); got != "Нидерланды · узел 1" {
		t.Fatalf("label = %q, want the operator's name", got)
	}
	if proxy.Name != "proxy-01-nl-edge-host-01" {
		t.Fatalf("the shared proxy was rewritten: %q", proxy.Name)
	}
	if got := proxyChecker.Label(plain); got != plain.Name {
		t.Fatalf("an unlabelled node = %q, want its subscription name", got)
	}

	// Replacing the set is how a refresh applies them, so an absent id clears.
	proxyChecker.ApplyDisplayNames(map[string]string{plain.StableID: "Узел 2"})
	if got := proxyChecker.DisplayName(proxy.StableID); got != "" {
		t.Fatalf("stale label survived a replacement: %q", got)
	}
	if got := proxyChecker.Label(plain); got != "Узел 2" {
		t.Fatalf("label = %q, want the newly applied one", got)
	}
}

func TestEmptyLabelsAreIgnored(t *testing.T) {
	proxy := testProxy("node-1", "Node one")
	proxyChecker := newTestProxyChecker([]*models.ProxyConfig{proxy})
	proxyChecker.ApplyDisplayNames(map[string]string{proxy.StableID: "   ", "": "orphan"})
	if got := proxyChecker.Label(proxy); got != "Node one" {
		t.Fatalf("label = %q, want the subscription name", got)
	}
	if proxyChecker.Label(nil) != "" {
		t.Fatal("a missing proxy has no label")
	}
}
