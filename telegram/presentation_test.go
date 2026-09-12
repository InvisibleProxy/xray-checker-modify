package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xray-checker/speedtest"
)

func TestIssuesKeepMutedAvailabilityAndSpeedProblems(t *testing.T) {
	proxies := testProxies("alpha")
	proxyChecker := testChecker(proxies)
	dir := t.TempDir()
	id := proxies[0].StableID
	state := map[string]any{
		"version": 1,
		"results": map[string]speedtest.Result{id: {StableID: id, Name: "alpha", Mbps: 2, CheckedAt: time.Now()}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "speedtest_results.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	manager := speedtest.NewManager(proxyChecker, 10000, filepath.Join(dir, "schedule.json"), speedtest.TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}
	service := NewService("", proxyChecker, manager, 10000)
	cfg := DefaultConfig()
	cfg.MutedNodeIDs = []string{id}
	cfg.LowSpeedThresholdMbps = 10
	service.setConfig(cfg)
	message := service.formatIssuesSummaryMessage()
	for _, text := range []string{message.HTML, message.RichHTML} {
		if strings.Contains(text, "Проблем не найдено") {
			t.Fatalf("mute hid real problems: %s", text)
		}
		for _, want := range []string{"alpha", "Доступность: <b>1</b>", "Скорость: <b>1</b>", "2 Мбит/с"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if strings.Count(text, "🔕 уведомления выключены") != 2 {
			t.Fatalf("both channels must explain mute: %s", text)
		}
	}
	if !service.alertMuteSet(cfg)[id] || !service.speedMuteSet(cfg)[id] {
		t.Fatal("view changed delivery mute")
	}
}

func TestMuteNoteResolvesEachScopeIndependently(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	cfg := DefaultConfig()
	cfg.MutedAlertNodeIDs = []string{"node"}
	service.mutes["node"] = nodeMute{Scope: muteScopeSpeed, Until: time.Now().Add(time.Hour)}
	if got := service.muteNoteHTML("node", cfg, muteScopeAlerts); !strings.Contains(got, "выключены") {
		t.Fatal(got)
	}
	if got := service.muteNoteHTML("node", cfg, muteScopeSpeed); !strings.Contains(got, "до ") {
		t.Fatal(got)
	}
	service.mutes["node"] = nodeMute{Scope: muteScopeSpeed, Until: time.Now().Add(-time.Minute)}
	if got := service.muteNoteHTML("node", cfg, muteScopeSpeed); got != "" {
		t.Fatal("expired mute is still shown: " + got)
	}
}

func TestMenuReportsEffectiveNotificationPause(t *testing.T) {
	service := NewService("", testChecker(testProxies("alpha")), nil, 10000)
	cfg := DefaultConfig()
	cfg.Enabled, cfg.NodeAlertsEnabled, cfg.SpeedReportsEnabled = true, true, true
	cfg.SpeedReportMode = "issues"
	service.SetProjectMaintenance(true)
	message := service.formatMenuMessage(cfg, true)
	for _, text := range []string{message.HTML, message.RichHTML} {
		if strings.Count(text, "пауза: обслуживание") != 2 {
			t.Fatal(text)
		}
	}
	cfg.Enabled = false
	alerts, reports := service.notificationLabels(cfg)
	if alerts != "выключены" || reports != "выключены" {
		t.Fatalf("disabled bot: %s, %s", alerts, reports)
	}
}

func TestSpeedReportCategoriesAndRecordedThresholds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LowSpeedThresholdMbps = 10
	report := speedtest.RunReport{Source: "telegram", Results: []speedtest.Result{
		{StableID: "a", Name: "healthy", Mbps: 100},
		{StableID: "b", Name: "slow", Mbps: 20, LowSpeedThresholdMbps: 30},
		{StableID: "c", Name: "tolerant", Mbps: 5, LowSpeedThresholdMbps: 2},
		{StableID: "d", Name: "error", Error: "connection refused"},
	}}
	message := (&Service{}).formatSpeedReportMessage(report, cfg, 1, 1, false)
	for _, text := range []string{message.HTML, message.RichHTML} {
		for _, want := range []string{"Проверено: <b>4</b>", "В норме: <b>2</b>", "Ниже порога: <b>1</b>", "Ошибки: <b>1</b>", "20 Мбит/с", "порог 30 Мбит/с"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if strings.Contains(text, "Успешно") || strings.Contains(text, "порог 10 Мбит/с") {
			t.Fatal(text)
		}
	}
}

func TestAutomaticReportExplainsExcludedResults(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	t.Cleanup(service.Stop)
	service.speedRetryDelay = time.Hour
	cfg := DefaultConfig()
	cfg.Enabled, cfg.SpeedReportsEnabled = true, true
	cfg.ChatID, cfg.SpeedReportMode = "1", "always"
	cfg.LowSpeedThresholdMbps = 10
	cfg.MutedSpeedNodeIDs = []string{"muted"}
	service.setConfig(cfg)
	var sent []formattedMessage
	service.speedReportSendFunc = func(_ string, _ int, message formattedMessage) { sent = append(sent, message) }
	service.NotifySpeedTest(speedtest.RunReport{Source: "schedule", Results: []speedtest.Result{
		{StableID: "healthy", Name: "healthy", Mbps: 100},
		{StableID: "muted", Name: "muted", Mbps: 1},
		{StableID: "pending", Name: "pending", Mbps: 2},
		{StableID: "fallback", Name: "fallback", Mbps: 100, TelegramAlertSuppressed: true},
	}})
	if len(sent) != 1 {
		t.Fatalf("sent %d reports", len(sent))
	}
	for _, text := range []string{sent[0].HTML, sent[0].RichHTML} {
		for _, want := range []string{"В отчёте: <b>1</b>", "Всего результатов: 4", "ждут подтверждения: 1", "заглушены: 1", "успешный резервный URL: 1"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
	}
	service.speedRetryMu.Lock()
	pending := service.speedRetryPendingForLocked(speedRetryKindConfirmation, "pending")
	service.speedRetryMu.Unlock()
	if !pending {
		t.Fatal("presentation changed confirmation scheduling")
	}
}

func TestSpeedReportKeepsWholeNodeBlocksAndDirectActions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SpeedReportLimit = 2
	cfg.LowSpeedThresholdMbps = 30
	report := speedtest.RunReport{Source: "telegram"}
	for i := 0; i < 3; i++ {
		report.Results = append(report.Results, speedtest.Result{StableID: fmt.Sprintf("id%d", i), Name: fmt.Sprintf("node%d", i), Mbps: 18,
			AgentDiagnostic: &speedtest.AgentDiagnostic{AgentName: "NL-03", RemoteStatus: "online", Mbps: 16, State: speedtest.AgentDiagnosticReproduced}})
	}
	message := (&Service{}).formatSpeedReportMessage(report, cfg, 0, 3, true)
	for _, text := range []string{message.HTML, message.RichHTML} {
		for _, want := range []string{"node0", "node1", "Ещё нод: 1"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if strings.Contains(text, "node2") || strings.Contains(text, "Вероятнее") {
			t.Fatal(text)
		}
		if strings.Count(text, "просадка воспроизведена") != 2 {
			t.Fatal("agent evidence was split from nodes: " + text)
		}
	}
	if strings.Index(message.RichHTML, "просадка воспроизведена") > strings.Index(message.RichHTML, "<details>") {
		t.Fatal("agent verdict is hidden")
	}
	var keyboard inlineKeyboardMarkup
	if err := json.Unmarshal([]byte(message.ReplyMarkup), &keyboard); err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			actions[button.CallbackData] = true
		}
	}
	for _, want := range []string{"node:id0", "speed:id0", "node:id1", "speed:id1", "speed:list", "issues"} {
		if !actions[want] {
			t.Fatalf("missing direct action %q", want)
		}
	}
}

func TestLongSpeedReportUsesSameBoundedSelectionInBothFormats(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SpeedReportLimit = 50
	report := speedtest.RunReport{Source: "telegram"}
	for i := 0; i < 50; i++ {
		report.Results = append(report.Results, speedtest.Result{StableID: fmt.Sprintf("id-%02d", i), Name: fmt.Sprintf("NODE-%02d", i),
			Error: strings.Repeat("<error&>", 300), AgentDiagnostic: &speedtest.AgentDiagnostic{AgentName: strings.Repeat("<&>", 50), State: speedtest.AgentDiagnosticUnavailable}})
	}
	message := (&Service{}).formatSpeedReportMessage(report, cfg, 50, 0, true)
	if utf8.RuneCountInString(message.HTML) > 3900 || utf8.RuneCountInString(message.RichHTML) > maxRichMessageRunes {
		t.Fatal("message exceeded budget")
	}
	if strings.Contains(message.HTML, "Сообщение сокращено") {
		t.Fatal("generic truncation cut through a node block")
	}
	for _, result := range report.Results {
		if strings.Contains(message.HTML, result.Name) != strings.Contains(message.RichHTML, result.Name) {
			t.Fatal("formats show different nodes: " + result.Name)
		}
	}
	if !strings.Contains(message.HTML, "Ещё нод:") || !strings.Contains(message.RichHTML, "Ещё нод:") {
		t.Fatal("hidden nodes were not counted")
	}
}

func TestNodeAlertReminderIsPresentInBothFormats(t *testing.T) {
	proxy := testProxies("alpha")[0]
	now := time.Now()
	message := formatNodeDownMessage(proxy, nodeAlertState{NextAlert: now.Add(15 * time.Minute)}, now)
	for _, text := range []string{message.HTML, message.RichHTML} {
		if !strings.Contains(text, "Следующее напоминание через <b>15 мин</b>") || !strings.Contains(text, "Диагностики пока нет") {
			t.Fatal(text)
		}
	}
}
