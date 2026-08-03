package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

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
