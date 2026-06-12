package checker

import (
	"testing"
	"time"

	"xray-checker/models"
)

func TestGetProxyStatusByStableIDFallsBackToStatusDetails(t *testing.T) {
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Name:     "NL",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "00000000-0000-0000-0000-000000000001",
	}
	proxy.StableID = proxy.GenerateStableID()

	proxyChecker := NewProxyChecker(
		[]*models.ProxyConfig{proxy},
		10000,
		"https://example.com/ip",
		30,
		"https://example.com/status",
		"https://example.com/file",
		60,
		51200,
		"ip",
	)
	proxyChecker.storeStatusDetails(proxy.StableID, true, 123*time.Millisecond, nil, nil)

	online, latency, err := proxyChecker.GetProxyStatusByStableID(proxy.StableID)
	if err != nil {
		t.Fatalf("expected status fallback, got error: %v", err)
	}
	if !online {
		t.Fatal("expected proxy to be online")
	}
	if latency != 123*time.Millisecond {
		t.Fatalf("expected latency %s, got %s", 123*time.Millisecond, latency)
	}
}
