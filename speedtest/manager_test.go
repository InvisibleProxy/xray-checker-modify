package speedtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

func TestLoadResultsKeepsInactiveHistory(t *testing.T) {
	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	resultPath := filepath.Join(dir, "speedtest_results.json")
	checkedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
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
	checkedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
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
