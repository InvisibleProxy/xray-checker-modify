package speedtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xray-checker/checker"
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
