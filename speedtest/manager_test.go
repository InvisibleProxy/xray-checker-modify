package speedtest

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

func TestLoadResultsKeepsInactiveHistory(t *testing.T) {
	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	resultPath := filepath.Join(dir, "speedtest_results.json")
	checkedAt := time.Now().UTC().Add(-time.Hour)
	state := resultStateFile{
		Version:   1,
		UpdatedAt: checkedAt,
		Results: map[string]Result{
			"inactive": {
				StableID:  "inactive",
				Name:      "Retired NL",
				Mbps:      50,
				CheckedAt: checkedAt,
			},
		},
		History: map[string][]Result{
			"inactive": {
				{
					StableID:  "inactive",
					Name:      "Retired NL",
					Mbps:      50,
					CheckedAt: checkedAt,
				},
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, schedulePath, TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}

	history := manager.ResultHistory("inactive")
	if len(history) != 1 {
		t.Fatalf("history length = %d, want 1", len(history))
	}
	if history[0].Name != "Retired NL" {
		t.Fatalf("history name = %q, want Retired NL", history[0].Name)
	}
}

func TestDeleteHistoryRemovesLatestAndHistory(t *testing.T) {
	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, schedulePath, TestConfig{})
	checkedAt := time.Now().UTC().Add(-time.Hour)
	manager.results["retired"] = Result{StableID: "retired", Name: "Retired", Mbps: 50, CheckedAt: checkedAt}
	manager.history["retired"] = []Result{{StableID: "retired", Name: "Retired", Mbps: 50, CheckedAt: checkedAt}}

	if err := manager.DeleteHistory("retired"); err != nil {
		t.Fatal(err)
	}
	if len(manager.ResultHistory("retired")) != 0 {
		t.Fatal("history was not deleted")
	}
	snapshot := manager.Snapshot()
	for _, result := range snapshot.Results {
		if result.StableID == "retired" {
			t.Fatal("latest result was not deleted")
		}
	}

	reloaded := NewManager(proxyChecker, 10000, schedulePath, TestConfig{})
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ResultHistory("retired")) != 0 {
		t.Fatal("deleted history was restored from disk")
	}
}

func TestDeleteHistoryRollsBackMemoryWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	manager.resultPath = filepath.Join(blocker, "speedtest_results.json")
	checkedAt := time.Now().UTC()
	manager.results["retired"] = Result{StableID: "retired", Name: "Retired", Mbps: 50, CheckedAt: checkedAt}
	manager.history["retired"] = []Result{{StableID: "retired", Name: "Retired", Mbps: 50, CheckedAt: checkedAt}}
	manager.results["keep"] = Result{StableID: "keep", Name: "Keep", Mbps: 60, CheckedAt: checkedAt}
	manager.history["keep"] = []Result{{StableID: "keep", Name: "Keep", Mbps: 60, CheckedAt: checkedAt}}

	if err := manager.DeleteHistory("retired"); err == nil {
		t.Fatal("DeleteHistory() succeeded despite persistence failure")
	}
	if got := manager.ResultHistory("retired"); len(got) != 1 || got[0].Name != "Retired" {
		t.Fatalf("retired history was not restored after persistence failure: %+v", got)
	}
	latest := manager.Snapshot().Results
	foundRetired := false
	foundKeep := false
	for _, result := range latest {
		foundRetired = foundRetired || result.StableID == "retired"
		foundKeep = foundKeep || result.StableID == "keep"
	}
	if !foundRetired || !foundKeep {
		t.Fatalf("latest results after rollback = %+v; want retired and keep", latest)
	}
}

