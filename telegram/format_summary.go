package telegram

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"xray-checker/checker"
	"xray-checker/speedtest"
)

// Report-only accounting; filtering must not make a partial run look complete.
// It does not change the measured results, notification gates or retry state.
type speedReportScope struct {
	Measured   int
	Muted      int
	Pending    int
	Suppressed int
}

func formatMbps(value float64) string {
	// Keep precision for small rates and non-integral thresholds, without
	// printing insignificant trailing zeroes for ordinary measurements.
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".") + " Мбит/с"
}

func (s *Service) notificationLabels(cfg Config) (alerts, reports string) {
	alerts, reports = "выключены", "выключены"
	if cfg.Enabled && cfg.NodeAlertsEnabled {
		alerts = "включены"
	}
	if cfg.Enabled && cfg.SpeedReportsEnabled && cfg.SpeedReportMode != "disabled" {
		reports = "все"
		if cfg.SpeedReportMode == "issues" {
			reports = "только проблемы"
		}
	}
	if s.ProjectMaintenanceEnabled() {
		if alerts != "выключены" {
			alerts = "пауза: обслуживание"
		}
		if reports != "выключены" {
			reports = "пауза: обслуживание"
		}
	}
	return
}

// A mute affects delivery, not whether a problem exists in an interactive view.
// Resolve the requested scope separately: a permanent alert mute may coexist
// with a temporary speed mute on the same node.
func (s *Service) muteNoteHTML(stableID string, cfg Config, scope string) string {
	permanent := mutedAlertNodeSet(cfg)
	if scope == muteScopeSpeed {
		permanent = mutedSpeedNodeSet(cfg)
	}
	if permanent[stableID] {
		return " · 🔕 уведомления выключены"
	}
	s.mu.RLock()
	mute, ok := s.mutes[stableID]
	s.mu.RUnlock()
	if ok && mute.Until.After(time.Now()) && (mute.Scope == muteScopeAll || mute.Scope == scope) {
		return " · 🔕 до " + htmlEscape(formatCheckedAt(mute.Until))
	}
	return ""
}

// Budget whole node blocks, including their agent evidence. Both renderings
// use this selection, so fallback never splits an agent from its node or
// counts diagnostic lines as additional nodes.
func visibleSpeedResults(results []speedtest.Result, threshold float64, limit, budget int) []speedtest.Result {
	visible := limitResults(results, limit)
	used := 0
	for i, result := range visible {
		block := formatSpeedResultHTML(result, threshold) + formatSpeedAgentDiagnosticHTML(result.AgentDiagnostic)
		used += utf8.RuneCountInString(block) + 16
		if used > budget {
			return visible[:i]
		}
	}
	return visible
}

func speedHiddenHTML(total, shown int) string {
	if total <= shown {
		return ""
	}
	return fmt.Sprintf("Ещё нод: %d · откройте «Все замеры»", total-shown)
}

func speedReportScopeHTML(scope speedReportScope, shown int) string {
	if scope.Measured <= shown {
		return ""
	}
	parts := []string{fmt.Sprintf("Всего результатов: %d", scope.Measured)}
	if scope.Pending > 0 {
		parts = append(parts, fmt.Sprintf("ждут подтверждения: %d", scope.Pending))
	}
	if scope.Muted > 0 {
		parts = append(parts, fmt.Sprintf("заглушены: %d", scope.Muted))
	}
	if scope.Suppressed > 0 {
		parts = append(parts, fmt.Sprintf("успешный резервный URL: %d", scope.Suppressed))
	}
	return strings.Join(parts, " · ")
}

func buildSpeedReport(report speedtest.RunReport, cfg Config, issuesOnly bool, scopes []speedReportScope) formattedMessage {
	failed, slow, healthy := groupSpeedResults(report.Results, cfg.LowSpeedThresholdMbps)
	issues := append(append([]speedtest.Result{}, failed...), slow...)
	visibleIssues := visibleSpeedResults(issues, cfg.LowSpeedThresholdMbps, cfg.SpeedReportLimit, 2200)
	visibleHealthy := visibleSpeedResults(healthy, cfg.LowSpeedThresholdMbps, cfg.SpeedReportLimit, 600)
	title := speedReportTitle(report.Source, issuesOnly)
	countLabel := "Проверено"
	scopeText := ""
	if len(scopes) > 0 {
		scopeText = speedReportScopeHTML(scopes[0], len(report.Results))
		if scopeText != "" {
			countLabel = "В отчёте"
		}
	}
	summary := fmt.Sprintf("%s: <b>%d</b> · В норме: <b>%d</b> · Ниже порога: <b>%d</b> · Ошибки: <b>%d</b>", countLabel, len(report.Results), len(healthy), len(slow), len(failed))
	stamp := htmlEscape(reportSourceLabel(report.Source)) + " · " + htmlEscape(formatCheckedAt(report.FinishedAt))
	lines := []string{"<b>" + htmlEscape(title) + "</b>", stamp, summary}
	var rich strings.Builder
	fmt.Fprintf(&rich, "<h2>%s</h2><p>%s</p><p>%s</p>", htmlEscape(title), stamp, summary)
	if scopeText != "" {
		lines = append(lines, scopeText)
		fmt.Fprintf(&rich, "<p>%s</p>", scopeText)
	}
	if report.Skipped > 0 {
		skipped := fmt.Sprintf("Пропущено без замера: <b>%d</b> · нода стала недоступна до своей очереди", report.Skipped)
		lines = append(lines, skipped)
		fmt.Fprintf(&rich, "<p>%s</p>", skipped)
	}
	if len(issues) > 0 {
		lines = append(lines, "", "<b>Требуют внимания</b>")
		rich.WriteString("<h3>Требуют внимания</h3><ul>")
		for _, result := range visibleIssues {
			lines = append(lines, speedIssuesHTML([]speedtest.Result{result}, cfg.LowSpeedThresholdMbps)...)
			rich.WriteString(formatSpeedIssueRichItem(result, cfg.LowSpeedThresholdMbps))
		}
		if hidden := speedHiddenHTML(len(issues), len(visibleIssues)); hidden != "" {
			lines = append(lines, hidden)
			fmt.Fprintf(&rich, "<li>%s</li>", hidden)
		}
		rich.WriteString("</ul>")
		rich.WriteString(formatSpeedDiagnosticsRichDetails(visibleIssues, 0))
	}
	if !issuesOnly && len(healthy) > 0 {
		lines = append(lines, "", fmt.Sprintf("<b>В норме: %d</b>", len(healthy)))
		fmt.Fprintf(&rich, "<details><summary>В норме: %d</summary><ul>", len(healthy))
		for _, result := range visibleHealthy {
			line := formatSpeedResultHTML(result, cfg.LowSpeedThresholdMbps)
			lines = append(lines, line)
			fmt.Fprintf(&rich, "<li>%s</li>", strings.TrimPrefix(line, "• "))
		}
		if hidden := speedHiddenHTML(len(healthy), len(visibleHealthy)); hidden != "" {
			lines = append(lines, hidden)
			fmt.Fprintf(&rich, "<li>%s</li>", hidden)
		}
		rich.WriteString("</ul></details>")
	}
	actions := visibleIssues
	if len(issues) == 0 && !issuesOnly {
		actions = visibleHealthy
	}
	return formattedMessage{
		HTML:        trimHTMLMessage(strings.Join(lines, "\n")),
		RichHTML:    rich.String(),
		ReplyMarkup: speedReportMarkup(actions),
	}
}

