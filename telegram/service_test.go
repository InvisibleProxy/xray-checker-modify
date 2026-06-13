package telegram

import (
	"strings"
	"testing"
	"time"

	"xray-checker/speedtest"
)

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
	for _, want := range []string{"low", "4.99 Mbps", "порог 5.00 Mbps", "low-id"} {
		if !strings.Contains(line, want) {
			t.Fatalf("low-speed issue line %q does not contain %q", line, want)
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
			name:     "manual issues mode skips without issues",
			cfg:      withReportMode(baseCfg, "issues"),
			source:   "manual",
			results:  []speedtest.Result{{Name: "fast", Mbps: 25}},
			wantSend: false,
		},
		{
			name:       "manual issues mode sends low speed",
			cfg:        withReportMode(baseCfg, "issues"),
			source:     "manual",
			results:    []speedtest.Result{{Name: "low", Mbps: 5}},
			wantSlow:   1,
			wantSend:   true,
			wantFailed: 0,
		},
		{
			name:           "schedule skips without issues even in always mode",
			cfg:            baseCfg,
			source:         "schedule",
			results:        []speedtest.Result{{Name: "fast", Mbps: 25}},
			wantIssuesOnly: true,
			wantSend:       false,
		},
		{
			name:           "schedule sends low speed as issues only",
			cfg:            baseCfg,
			source:         "schedule",
			results:        []speedtest.Result{{Name: "low", Mbps: 5}},
			wantSlow:       1,
			wantIssuesOnly: true,
			wantSend:       true,
		},
		{
			name:       "error sends in issues mode",
			cfg:        withReportMode(baseCfg, "issues"),
			source:     "manual",
			results:    []speedtest.Result{{Name: "failed", Error: "timeout"}},
			wantFailed: 1,
			wantSend:   true,
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
			name:           "threshold disabled means low speed is not an issue",
			cfg:            withLowSpeedThreshold(baseCfg, 0),
			source:         "schedule",
			results:        []speedtest.Result{{Name: "slow but threshold disabled", Mbps: 1}},
			wantIssuesOnly: true,
			wantSend:       false,
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

	text := service.formatSpeedReport(report, cfg, 0, 1, false)
	for _, want := range []string{
		"Speed-test завершен",
		"Низкая скорость: <b>1</b>",
		"Порог низкой скорости: <b>10.00 Mbps</b>",
		"⚠️ <b>5.00 Mbps</b> · порог 10.00 Mbps",
		"<b>LOW 5.00 Mbps</b>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("manual report does not contain %q:\n%s", want, text)
		}
	}

	report.Source = "schedule"
	text = service.formatSpeedReport(report, cfg, 0, 1, true)
	for _, want := range []string{
		"Speed-test по расписанию: проблемы",
		"Порог низкой скорости: <b>10.00 Mbps</b>",
		"⚠️ <b>5.00 Mbps</b> · порог 10.00 Mbps",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("schedule issues report does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Лучшие результаты") {
		t.Fatalf("schedule issues-only report should not include best results:\n%s", text)
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

func TestNormalizeMutedNodeIDs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MutedNodeIDs = []string{" b ", "", "a", "a"}
	cfg.Normalize()

	if got := strings.Join(cfg.MutedNodeIDs, ","); got != "a,b" {
		t.Fatalf("muted IDs = %q, want %q", got, "a,b")
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
