package speedtest

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/projectmaintenance"
)

type reportRecorder chan RunReport

func (r reportRecorder) NotifySpeedTest(report RunReport) {
	r <- report
}

func TestMaintenanceNodesAreExcludedFromSpeedTestSelection(t *testing.T) {
	paused := &models.ProxyConfig{StableID: "paused", Name: "Paused"}
	active := &models.ProxyConfig{StableID: "active", Name: "Active"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{paused, active}, 10000, "", 1, "", "", 1, 0, "status")
	if err := proxyChecker.SetMaintenanceMode(paused.StableID, true); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	selected := manager.selectProxies(RunRequest{}, false)
	if len(selected) != 1 || selected[0].StableID != active.StableID {
		t.Fatalf("selected proxies = %+v, want only active", selected)
	}
	manualSelected := manager.selectProxies(RunRequest{ProxyIDs: []string{paused.StableID}}, true)
	if len(manualSelected) != 1 || manualSelected[0].StableID != paused.StableID {
		t.Fatalf("manual selection = %+v, want maintenance node", manualSelected)
	}
	if err := manager.Run(RunRequest{ProxyIDs: []string{paused.StableID}}, "telegram"); !errors.Is(err, checker.ErrMaintenanceMode) {
		t.Fatalf("paused-only run error = %v, want ErrMaintenanceMode", err)
	}
}

func TestAutomaticSpeedTestSelectionRequiresHealthyProxy(t *testing.T) {
	proxyFailure := &models.ProxyConfig{StableID: "proxy-failure", Name: "Proxy failure"}
	offline := &models.ProxyConfig{StableID: "offline", Name: "Offline"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxyFailure, offline}, 10000, "", 1, "", "", 1, 0, "status")
	if !proxyChecker.RestoreProxyFailureStatus(proxyFailure.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{Checked: true, Online: true}, checker.PingCheckDetails{}) {
		t.Fatal("failed to prepare proxy-failure state")
	}
	if !proxyChecker.RestoreOfflineStatus(offline.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{Checked: true, Online: false}, checker.PingCheckDetails{Checked: true, Online: false}) {
		t.Fatal("failed to prepare offline state")
	}
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	selected := manager.selectProxies(RunRequest{ProxyIDs: []string{proxyFailure.StableID, offline.StableID}, OnlyOnline: true}, false)
	if len(selected) != 0 {
		t.Fatalf("automatic selection = %+v, want no unhealthy nodes", selected)
	}
	manual := manager.selectProxies(RunRequest{ProxyIDs: []string{proxyFailure.StableID, offline.StableID}}, false)
	if len(manual) != 2 {
		t.Fatalf("manual selection = %+v, want both explicitly selected nodes", manual)
	}
}

func TestUnavailableNodesAreSkippedWithoutSpeedResultOrHistory(t *testing.T) {
	proxyFailure := &models.ProxyConfig{StableID: "proxy-failure", Name: "Proxy failure"}
	offline := &models.ProxyConfig{StableID: "offline", Name: "Offline"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxyFailure, offline}, 10000, "", 1, "", "", 1, 0, "status")
	if !proxyChecker.RestoreProxyFailureStatus(proxyFailure.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{Checked: true, Online: true}, checker.PingCheckDetails{Checked: true, Online: true}) {
		t.Fatal("failed to prepare proxy-failure state")
	}
	if !proxyChecker.RestoreOfflineStatus(offline.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{Checked: true}, checker.PingCheckDetails{Checked: true}) {
		t.Fatal("failed to prepare offline state")
	}

	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	reports := make(reportRecorder, 1)
	manager.SetReporter(reports)
	if err := manager.Run(RunRequest{
		ProxyIDs:    []string{proxyFailure.StableID, offline.StableID},
		SkipOffline: true,
	}, "schedule"); err != nil {
		t.Fatal(err)
	}

	select {
	case report := <-reports:
		if report.Selected != 2 || len(report.Results) != 0 {
			t.Fatalf("skipped report = %+v, want selected nodes but no speed results", report)
		}
		if strings.Join(report.RequestedStableIDs, ",") != "proxy-failure,offline" {
			t.Fatalf("requested StableIDs = %v, want skipped request IDs preserved", report.RequestedStableIDs)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for skipped speed-test report")
	}
	if results := manager.Snapshot().Results; len(results) != 0 {
		t.Fatalf("unavailable nodes created latest results: %+v", results)
	}
	if history := manager.ResultHistory(proxyFailure.StableID); len(history) != 0 {
		t.Fatalf("proxy failure created history: %+v", history)
	}
	if history := manager.ResultHistory(offline.StableID); len(history) != 0 {
		t.Fatalf("offline node created history: %+v", history)
	}
}

