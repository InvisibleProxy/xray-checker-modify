package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xray-checker/agentautomation"
	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

type fakeSpeedDiagnosticAutomation struct {
	enabled     bool
	starts      int
	awaits      int
	annotations map[string]speedtest.AgentDiagnostic
}

func (f *fakeSpeedDiagnosticAutomation) Enabled() bool { return f.enabled }

func (f *fakeSpeedDiagnosticAutomation) AlertWait() time.Duration { return time.Second }

func (f *fakeSpeedDiagnosticAutomation) StartSpeedDiagnostics(report speedtest.RunReport, _ float64) map[string]agentautomation.Handle {
	f.starts++
	handles := make(map[string]agentautomation.Handle, len(report.Results))
	for _, result := range report.Results {
		handles[result.StableID] = agentautomation.Handle{StableID: result.StableID, SessionID: "diag-" + result.StableID}
	}
	return handles
}

func (f *fakeSpeedDiagnosticAutomation) Annotations(map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic {
	return f.annotations
}

func (f *fakeSpeedDiagnosticAutomation) Await(_ context.Context, _ map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic {
	f.awaits++
	return f.annotations
}

func TestTelegramAPIErrorTextRedactsBotToken(t *testing.T) {
	token := "123456:secret-token"
	err := fmt.Errorf("Post https://api.telegram.org/bot%s/getUpdates: connection reset", token)
	text := telegramAPIErrorText(err, token)
	if strings.Contains(text, token) {
		t.Fatalf("redacted error still contains bot token: %q", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("redacted marker missing: %q", text)
	}
}

func TestTrimMessageKeepsValidUTF8(t *testing.T) {
	trimmed := trimMessage(strings.Repeat("я", 5000))
	if !utf8.ValidString(trimmed) {
		t.Fatal("trimmed message is not valid UTF-8")
	}
	if utf8.RuneCountInString(trimmed) > 3900 {
		t.Fatalf("trimmed message has %d runes, want at most 3900", utf8.RuneCountInString(trimmed))
	}
}

func TestTrimHTMLMessageKeepsCompleteLinesAndTags(t *testing.T) {
	message := "<b>Report</b>\n" + strings.Repeat("<code>длинная строка</code>\n", 400)
	trimmed := trimHTMLMessage(message)
	if !utf8.ValidString(trimmed) {
		t.Fatal("trimmed HTML message is not valid UTF-8")
	}
	if strings.Count(trimmed, "<code>") != strings.Count(trimmed, "</code>") {
		t.Fatalf("trimmed HTML contains an unclosed code tag: %q", trimmed)
	}
	if !strings.HasSuffix(trimmed, "...truncated") {
		t.Fatalf("trimmed HTML does not contain truncation suffix: %q", trimmed)
	}
}

func TestCountSpeedIssuesLowSpeedScenarios(t *testing.T) {
	results := []speedtest.Result{
		{Name: "fast", Mbps: 25},
		{Name: "low", Mbps: 4.9},
		{Name: "equal", Mbps: 5},
		{Name: "failed", Error: "timeout"},
		{Name: "offline", Offline: true},
	}

	failed, slow := countSpeedIssues(results, 5)
	if failed != 2 {
		t.Fatalf("failed count = %d, want 2", failed)
	}
	if slow != 1 {
		t.Fatalf("slow count = %d, want 1", slow)
	}

	failed, slow = countSpeedIssues(results, 0)
	if failed != 2 {
		t.Fatalf("failed count without threshold = %d, want 2", failed)
	}
	if slow != 0 {
		t.Fatalf("slow count without threshold = %d, want 0", slow)
	}
}

func TestSpeedIssuesHTMLIncludesLowSpeedOnlyBelowThreshold(t *testing.T) {
	lines := speedIssuesHTML([]speedtest.Result{
		{Name: "fast", StableID: "fast-id", Mbps: 10, DownloadedBytes: 10 * 1024 * 1024, DurationMs: 1000},
		{Name: "low", StableID: "low-id", Mbps: 4.99, DownloadedBytes: 2 * 1024 * 1024, DurationMs: 2000},
		{Name: "equal", StableID: "equal-id", Mbps: 5, DownloadedBytes: 2 * 1024 * 1024, DurationMs: 2000},
	}, 5)

	if len(lines) != 1 {
		t.Fatalf("issue lines = %d, want 1: %#v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"low", "4.99 Mbps"} {
		if !strings.Contains(line, want) {
			t.Fatalf("low-speed issue line %q does not contain %q", line, want)
		}
	}
	for _, noisy := range []string{"порог", "low-id", "MB", "ms"} {
		if strings.Contains(line, noisy) {
			t.Fatalf("low-speed issue line contains repeated technical noise %q: %q", noisy, line)
		}
	}
	if strings.Contains(line, "fast") || strings.Contains(line, "equal") {
		t.Fatalf("low-speed issue line contains non-low result: %q", line)
	}
}

func TestSpeedReportDecisionScenarios(t *testing.T) {
	baseCfg := DefaultConfig()
	baseCfg.Enabled = true
	baseCfg.ChatID = "123"
	baseCfg.SpeedReportsEnabled = true
	baseCfg.SpeedReportMode = "always"
	baseCfg.LowSpeedThresholdMbps = 10

	tests := []struct {
		name           string
		cfg            Config
		source         string
		results        []speedtest.Result
		wantFailed     int
		wantSlow       int
		wantIssuesOnly bool
		wantSend       bool
	}{
		{
			name:     "manual always sends without issues",
			cfg:      baseCfg,
			source:   "manual",
			results:  []speedtest.Result{{Name: "fast", Mbps: 25}},
			wantSend: true,
		},
		{
			name:           "manual issues mode skips without issues",
			cfg:            withReportMode(baseCfg, "issues"),
			source:         "manual",
			results:        []speedtest.Result{{Name: "fast", Mbps: 25}},
			wantIssuesOnly: true,
			wantSend:       false,
		},
		{
			name:           "manual issues mode sends low speed",
			cfg:            withReportMode(baseCfg, "issues"),
			source:         "manual",
			results:        []speedtest.Result{{Name: "low", Mbps: 5}},
			wantSlow:       1,
			wantSend:       true,
			wantFailed:     0,
			wantIssuesOnly: true,
		},
		{
			name:     "schedule always sends without issues",
			cfg:      baseCfg,
			source:   "schedule",
			results:  []speedtest.Result{{Name: "fast", Mbps: 25}},
			wantSend: true,
		},
		{
			name:     "schedule always sends low speed as full report",
			cfg:      baseCfg,
			source:   "schedule",
			results:  []speedtest.Result{{Name: "low", Mbps: 5}},
			wantSlow: 1,
			wantSend: true,
		},
		{
			name:           "error sends in issues mode",
			cfg:            withReportMode(baseCfg, "issues"),
			source:         "manual",
			results:        []speedtest.Result{{Name: "failed", Error: "timeout"}},
			wantFailed:     1,
			wantSend:       true,
			wantIssuesOnly: true,
		},
		{
			name:     "disabled report mode skips low speed",
			cfg:      withReportMode(baseCfg, "disabled"),
			source:   "manual",
			results:  []speedtest.Result{{Name: "low", Mbps: 5}},
			wantSend: false,
		},
		{
			name:     "empty chat skips low speed",
			cfg:      withChatID(baseCfg, ""),
			source:   "manual",
			results:  []speedtest.Result{{Name: "low", Mbps: 5}},
			wantSend: false,
		},
		{
			name:     "threshold disabled still sends in always mode",
			cfg:      withLowSpeedThreshold(baseCfg, 0),
			source:   "schedule",
			results:  []speedtest.Result{{Name: "slow but threshold disabled", Mbps: 1}},
			wantSend: true,
		},
		{
			name:     "schedule skips when only low node is muted",
			cfg:      withMutedNodeIDs(baseCfg, []string{"muted-id"}),
			source:   "schedule",
			results:  []speedtest.Result{{Name: "muted low", StableID: "muted-id", Mbps: 1}},
			wantSend: false,
		},
		{
			name:       "manual issues mode skips when only low node is muted",
			cfg:        withMutedNodeIDs(withReportMode(baseCfg, "issues"), []string{"muted-id"}),
			source:     "manual",
			results:    []speedtest.Result{{Name: "muted low", StableID: "muted-id", Mbps: 1}},
			wantSend:   false,
			wantFailed: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failed, slow, issuesOnly, shouldSend := speedReportDecision(speedtest.RunReport{
				Source:  tt.source,
				Results: tt.results,
			}, tt.cfg)
			if failed != tt.wantFailed {
				t.Fatalf("failed = %d, want %d", failed, tt.wantFailed)
			}
			if slow != tt.wantSlow {
				t.Fatalf("slow = %d, want %d", slow, tt.wantSlow)
			}
			if issuesOnly != tt.wantIssuesOnly {
				t.Fatalf("issuesOnly = %v, want %v", issuesOnly, tt.wantIssuesOnly)
			}
			if shouldSend != tt.wantSend {
				t.Fatalf("shouldSend = %v, want %v", shouldSend, tt.wantSend)
			}
		})
	}
}

func TestFormatSpeedReportLowSpeedModes(t *testing.T) {
	service := &Service{}
	cfg := DefaultConfig()
	cfg.LowSpeedThresholdMbps = 10
	cfg.SpeedReportLimit = 10
	report := speedtest.RunReport{
		Source:     "manual",
		FinishedAt: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Selected:   2,
		Results: []speedtest.Result{
			{Name: "low", StableID: "low-id", Mbps: 5, DownloadedBytes: 1024 * 1024, DurationMs: 1000},
			{Name: "fast", StableID: "fast-id", Mbps: 50, DownloadedBytes: 10 * 1024 * 1024, DurationMs: 1000},
		},
	}

	message := service.formatSpeedReportMessage(report, cfg, 0, 1, false)
	text := message.HTML
	for _, want := range []string{
		"Speed-test завершён",
		"Низкая скорость: <b>1</b>",
		"Порог низкой скорости: <b>10.00 Mbps</b>",
		"⚠️ <b>low</b> · <b>5.00 Mbps</b>",
		"✅ <b>fast</b> · <b>50.00 Mbps</b>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("manual report does not contain %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "low") != 1 {
		t.Fatalf("low-speed node is duplicated in compact report:\n%s", text)
	}
	for _, want := range []string{"<table bordered>", "<details><summary>Технические детали", "<details><summary>Без проблем: 1"} {
		if !strings.Contains(message.RichHTML, want) {
			t.Fatalf("rich report does not contain %q:\n%s", want, message.RichHTML)
		}
	}
	if strings.Contains(message.RichHTML, "<summary>Без проблем: 2</summary>") {
		t.Fatalf("low-speed node is duplicated in the healthy rich-report section:\n%s", message.RichHTML)
	}

	report.Source = "schedule"
	message = service.formatSpeedReportMessage(report, cfg, 0, 1, true)
	text = message.HTML
	for _, want := range []string{
		"Speed-test: есть проблемы",
		"расписание",
		"Порог низкой скорости: <b>10.00 Mbps</b>",
		"⚠️ <b>low</b> · <b>5.00 Mbps</b>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("schedule issues report does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Лучшие результаты") {
		t.Fatalf("schedule issues-only report should not include best results:\n%s", text)
	}
	if strings.Contains(message.RichHTML, "Без проблем") {
		t.Fatalf("issues-only rich report should not include successful results:\n%s", message.RichHTML)
	}
}

func TestFormatSpeedReportSkipsMutedNodes(t *testing.T) {
	service := &Service{}
	cfg := DefaultConfig()
	cfg.LowSpeedThresholdMbps = 10
	cfg.SpeedReportLimit = 10
	cfg.MutedNodeIDs = []string{"muted-id"}
	report := filterMutedRunReport(speedtest.RunReport{
		Source:     "manual",
		FinishedAt: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Selected:   2,
		Results: []speedtest.Result{
			{Name: "muted low", StableID: "muted-id", Mbps: 1, DownloadedBytes: 1024, DurationMs: 1000},
			{Name: "visible", StableID: "visible-id", Mbps: 50, DownloadedBytes: 10 * 1024 * 1024, DurationMs: 1000},
		},
	}, cfg)
	failed, slow := countSpeedIssues(report.Results, cfg.LowSpeedThresholdMbps)

	text := service.formatSpeedReport(report, cfg, failed, slow, false)
	if strings.Contains(text, "muted low") || strings.Contains(text, "muted-id") {
		t.Fatalf("report contains muted node:\n%s", text)
	}
	for _, want := range []string{
		"Проверено: <b>1</b>",
		"Успешно: <b>1</b>",
		"Низкая скорость: <b>0</b>",
		"visible",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report does not contain %q:\n%s", want, text)
		}
	}
}

func TestScopedMuteFiltersOnlySelectedTelegramReports(t *testing.T) {
	results := []speedtest.Result{
		{Name: "muted", StableID: "muted-id", Mbps: 1, DownloadedBytes: 1024, DurationMs: 1000},
		{Name: "visible", StableID: "visible-id", Mbps: 50, DownloadedBytes: 10 * 1024 * 1024, DurationMs: 1000},
	}

	cfg := DefaultConfig()
	cfg.MutedAlertNodeIDs = []string{"muted-id"}
	filtered := filterMutedSpeedResults(results, cfg)
	if len(filtered) != 2 {
		t.Fatalf("alert-only mute filtered speed results: got %d, want 2", len(filtered))
	}
	if !mutedAlertNodeSet(cfg)["muted-id"] {
		t.Fatal("alert-only mute did not mute availability alerts")
	}
	if mutedSpeedNodeSet(cfg)["muted-id"] {
		t.Fatal("alert-only mute muted speed reports")
	}

	cfg = DefaultConfig()
	cfg.MutedSpeedNodeIDs = []string{"muted-id"}
	filtered = filterMutedSpeedResults(results, cfg)
	if len(filtered) != 1 || filtered[0].StableID != "visible-id" {
		t.Fatalf("speed-only mute did not filter only speed results: %#v", filtered)
	}
	if mutedAlertNodeSet(cfg)["muted-id"] {
		t.Fatal("speed-only mute muted availability alerts")
	}

	cfg = DefaultConfig()
	cfg.MutedNodeIDs = []string{"muted-id"}
	filtered = filterMutedSpeedResults(results, cfg)
	if len(filtered) != 1 || filtered[0].StableID != "visible-id" {
		t.Fatalf("global mute did not filter speed results: %#v", filtered)
	}
	if !mutedAlertNodeSet(cfg)["muted-id"] {
		t.Fatal("global mute did not mute availability alerts")
	}
}

func TestNormalizeMutedNodeIDs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MutedNodeIDs = []string{" b ", "", "a", "a"}
	cfg.MutedSpeedNodeIDs = []string{" speed ", "", "speed"}
	cfg.MutedAlertNodeIDs = []string{" alert ", "", "alert"}
	cfg.Normalize()

	if got := strings.Join(cfg.MutedNodeIDs, ","); got != "a,b" {
		t.Fatalf("muted IDs = %q, want %q", got, "a,b")
	}
	if got := strings.Join(cfg.MutedSpeedNodeIDs, ","); got != "speed" {
		t.Fatalf("muted speed IDs = %q, want %q", got, "speed")
	}
	if got := strings.Join(cfg.MutedAlertNodeIDs, ","); got != "alert" {
		t.Fatalf("muted alert IDs = %q, want %q", got, "alert")
	}
}

func TestFilterActiveNodeIDsPrunesInactiveIDs(t *testing.T) {
	filtered := filterActiveNodeIDs([]string{" stale ", "active", "active"}, map[string]bool{
		"active": true,
	})

	if got := strings.Join(filtered, ","); got != "active" {
		t.Fatalf("filtered muted IDs = %q, want %q", got, "active")
	}
}

func TestConfigNormalizeRequiresConfirmedNodeAlert(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.AlertAfterFailures != 2 {
		t.Fatalf("default alertAfterFailures = %d, want 2", cfg.AlertAfterFailures)
	}

	cfg.AlertAfterFailures = 1
	cfg.Normalize()
	if cfg.AlertAfterFailures != 2 {
		t.Fatalf("normalized alertAfterFailures = %d, want 2", cfg.AlertAfterFailures)
	}
}

func TestShouldNotifyNodeRecoveryRequiresSentDownAlert(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NotifyRecovery = true

	pending := nodeAlertState{
		FailCount: 1,
		WasDown:   true,
		DownSince: time.Now().Add(-time.Minute),
	}
	if shouldNotifyNodeRecovery(pending, cfg, false) {
		t.Fatal("recovery was allowed before a down alert was sent")
	}

	confirmed := pending
	confirmed.AlertCount = 1
	if !shouldNotifyNodeRecovery(confirmed, cfg, false) {
		t.Fatal("recovery was not allowed after a down alert was sent")
	}
	if shouldNotifyNodeRecovery(confirmed, cfg, true) {
		t.Fatal("recovery was allowed for a muted node")
	}

	cfg.NotifyRecovery = false
	if shouldNotifyNodeRecovery(confirmed, cfg, false) {
		t.Fatal("recovery was allowed while notifyRecovery is disabled")
	}
}

func TestPendingNodeDownAlertRequiresRepeatedFailure(t *testing.T) {
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Server:   "example.com",
		Port:     443,
		Name:     "US",
		UUID:     "uuid",
	}
	proxy.StableID = proxy.GenerateStableID()
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

	downSince := time.Now().Add(-time.Minute).Truncate(time.Second)
	hostCheck := checker.HostCheckDetails{
		Checked:   true,
		Online:    false,
		CheckedAt: time.Now(),
		Target:    "example.com:443",
		Error:     "timeout",
	}
	pingCheck := checker.PingCheckDetails{
		Checked:   true,
		Online:    false,
		CheckedAt: time.Now(),
		Target:    "example.com",
		Error:     "timeout",
	}
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, downSince, hostCheck, pingCheck) {
		t.Fatal("failed to seed offline status")
	}

	service := NewService("", proxyChecker, nil, 10000)
	cfg := DefaultConfig()
	now := time.Now().Truncate(time.Second)
	state := nodeAlertState{
		FailCount: 1,
		WasDown:   true,
		DownSince: downSince,
		NextAlert: now,
	}
	service.alerts[proxy.StableID] = state
	if _, ok := service.pendingNodeDownAlert(proxy, cfg, now); ok {
		t.Fatal("down alert was allowed after the first failed check")
	}

	state.FailCount = 2
	service.alerts[proxy.StableID] = state
	alert, ok := service.pendingNodeDownAlert(proxy, cfg, now)
	if !ok {
		t.Fatal("down alert was not allowed after the repeated failed check")
	}
	if !alert.State.DownSince.Equal(downSince) {
		t.Fatalf("downSince = %s, want %s", alert.State.DownSince, downSince)
	}
	if alert.State.AlertCount != 1 {
		t.Fatalf("alert count = %d, want 1", alert.State.AlertCount)
	}
}