func TestRunGateBlocksXrayLifecycleWriterUntilSpeedTestFinishes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "Node 1",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "uuid",
	}
	startPort := listener.Addr().(*net.TCPAddr).Port
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, startPort, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, startPort, "", TestConfig{})
	var lifecycle sync.RWMutex
	manager.SetRunGate(lifecycle.RLocker())
	if err := manager.Run(RunRequest{
		ProxyIDs: []string{proxy.StableID},
		Config: TestConfig{
			URL:         "https://example.com/test.bin",
			MaxBytes:    1024,
			TimeoutSec:  2,
			Concurrency: 1,
		},
	}, "telegram"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var conn net.Conn
	select {
	case conn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(time.Second):
		t.Fatal("speed test did not connect to the test SOCKS endpoint")
	}
	writerAcquired := make(chan struct{})
	go func() {
		lifecycle.Lock()
		close(writerAcquired)
		lifecycle.Unlock()
	}()
	select {
	case <-writerAcquired:
		t.Fatal("Xray lifecycle writer acquired the lock while speed test was running")
	case <-time.After(50 * time.Millisecond):
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close accepted connection: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for manager.Snapshot().LastRun.Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if manager.Snapshot().LastRun.Running {
		t.Fatal("speed test did not finish after the test connection closed")
	}
	select {
	case <-writerAcquired:
	case <-time.After(time.Second):
		t.Fatal("Xray lifecycle writer remained blocked after speed test finished")
	}
}

func TestUpdatedTestURLIsUsedByScheduledConfig(t *testing.T) {
	dir := t.TempDir()
	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "Node 1",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "uuid",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, filepath.Join(dir, "speedtest_schedule.json"), TestConfig{
		URL: "https://old.example.com/test.bin",
	})
	manager.schedule = ScheduleConfig{
		Enabled:     true,
		IntervalSec: 3600,
		Config: TestConfig{
			URL: "https://old.example.com/test.bin",
		},
	}

	if err := manager.updateScheduleTestURL("https://new.example.com/test.bin"); err != nil {
		t.Fatal(err)
	}

	schedule := manager.Schedule()
	if schedule.Config.URL != "https://new.example.com/test.bin" {
		t.Fatalf("scheduled URL = %q, want updated URL", schedule.Config.URL)
	}
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{}, checker.PingCheckDetails{}) {
		t.Fatal("failed to prepare offline proxy state")
	}
	if err := manager.Run(RunRequest{
		ProxyIDs:    []string{proxy.StableID},
		SkipOffline: true,
		Config:      schedule.Config,
	}, "schedule"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for manager.Snapshot().LastRun.Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	results := manager.Snapshot().Results
	if len(results) != 1 || results[0].URL != "https://new.example.com/test.bin" {
		t.Fatalf("scheduled result = %+v, want updated URL", results)
	}

	reloaded := NewManager(proxyChecker, 10000, manager.statePath, TestConfig{})
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if reloaded.Schedule().Config.URL != "https://new.example.com/test.bin" {
		t.Fatalf("reloaded scheduled URL = %q, want updated URL", reloaded.Schedule().Config.URL)
	}
}

func TestNodeTestURLOverridesUpdatedScheduledURL(t *testing.T) {
	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "Node 1",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "uuid",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	manager.schedule = ScheduleConfig{Config: TestConfig{URL: "https://old.example.com/test.bin"}}

	if err := manager.UpdateNodeTestURL(proxy.StableID, "https://node.example.com/current.bin"); err != nil {
		t.Fatal(err)
	}
	if err := manager.updateScheduleTestURL("https://global.example.com/current.bin"); err != nil {
		t.Fatal(err)
	}

	effective := manager.configForProxy(manager.Schedule().Config, proxy)
	if effective.URL != "https://node.example.com/current.bin" {
		t.Fatalf("effective URL = %q, want per-node URL", effective.URL)
	}
}

func TestHistoryRetentionDefaultsToSixtyDaysForLegacySchedule(t *testing.T) {
	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	legacySchedule := []byte(`{"enabled":false,"intervalSec":7200,"historyLimit":1000,"config":{}}`)
	if err := os.WriteFile(schedulePath, legacySchedule, 0644); err != nil {
		t.Fatal(err)
	}

	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, schedulePath, TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}

	if got := manager.Schedule().HistoryRetentionDays; got != defaultHistoryRetentionDays {
		t.Fatalf("history retention = %d days, want %d", got, defaultHistoryRetentionDays)
	}
}

func TestResultHistoryRetainsOnlyConfiguredDays(t *testing.T) {
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	manager.schedule.HistoryRetentionDays = 60
	now := time.Now().UTC()
	manager.history["node-1"] = []Result{
		{StableID: "node-1", CheckedAt: now.Add(-59 * 24 * time.Hour)},
		{StableID: "node-1", CheckedAt: now.Add(-61 * 24 * time.Hour)},
		{StableID: "node-1"},
	}

	history := manager.ResultHistory("node-1")
	if len(history) != 1 {
		t.Fatalf("history length = %d, want 1", len(history))
	}
	if history[0].CheckedAt.Before(now.Add(-60 * 24 * time.Hour)) {
		t.Fatalf("retained result is older than 60 days: %s", history[0].CheckedAt)
	}
}

func TestUpdateSchedulePrunesHistoryOutsideRetention(t *testing.T) {
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	manager.schedule.HistoryRetentionDays = 60
	now := time.Now().UTC()
	manager.history["node-1"] = []Result{
		{StableID: "node-1", CheckedAt: now.Add(-20 * 24 * time.Hour)},
		{StableID: "node-1", CheckedAt: now.Add(-40 * 24 * time.Hour)},
	}

	if err := manager.UpdateSchedule(ScheduleConfig{IntervalSec: 7200, HistoryRetentionDays: 30}); err != nil {
		t.Fatal(err)
	}

	if got := manager.Schedule().HistoryRetentionDays; got != 30 {
		t.Fatalf("history retention = %d days, want 30", got)
	}
	if got := len(manager.ResultHistory("node-1")); got != 1 {
		t.Fatalf("history length after pruning = %d, want 1", got)
	}
}