func TestManualMaintenanceProbeIsVisibleButNotAddedToHistory(t *testing.T) {
	paused := &models.ProxyConfig{StableID: "paused", Name: "Paused"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{paused}, 10000, "", 1, "", "", 1, 0, "status")
	if err := proxyChecker.SetMaintenanceMode(paused.StableID, true); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	manager := NewManager(proxyChecker, 10000, filepath.Join(dir, "speedtest_schedule.json"), TestConfig{})
	manager.testAttempt = func(proxy *models.ProxyConfig, cfg TestConfig, source string) Result {
		return Result{StableID: proxy.StableID, Name: proxy.Name, URL: cfg.URL, Mbps: 42, CheckedAt: time.Now(), Source: source}
	}
	reports := make(reportRecorder, 1)
	manager.SetReporter(reports)

	if err := manager.Run(RunRequest{ProxyIDs: []string{paused.StableID}}, "manual"); err != nil {
		t.Fatalf("manual maintenance run: %v", err)
	}
	var report RunReport
	select {
	case report = <-reports:
	case <-time.After(2 * time.Second):
		t.Fatal("manual maintenance run did not finish")
	}
	if len(report.Results) != 1 || !report.Results[0].MaintenanceProbe {
		t.Fatalf("maintenance run report = %+v", report.Results)
	}
	if history := manager.ResultHistory(paused.StableID); len(history) != 0 {
		t.Fatalf("maintenance probe polluted history: %+v", history)
	}
	snapshot := manager.Snapshot()
	if len(snapshot.Results) != 1 || !snapshot.Results[0].MaintenanceProbe {
		t.Fatalf("maintenance probe is not visible in admin snapshot: %+v", snapshot.Results)
	}
	if _, err := os.Stat(manager.resultPath); !os.IsNotExist(err) {
		t.Fatalf("maintenance probe was persisted: %v", err)
	}
	manager.ClearMaintenanceProbe(paused.StableID)
	if results := manager.Snapshot().Results; len(results) != 0 {
		t.Fatalf("cleared maintenance probe is still visible: %+v", results)
	}
}

func TestProjectMaintenanceAllowsOnlyEphemeralAdminSpeedProbe(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node one"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, filepath.Join(t.TempDir(), "speedtest_schedule.json"), TestConfig{})
	manager.SetProjectMaintenance(true)
	manager.testAttempt = func(proxy *models.ProxyConfig, cfg TestConfig, source string) Result {
		return Result{StableID: proxy.StableID, Name: proxy.Name, Mbps: 42, CheckedAt: time.Now(), Source: source}
	}

	if err := manager.Run(RunRequest{ProxyIDs: []string{proxy.StableID}}, ScheduleSource); !errors.Is(err, projectmaintenance.ErrEnabled) {
		t.Fatalf("scheduled run error = %v, want project maintenance", err)
	}
	reports := make(reportRecorder, 1)
	manager.SetReporter(reports)
	if err := manager.Run(RunRequest{ProxyIDs: []string{proxy.StableID}}, ManualSource); err != nil {
		t.Fatalf("manual project maintenance probe: %v", err)
	}
	select {
	case report := <-reports:
		if len(report.Results) != 1 || !report.Results[0].MaintenanceProbe || !report.Results[0].ProjectMaintenanceProbe {
			t.Fatalf("project maintenance report = %+v", report.Results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("project maintenance probe did not finish")
	}
	if history := manager.ResultHistory(proxy.StableID); len(history) != 0 {
		t.Fatalf("project maintenance probe polluted history: %+v", history)
	}
	manager.ClearProjectMaintenanceProbes()
	if results := manager.Snapshot().Results; len(results) != 0 {
		t.Fatalf("project maintenance probe remained after boundary: %+v", results)
	}
}

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

func TestLegacySpeedResultsDefaultNewFallbackMetadataSafely(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "speedtest_results.json")
	legacy := []byte(`{"version":1,"updatedAt":"2026-08-31T12:00:00Z","results":{"node-1":{"stableId":"node-1","name":"Node 1","mbps":5,"checkedAt":"2026-08-31T12:00:00Z"}},"history":{}}`)
	if err := os.WriteFile(resultPath, legacy, 0644); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status"), 10000, filepath.Join(dir, "speedtest_schedule.json"), TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatalf("load legacy results: %v", err)
	}
	results := manager.Snapshot().Results
	if len(results) != 1 || results[0].FallbackAttempted || results[0].FallbackAttempts != 0 || results[0].FallbackExhausted || results[0].PrimaryMbps != 0 {
		t.Fatalf("normalized legacy result = %+v", results)
	}
}