func TestPendingProxyFailureAlertUsesSeparateStatusAndDuration(t *testing.T) {
	proxy := &models.ProxyConfig{Protocol: "vless", Server: "example.com", Port: 443, Name: "NL", UUID: "uuid"}
	proxy.StableID = proxy.GenerateStableID()
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	failureSince := time.Now().Add(-time.Hour).Truncate(time.Second)
	hostCheck := checker.HostCheckDetails{Checked: true, Online: true, Target: "example.com:443"}
	pingCheck := checker.PingCheckDetails{Checked: true, Online: true, Target: "example.com"}
	failure := checker.FailureDetails{Code: checker.FailureCodeProxyTimeout, Summary: checker.FailureSummary(checker.FailureCodeProxyTimeout)}
	if !proxyChecker.RestoreProxyFailureStatus(proxy.StableID, failureSince, hostCheck, pingCheck, failure) {
		t.Fatal("failed to seed proxy-failure status")
	}

	service := NewService("", proxyChecker, nil, 10000)
	cfg := DefaultConfig()
	now := time.Now().Truncate(time.Second)
	service.alerts[proxy.StableID] = nodeAlertState{
		FailCount:         2,
		WasDown:           true,
		Status:            checker.AvailabilityStateProxyFailure,
		ProxyFailureSince: failureSince,
		NextAlert:         now,
		HostCheck:         hostCheck,
		PingCheck:         pingCheck,
		Failure:           failure,
	}

	alert, ok := service.pendingNodeDownAlert(proxy, cfg, now)
	if !ok {
		t.Fatal("proxy-failure alert was not allowed after repeated failures")
	}
	message := formatNodeDownMessage(proxy, alert.State, now)
	if !strings.Contains(message.HTML, "Proxy failure") || strings.Contains(message.HTML, "Простой:") || !strings.Contains(message.HTML, checker.FailureCodeProxyTimeout) {
		t.Fatalf("proxy-failure alert = %q", message.HTML)
	}
}

