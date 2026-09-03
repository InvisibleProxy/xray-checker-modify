package telegram

import (
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

func downAlertFor(name string, code string) nodeDownAlert {
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Server:   name + ".example.com",
		Port:     443,
		Name:     name,
		UUID:     "uuid-" + name,
	}
	proxy.StableID = proxy.GenerateStableID()
	return nodeDownAlert{
		Proxy: proxy,
		State: nodeAlertState{
			WasDown:   true,
			Status:    checker.AvailabilityStateOffline,
			DownSince: time.Now().Add(-time.Hour),
			Failure:   checker.FailureDetails{Code: code},
		},
	}
}

// A paused node cannot fail, so counting it only raises the bar for
// correlation. Three failures out of five monitored nodes are a mass incident
// even when five more nodes are in maintenance.
func TestMaintenanceNodesAreNotCountedInTheMassIncidentDenominator(t *testing.T) {
	alerts := []nodeDownAlert{
		downAlertFor("a", checker.FailureCodeDNS),
		downAlertFor("b", checker.FailureCodeDNS),
		downAlertFor("c", checker.FailureCodeDNS),
	}
	proxies := make([]*models.ProxyConfig, 0, 10)
	monitored := make(map[string]bool)
	for _, alert := range alerts {
		proxies = append(proxies, alert.Proxy)
		monitored[alert.Proxy.StableID] = true
	}
	for _, name := range []string{"d", "e"} {
		healthy := downAlertFor(name, "").Proxy
		proxies = append(proxies, healthy)
		monitored[healthy.StableID] = true
	}
	// Five more nodes, all paused for maintenance.
	for _, name := range []string{"m1", "m2", "m3", "m4", "m5"} {
		proxies = append(proxies, downAlertFor(name, "").Proxy)
	}

	groups, remaining := partitionMassNodeDownAlerts(alerts, proxies, monitored, nil)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want one mass incident over the five monitored nodes", len(groups))
	}
	if groups[0].Total != 5 {
		t.Fatalf("denominator = %d, want only the monitored nodes", groups[0].Total)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %d, want every alert folded into the incident", len(remaining))
	}

	// Without the monitoring filter the same failures fall short of 50% of ten
	// and each node alerts on its own — the behaviour this test pins down.
	groups, remaining = partitionMassNodeDownAlerts(alerts, proxies, nil, nil)
	if len(groups) != 0 || len(remaining) != 3 {
		t.Fatalf("with a ten-node denominator the incident must not form: groups=%d remaining=%d", len(groups), len(remaining))
	}
}

// The breakdown has to add up to what is listed under it: a node skipped
// without a measurement is named separately rather than folded into the
// checked count.
func TestSpeedReportCountsOnlyMeasuredNodes(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	cfg := Config{
		Enabled:               true,
		ChatID:                "1",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "always",
		SpeedReportLimit:      10,
		LowSpeedThresholdMbps: 100,
		TimeoutSec:            5,
	}
	service.setConfig(cfg)

	report := speedtest.RunReport{
		Source:   "schedule",
		Selected: 4,
		Skipped:  2,
		Results: []speedtest.Result{
			{StableID: "a", Name: "A", Mbps: 400},
			{StableID: "b", Name: "B", Mbps: 5},
		},
	}
	failed, slow := countSpeedIssues(report.Results, cfg.LowSpeedThresholdMbps)
	text := service.formatSpeedReport(report, cfg, failed, slow, false)

	if !strings.Contains(text, "Проверено: <b>2</b>") {
		t.Fatalf("report must count only measured nodes:\n%s", text)
	}
	if !strings.Contains(text, "Пропущено без замера: <b>2</b>") {
		t.Fatalf("report must name the skipped nodes:\n%s", text)
	}
}

// A per-node threshold decides how that node's result reads, and the threshold
// stored with the result wins over whatever the global setting is now.
func TestResultThresholdOverridesTheGlobalSetting(t *testing.T) {
	slowGlobally := speedtest.Result{StableID: "a", Mbps: 50, LowSpeedThresholdMbps: 10}
	if speedResultClass(slowGlobally, 100) != speedClassHealthy {
		t.Fatal("a node above its own threshold must read as healthy")
	}

	fastGlobally := speedtest.Result{StableID: "b", Mbps: 150, LowSpeedThresholdMbps: 500}
	if speedResultClass(fastGlobally, 100) != speedClassSlow {
		t.Fatal("a node below its own threshold must read as slow")
	}

	// History written before results carried a threshold falls back to the
	// current global one.
	legacy := speedtest.Result{StableID: "c", Mbps: 50}
	if speedResultClass(legacy, 100) != speedClassSlow {
		t.Fatal("a result without a recorded threshold must use the global one")
	}
}

func TestConfirmationRetryUsesThePerNodeThreshold(t *testing.T) {
	results := []speedtest.Result{
		// Slow globally, fine for itself: no retry, no alert.
		{StableID: "tolerant", Mbps: 50, LowSpeedThresholdMbps: 10},
		// Fast globally, slow for itself: this one needs confirming.
		{StableID: "demanding", Mbps: 150, LowSpeedThresholdMbps: 500},
	}
	ids := speedConfirmationRetryIDs(results, 100)
	if len(ids) != 1 || ids[0] != "demanding" {
		t.Fatalf("confirmation retry ids = %v, want only the node below its own threshold", ids)
	}

	successful := successfulSpeedResultIDs(results, 100)
	if len(successful) != 1 || successful[0] != "tolerant" {
		t.Fatalf("successful ids = %v, want only the node above its own threshold", successful)
	}
}