func TestAgentDiagnosticIsNeverSerializedWithSpeedResult(t *testing.T) {
	data, err := json.Marshal(Result{
		StableID: "node-1",
		AgentDiagnostic: &AgentDiagnostic{
			State: AgentDiagnosticReproduced, SessionID: "diag-secret-context", AgentName: "EU probe",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"agentDiagnostic", "diag-secret-context", "EU probe"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("serialized speed result contains ephemeral agent field %q: %s", forbidden, data)
		}
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
	proxyConfig := manager.configForProxy(schedule.Config, proxy)
	if proxyConfig.URL != "https://new.example.com/test.bin" {
		t.Fatalf("scheduled proxy URL = %q, want updated URL", proxyConfig.URL)
	}

	reloaded := NewManager(proxyChecker, 10000, manager.statePath, TestConfig{})
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if reloaded.Schedule().Config.URL != "https://new.example.com/test.bin" {
		t.Fatalf("reloaded scheduled URL = %q, want updated URL", reloaded.Schedule().Config.URL)
	}
}

func TestScheduleDeadlineSurvivesReloadAndNonTimingUpdate(t *testing.T) {
	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, schedulePath, TestConfig{})
	initial := ScheduleConfig{
		Enabled:              true,
		IntervalSec:          3600,
		HistoryRetentionDays: 60,
		Config:               TestConfig{URL: "https://example.com/initial.bin"},
	}
	if err := manager.UpdateSchedule(initial); err != nil {
		t.Fatal(err)
	}
	first := manager.Snapshot().NextScheduledRunAt
	if first == nil {
		t.Fatal("enabled schedule has no persisted deadline")
	}

	initial.HistoryRetentionDays = 30
	initial.Config.URL = "https://example.com/updated.bin"
	if err := manager.UpdateSchedule(initial); err != nil {
		t.Fatal(err)
	}
	updated := manager.Snapshot().NextScheduledRunAt
	if updated == nil || !updated.Equal(*first) {
		t.Fatalf("non-timing update moved deadline from %v to %v", first, updated)
	}

	reloaded := NewManager(proxyChecker, 10000, schedulePath, TestConfig{})
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if schedule, _ := reloaded.ensureSchedulerDeadline(time.Now()); !schedule.Enabled {
		t.Fatal("reloaded schedule is disabled")
	}
	afterRestart := reloaded.Snapshot().NextScheduledRunAt
	if afterRestart == nil || !afterRestart.Equal(*first) {
		t.Fatalf("restart moved deadline from %v to %v", first, afterRestart)
	}
}

func TestScheduleIntervalUpdateKeepsOriginalAnchor(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	current := now.Add(40 * time.Minute)
	existing := ScheduleConfig{Enabled: true, IntervalSec: 3600}
	updated := ScheduleConfig{Enabled: true, IntervalSec: 7200}

	got := nextScheduledRunAfterUpdate(existing, updated, current, now)
	want := current.Add(time.Hour)
	if !got.Equal(want) {
		t.Fatalf("interval update deadline = %s, want anchored %s", got, want)
	}

	overdue := nextScheduledRunAfterTick(now.Add(-3*time.Hour), time.Hour, now)
	if want := now.Add(time.Hour); !overdue.Equal(want) {
		t.Fatalf("overdue tick advanced to %s, want one future interval %s", overdue, want)
	}
}

func TestRunReportPreservesEphemeralReportTarget(t *testing.T) {
	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "Node 1",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "uuid",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{}, checker.PingCheckDetails{}) {
		t.Fatal("failed to prepare offline proxy state")
	}
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	reports := make(reportRecorder, 1)
	manager.SetReporter(reports)
	target := ReportTarget{ChatID: "-100123", MessageThreadID: 77}
	req := RunRequest{
		ProxyIDs:     []string{proxy.StableID},
		SkipOffline:  true,
		Config:       TestConfig{URL: "https://example.com/test.bin"},
		ReportTarget: target,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || string(data) == "null" {
		t.Fatalf("marshaled request is empty: %q", data)
	}
	if strings.Contains(string(data), target.ChatID) || strings.Contains(string(data), "77") {
		t.Fatalf("ephemeral report target leaked into JSON: %s", data)
	}

	if err := manager.Run(req, "telegram"); err != nil {
		t.Fatal(err)
	}
	select {
	case report := <-reports:
		if report.ReportTarget != target {
			t.Fatalf("report target = %+v, want %+v", report.ReportTarget, target)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for speed-test report")
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