func TestDiagnosticsTransitionOfflineToProxyFailurePreservesAlertSchedule(t *testing.T) {
	stableID := "node-1"
	downSince := time.Now().Add(-time.Hour).Truncate(time.Second)
	lastAlert := downSince.Add(15 * time.Minute)
	nextAlert := downSince.Add(45 * time.Minute)
	proxyFailureSince := time.Now().Add(-time.Minute).Truncate(time.Second)
	service := &Service{alerts: map[string]nodeAlertState{
		stableID: {
			FailCount:  5,
			WasDown:    true,
			Status:     checker.AvailabilityStateOffline,
			DownSince:  downSince,
			LastAlert:  lastAlert,
			AlertCount: 2,
			NextAlert:  nextAlert,
			HostCheck:  checker.HostCheckDetails{Checked: true},
			PingCheck:  checker.PingCheckDetails{Checked: true},
		},
	}}
	details := checker.ProxyStatusDetails{
		Status:            checker.AvailabilityStateProxyFailure,
		ProxyFailureSince: proxyFailureSince,
		HostCheck:         checker.HostCheckDetails{Checked: true, Online: true},
		PingCheck:         checker.PingCheckDetails{Checked: true, Online: true},
		Failure:           checker.FailureDetails{Code: checker.FailureCodeProxyTimeout},
	}
	if !service.updateAlertDiagnostics(stableID, details) {
		t.Fatal("diagnostics transition was not stored")
	}
	state := service.alerts[stableID]
	if state.Status != checker.AvailabilityStateProxyFailure || !state.DownSince.IsZero() || !state.ProxyFailureSince.Equal(proxyFailureSince) {
		t.Fatalf("proxy-failure transition = %+v", state)
	}
	if state.FailCount != 5 || state.AlertCount != 2 || !state.LastAlert.Equal(lastAlert) || !state.NextAlert.Equal(nextAlert) {
		t.Fatalf("diagnostics transition changed alert schedule: %+v", state)
	}
}

func TestNotifyNodeStatusesKeepsAlertLifecycleAcrossIssueTransition(t *testing.T) {
	proxy := &models.ProxyConfig{Protocol: "vless", Server: "example.com", Port: 443, Name: "NL", UUID: "uuid"}
	proxy.StableID = proxy.GenerateStableID()
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	proxyFailureSince := time.Now().Add(-time.Minute).Truncate(time.Second)
	if !proxyChecker.RestoreProxyFailureStatus(
		proxy.StableID,
		proxyFailureSince,
		checker.HostCheckDetails{Checked: true, Online: true},
		checker.PingCheckDetails{Checked: true, Online: true},
		checker.FailureDetails{Code: checker.FailureCodeProxyTimeout},
	) {
		t.Fatal("failed to seed proxy-failure status")
	}

	lastAlert := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	nextAlert := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	service := NewService("", proxyChecker, nil, 10000)
	service.setConfig(Config{
		Enabled:            true,
		ChatID:             "1",
		NodeAlertsEnabled:  true,
		AlertAfterFailures: 2,
		MutedNodeIDs:       []string{proxy.StableID},
		TimeoutSec:         1,
	})
	service.alerts[proxy.StableID] = nodeAlertState{
		FailCount:  4,
		WasDown:    true,
		Status:     checker.AvailabilityStateOffline,
		DownSince:  time.Now().Add(-time.Hour),
		LastAlert:  lastAlert,
		AlertCount: 2,
		NextAlert:  nextAlert,
	}

	service.NotifyNodeStatuses()
	state := service.alerts[proxy.StableID]
	if state.Status != checker.AvailabilityStateProxyFailure || !state.DownSince.IsZero() || !state.ProxyFailureSince.Equal(proxyFailureSince) {
		t.Fatalf("issue transition = %+v", state)
	}
	if state.FailCount != 5 || state.AlertCount != 2 || !state.LastAlert.Equal(lastAlert) || !state.NextAlert.Equal(nextAlert) {
		t.Fatalf("issue transition reset alert lifecycle: %+v", state)
	}
}

func TestNotifyNodeStatusesPreservesMutedOfflineState(t *testing.T) {
	proxy := &models.ProxyConfig{
		Protocol: "vless",
		Server:   "example.com",
		Port:     443,
		Name:     "US",
		UUID:     "uuid",
	}
	proxy.StableID = proxy.GenerateStableID()
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

	downSince := time.Now().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	hostCheck := checker.HostCheckDetails{
		Checked:   true,
		Online:    false,
		CheckedAt: time.Now(),
		Target:    "example.com:443",
		Error:     "timeout",
	}
	pingCheck := checker.PingCheckDetails{
		Checked:   true,
		Online:    false,
		CheckedAt: time.Now(),
		Target:    "example.com",
		Error:     "timeout",
	}
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, downSince, hostCheck, pingCheck) {
		t.Fatal("failed to seed offline status")
	}

	service := NewService("", proxyChecker, nil, 10000)
	service.setConfig(Config{
		Enabled:                 true,
		ChatID:                  "1",
		NodeAlertsEnabled:       true,
		AlertCheckMinutes:       1,
		AlertAfterFailures:      2,
		AlertDiagnosticsMinutes: 60,
		MutedNodeIDs:            []string{proxy.StableID},
		TimeoutSec:              1,
	})
	service.alerts[proxy.StableID] = nodeAlertState{
		FailCount:       3,
		WasDown:         true,
		DownSince:       downSince,
		LastDiagnostics: time.Now(),
		HostCheck:       hostCheck,
		PingCheck:       pingCheck,
	}

	service.NotifyNodeStatuses()

	service.mu.RLock()
	state, ok := service.alerts[proxy.StableID]
	service.mu.RUnlock()
	if !ok {
		t.Fatal("muted offline state was removed")
	}
	if !state.DownSince.Equal(downSince) {
		t.Fatalf("downSince = %s, want %s", state.DownSince, downSince)
	}
	if state.FailCount <= 3 {
		t.Fatalf("failCount = %d, want incremented value", state.FailCount)
	}
}

func TestTelegramRunRequestUsesSavedScheduleConfig(t *testing.T) {
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := speedtest.NewManager(proxyChecker, 10000, "", speedtest.TestConfig{})
	want := speedtest.TestConfig{
		URL:         "https://speed.example.test/file.bin",
		MaxBytes:    8 * 1024 * 1024,
		TimeoutSec:  33,
		Concurrency: 3,
	}
	if err := manager.UpdateSchedule(speedtest.ScheduleConfig{Config: want}); err != nil {
		t.Fatalf("UpdateSchedule() error = %v", err)
	}

	service := NewService("", proxyChecker, manager, 10000)
	got := service.newSpeedTestRunRequest().Config
	if got != want {
		t.Fatalf("Telegram run config = %+v, want saved schedule config %+v", got, want)
	}
}

func TestTelegramRunRequestTargetsOriginChatAndThread(t *testing.T) {
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := speedtest.NewManager(proxyChecker, 10000, "", speedtest.TestConfig{})
	service := NewService("", proxyChecker, manager, 10000)
	service.setConfig(Config{ChatID: "configured-chat", MessageThreadID: 11})
	msg := &message{
		Chat:            chat{ID: -100987654321},
		MessageThreadID: 77,
	}

	req := service.newTelegramSpeedTestRunRequest(msg)
	want := speedtest.ReportTarget{ChatID: "-100987654321", MessageThreadID: 77}
	if req.ReportTarget != want {
		t.Fatalf("report target = %+v, want %+v", req.ReportTarget, want)
	}
}

func TestRequestedTelegramSpeedReportUsesOriginEvenWhenAutomatedReportsAreDisabled(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{
		Enabled:             true,
		ChatID:              "configured-chat",
		SpeedReportsEnabled: false,
		SpeedReportMode:     "disabled",
		TimeoutSec:          1,
	})

	type sentReport struct {
		chatID   string
		threadID int
		content  formattedMessage
	}
	sent := make(chan sentReport, 1)
	service.speedReportSendFunc = func(chatID string, threadID int, content formattedMessage) {
		sent <- sentReport{chatID: chatID, threadID: threadID, content: content}
	}

	service.NotifySpeedTest(speedtest.RunReport{
		Source:       "telegram",
		FinishedAt:   time.Now(),
		Selected:     1,
		Results:      []speedtest.Result{{StableID: "node-1", Name: "Node 1", Mbps: 50}},
		ReportTarget: speedtest.ReportTarget{ChatID: "origin-chat", MessageThreadID: 42},
	})

	select {
	case got := <-sent:
		if got.chatID != "origin-chat" || got.threadID != 42 {
			t.Fatalf("sent to %q/%d, want origin-chat/42", got.chatID, got.threadID)
		}
		if !strings.Contains(got.content.HTML, "Speed-test завершён") {
			t.Fatalf("requested result is not a full report:\n%s", got.content.HTML)
		}
	default:
		t.Fatal("requested Telegram result was not sent")
	}
}

