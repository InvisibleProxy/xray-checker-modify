package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

func (s *Service) formatHelp(cfg Config) string {
	var lines []string
	lines = append(lines,
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Команды</b>",
		"Главное управление доступно через кнопки под сообщением.",
		"",
		"• <code>/start</code> — открыть главное меню",
		"• <code>/status</code> — статусы нод",
		"• <code>/speed &lt;id или имя&gt;</code> — история замеров ноды",
		"• <code>/id</code> — ID чата, топика и пользователя",
	)
	if len(cfg.AdminUserIDs) > 0 {
		lines = append(lines,
			"• <code>/speedtest</code> — speed-test доступных нод",
			"• <code>/speedtest all</code> — speed-test всех нод",
			"• <code>/speedtest &lt;id или имя&gt;</code> — speed-test одной ноды",
		)
	}
	return strings.Join(lines, "\n")
}

func (s *Service) formatHelpMessage(cfg Config) formattedMessage {
	fallback := s.formatHelp(cfg)
	items := []string{
		"<li><code>/start</code> — главное меню</li>",
		"<li><code>/status</code> — состояние нод</li>",
		"<li><code>/speed &lt;ID или имя&gt;</code> — история замеров</li>",
		"<li><code>/id</code> — ID чата, топика и пользователя</li>",
	}
	if len(cfg.AdminUserIDs) > 0 {
		items = append(items,
			"<li><code>/speedtest</code> — проверить доступные ноды</li>",
			"<li><code>/speedtest all</code> — проверить все ноды</li>",
			"<li><code>/speedtest &lt;ID или имя&gt;</code> — проверить одну ноду</li>",
		)
	}
	rich := "<h2>Команды</h2><p>Основное управление — кнопками под сообщением.</p><ul>" + strings.Join(items, "") + "</ul>"
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func (s *Service) formatMenu(cfg Config, isAdmin bool) string {
	total, online, proxyFailures, offline := s.nodeCounts()
	speedReports := "выключены"
	if cfg.SpeedReportsEnabled && cfg.SpeedReportMode != "disabled" {
		speedReports = "включены"
		if cfg.SpeedReportMode == "issues" {
			speedReports = "только проблемы"
		}
	}
	alerts := "выключены"
	if cfg.NodeAlertsEnabled {
		alerts = "включены"
	}
	thresholdText := "не задан"
	if cfg.LowSpeedThresholdMbps > 0 {
		thresholdText = fmt.Sprintf("%.2f Mbps", cfg.LowSpeedThresholdMbps)
	}
	adminHint := ""
	if isAdmin {
		adminHint = " · доступны ручные проверки"
	}

	return strings.Join([]string{
		"<b>InvisibleProxyChecker</b>",
		"",
		fmt.Sprintf("🟢 <b>%d</b> из %d · 🟡 <b>%d</b> · 🔴 <b>%d</b>", online, total, proxyFailures, offline),
		fmt.Sprintf("⚡ Отчёты: <b>%s</b> · порог %s", htmlEscape(speedReports), htmlEscape(thresholdText)),
		fmt.Sprintf("🔔 Алерты: <b>%s</b>%s", htmlEscape(alerts), htmlEscape(adminHint)),
		"",
		"Выберите раздел:",
	}, "\n")
}

func (s *Service) formatMenuMessage(cfg Config, isAdmin bool) formattedMessage {
	fallback := s.formatMenu(cfg, isAdmin)
	total, online, proxyFailures, offline := s.nodeCounts()
	speedReports := "выключены"
	if cfg.SpeedReportsEnabled && cfg.SpeedReportMode != "disabled" {
		speedReports = "все"
		if cfg.SpeedReportMode == "issues" {
			speedReports = "только проблемы"
		}
	}
	threshold := "не задан"
	if cfg.LowSpeedThresholdMbps > 0 {
		threshold = fmt.Sprintf("%.2f Mbps", cfg.LowSpeedThresholdMbps)
	}
	alerts := "выключены"
	if cfg.NodeAlertsEnabled {
		alerts = "включены"
	}

	rich := strings.Join([]string{
		"<h2>InvisibleProxyChecker</h2>",
		"<table bordered>",
		"<tr><th>Ноды</th><td>🟢 " + strconv.Itoa(online) + " / " + strconv.Itoa(total) + " · 🟡 " + strconv.Itoa(proxyFailures) + " · 🔴 " + strconv.Itoa(offline) + "</td></tr>",
		"<tr><th>Speed-test</th><td>" + htmlEscape(speedReports) + " · " + htmlEscape(threshold) + "</td></tr>",
		"<tr><th>Алерты</th><td>" + htmlEscape(alerts) + "</td></tr>",
		"</table>",
		"<footer>Выберите раздел кнопкой ниже.</footer>",
	}, "")
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func (s *Service) formatNodeList() string {
	proxies := s.sortedProxies()
	total, online, proxyFailures, offline := s.nodeCounts()
	lines := []string{
		"<b>Ноды</b>",
		fmt.Sprintf("🟢 <b>%d</b> из %d · 🟡 <b>%d</b> · 🔴 <b>%d</b>", online, total, proxyFailures, offline),
		"",
		"Выберите ноду кнопкой ниже, чтобы открыть статус, последние замеры и действия.",
	}
	if len(proxies) == 0 {
		lines = append(lines, "", "Ноды не найдены.")
	}
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatNodeListMessage() formattedMessage {
	fallback := s.formatNodeList()
	total, online, proxyFailures, offline := s.nodeCounts()
	rich := fmt.Sprintf(
		"<h2>Ноды</h2><p>🟢 <b>%d</b> из %d · 🟡 <b>%d</b> · 🔴 <b>%d</b></p><footer>Откройте ноду кнопкой ниже — там статус, замеры и действия.</footer>",
		online,
		total,
		proxyFailures,
		offline,
	)
	if total == 0 {
		rich = "<h2>Ноды</h2><p>Список пуст.</p>"
	}
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func (s *Service) formatNodeDetails(stableID string) string {
	proxy, matches := s.findProxy(stableID)
	if proxy == nil {
		return formatProxySearchMiss(matches)
	}
	details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	availabilityStatus := checker.AvailabilityStateOffline
	if err == nil {
		availabilityStatus = details.EffectiveStatus()
	}
	status := "🟢 доступна"
	if availabilityStatus == checker.AvailabilityStateProxyFailure {
		status = "🟡 proxy failure"
	} else if availabilityStatus == checker.AvailabilityStateOffline {
		status = "🔴 недоступна"
	}
	latencyText := "—"
	if details.Latency > 0 {
		latencyText = fmt.Sprintf("%d ms", details.Latency.Milliseconds())
	}

	lines := []string{
		fmt.Sprintf("<b>%s</b>", htmlEscape(proxy.Name)),
		fmt.Sprintf("ID: %s", htmlCode(proxy.StableID)),
		fmt.Sprintf("Статус: <b>%s</b> · %s", htmlEscape(status), htmlEscape(latencyText)),
		fmt.Sprintf("Протокол: %s", htmlCode(proxy.Protocol)),
	}
	if availabilityStatus == checker.AvailabilityStateOffline && !details.DownSince.IsZero() {
		lines = append(lines, fmt.Sprintf("Недоступна с: <b>%s</b>", htmlEscape(formatCheckedAt(details.DownSince))))
		lines = append(lines, fmt.Sprintf("Простой: <b>%s</b>", htmlEscape(formatDuration(time.Since(details.DownSince)))))
	}
	if availabilityStatus == checker.AvailabilityStateProxyFailure && !details.ProxyFailureSince.IsZero() {
		lines = append(lines, fmt.Sprintf("Proxy failure с: <b>%s</b>", htmlEscape(formatCheckedAt(details.ProxyFailureSince))))
		lines = append(lines, fmt.Sprintf("Длительность proxy failure: <b>%s</b>", htmlEscape(formatDuration(time.Since(details.ProxyFailureSince)))))
	}
	if availabilityStatus != checker.AvailabilityStateOnline {
		if failure := formatFailureHTML(details.Failure); failure != "" {
			lines = append(lines, failure)
		}
		if diagnostics := formatHostDiagnosticsHTML(details.HostCheck, details.PingCheck); diagnostics != "" {
			lines = append(lines, fmt.Sprintf("Диагностика: %s", diagnostics))
		}
	}
	if proxy.SubName != "" {
		lines = append(lines, fmt.Sprintf("Подписка: <b>%s</b>", htmlEscape(proxy.SubName)))
	}
	if proxy.Server != "" {
		lines = append(lines, fmt.Sprintf("Сервер: %s", htmlCode(fmt.Sprintf("%s:%d", proxy.Server, proxy.Port))))
	}

	history := s.speedManager.ResultHistory(proxy.StableID)
	if len(history) == 0 {
		if result := s.latestSpeedResult(proxy.StableID); result != nil {
			history = []speedtest.Result{*result}
		}
	}
	lines = append(lines, "", "<b>Последние замеры</b>")
	if len(history) == 0 {
		lines = append(lines, "Пока нет результатов speed-test.")
	} else {
		cfg := s.Config()
		for _, result := range limitResults(history, 5) {
			lines = append(lines, formatSpeedHistoryLine(result, cfg.LowSpeedThresholdMbps))
		}
	}
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatNodeDetailsMessage(stableID string) formattedMessage {
	fallback := s.formatNodeDetails(stableID)
	proxy, matches := s.findProxy(stableID)
	if proxy == nil {
		return formattedMessage{HTML: fallback, RichHTML: formatProxySearchMissRich(matches)}
	}

	details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	availabilityStatus := checker.AvailabilityStateOffline
	if err == nil {
		availabilityStatus = details.EffectiveStatus()
	}
	status := "🔴 недоступна"
	if availabilityStatus == checker.AvailabilityStateOnline {
		status = "🟢 доступна"
	} else if availabilityStatus == checker.AvailabilityStateProxyFailure {
		status = "🟡 proxy failure"
	}
	latency := "—"
	if details.Latency > 0 {
		latency = fmt.Sprintf("%d ms", details.Latency.Milliseconds())
	}

	var rich strings.Builder
	fmt.Fprintf(&rich, "<h2>%s</h2>", htmlEscape(proxy.Name))
	fmt.Fprintf(&rich, "<p><b>%s</b> · %s · %s</p>", htmlEscape(status), htmlEscape(strings.ToUpper(proxy.Protocol)), htmlEscape(latency))
	if availabilityStatus == checker.AvailabilityStateOffline && !details.DownSince.IsZero() {
		fmt.Fprintf(&rich, "<p>Простой: <b>%s</b> · с %s</p>", htmlEscape(formatDuration(time.Since(details.DownSince))), htmlEscape(formatCheckedAt(details.DownSince)))
	}
	if availabilityStatus == checker.AvailabilityStateProxyFailure && !details.ProxyFailureSince.IsZero() {
		fmt.Fprintf(&rich, "<p>Proxy failure: <b>%s</b> · с %s</p>", htmlEscape(formatDuration(time.Since(details.ProxyFailureSince))), htmlEscape(formatCheckedAt(details.ProxyFailureSince)))
	}
	if availabilityStatus != checker.AvailabilityStateOnline {
		if failure := formatFailureHTML(details.Failure); failure != "" {
			fmt.Fprintf(&rich, "<p>%s</p>", failure)
		}
		if diagnostics := formatHostDiagnosticsHTML(details.HostCheck, details.PingCheck); diagnostics != "" {
			fmt.Fprintf(&rich, "<blockquote>%s</blockquote>", diagnostics)
		}
	}

	rich.WriteString("<details><summary>Технические данные</summary><table bordered>")
	fmt.Fprintf(&rich, "<tr><th>StableID</th><td><code>%s</code></td></tr>", htmlEscape(proxy.StableID))
	if proxy.SubName != "" {
		fmt.Fprintf(&rich, "<tr><th>Подписка</th><td>%s</td></tr>", htmlEscape(proxy.SubName))
	}
	if proxy.Server != "" {
		fmt.Fprintf(&rich, "<tr><th>Сервер</th><td><code>%s</code></td></tr>", htmlEscape(fmt.Sprintf("%s:%d", proxy.Server, proxy.Port)))
	}
	rich.WriteString("</table></details>")

	history := s.speedManager.ResultHistory(proxy.StableID)
	if len(history) == 0 {
		if result := s.latestSpeedResult(proxy.StableID); result != nil {
			history = []speedtest.Result{*result}
		}
	}
	if len(history) == 0 {
		rich.WriteString("<h3>Последние замеры</h3><p>Результатов пока нет.</p>")
	} else {
		cfg := s.Config()
		rich.WriteString("<h3>Последние замеры</h3>")
		rich.WriteString(formatSpeedHistoryRichTable(limitResults(history, 5), cfg.LowSpeedThresholdMbps))
	}

	return formattedMessage{HTML: fallback, RichHTML: rich.String()}
}

func (s *Service) nodeCounts() (total int, online int, proxyFailures int, offline int) {
	proxies := s.sortedProxies()
	total = len(proxies)
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil || details.EffectiveStatus() == checker.AvailabilityStateOffline {
			offline++
			continue
		}
		if details.EffectiveStatus() == checker.AvailabilityStateProxyFailure {
			proxyFailures++
			continue
		}
		online++
	}
	return total, online, proxyFailures, offline
}

func (s *Service) formatStatus() string {
	proxies := s.sortedProxies()

	var onlineLines []string
	var issueLines []string
	proxyFailures := 0
	offline := 0
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil {
			details.Status = checker.AvailabilityStateOffline
		}
		line := formatProxyLineHTML(proxy, details)
		switch details.EffectiveStatus() {
		case checker.AvailabilityStateOnline:
			onlineLines = append(onlineLines, line)
		case checker.AvailabilityStateProxyFailure:
			proxyFailures++
			issueLines = append(issueLines, line)
		default:
			offline++
			issueLines = append(issueLines, line)
		}
	}

	lines := []string{
		"<b>Статусы нод</b>",
		fmt.Sprintf("🟢 <b>%d</b> из %d · 🟡 <b>%d</b> · 🔴 <b>%d</b>", len(onlineLines), len(proxies), proxyFailures, offline),
	}
	if len(issueLines) > 0 {
		lines = append(lines, "", "<b>Требуют внимания</b>")
		lines = append(lines, limitLines(issueLines, 12)...)
	}
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatStatusMessage() formattedMessage {
	fallback := s.formatStatus()
	proxies := s.sortedProxies()
	var onlineItems []string
	var issueItems []string
	proxyFailures := 0
	offline := 0
	for _, proxy := range proxies {
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil {
			details.Status = checker.AvailabilityStateOffline
		}
		item := formatProxyRichItem(proxy, details)
		switch details.EffectiveStatus() {
		case checker.AvailabilityStateOnline:
			onlineItems = append(onlineItems, item)
		case checker.AvailabilityStateProxyFailure:
			proxyFailures++
			issueItems = append(issueItems, item)
		default:
			offline++
			issueItems = append(issueItems, item)
		}
	}

	var rich strings.Builder
	rich.WriteString("<h2>Статусы нод</h2>")
	fmt.Fprintf(&rich, "<p>🟢 <b>%d</b> из %d · 🟡 <b>%d</b> · 🔴 <b>%d</b></p>", len(onlineItems), len(proxies), proxyFailures, offline)
	if len(issueItems) > 0 {
		rich.WriteString("<h3>Требуют внимания</h3><ul>")
		rich.WriteString(strings.Join(limitRichItems(issueItems, 12), ""))
		rich.WriteString("</ul>")
	}
	if len(onlineItems) > 0 {
		fmt.Fprintf(&rich, "<details><summary>Доступны: %d</summary><ul>", len(onlineItems))
		rich.WriteString(strings.Join(limitRichItems(onlineItems, 20), ""))
		rich.WriteString("</ul></details>")
	}
	return formattedMessage{HTML: fallback, RichHTML: rich.String()}
}

func formatStatusRefreshStarted() string {
	return strings.Join([]string{
		"<b>Статусы нод</b>",
		"Проверяю доступность нод. Сообщение обновится после завершения.",
	}, "\n")
}

func formatStatusRefreshStartedMessage() formattedMessage {
	return formattedMessage{
		HTML:     formatStatusRefreshStarted(),
		RichHTML: "<h2>Статусы нод</h2><p>Проверяю доступность. Это сообщение обновится автоматически.</p>",
	}
}

func formatNodeAvailabilityCheckStartedMessage(proxy *models.ProxyConfig) formattedMessage {
	name := "Нода"
	if proxy != nil && strings.TrimSpace(proxy.Name) != "" {
		name = proxy.Name
	}
	return formattedMessage{
		HTML:     fmt.Sprintf("<b>%s</b>\nПроверяю доступность, TCP и ping…", htmlEscape(name)),
		RichHTML: fmt.Sprintf("<h2>%s</h2><p>Проверяю доступность, TCP и ping…</p>", htmlEscape(name)),
	}
}

func (s *Service) formatIssuesSummary() string {
	cfg := s.Config()
	muted := mutedAlertNodeSet(cfg)
	var issueLines []string
	for _, proxy := range s.sortedProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if muted[proxy.StableID] {
			continue
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil {
			details.Status = checker.AvailabilityStateOffline
		}
		if details.EffectiveStatus() != checker.AvailabilityStateOnline {
			issueLines = append(issueLines, formatProxyLineHTML(proxy, details))
		}
	}

	speedLines := speedIssuesHTML(filterMutedSpeedResults(s.activeSpeedResults(s.speedManager.Snapshot().Results), cfg), cfg.LowSpeedThresholdMbps)
	lines := []string{
		"<b>Проблемные ноды</b>",
	}
	if len(issueLines) == 0 && len(speedLines) == 0 {
		lines = append(lines, "", "Проблем не найдено.")
		return strings.Join(lines, "\n")
	}
	if len(issueLines) > 0 {
		lines = append(lines, "", "<b>Проблемы доступности</b>")
		lines = append(lines, limitLines(issueLines, 12)...)
	}
	if len(speedLines) > 0 {
		lines = append(lines, "", "<b>Speed-test ниже порога или с ошибками</b>")
		lines = append(lines, limitLines(speedLines, cfg.SpeedReportLimit)...)
	}
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatIssuesSummaryMessage() formattedMessage {
	fallback := s.formatIssuesSummary()
	cfg := s.Config()
	muted := mutedAlertNodeSet(cfg)
	var issueItems []string
	for _, proxy := range s.sortedProxies() {
		if muted[proxy.StableID] {
			continue
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil {
			details.Status = checker.AvailabilityStateOffline
		}
		if details.EffectiveStatus() != checker.AvailabilityStateOnline {
			issueItems = append(issueItems, formatProxyRichItem(proxy, details))
		}
	}
	speedResults := speedIssueResults(filterMutedSpeedResults(s.activeSpeedResults(s.speedManager.Snapshot().Results), cfg), cfg.LowSpeedThresholdMbps)

	var rich strings.Builder
	rich.WriteString("<h2>Проблемные ноды</h2>")
	if len(issueItems) == 0 && len(speedResults) == 0 {
		rich.WriteString("<p>✅ Проблем не найдено.</p>")
		return formattedMessage{HTML: fallback, RichHTML: rich.String()}
	}
	fmt.Fprintf(&rich, "<p>⚠️ Доступность: <b>%d</b> · ⚡ Speed-test: <b>%d</b></p>", len(issueItems), len(speedResults))
	if len(issueItems) > 0 {
		rich.WriteString("<h3>Проблемы доступности</h3><ul>")
		rich.WriteString(strings.Join(limitRichItems(issueItems, 12), ""))
		rich.WriteString("</ul>")
	}
	if len(speedResults) > 0 {
		rich.WriteString("<h3>Speed-test</h3><ul>")
		for _, result := range limitResults(speedResults, cfg.SpeedReportLimit) {
			rich.WriteString(formatSpeedIssueRichItem(result, cfg.LowSpeedThresholdMbps))
		}
		rich.WriteString("</ul>")
		rich.WriteString(formatSpeedDiagnosticsRichDetails(speedResults, cfg.SpeedReportLimit))
	}
	return formattedMessage{HTML: fallback, RichHTML: rich.String()}
}
