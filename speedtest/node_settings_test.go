package speedtest

import (
	"path/filepath"
	"testing"

	"xray-checker/checker"
	"xray-checker/models"
)

func nodeSettingsManager(t *testing.T) (*Manager, *models.ProxyConfig) {
	t.Helper()
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		Name:     "Node",
		UUID:     "uuid",
	}
	proxy.StableID = proxy.GenerateStableID()
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")

	manager := NewManager(proxyChecker, 10000, filepath.Join(t.TempDir(), "speedtest_schedule.json"), TestConfig{
		URL:         "https://global.example/file",
		MaxBytes:    int64(50 * 1024 * 1024),
		TimeoutSec:  60,
		Concurrency: 2,
	})
	if err := manager.Load(); err != nil {
		t.Fatalf("load manager: %v", err)
	}
	return manager, proxy
}

func TestPerNodeDownloadSizeOverridesTheGlobalOne(t *testing.T) {
	manager, proxy := nodeSettingsManager(t)
	size := int64(4 * 1024 * 1024)

	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{MaxBytes: &size}); err != nil {
		t.Fatalf("save node settings: %v", err)
	}

	cfg := manager.configForProxy(manager.normalizeConfig(TestConfig{}), proxy)
	if cfg.MaxBytes != size {
		t.Fatalf("maxBytes = %d, want the node override %d", cfg.MaxBytes, size)
	}

	// Clearing it puts the node back on the global size rather than on zero.
	cleared := int64(0)
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{MaxBytes: &cleared}); err != nil {
		t.Fatalf("clear node settings: %v", err)
	}
	cfg = manager.configForProxy(manager.normalizeConfig(TestConfig{}), proxy)
	if cfg.MaxBytes != int64(50*1024*1024) {
		t.Fatalf("maxBytes after clearing = %d, want the global size", cfg.MaxBytes)
	}
}

func TestPerNodeThresholdDecidesWhatIsSlowForThatNode(t *testing.T) {
	manager, proxy := nodeSettingsManager(t)
	manager.SetLowSpeedThresholdMbps(100)

	if got := manager.LowSpeedThresholdFor(proxy.StableID); got != 100 {
		t.Fatalf("threshold without an override = %v, want the global 100", got)
	}

	nodeThreshold := float64(10)
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{LowSpeedThresholdMbps: &nodeThreshold}); err != nil {
		t.Fatalf("save node settings: %v", err)
	}
	if got := manager.LowSpeedThresholdFor(proxy.StableID); got != 10 {
		t.Fatalf("threshold = %v, want the node override 10", got)
	}
	if got := manager.LowSpeedThresholdFor("another-node"); got != 100 {
		t.Fatalf("another node's threshold = %v, want the global 100", got)
	}

	// 50 Mbps is slow globally and fine for this node, which is the whole point
	// of the override: no fallback, no alert.
	result := Result{StableID: proxy.StableID, Mbps: 50}
	if shouldAttemptFallback(result, manager.LowSpeedThresholdFor(result.StableID)) {
		t.Fatal("a node above its own threshold must not trigger country fallback")
	}
	if !shouldAttemptFallback(result, manager.fallbackLowSpeedThreshold()) {
		t.Fatal("the same speed is still slow against the global threshold; the test fixture is wrong")
	}
}

func TestNodeSettingsSurviveAScheduleSaveThatOmitsThem(t *testing.T) {
	manager, proxy := nodeSettingsManager(t)
	size := int64(2 * 1024 * 1024)
	threshold := float64(25)
	testURL := "https://node.example/file"
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{
		TestURL:               &testURL,
		MaxBytes:              &size,
		LowSpeedThresholdMbps: &threshold,
	}); err != nil {
		t.Fatalf("save node settings: %v", err)
	}

	// The schedule form does not send per-node overrides; saving it must not
	// wipe them.
	if err := manager.UpdateSchedule(ScheduleConfig{Enabled: true, IntervalSec: 3600}); err != nil {
		t.Fatalf("save schedule: %v", err)
	}

	snapshot := manager.Snapshot()
	if snapshot.NodeMaxBytes[proxy.StableID] != size {
		t.Fatalf("download size lost after a schedule save: %+v", snapshot.NodeMaxBytes)
	}
	if snapshot.NodeLowSpeedThresholds[proxy.StableID] != threshold {
		t.Fatalf("threshold lost after a schedule save: %+v", snapshot.NodeLowSpeedThresholds)
	}
	if snapshot.NodeTestURLs[proxy.StableID] != testURL {
		t.Fatalf("test URL lost after a schedule save: %+v", snapshot.NodeTestURLs)
	}
}

func TestNodeSettingsSurviveRestart(t *testing.T) {
	manager, proxy := nodeSettingsManager(t)
	size := int64(8 * 1024 * 1024)
	threshold := float64(15)
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{MaxBytes: &size, LowSpeedThresholdMbps: &threshold}); err != nil {
		t.Fatalf("save node settings: %v", err)
	}

	restored := NewManager(manager.proxyChecker, 10000, manager.statePath, manager.defaults)
	if err := restored.Load(); err != nil {
		t.Fatalf("reload manager: %v", err)
	}
	if got := restored.LowSpeedThresholdFor(proxy.StableID); got != threshold {
		t.Fatalf("threshold after restart = %v, want %v", got, threshold)
	}
	cfg := restored.configForProxy(restored.normalizeConfig(TestConfig{}), proxy)
	if cfg.MaxBytes != size {
		t.Fatalf("download size after restart = %d, want %d", cfg.MaxBytes, size)
	}
}

func TestNodeSettingsRejectOutOfRangeValues(t *testing.T) {
	manager, proxy := nodeSettingsManager(t)

	tooSmall := int64(1024)
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{MaxBytes: &tooSmall}); err == nil {
		t.Fatal("a download size below the floor must be rejected")
	}
	tooBig := maxBytesLimit + 1
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{MaxBytes: &tooBig}); err == nil {
		t.Fatal("a download size above the global ceiling must be rejected")
	}
	negative := float64(-1)
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{LowSpeedThresholdMbps: &negative}); err == nil {
		t.Fatal("a negative threshold must be rejected")
	}
	if err := manager.UpdateNodeSettings("unknown-node", NodeSettings{MaxBytes: &tooSmall}); err == nil {
		t.Fatal("settings for a node that does not exist must be rejected")
	}
}

func TestResultCarriesTheThresholdItWasJudgedAgainst(t *testing.T) {
	manager, proxy := nodeSettingsManager(t)
	manager.SetLowSpeedThresholdMbps(100)
	nodeThreshold := float64(10)
	if err := manager.UpdateNodeSettings(proxy.StableID, NodeSettings{LowSpeedThresholdMbps: &nodeThreshold}); err != nil {
		t.Fatalf("save node settings: %v", err)
	}
	manager.testAttempt = func(p *models.ProxyConfig, _ TestConfig, _ string) Result {
		return Result{StableID: p.StableID, Mbps: 50}
	}

	result := manager.executeTestAttempt(proxy, TestConfig{}, ManualSource)
	// Stamped at measurement time, so a later change to the global setting
	// cannot reclassify this result.
	if result.LowSpeedThresholdMbps != nodeThreshold {
		t.Fatalf("recorded threshold = %v, want the node override %v", result.LowSpeedThresholdMbps, nodeThreshold)
	}
}