func TestProjectMaintenanceSuppressesTelegramReportsAndClearsPendingState(t *testing.T) {
	dir := t.TempDir()
	service := NewService(filepath.Join(dir, "telegram_config.json"), nil, nil, 10000)
	service.setConfig(Config{Enabled: true, ChatID: "alerts-chat", SpeedReportsEnabled: true, SpeedReportMode: "always"})
	service.alerts["node-1"] = nodeAlertState{WasDown: true, FailCount: 2}
	service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "node-1"}] = true
	service.speedRetryEntries[1] = pendingSpeedRetry{Kind: speedRetryKindConfirmation, StableIDs: []string{"node-1"}, DueAt: time.Now().Add(time.Hour)}
	sent := 0
	service.speedReportSendFunc = func(string, int, formattedMessage) { sent++ }
	service.SetProjectMaintenance(true)

	service.NotifySpeedTest(speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{StableID: "node-1", Mbps: 50}}})
	if sent != 0 {
		t.Fatalf("project maintenance sent %d Telegram reports", sent)
	}
	if err := service.ClearAllMonitoringState(); err != nil {
		t.Fatal(err)
	}
	if len(service.alerts) != 0 || len(service.speedRetryPending) != 0 || len(service.speedRetryEntries) != 0 {
		t.Fatalf("monitoring state was not cleared: alerts=%d pending=%d entries=%d", len(service.alerts), len(service.speedRetryPending), len(service.speedRetryEntries))
	}
	// An empty state removes the file instead of leaving an empty one behind, so
	// absence is the strongest form of "nothing stale was persisted".
	persisted, err := os.ReadFile(filepath.Join(dir, "node_alert_state.json"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err == nil && strings.Contains(string(persisted), "node-1") {
		t.Fatalf("stale project maintenance state persisted:\n%s", persisted)
	}
}

func TestSuccessfulFallbackSuppressesAutomatedTelegramReport(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "always",
		LowSpeedThresholdMbps: 10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	service.NotifySpeedTest(speedtest.RunReport{
		Source:     "schedule",
		FinishedAt: time.Now(),
		Selected:   1,
		Results: []speedtest.Result{{
			StableID:                "node-1",
			Name:                    "Node 1",
			Mbps:                    20,
			FallbackUsed:            true,
			TelegramAlertSuppressed: true,
		}},
	})

	if len(sent) != 0 {
		t.Fatalf("successful fallback sent %d automated reports, want none", len(sent))
	}
	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "node-1"}]
	service.speedRetryMu.Unlock()
	if pending {
		t.Fatal("successful fallback scheduled a low-speed confirmation alert")
	}
}

func TestLowSpeedFallbackSchedulesConfirmation(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	service.NotifySpeedTest(speedtest.RunReport{
		Source:     "schedule",
		FinishedAt: time.Now(),
		Selected:   1,
		Config:     speedtest.TestConfig{URL: "https://primary.example.test/file.bin"},
		Results: []speedtest.Result{{
			StableID:     "node-1",
			Name:         "Node 1",
			Mbps:         2,
			FallbackUsed: true,
		}},
	})

	if len(sent) != 0 {
		t.Fatalf("initial low-speed fallback sent %d reports, want none", len(sent))
	}
	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "node-1"}]
	service.speedRetryMu.Unlock()
	if !pending {
		t.Fatal("low-speed fallback did not schedule a confirmation test")
	}
}

func TestRequestedTelegramSpeedReportStillReturnsSuccessfulFallback(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{Enabled: true, TimeoutSec: 1})

	sent := make(chan formattedMessage, 1)
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent <- content
	}
	service.NotifySpeedTest(speedtest.RunReport{
		Source:       "telegram",
		FinishedAt:   time.Now(),
		Selected:     1,
		ReportTarget: speedtest.ReportTarget{ChatID: "origin-chat", MessageThreadID: 42},
		Results: []speedtest.Result{{
			StableID:                "node-1",
			Name:                    "Node 1",
			Mbps:                    50,
			FallbackUsed:            true,
			TelegramAlertSuppressed: true,
		}},
	})

	select {
	case <-sent:
	default:
		t.Fatal("requested Telegram fallback result was suppressed")
	}
}

func TestAutomaticLowSpeedAlertRequiresFailedConfirmation(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		SpeedReportLimit:      10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	initial := speedtest.RunReport{
		Source:     "schedule",
		FinishedAt: time.Now(),
		Selected:   1,
		Config:     speedtest.TestConfig{URL: "https://speed.example.test/file.bin"},
		Results:    []speedtest.Result{{StableID: "node-1", Name: "Node 1", Mbps: 5}},
	}

	service.NotifySpeedTest(initial)
	if len(sent) != 0 {
		t.Fatalf("initial low-speed result sent %d reports, want none", len(sent))
	}
	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "node-1"}]
	service.speedRetryMu.Unlock()
	if !pending {
		t.Fatal("initial low-speed result did not schedule confirmation")
	}

	recovered := initial
	recovered.Source = speedConfirmationRetrySource
	recovered.Results = []speedtest.Result{{StableID: "node-1", Name: "Node 1", Mbps: 25}}
	service.NotifySpeedTest(recovered)
	if len(sent) != 0 {
		t.Fatalf("successful confirmation sent %d reports, want none", len(sent))
	}

	service.NotifySpeedTest(initial)
	confirmed := initial
	confirmed.Source = speedConfirmationRetrySource
	confirmed.Results = []speedtest.Result{{StableID: "node-1", Name: "Node 1", Mbps: 4}}
	service.NotifySpeedTest(confirmed)
	if len(sent) != 1 {
		t.Fatalf("failed confirmation sent %d reports, want 1", len(sent))
	}
	for _, want := range []string{"проблема подтверждена", "повтор через 30 минут", "4.00 Mbps"} {
		if !strings.Contains(sent[0].HTML, want) {
			t.Fatalf("confirmed report does not contain %q:\n%s", want, sent[0].HTML)
		}
	}
}

func TestConfirmedSpeedAlertIncludesReadOnlyAgentEvidence(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{
		Enabled: true, ChatID: "alerts-chat", SpeedReportsEnabled: true, SpeedReportMode: "issues",
		LowSpeedThresholdMbps: 10, SpeedReportLimit: 10, TimeoutSec: 1,
	})
	automation := &fakeSpeedDiagnosticAutomation{enabled: true, annotations: map[string]speedtest.AgentDiagnostic{
		"node-1": {
			State: speedtest.AgentDiagnosticReproduced, SessionID: "diag-node-1",
			AgentName: "EU probe", Region: "DE", Mbps: 3,
		},
	}}
	service.SetSpeedDiagnosticAutomation(automation)
	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	initial := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-1", Name: "Node 1", Mbps: 4, FallbackUsed: true,
		FallbackAttempted: true, FallbackAttempts: 1,
	}}}
	service.NotifySpeedTest(initial)
	if len(sent) != 0 {
		t.Fatalf("initial result sent an alert: %+v", sent)
	}
	confirmed := initial
	confirmed.Source = speedConfirmationRetrySource
	service.NotifySpeedTest(confirmed)
	if len(sent) != 1 || automation.awaits != 1 {
		t.Fatalf("sent=%d awaits=%d, want one enriched alert", len(sent), automation.awaits)
	}
	for _, want := range []string{"Agent EU probe / DE", "проблема воспроизведена", "Вероятнее общая проблема"} {
		if !strings.Contains(sent[0].HTML, want) {
			t.Fatalf("agent-enriched alert does not contain %q:\n%s", want, sent[0].HTML)
		}
	}
	if initial.Results[0].AgentDiagnostic != nil {
		t.Fatalf("source speed result was mutated: %+v", initial.Results[0])
	}
}

func TestExhaustedTechnicalFallbackWaitsForAgentEvidenceInImmediateAlert(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{
		Enabled: true, ChatID: "alerts-chat", SpeedReportsEnabled: true, SpeedReportMode: "issues",
		LowSpeedThresholdMbps: 10, SpeedReportLimit: 10, TimeoutSec: 1,
	})
	automation := &fakeSpeedDiagnosticAutomation{enabled: true, annotations: map[string]speedtest.AgentDiagnostic{
		"node-1": {
			State: speedtest.AgentDiagnosticNotReproduced, SessionID: "diag-node-1",
			AgentName: "US probe", Region: "US", RemoteStatus: "online", Mbps: 40,
		},
	}}
	service.SetSpeedDiagnosticAutomation(automation)
	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	report := speedtest.RunReport{Source: speedtest.ScheduleSource, Results: []speedtest.Result{{
		StableID: "node-1", Name: "Node 1", Error: "connection refused",
		FallbackAttempted: true, FallbackAttempts: 2, FallbackExhausted: true,
	}}}
	service.NotifySpeedTest(report)
	if len(sent) != 1 || automation.starts != 1 || automation.awaits != 1 {
		t.Fatalf("sent=%d starts=%d awaits=%d", len(sent), automation.starts, automation.awaits)
	}
	for _, want := range []string{"connection refused", "Agent US probe / US", "проблема не воспроизведена", "40 Mbps"} {
		if !strings.Contains(sent[0].HTML, want) {
			t.Fatalf("technical fallback alert does not contain %q:\n%s", want, sent[0].HTML)
		}
	}
	service.speedRetryMu.Lock()
	pending := len(service.speedRetryPending)
	service.speedRetryMu.Unlock()
	if pending != 0 {
		t.Fatalf("ordinary technical failure unexpectedly scheduled %d confirmation retries", pending)
	}
}