func (s *Service) buildIssuesSummary() formattedMessage {
	cfg := s.Config()
	var availability, richAvailability []string
	for _, proxy := range s.sortedProxies() {
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil {
			details.Status = checker.AvailabilityStateOffline
		}
		if details.EffectiveStatus() == checker.AvailabilityStateOnline {
			continue
		}
		note := s.muteNoteHTML(proxy.StableID, cfg, muteScopeAlerts)
		availability = append(availability, formatProxyLineHTML(proxy, details)+note)
		richAvailability = append(richAvailability, strings.TrimSuffix(formatProxyRichItem(proxy, details), "</li>")+note+"</li>")
	}
	var speedResults []speedtest.Result
	if s.speedManager != nil {
		speedResults = speedIssueResults(s.activeSpeedResults(s.speedManager.Snapshot().Results), cfg.LowSpeedThresholdMbps)
	}
	lines := []string{"<b>Проблемные ноды</b>"}
	var rich strings.Builder
	rich.WriteString("<h2>Проблемные ноды</h2>")
	if len(availability) == 0 && len(speedResults) == 0 {
		lines = append(lines, "Проблем не найдено.")
		rich.WriteString("<p>Проблем не найдено.</p>")
	} else {
		summary := fmt.Sprintf("Доступность: <b>%d</b> · Скорость: <b>%d</b>", len(availability), len(speedResults))
		lines = append(lines, summary)
		fmt.Fprintf(&rich, "<p>%s</p>", summary)
	}
	if len(availability) > 0 {
		lines = append(lines, "", "<b>Доступность</b>")
		rich.WriteString("<h3>Доступность</h3><ul>")
		used, shown := 0, 0
		for i, line := range availability {
			used += utf8.RuneCountInString(line) + 1
			if i >= 12 || used > 1100 {
				break
			}
			lines = append(lines, line)
			rich.WriteString(richAvailability[i])
			shown++
		}
		if shown < len(availability) {
			hidden := fmt.Sprintf("Ещё нод: %d · откройте «Все статусы»", len(availability)-shown)
			lines = append(lines, hidden)
			fmt.Fprintf(&rich, "<li>%s</li>", hidden)
		}
		rich.WriteString("</ul>")
	}
	if len(speedResults) > 0 {
		lines = append(lines, "", "<b>Скорость</b>")
		rich.WriteString("<h3>Скорость</h3><ul>")
		visible := visibleSpeedResults(speedResults, cfg.LowSpeedThresholdMbps, cfg.SpeedReportLimit, 1500)
		used, shown := 0, 0
		for _, result := range visible {
			note := s.muteNoteHTML(result.StableID, cfg, muteScopeSpeed)
			block := speedIssuesHTML([]speedtest.Result{result}, cfg.LowSpeedThresholdMbps)[0]
			used += utf8.RuneCountInString(block+note) + 1
			if used > 1500 {
				break
			}
			shown++
			lines = append(lines, block+note)
			rich.WriteString(strings.TrimSuffix(formatSpeedIssueRichItem(result, cfg.LowSpeedThresholdMbps), "</li>") + note + "</li>")
		}
		visible = visible[:shown]
		if hidden := speedHiddenHTML(len(speedResults), len(visible)); hidden != "" {
			lines = append(lines, hidden)
			fmt.Fprintf(&rich, "<li>%s</li>", hidden)
		}
		rich.WriteString("</ul>")
		rich.WriteString(formatSpeedDiagnosticsRichDetails(visible, 0))
	}
	return formattedMessage{HTML: trimHTMLMessage(strings.Join(lines, "\n")), RichHTML: rich.String()}
}
