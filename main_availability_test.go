package main

import (
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

func TestUnavailableStableIDSetIncludesProxyFailure(t *testing.T) {
	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "Node one",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "node-1-uuid",
	}
	proxyChecker := checker.NewProxyChecker(
		[]*models.ProxyConfig{proxy},
		10000,
		"",
		1,
		"",
		"",
		1,
		0,
		"status",
	)
	if !proxyChecker.RestoreProxyFailureStatus(
		proxy.StableID,
		time.Now().Add(-time.Minute),
		checker.HostCheckDetails{Checked: true, Online: true},
		checker.PingCheckDetails{},
	) {
		t.Fatal("failed to seed proxy-failure status")
	}

	if !unavailableStableIDSet(proxyChecker)[proxy.StableID] {
		t.Fatal("proxy_failure was omitted from the full-check recovery candidates")
	}
}