func TestInitialTransportFailureIsNotDelayedWithLowSpeedRetry(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		SpeedReportLimit:      10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	service.NotifySpeedTest(speedtest.RunReport{
		Source:     "schedule",
		FinishedAt: time.Now(),
		Selected:   2,
		Results: []speedtest.Result{
			{StableID: "slow", Name: "Slow node", Mbps: 5},
			{StableID: "failed", Name: "Failed node", Error: "timeout"},
		},
	})

	if len(sent) != 1 {
		t.Fatalf("initial transport failure sent %d reports, want 1", len(sent))
	}
	if !strings.Contains(sent[0].HTML, "Failed node") || !strings.Contains(sent[0].HTML, "timeout") {
		t.Fatalf("initial transport failure is missing:\n%s", sent[0].HTML)
	}
	if strings.Contains(sent[0].HTML, "Slow node") {
		t.Fatalf("unconfirmed low-speed node leaked into initial alert:\n%s", sent[0].HTML)
	}
}

func TestSpeedConfirmationKeepsDeadlineAsTechnicalError(t *testing.T) {
	errorText := "Get https://speed.example.test/file.bin: context deadline exceeded"
	results := []speedtest.Result{{StableID: "deadline-node", Error: errorText}}

	ids := speedConfirmationRetryIDs(results, 10)
	if strings.Join(ids, ",") != "deadline-node" {
		t.Fatalf("confirmation IDs = %v", ids)
	}
	if results[0].Error != errorText {
		t.Fatalf("deadline error changed to %q, want %q", results[0].Error, errorText)
	}
	failed, slow := countSpeedIssues(results, 10)
	if failed != 1 || slow != 0 {
		t.Fatalf("deadline classification: failed=%d slow=%d, want failed=1 slow=0", failed, slow)
	}
}

func TestContextDeadlineUsesThirtyMinuteConfirmation(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		SpeedReportLimit:      10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	before := time.Now()
	service.NotifySpeedTest(speedtest.RunReport{
		Source:     "schedule",
		FinishedAt: time.Now(),
		Selected:   1,
		Config:     speedtest.TestConfig{URL: "https://speed.example.test/file.bin", TimeoutSec: 30},
		Results: []speedtest.Result{{
			StableID: "deadline-node",
			Name:     "Deadline node",
			Error:    "Get https://speed.example.test/file.bin: context deadline exceeded",
		}},
	})

	if len(sent) != 0 {
		t.Fatalf("initial deadline error sent %d reports, want none before confirmation", len(sent))
	}

	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "deadline-node"}]
	var entry pendingSpeedRetry
	for _, candidate := range service.speedRetryEntries {
		entry = candidate
	}
	service.speedRetryMu.Unlock()
	if !pending {
		t.Fatal("deadline error did not enter the shared confirmation queue")
	}
	if entry.Kind != speedRetryKindConfirmation || entry.DueAt.Before(before.Add(29*time.Minute+59*time.Second)) || entry.DueAt.After(before.Add(30*time.Minute+time.Second)) {
		t.Fatalf("deadline retry entry = %+v, want shared confirmation due in 30 minutes", entry)
	}

	service.NotifySpeedTest(speedtest.RunReport{
		Source:     speedConfirmationRetrySource,
		FinishedAt: time.Now(),
		Selected:   1,
		Results: []speedtest.Result{{
			StableID: "deadline-node",
			Name:     "Deadline node",
			Error:    "context deadline exceeded",
		}},
	})
	if len(sent) != 1 || !strings.Contains(sent[0].HTML, "проблема подтверждена") || !strings.Contains(sent[0].HTML, "context deadline exceeded") {
		t.Fatalf("confirmed deadline notification = %+v", sent)
	}
}

func TestDeadlineFallbackLowSpeedJoinsThirtyMinuteConfirmation(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		SpeedReportLimit:      10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	service.NotifySpeedTest(speedtest.RunReport{
		Source:     "schedule",
		FinishedAt: time.Now(),
		Selected:   2,
		Config:     speedtest.TestConfig{URL: "https://primary.example.test/file.bin", TimeoutSec: 30},
		Results: []speedtest.Result{{
			StableID:     "fallback-node",
			Name:         "Fallback node",
			Mbps:         2,
			FallbackUsed: true,
			PrimaryError: "context deadline exceeded",
		}, {
			StableID: "ordinary-slow",
			Name:     "Ordinary slow node",
			Mbps:     3,
		}},
	})

	if len(sent) != 0 {
		t.Fatalf("unconfirmed low-speed results sent %d reports, want none", len(sent))
	}
	service.speedRetryMu.Lock()
	pendingFallback := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "fallback-node"}]
	pendingOrdinaryLowSpeed := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "ordinary-slow"}]
	service.speedRetryMu.Unlock()
	if !pendingFallback || !pendingOrdinaryLowSpeed {
		t.Fatalf("pending fallback=%t ordinary-low-speed=%t", pendingFallback, pendingOrdinaryLowSpeed)
	}
}

func TestConfirmationRetryForDeadlineLowSpeedResultIsNotDelayedAgain(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		SpeedReportLimit:      10,
		TimeoutSec:            1,
	})

	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, content formattedMessage) {
		sent = append(sent, content)
	}
	service.NotifySpeedTest(speedtest.RunReport{
		Source:     speedConfirmationRetrySource,
		FinishedAt: time.Now(),
		Selected:   1,
		Results:    []speedtest.Result{{StableID: "deadline-node", Name: "Deadline node", Mbps: 4}},
	})

	if len(sent) != 1 {
		t.Fatalf("low-speed confirmation sent %d reports, want 1", len(sent))
	}
	for _, want := range []string{"повтор через 30 минут", "Deadline node", "4.00 Mbps"} {
		if !strings.Contains(sent[0].HTML, want) {
			t.Fatalf("confirmation notification does not contain %q:\n%s", want, sent[0].HTML)
		}
	}
	service.speedRetryMu.Lock()
	pendingConfirmation := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "deadline-node"}]
	service.speedRetryMu.Unlock()
	if pendingConfirmation {
		t.Fatal("confirmation result was delayed by another confirmation window")
	}
}

func TestConfirmationRetryRunsOnlyLowSpeedAndUnresolvedDeadlineNodes(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = 20 * time.Millisecond
	service.setConfig(Config{LowSpeedThresholdMbps: 10})

	type runCall struct {
		req    speedtest.RunRequest
		source string
	}
	runs := make(chan runCall, 2)
	service.speedRunFunc = func(req speedtest.RunRequest, source string) error {
		runs <- runCall{req: req, source: source}
		return nil
	}
	report := speedtest.RunReport{
		Config: speedtest.TestConfig{URL: "https://speed.example.test/file.bin", TimeoutSec: 30},
		Results: []speedtest.Result{
			{StableID: "deadline-b", Error: "context deadline exceeded"},
			{StableID: "plain-timeout", Error: "request timeout"},
			{StableID: "healthy", Mbps: 50},
			{StableID: "deadline-a", PrimaryError: "Get: context deadline exceeded", Mbps: 25},
			{StableID: "slow-a", Mbps: 2},
		},
	}
	if !service.scheduleSpeedConfirmationRetry(report) || !service.scheduleSpeedConfirmationRetry(report) {
		t.Fatal("confirmation retry was not scheduled")
	}

	select {
	case got := <-runs:
		if got.source != speedConfirmationRetrySource {
			t.Fatalf("retry source = %q, want %q", got.source, speedConfirmationRetrySource)
		}
		if strings.Join(got.req.ProxyIDs, ",") != "deadline-b,slow-a" {
			t.Fatalf("retry proxy IDs = %v, want low-speed and unresolved deadline nodes", got.req.ProxyIDs)
		}
		if got.req.Config != report.Config {
			t.Fatalf("retry config = %+v, want %+v", got.req.Config, report.Config)
		}
		if !got.req.OnlyOnline {
			t.Fatal("confirmation retry must require a healthy proxy")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for confirmation retry")
	}

	select {
	case duplicate := <-runs:
		t.Fatalf("duplicate confirmation retry: %+v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSuccessfulManualSpeedTestCancelsPendingRetries(t *testing.T) {
	dataDir := t.TempDir()
	stableID := "node-1"
	proxy := &models.ProxyConfig{StableID: stableID, Name: "Node 1", Protocol: "vless", Server: "node.example", Port: 443}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	service := NewService(filepath.Join(dataDir, "telegram_config.json"), proxyChecker, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{
		Enabled:               true,
		ChatID:                "alerts-chat",
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "issues",
		LowSpeedThresholdMbps: 10,
		TimeoutSec:            1,
	})

	if !service.scheduleSpeedConfirmationRetry(speedtest.RunReport{
		Results: []speedtest.Result{{StableID: stableID, Error: "context deadline exceeded"}},
	}) {
		t.Fatal("initial confirmation retry was not scheduled")
	}
	alertStatePath := filepath.Join(dataDir, "node_alert_state.json")
	if _, err := os.Stat(alertStatePath); err != nil {
		t.Fatalf("persisted retry state: %v", err)
	}

	service.NotifySpeedTest(speedtest.RunReport{
		Source:   "manual",
		Selected: 1,
		Results: []speedtest.Result{{
			StableID:                stableID,
			Mbps:                    25,
			FallbackUsed:            true,
			PrimaryError:            "context deadline exceeded",
			TelegramAlertSuppressed: true,
		}},
	})

	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: stableID}]
	entryCount := len(service.speedRetryEntries)
	service.speedRetryMu.Unlock()
	if pending || entryCount != 0 {
		t.Fatalf("pending confirmation=%t entries=%d", pending, entryCount)
	}
	if _, err := os.Stat(alertStatePath); !os.IsNotExist(err) {
		t.Fatalf("cancelled retry state still exists: %v", err)
	}
}

func TestNonManualSuccessDoesNotCancelPendingRetries(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{LowSpeedThresholdMbps: 10})

	stableID := "node-1"
	if !service.scheduleSpeedConfirmationRetry(speedtest.RunReport{
		Results: []speedtest.Result{{StableID: stableID, Error: "context deadline exceeded"}},
	}) {
		t.Fatal("initial confirmation retry was not scheduled")
	}

	service.NotifySpeedTest(speedtest.RunReport{
		Source:   "schedule",
		Selected: 1,
		Results:  []speedtest.Result{{StableID: stableID, Mbps: 25}},
	})

	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: stableID}]
	entryCount := len(service.speedRetryEntries)
	service.speedRetryMu.Unlock()
	if !pending || entryCount != 1 {
		t.Fatalf("pending confirmation=%t entries=%d", pending, entryCount)
	}
}

func TestDeadlineAndLowSpeedShareConfirmationRetry(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{LowSpeedThresholdMbps: 10})

	stableID := "shared-node"
	if !service.scheduleSpeedConfirmationRetry(speedtest.RunReport{
		Results: []speedtest.Result{{StableID: stableID, Mbps: 2}},
	}) || !service.scheduleSpeedConfirmationRetry(speedtest.RunReport{
		Results: []speedtest.Result{{StableID: stableID, Error: "context deadline exceeded"}},
	}) {
		t.Fatal("shared confirmation retry was not scheduled")
	}

	service.speedRetryMu.Lock()
	pending := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: stableID}]
	entryCount := len(service.speedRetryEntries)
	service.speedRetryMu.Unlock()
	if !pending || entryCount != 1 {
		t.Fatalf("pending confirmation=%t entries=%d", pending, entryCount)
	}
}

func TestLowSpeedConfirmationRunsOnlyAffectedNodes(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = 20 * time.Millisecond
	service.setConfig(Config{LowSpeedThresholdMbps: 10})

	type runCall struct {
		req    speedtest.RunRequest
		source string
	}
	runs := make(chan runCall, 2)
	service.speedRunFunc = func(req speedtest.RunRequest, source string) error {
		runs <- runCall{req: req, source: source}
		return nil
	}
	report := speedtest.RunReport{
		Config: speedtest.TestConfig{URL: "https://speed.example.test/file.bin", TimeoutSec: 30},
		Results: []speedtest.Result{
			{StableID: "slow-b", Mbps: 2},
			{StableID: "fast", Mbps: 50},
			{StableID: "slow-a", Mbps: 3},
		},
	}
	if !service.scheduleSpeedConfirmationRetry(report) || !service.scheduleSpeedConfirmationRetry(report) {
		t.Fatal("low-speed confirmation was not scheduled")
	}

	select {
	case got := <-runs:
		if got.source != speedConfirmationRetrySource {
			t.Fatalf("retry source = %q, want %q", got.source, speedConfirmationRetrySource)
		}
		if strings.Join(got.req.ProxyIDs, ",") != "slow-a,slow-b" {
			t.Fatalf("retry proxy IDs = %v, want only slow nodes", got.req.ProxyIDs)
		}
		if got.req.Config != report.Config {
			t.Fatalf("retry config = %+v, want %+v", got.req.Config, report.Config)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for low-speed confirmation run")
	}

	select {
	case duplicate := <-runs:
		t.Fatalf("duplicate confirmation run: %+v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSpeedConfirmationClearsRequestedNodesWithoutResults(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	defer service.Stop()
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{LowSpeedThresholdMbps: 10})

	initial := speedtest.RunReport{
		Results: []speedtest.Result{
			{StableID: "measured", Mbps: 2},
			{StableID: "skipped", Mbps: 3},
		},
	}
	if !service.scheduleSpeedConfirmationRetry(initial) {
		t.Fatal("confirmation retry was not scheduled")
	}

	service.NotifySpeedTest(speedtest.RunReport{
		Source:             speedConfirmationRetrySource,
		RequestedStableIDs: []string{"measured", "skipped"},
		Results:            []speedtest.Result{{StableID: "measured", Mbps: 25}},
	})

	service.speedRetryMu.Lock()
	pendingMeasured := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "measured"}]
	pendingSkipped := service.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: "skipped"}]
	entryCount := len(service.speedRetryEntries)
	timerCount := len(service.speedRetryTimers)
	service.speedRetryMu.Unlock()
	if pendingMeasured || pendingSkipped || entryCount != 0 || timerCount != 0 {
		t.Fatalf(
			"retry remained armed: measured=%t skipped=%t entries=%d timers=%d",
			pendingMeasured,
			pendingSkipped,
			entryCount,
			timerCount,
		)
	}
}

func TestPendingLowSpeedRetrySurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node 1", Protocol: "vless", Server: "node.example", Port: 443}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "https://check.example/ip", 5, "https://check.example/status", "", 5, 1, "status")
	statePath := filepath.Join(dataDir, "telegram_config.json")

	service := NewService(statePath, proxyChecker, nil, 10000)
	service.speedRetryDelay = time.Hour
	service.setConfig(Config{LowSpeedThresholdMbps: 10})
	if !service.scheduleSpeedConfirmationRetry(speedtest.RunReport{
		Config:  speedtest.TestConfig{URL: "https://speed.example/file.bin", TimeoutSec: 30},
		Results: []speedtest.Result{{StableID: proxy.StableID, Mbps: 2}},
	}) {
		t.Fatal("retry was not scheduled")
	}
	service.Stop()

	restored := NewService(statePath, proxyChecker, nil, 10000)
	if err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	restored.speedRetryMu.Lock()
	if !restored.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: proxy.StableID}] || len(restored.speedRetryEntries) != 1 {
		restored.speedRetryMu.Unlock()
		t.Fatalf("restored retries = %+v", restored.speedRetryEntries)
	}
	for timerID, entry := range restored.speedRetryEntries {
		entry.DueAt = time.Now().Add(-time.Second)
		restored.speedRetryEntries[timerID] = entry
	}
	restored.speedRetryMu.Unlock()

	runs := make(chan speedtest.RunRequest, 1)
	restored.speedRunFunc = func(req speedtest.RunRequest, source string) error {
		if source != speedConfirmationRetrySource {
			t.Errorf("source = %q", source)
		}
		runs <- req
		return nil
	}
	restored.Start()
	defer restored.Stop()
	select {
	case req := <-runs:
		if len(req.ProxyIDs) != 1 || req.ProxyIDs[0] != proxy.StableID {
			t.Fatalf("restored request = %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("restored retry did not run")
	}
}

func TestLegacyDeadlineRetryMigratesToThirtyMinuteConfirmation(t *testing.T) {
	dataDir := t.TempDir()
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node 1", Protocol: "vless", Server: "node.example", Port: 443}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "https://check.example/ip", 5, "https://check.example/status", "", 5, 1, "status")
	statePath := filepath.Join(dataDir, "telegram_config.json")
	legacyDueAt := time.Now().Add(legacyDeadlineRetryDelay)
	data, err := json.Marshal(nodeAlertStateFile{
		Version: 1,
		Nodes:   map[string]persistedNodeAlertState{},
		SpeedRetries: []persistedSpeedRetry{{
			Kind:      legacySpeedRetryKindDeadline,
			StableIDs: []string{proxy.StableID},
			Config:    speedtest.TestConfig{URL: "https://speed.example/file.bin", TimeoutSec: 30},
			DueAt:     legacyDueAt,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	alertStatePath := filepath.Join(dataDir, "node_alert_state.json")
	if err := os.WriteFile(alertStatePath, data, 0600); err != nil {
		t.Fatal(err)
	}

	restored := NewService(statePath, proxyChecker, nil, 10000)
	if err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	restored.speedRetryMu.Lock()
	if !restored.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: proxy.StableID}] || len(restored.speedRetryEntries) != 1 {
		restored.speedRetryMu.Unlock()
		t.Fatalf("restored retries = %+v", restored.speedRetryEntries)
	}
	for timerID, entry := range restored.speedRetryEntries {
		if entry.Kind != speedRetryKindConfirmation {
			restored.speedRetryMu.Unlock()
			t.Fatalf("restored retry kind = %q", entry.Kind)
		}
		wantDueAt := legacyDueAt.Add(speedConfirmationRetryDelay - legacyDeadlineRetryDelay)
		if entry.DueAt.Before(wantDueAt.Add(-time.Second)) || entry.DueAt.After(wantDueAt.Add(time.Second)) {
			restored.speedRetryMu.Unlock()
			t.Fatalf("migrated due at = %v, want %v", entry.DueAt, wantDueAt)
		}
		entry.DueAt = time.Now().Add(-time.Second)
		restored.speedRetryEntries[timerID] = entry
	}
	restored.speedRetryMu.Unlock()
	persisted, err := os.ReadFile(alertStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), `"kind": "speed-confirmation"`) || strings.Contains(string(persisted), `"kind": "deadline"`) {
		t.Fatalf("legacy retry was not rewritten to the shared kind:\n%s", persisted)
	}

	runs := make(chan speedtest.RunRequest, 1)
	restored.speedRunFunc = func(req speedtest.RunRequest, source string) error {
		if source != speedConfirmationRetrySource {
			t.Errorf("source = %q", source)
		}
		runs <- req
		return nil
	}
	restored.Start()
	defer restored.Stop()
	select {
	case req := <-runs:
		if len(req.ProxyIDs) != 1 || req.ProxyIDs[0] != proxy.StableID {
			t.Fatalf("restored request = %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("migrated confirmation retry did not run")
	}
}

func TestLegacyPendingSpeedRetryMigratesToConfirmation(t *testing.T) {
	dataDir := t.TempDir()
	proxies := []*models.ProxyConfig{
		{StableID: "node-1", Name: "Node 1", Protocol: "vless", Server: "node-1.example", Port: 443},
		{StableID: "node-2", Name: "Node 2", Protocol: "vless", Server: "node-2.example", Port: 443},
	}
	proxyChecker := checker.NewProxyChecker(proxies, 10000, "https://check.example/ip", 5, "https://check.example/status", "", 5, 1, "status")
	statePath := filepath.Join(dataDir, "telegram_config.json")
	data, err := json.Marshal(nodeAlertStateFile{
		Version: 1,
		Nodes:   map[string]persistedNodeAlertState{},
		SpeedRetries: []persistedSpeedRetry{{
			StableIDs: []string{proxies[0].StableID},
			Config:    speedtest.TestConfig{URL: "https://speed.example/file.bin", TimeoutSec: 30},
			DueAt:     time.Now().Add(time.Hour),
		}, {
			Kind:      legacySpeedRetryKindLowSpeed,
			StableIDs: []string{proxies[1].StableID},
			Config:    speedtest.TestConfig{URL: "https://speed.example/file.bin", TimeoutSec: 30},
			DueAt:     time.Now().Add(time.Hour),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "node_alert_state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	restored := NewService(statePath, proxyChecker, nil, 10000)
	defer restored.Stop()
	if err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	restored.speedRetryMu.Lock()
	if !restored.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: proxies[0].StableID}] ||
		!restored.speedRetryPending[speedRetryKey{Kind: speedRetryKindConfirmation, StableID: proxies[1].StableID}] ||
		len(restored.speedRetryEntries) != 2 {
		restored.speedRetryMu.Unlock()
		t.Fatalf("legacy retries = %+v", restored.speedRetryEntries)
	}
	for _, entry := range restored.speedRetryEntries {
		if entry.Kind != speedRetryKindConfirmation {
			restored.speedRetryMu.Unlock()
			t.Fatalf("legacy retry kind = %q, want %q", entry.Kind, speedRetryKindConfirmation)
		}
	}
	restored.speedRetryMu.Unlock()

	persisted, err := os.ReadFile(filepath.Join(dataDir, "node_alert_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), `"kind": "low-speed"`) || !strings.Contains(string(persisted), `"kind": "speed-confirmation"`) {
		t.Fatalf("legacy retry kinds were not rewritten:\n%s", persisted)
	}
}

func TestMassNodeFailuresAreCorrelatedByCause(t *testing.T) {
	proxies := []*models.ProxyConfig{
		{StableID: "one", Name: "One", SubName: "Primary", Server: "one.example"},
		{StableID: "two", Name: "Two", SubName: "Primary", Server: "two.example"},
		{StableID: "three", Name: "Three", SubName: "Primary", Server: "three.example"},
		{StableID: "four", Name: "Four", SubName: "Primary", Server: "four.example"},
	}
	failure := checker.FailureDetails{Code: checker.FailureCodeTCPRefused, Summary: checker.FailureSummary(checker.FailureCodeTCPRefused)}
	alerts := []nodeDownAlert{
		{Proxy: proxies[0], State: nodeAlertState{Failure: failure}},
		{Proxy: proxies[1], State: nodeAlertState{Failure: failure}},
		{Proxy: proxies[2], State: nodeAlertState{Failure: failure}},
	}
	groups, remaining := partitionMassNodeDownAlerts(alerts, proxies, nil)
	if len(groups) != 1 || len(groups[0].Alerts) != 3 || len(remaining) != 0 {
		t.Fatalf("groups=%+v remaining=%+v", groups, remaining)
	}
	message := formatMassNodeDownMessage(groups[0], time.Now())
	if !strings.Contains(message.HTML, "Массовый сбой") || !strings.Contains(message.HTML, "3 из 4") || !strings.Contains(message.HTML, checker.FailureCodeTCPRefused) {
		t.Fatalf("mass incident message:\n%s", message.HTML)
	}

	alerts[2].State.Failure = checker.FailureDetails{Code: checker.FailureCodeTLS, Summary: checker.FailureSummary(checker.FailureCodeTLS)}
	groups, _ = partitionMassNodeDownAlerts(alerts, proxies, nil)
	if len(groups) != 0 {
		t.Fatalf("mixed causes were correlated: %+v", groups)
	}
}

func TestIsChatAllowedRequiresConfiguredChat(t *testing.T) {
	service := &Service{}
	admin := &user{ID: 7}
	cfg := Config{AdminUserIDs: []int64{7}}
	if service.isChatAllowedFor(42, admin, cfg) {
		t.Fatal("empty configured chat authorized an incoming message")
	}

	cfg.ChatID = "42"
	if !service.isChatAllowedFor(42, nil, cfg) {
		t.Fatal("configured chat was rejected")
	}
	if service.isChatAllowedFor(43, &user{ID: 8}, cfg) {
		t.Fatal("unconfigured chat with a non-admin user was authorized")
	}
	if !service.isChatAllowedFor(43, admin, cfg) {
		t.Fatal("configured admin user was rejected")
	}
}

func TestRecoveryStateSurvivesPersistenceAndClearsOnlyAfterConfirmation(t *testing.T) {
	recoveredAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	want := nodeAlertState{
		FailCount:       3,
		WasDown:         true,
		DownSince:       recoveredAt.Add(-15 * time.Minute),
		LastAlert:       recoveredAt.Add(-10 * time.Minute),
		AlertCount:      1,
		RecoveryPending: true,
		RecoveredAt:     recoveredAt,
		RecoveryLatency: 42 * time.Millisecond,
	}
	got := persistedNodeAlertStateFrom(want).toNodeAlertState()
	if !got.RecoveryPending || !got.RecoveredAt.Equal(want.RecoveredAt) || got.RecoveryLatency != want.RecoveryLatency {
		t.Fatalf("persisted recovery state = %+v, want pending recovery %+v", got, want)
	}

	service := &Service{alerts: map[string]nodeAlertState{"node-1": got}}
	if service.confirmNodeRecoverySent("node-1", recoveredAt.Add(time.Second)) {
		t.Fatal("recovery was confirmed for a different notification instance")
	}
	if _, ok := service.alerts["node-1"]; !ok {
		t.Fatal("pending recovery was removed after failed confirmation")
	}
	if !service.confirmNodeRecoverySent("node-1", recoveredAt) {
		t.Fatal("matching pending recovery was not confirmed")
	}
	if _, ok := service.alerts["node-1"]; ok {
		t.Fatal("confirmed recovery was not removed")
	}
}

func TestProxyFailureAlertStateSurvivesPersistence(t *testing.T) {
	failureSince := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	want := nodeAlertState{
		FailCount:         4,
		WasDown:           true,
		Status:            checker.AvailabilityStateProxyFailure,
		ProxyFailureSince: failureSince,
		LastAlert:         failureSince.Add(5 * time.Minute),
		AlertCount:        2,
		Failure: checker.FailureDetails{
			Code:    checker.FailureCodeProxyTimeout,
			Summary: checker.FailureSummary(checker.FailureCodeProxyTimeout),
		},
	}
	got := persistedNodeAlertStateFrom(want).toNodeAlertState()
	if got.Status != checker.AvailabilityStateProxyFailure || !got.ProxyFailureSince.Equal(failureSince) || !got.DownSince.IsZero() || got.Failure.Code != checker.FailureCodeProxyTimeout {
		t.Fatalf("persisted proxy-failure state = %+v, want %+v", got, want)
	}
}

func TestPrepareNodeRecoveryDoesNotAdvanceFailureOrReminderState(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node one"}
	recoveredAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	downSince := recoveredAt.Add(-10 * time.Minute)
	wantState := nodeAlertState{
		FailCount:  4,
		WasDown:    true,
		DownSince:  downSince,
		LastAlert:  recoveredAt.Add(-5 * time.Minute),
		AlertCount: 2,
		NextAlert:  recoveredAt.Add(time.Hour),
		HostCheck:  checker.HostCheckDetails{Checked: true, Online: true},
		PingCheck:  checker.PingCheckDetails{Checked: true, Online: true},
	}
	service := &Service{alerts: map[string]nodeAlertState{proxy.StableID: wantState}}
	cfg := DefaultConfig()
	details := checker.ProxyStatusDetails{Online: true, Latency: 42 * time.Millisecond, CheckedAt: recoveredAt}

	alert, shouldSend, changed := service.prepareNodeRecovery(proxy, details, cfg, false)
	if !shouldSend || !changed {
		t.Fatalf("prepareNodeRecovery() shouldSend = %v, changed = %v", shouldSend, changed)
	}
	if alert.StableID != proxy.StableID || !alert.RecoveredAt.Equal(recoveredAt) {
		t.Fatalf("recovery alert = %+v", alert)
	}
	got := service.alerts[proxy.StableID]
	if got.FailCount != wantState.FailCount || got.AlertCount != wantState.AlertCount || got.NextAlert != wantState.NextAlert {
		t.Fatalf("recovery changed failure/reminder state: got %+v, want base %+v", got, wantState)
	}
	if !got.RecoveryPending || !got.RecoveredAt.Equal(recoveredAt) || got.RecoveryLatency != details.Latency {
		t.Fatalf("recovery pending state = %+v", got)
	}
}

func TestAvailabilityCheckUsesInjectedWorkflow(t *testing.T) {
	service := &Service{}
	var got []string
	service.SetAvailabilityCheckFunc(func(stableIDs []string) error {
		got = append([]string(nil), stableIDs...)
		return nil
	})
	if err := service.runAvailabilityCheck([]string{"node-1"}); err != nil {
		t.Fatalf("runAvailabilityCheck() error = %v", err)
	}
	if len(got) != 1 || got[0] != "node-1" {
		t.Fatalf("availability callback IDs = %v", got)
	}
}

func TestTelegramSpeedTestChecksAvailabilityBeforeRun(t *testing.T) {
	service := &Service{}
	checked := false
	run := false
	service.SetAvailabilityCheckFunc(func(stableIDs []string) error {
		checked = true
		if len(stableIDs) != 1 || stableIDs[0] != "node-1" {
			t.Fatalf("availability IDs = %v", stableIDs)
		}
		return nil
	})
	service.speedRunFunc = func(req speedtest.RunRequest, source string) error {
		run = true
		if source != "telegram" || !req.OnlyOnline || !req.SkipOffline {
			t.Fatalf("telegram run request = %+v, source=%q", req, source)
		}
		return nil
	}
	if err := service.runSpeedTest(speedtest.RunRequest{ProxyIDs: []string{"node-1"}}, "telegram"); err != nil {
		t.Fatalf("runSpeedTest() error = %v", err)
	}
	if !checked || !run {
		t.Fatalf("availability checked=%t speedtest run=%t", checked, run)
	}
}

func TestNodeDetailMarkupIncludesManualAvailabilityCheckForAdmin(t *testing.T) {
	service := &Service{}
	adminMarkup := service.nodeDetailMarkup("node-1", true)
	if !strings.Contains(adminMarkup, "Проверить доступность") || !strings.Contains(adminMarkup, "node:check:node-1") {
		t.Fatalf("admin node markup does not include availability check: %s", adminMarkup)
	}
	userMarkup := service.nodeDetailMarkup("node-1", false)
	if strings.Contains(userMarkup, "node:check:node-1") {
		t.Fatalf("non-admin node markup exposes availability check: %s", userMarkup)
	}
}

func TestRichMessagePayloadAndFallbackPolicy(t *testing.T) {
	encoded, err := json.Marshal(inputRichMessage{
		HTML:                "<h2>Сводка</h2><table><tr><td>1</td></tr></table>",
		SkipEntityDetection: true,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload["html"] == "" || payload["skip_entity_detection"] != true {
		t.Fatalf("rich-message payload = %#v", payload)
	}

	if !shouldFallbackRichMessage(fmt.Errorf("Telegram API error: Bad Request")) {
		t.Fatal("definite Telegram rejection did not enable compact fallback")
	}
	if shouldFallbackRichMessage(fmt.Errorf("connection reset by peer")) {
		t.Fatal("ambiguous network failure enabled fallback and could duplicate a message")
	}
	if !richMessageUnsupported(fmt.Errorf("HTTP 404: method not found")) {
		t.Fatal("missing Rich Messages method was not cached as unsupported")
	}
	if !canSendRichMessage("<h2>Сводка</h2>") {
		t.Fatal("valid rich message was rejected")
	}
	if canSendRichMessage(strings.Repeat("я", maxRichMessageRunes+1)) {
		t.Fatal("oversized rich message was not routed to compact fallback")
	}
}

func TestRichSpeedReportEscapesContentAndStaysWithinTelegramLimit(t *testing.T) {
	service := &Service{}
	cfg := DefaultConfig()
	cfg.LowSpeedThresholdMbps = 10
	cfg.SpeedReportLimit = 50
	report := speedtest.RunReport{
		Source:     "telegram",
		FinishedAt: time.Now(),
		Selected:   2,
		Results: []speedtest.Result{
			{Name: "<slow & node>", StableID: "slow-id", Mbps: 1},
			{Name: "healthy", StableID: "healthy-id", Mbps: 100, TTFBMs: 12},
		},
	}
	message := service.formatSpeedReportMessage(report, cfg, 0, 1, false)
	if strings.Contains(message.RichHTML, "<slow & node>") || !strings.Contains(message.RichHTML, "&lt;slow &amp; node&gt;") {
		t.Fatalf("rich report did not escape node name:\n%s", message.RichHTML)
	}
	if utf8.RuneCountInString(message.RichHTML) > 32768 {
		t.Fatalf("rich report has %d runes, Telegram limit is 32768", utf8.RuneCountInString(message.RichHTML))
	}
}

func TestRecentMeasurementsUseRichFormattedTable(t *testing.T) {
	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "<Node & one>",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "uuid",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	checkedAt := time.Now().UTC().Add(-time.Minute)
	state := map[string]interface{}{
		"version":   1,
		"updatedAt": checkedAt,
		"results": map[string]speedtest.Result{
			proxy.StableID: {
				StableID:  proxy.StableID,
				Name:      proxy.Name,
				Mbps:      42,
				CheckedAt: checkedAt,
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "speedtest_results.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manager := speedtest.NewManager(proxyChecker, 10000, schedulePath, speedtest.TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}

	service := NewService("", proxyChecker, manager, 10000)
	message := service.formatRecentSpeedOverviewMessage()
	for _, want := range []string{"<h2>Замеры</h2>", "<table bordered striped>", "<th>Результат</th>", "&lt;Node &amp; one&gt;"} {
		if !strings.Contains(message.RichHTML, want) {
			t.Fatalf("rich measurements do not contain %q:\n%s", want, message.RichHTML)
		}
	}
	if !strings.Contains(message.HTML, "<b>Замеры</b>") {
		t.Fatalf("compact measurements are not formatted:\n%s", message.HTML)
	}
}

func TestTelegramInteractiveSpeedViewsExcludeInactiveResults(t *testing.T) {
	active := &models.ProxyConfig{
		StableID: "active-node",
		Name:     "Current Active",
		Protocol: "vless",
		Server:   "active.example.com",
		Port:     443,
		UUID:     "active-uuid",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{active}, 10000, "", 1, "", "", 1, 0, "status")

	dir := t.TempDir()
	schedulePath := filepath.Join(dir, "speedtest_schedule.json")
	checkedAt := time.Now().UTC().Add(-time.Minute)
	state := map[string]interface{}{
		"version":   1,
		"updatedAt": checkedAt,
		"results": map[string]speedtest.Result{
			active.StableID: {
				StableID:  active.StableID,
				Name:      active.Name,
				Error:     "active failure",
				CheckedAt: checkedAt,
			},
			"retired-node": {
				StableID:  "retired-node",
				Name:      "Old Retired",
				Error:     "retired failure",
				CheckedAt: checkedAt.Add(-time.Minute),
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "speedtest_results.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	manager := speedtest.NewManager(proxyChecker, 10000, schedulePath, speedtest.TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}
	if got := len(manager.Snapshot().Results); got != 2 {
		t.Fatalf("test fixture latest results = %d, want active and retired results", got)
	}

	service := NewService("", proxyChecker, manager, 10000)
	views := map[string]string{
		"issues compact": service.formatIssuesSummary(),
		"issues rich":    service.formatIssuesSummaryMessage().RichHTML,
		"recent compact": service.formatRecentSpeedOverview(),
		"recent rich":    service.formatRecentSpeedOverviewMessage().RichHTML,
		"recent buttons": service.speedHistoryMarkup(1),
	}
	for name, view := range views {
		if strings.Contains(view, "Old Retired") || strings.Contains(view, "retired-node") {
			t.Fatalf("%s contains inactive speed-test result:\n%s", name, view)
		}
		if !strings.Contains(view, "Current Active") && !strings.Contains(view, active.StableID) {
			t.Fatalf("%s does not contain active speed-test result:\n%s", name, view)
		}
	}
}

func TestTelegramInteractiveViewsAndTransportExcludeMaintenanceNodes(t *testing.T) {
	active := &models.ProxyConfig{StableID: "active", Name: "Active"}
	paused := &models.ProxyConfig{StableID: "paused", Name: "Paused"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{active, paused}, 10000, "", 1, "", "", 1, 0, "status")
	if err := proxyChecker.SetMaintenanceMode(paused.StableID, true); err != nil {
		t.Fatal(err)
	}
	service := NewService("", proxyChecker, nil, 10000)

	if proxies := service.sortedProxies(); len(proxies) != 1 || proxies[0].StableID != active.StableID {
		t.Fatalf("Telegram proxies = %+v, want only active", proxies)
	}
	if total, _, _, _ := service.nodeCounts(); total != 1 {
		t.Fatalf("Telegram node total = %d, want 1", total)
	}
	if proxy, matches := service.findProxy(paused.StableID); proxy != nil || len(matches) != 0 {
		t.Fatalf("maintenance node is reachable from Telegram: proxy=%+v matches=%+v", proxy, matches)
	}
	if candidates := service.proxyCandidates(); len(candidates) != 1 || candidates[0].Proxy.StableID != active.StableID {
		t.Fatalf("Telegram transport candidates = %+v, want only active", candidates)
	}
	results := service.activeSpeedResults([]speedtest.Result{
		{StableID: active.StableID, Name: active.Name, Mbps: 50},
		{StableID: paused.StableID, Name: paused.Name, Mbps: 50, MaintenanceProbe: true},
	})
	if len(results) != 1 || results[0].StableID != active.StableID {
		t.Fatalf("Telegram speed results = %+v, want only active", results)
	}
	report := service.excludeMaintenanceSpeedResults(speedtest.RunReport{
		Selected: 3,
		Results: []speedtest.Result{
			{StableID: active.StableID, Name: active.Name, Mbps: 50},
			{StableID: active.StableID, Name: active.Name, Mbps: 50, MaintenanceProbe: true},
			{StableID: paused.StableID, Name: paused.Name, Mbps: 50},
		},
	})
	if report.Selected != 1 || len(report.Results) != 1 || report.Results[0].StableID != active.StableID || report.Results[0].MaintenanceProbe {
		t.Fatalf("Telegram filtered report = %+v, want only the regular active result", report)
	}
}

func withReportMode(cfg Config, mode string) Config {
	cfg.SpeedReportMode = mode
	return cfg
}

func withChatID(cfg Config, chatID string) Config {
	cfg.ChatID = chatID
	return cfg
}

func withLowSpeedThreshold(cfg Config, threshold float64) Config {
	cfg.LowSpeedThresholdMbps = threshold
	return cfg
}

func withMutedNodeIDs(cfg Config, ids []string) Config {
	cfg.MutedNodeIDs = ids
	return cfg
}
