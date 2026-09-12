package telegram

import (
	"fmt"
	"sort"
	"strings"

	"xray-checker/checker"
	"xray-checker/speedtest"
)

func (s *Service) formatSpeedHistory(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "<b>История замеров</b>\n\nИспользование: <code>/speed &lt;id или имя&gt;</code>"
	}

	proxy, matches := s.findProxy(query)
	if proxy == nil {
		return formatProxySearchMiss(matches)
	}

	history := s.speedManager.ResultHistory(proxy.StableID)
	if len(history) == 0 {
		for _, result := range s.speedManager.Snapshot().Results {
			if result.StableID == proxy.StableID {
				history = []speedtest.Result{result}
				break
			}
		}
	}
	if len(history) == 0 {
		return fmt.Sprintf("<b>История замеров</b>\n\nДля ноды <b>%s</b> пока нет результатов speed-test.", htmlEscape(proxy.Name))
	}

	lines := []string{
		"<b>История замеров</b>",
		fmt.Sprintf("<b>%s</b>", htmlEscape(proxy.Name)),
	}
	cfg := s.Config()
	for _, result := range limitResults(history, 5) {
		lines = append(lines, formatSpeedHistoryLine(result, cfg.LowSpeedThresholdMbps))
	}
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatSpeedHistoryMessage(query string) formattedMessage {
	fallback := s.formatSpeedHistory(query)
	query = strings.TrimSpace(query)
	if query == "" {
		return formattedMessage{HTML: fallback, RichHTML: "<h2>История замеров</h2><p>Укажите <code>/speed &lt;ID или имя&gt;</code>.</p>"}
	}
	proxy, matches := s.findProxy(query)
	if proxy == nil {
		return formattedMessage{HTML: fallback, RichHTML: formatProxySearchMissRich(matches)}
	}
	history := s.speedManager.ResultHistory(proxy.StableID)
	if len(history) == 0 {
		if result := s.latestSpeedResult(proxy.StableID); result != nil {
			history = []speedtest.Result{*result}
		}
	}
	if len(history) == 0 {
		return formattedMessage{
			HTML:     fallback,
			RichHTML: fmt.Sprintf("<h2>История замеров</h2><p><b>%s</b>: результатов пока нет.</p>", htmlEscape(proxy.Name)),
		}
	}
	cfg := s.Config()
	rich := fmt.Sprintf("<h2>История замеров</h2><p><b>%s</b></p>%s<details><summary>StableID</summary><p><code>%s</code></p></details>",
		htmlEscape(proxy.Name),
		formatSpeedHistoryRichTable(limitResults(history, 5), cfg.LowSpeedThresholdMbps),
		htmlEscape(proxy.StableID),
	)
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func (s *Service) formatRecentSpeedOverview() string {
	results := s.activeSpeedResults(s.speedManager.Snapshot().Results)
	if len(results) == 0 {
		return "<b>Замеры</b>\n\nПока нет результатов speed-test."
	}

	cfg := s.Config()
	failed, slow, healthy := groupSpeedResults(results, cfg.LowSpeedThresholdMbps)
	lines := []string{
		"<b>Замеры</b>",
		fmt.Sprintf("✅ В норме: <b>%d</b> · ⚠️ Низкая: <b>%d</b> · ❌ Ошибки: <b>%d</b>", len(healthy), len(slow), len(failed)),
	}
	appendGroup := func(title string, group []speedtest.Result) {
		if len(group) == 0 {
			return
		}
		lines = append(lines, "", title)
		for _, result := range group {
			lines = append(lines, fmt.Sprintf("• <b>%s</b> · %s · %s",
				htmlEscape(result.Name),
				speedResultStatusHTML(result, cfg.LowSpeedThresholdMbps),
				htmlEscape(formatCheckedAt(result.CheckedAt)),
			))
		}
	}
	appendGroup("<b>Ошибки</b>", failed)
	appendGroup("<b>Низкая скорость</b>", slow)
	appendGroup("<b>В норме</b>", healthy)

	lines = append(lines, "", "Нажмите на ноду ниже, чтобы открыть историю.")
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func speedResultStatusHTML(result speedtest.Result, threshold float64) string {
	return formatSpeedStatusRich(result, threshold)
}

func (s *Service) formatRecentSpeedOverviewMessage() formattedMessage {
	fallback := s.formatRecentSpeedOverview()
	results := s.activeSpeedResults(s.speedManager.Snapshot().Results)
	if len(results) == 0 {
		return formattedMessage{HTML: fallback, RichHTML: "<h2>Замеры</h2><p>Результатов пока нет.</p>"}
	}

	cfg := s.Config()
	failed, slow, healthy := groupSpeedResults(results, cfg.LowSpeedThresholdMbps)

	var rich strings.Builder
	rich.WriteString("<h2>Замеры</h2>")
	fmt.Fprintf(&rich, "<p>✅ В норме: <b>%d</b> · ⚠️ Низкая: <b>%d</b> · ❌ Ошибки: <b>%d</b></p>", len(healthy), len(slow), len(failed))
	writeSpeedGroupTable(&rich, "Ошибки", failed, cfg.LowSpeedThresholdMbps)
	writeSpeedGroupTable(&rich, "Низкая скорость", slow, cfg.LowSpeedThresholdMbps)
	// Nodes that are simply fine are the bulk of the list and the least worth
	// scrolling past, so they stay collapsed behind their own count.
	if len(healthy) > 0 {
		fmt.Fprintf(&rich, "<details><summary>В норме: %d</summary>", len(healthy))
		writeSpeedResultRows(&rich, healthy, cfg.LowSpeedThresholdMbps)
		rich.WriteString("</details>")
	}
	rich.WriteString("<footer>Откройте ноду ниже, чтобы посмотреть историю.</footer>")
	return formattedMessage{HTML: fallback, RichHTML: rich.String()}
}

func writeSpeedGroupTable(rich *strings.Builder, title string, group []speedtest.Result, threshold float64) {
	if len(group) == 0 {
		return
	}
	fmt.Fprintf(rich, "<h3>%s</h3>", htmlEscape(title))
	writeSpeedResultRows(rich, group, threshold)
}

func writeSpeedResultRows(rich *strings.Builder, group []speedtest.Result, threshold float64) {
	rich.WriteString("<table bordered striped><tr><th>Нода</th><th>Результат</th><th>Время</th></tr>")
	for _, result := range group {
		fmt.Fprintf(rich, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>",
			htmlEscape(result.Name),
			formatSpeedStatusRich(result, threshold),
			htmlEscape(formatCheckedAt(result.CheckedAt)),
		)
	}
	rich.WriteString("</table>")
}

func (s *Service) formatSpeedReport(report speedtest.RunReport, cfg Config, failed int, slow int, issuesOnly bool, scopes ...speedReportScope) string {
	return buildSpeedReport(report, cfg, issuesOnly, scopes).HTML
}

func (s *Service) formatSpeedReportMessage(report speedtest.RunReport, cfg Config, failed int, slow int, issuesOnly bool, scopes ...speedReportScope) formattedMessage {
	return buildSpeedReport(report, cfg, issuesOnly, scopes)
}

func speedIssuesHTML(results []speedtest.Result, threshold float64) []string {
	var blocks []string
	for _, result := range speedIssueResults(results, threshold) {
		block := formatSpeedResultHTML(result, threshold)
		if diagnostic := formatSpeedAgentDiagnosticHTML(result.AgentDiagnostic); diagnostic != "" {
			block += "\n  ↳ " + diagnostic
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// Speed result classes, ordered so the worst reads first.
const (
	speedClassFailed = iota
	speedClassSlow
	speedClassHealthy
)

// resultThreshold answers "what counted as slow for this measurement". The
// result carries the threshold it was judged against — the node's own override
// when it has one — so a later reader reaches the same verdict as the run did.
// The passed threshold is the fallback for history written before results
// recorded one.
func resultThreshold(result speedtest.Result, threshold float64) float64 {
	if result.LowSpeedThresholdMbps > 0 {
		return result.LowSpeedThresholdMbps
	}
	return threshold
}

func speedResultClass(result speedtest.Result, threshold float64) int {
	threshold = resultThreshold(result, threshold)
	switch {
	case result.Offline || result.Error != "":
		return speedClassFailed
	case threshold > 0 && result.Mbps < threshold:
		return speedClassSlow
	default:
		return speedClassHealthy
	}
}

// groupSpeedResults orders results by what needs attention rather than by
// clock time, so the problems are on screen — and on the first page of
// buttons — before the nodes that are simply fine.
func groupSpeedResults(results []speedtest.Result, threshold float64) (failed []speedtest.Result, slow []speedtest.Result, healthy []speedtest.Result) {
	byRecency := append([]speedtest.Result(nil), results...)
	sort.SliceStable(byRecency, func(i, j int) bool {
		return byRecency[i].CheckedAt.After(byRecency[j].CheckedAt)
	})
	for _, result := range byRecency {
		switch speedResultClass(result, threshold) {
		case speedClassFailed:
			failed = append(failed, result)
		case speedClassSlow:
			slow = append(slow, result)
		default:
			healthy = append(healthy, result)
		}
	}
	return failed, slow, healthy
}

func orderedSpeedResults(results []speedtest.Result, threshold float64) []speedtest.Result {
	failed, slow, healthy := groupSpeedResults(results, threshold)
	ordered := make([]speedtest.Result, 0, len(results))
	ordered = append(ordered, failed...)
	ordered = append(ordered, slow...)
	return append(ordered, healthy...)
}

func speedIssueResults(results []speedtest.Result, threshold float64) []speedtest.Result {
	issues := make([]speedtest.Result, 0)
	for _, result := range results {
		if effective := resultThreshold(result, threshold); result.Offline || result.Error != "" || (effective > 0 && result.Mbps < effective) {
			issues = append(issues, result)
		}
	}
	return issues
}

func formatSpeedIssueRichItem(result speedtest.Result, threshold float64) string {
	item := fmt.Sprintf("<li><b>%s</b> — %s", htmlEscape(compactText(result.Name, 80)), formatSpeedStatusRich(result, threshold))
	if diagnostic := formatSpeedAgentDiagnosticHTML(result.AgentDiagnostic); diagnostic != "" {
		item += "<br>↳ " + diagnostic
	}
	return item + "</li>"
}

func formatSpeedStatusRich(result speedtest.Result, threshold float64) string {
	if result.Offline {
		return "🔴 недоступна"
	}
	if result.Error != "" {
		return "❌ " + htmlEscape(compactText(result.Error, 90))
	}
	if effective := resultThreshold(result, threshold); effective > 0 && result.Mbps < effective {
		return fmt.Sprintf("⚠️ <b>%s</b> · порог %s", formatMbps(result.Mbps), formatMbps(effective))
	}
	return "✅ <b>" + formatMbps(result.Mbps) + "</b>"
}

func formatSpeedDiagnosticsRichDetails(results []speedtest.Result, limit int) string {
	results = limitResults(results, limit)
	if len(results) == 0 {
		return ""
	}
	var items []string
	for _, result := range results {
		parts := []string{htmlCode(result.StableID)}
		switch {
		case result.Offline:
			if diagnostics := formatSpeedResultDiagnosticsHTML(result); diagnostics != "" {
				parts = append(parts, diagnostics)
			}
		case result.Error != "":
			parts = append(parts, htmlEscape(compactText(result.Error, 1200)))
		default:
			parts = append(parts,
				htmlEscape(formatBytes(result.DownloadedBytes)),
				fmt.Sprintf("%d ms", result.DurationMs),
				fmt.Sprintf("TTFB %d ms", result.TTFBMs),
			)
		}
		if diagnostic := formatSpeedAgentDiagnosticRich(result.AgentDiagnostic); diagnostic != "" {
			parts = append(parts, diagnostic)
		}
		items = append(items, fmt.Sprintf("<li><b>%s</b> — %s</li>", htmlEscape(result.Name), strings.Join(parts, " · ")))
	}
	return "<details><summary>Технические детали</summary><ul>" + strings.Join(items, "") + "</ul></details>"
}

func formatSpeedAgentDiagnosticHTML(diagnostic *speedtest.AgentDiagnostic) string {
	if diagnostic == nil {
		return ""
	}
	name := diagnostic.AgentName
	if strings.TrimSpace(name) == "" {
		name = diagnostic.AgentID
	}
	prefix := "Агент"
	if name != "" {
		prefix += " " + compactText(name, 48)
	}
	verdict := "результат неизвестен"
	switch diagnostic.State {
	case speedtest.AgentDiagnosticRunning:
		verdict = "проверка идёт"
	case speedtest.AgentDiagnosticReproduced:
		verdict = "проблема воспроизведена"
		if diagnostic.RemoteStatus == "online" {
			verdict = "просадка воспроизведена"
		}
	case speedtest.AgentDiagnosticNotReproduced:
		verdict = "проблема не воспроизвелась"
		if diagnostic.AlternativeStatus == "online" {
			verdict = "основной URL не сработал, резервный доступен"
		}
	case speedtest.AgentDiagnosticUnreliable, speedtest.AgentDiagnosticUnavailable:
		verdict = localizedSpeedDiagnosticDetail(diagnostic.Detail)
	}
	rate := ""
	if diagnostic.Mbps > 0 || (diagnostic.RemoteStatus == "online" && diagnostic.State == speedtest.AgentDiagnosticReproduced) {
		rate = formatMbps(float64(diagnostic.Mbps)) + " · "
	}
	return "<b>" + htmlEscape(prefix) + "</b>: " + rate + htmlEscape(verdict)
}

func formatSpeedAgentDiagnosticRich(diagnostic *speedtest.AgentDiagnostic) string {
	if diagnostic == nil {
		return ""
	}
	parts := []string{"Агент " + htmlEscape(speedAgentDiagnosticLabel(diagnostic))}
	if detail := speedAgentDiagnosticDetail(diagnostic); detail != "" {
		parts = append(parts, detail)
	}
	if diagnostic.Detail != "" {
		parts = append(parts, htmlEscape(localizedSpeedDiagnosticDetail(diagnostic.Detail)))
	}
	return strings.Join(parts, " · ")
}

func speedAgentDiagnosticLabel(diagnostic *speedtest.AgentDiagnostic) string {
	if diagnostic == nil {
		return ""
	}
	name := strings.TrimSpace(diagnostic.AgentName)
	if name == "" {
		name = strings.TrimSpace(diagnostic.AgentID)
	}
	parts := []string{name}
	if region := strings.TrimSpace(diagnostic.Region); region != "" {
		parts = append(parts, region)
	}
	if provider := strings.TrimSpace(diagnostic.Provider); provider != "" {
		parts = append(parts, provider)
	}
	return strings.Join(nonEmptyStrings(parts), " / ")
}

func speedAgentDiagnosticDetail(diagnostic *speedtest.AgentDiagnostic) string {
	if diagnostic == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if diagnostic.RemoteStatus != "" {
		parts = append(parts, diagnostic.RemoteStatus)
	}
	if diagnostic.Mbps > 0 {
		parts = append(parts, formatMbps(float64(diagnostic.Mbps)))
	}
	if diagnostic.FailureCode != "" {
		failure := diagnostic.FailureCode
		if diagnostic.FailureStage != "" {
			failure = diagnostic.FailureStage + "/" + failure
		}
		parts = append(parts, failure)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + htmlEscape(strings.Join(parts, " · ")) + ")"
}

func localizedSpeedDiagnosticDetail(detail string) string {
	switch detail {
	case "automation capacity is busy":
		return "все слоты диагностики заняты"
	case "no healthy idle diagnostic agent is connected":
		return "нет свободного доступного агента"
	case "automatic diagnostics are paused by maintenance":
		return "диагностика на паузе: обслуживание"
	case "remote diagnostics are disabled":
		return "диагностика агентами отключена"
	case "automatic diagnostic could not be started":
		return "не удалось запустить проверку"
	case "diagnostic session is no longer available":
		return "результат проверки больше недоступен"
	case "no signed remote observation was received":
		return "агент не прислал результат вовремя"
	case "agent direct connectivity control failed":
		return "у агента не прошла контрольная проверка интернета"
	case "agent download observation has no throughput evidence":
		return "агент не передал данные о скорости"
	default:
		return "недостаточно данных для вывода"
	}
}

func nonEmptyStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

func formatSpeedHistoryRichTable(results []speedtest.Result, threshold float64) string {
	if len(results) == 0 {
		return "<p>Нет результатов.</p>"
	}
	var rows strings.Builder
	rows.WriteString("<table bordered striped><tr><th>Время</th><th>Результат</th><th>TTFB</th></tr>")
	for _, result := range results {
		ttfb := "—"
		if result.TTFBMs > 0 {
			ttfb = fmt.Sprintf("%d ms", result.TTFBMs)
		}
		fmt.Fprintf(&rows, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>",
			htmlEscape(formatCheckedAt(result.CheckedAt)),
			formatSpeedStatusRich(result, threshold),
			htmlEscape(ttfb),
		)
	}
	rows.WriteString("</table>")
	return rows.String()
}

func limitResults(results []speedtest.Result, limit int) []speedtest.Result {
	if limit <= 0 || len(results) <= limit {
		return results
	}
	return results[:limit]
}

func formatSpeedResultHTML(result speedtest.Result, threshold float64) string {
	line := fmt.Sprintf("• <b>%s</b> · %s", htmlEscape(compactText(result.Name, 80)), formatSpeedStatusRich(result, threshold))
	if result.Offline {
		if diagnostics := formatSpeedResultDiagnosticsHTML(result); diagnostics != "" {
			line += " · " + diagnostics
		}
	}
	return line
}

func reportSourceLabel(source string) string {
	switch source {
	case "manual":
		return "админ-панель"
	case "telegram":
		return "Telegram"
	case "schedule":
		return "расписание"
	case speedConfirmationRetrySource:
		return "повтор через 30 минут"
	default:
		if source == "" {
			return "неизвестно"
		}
		return source
	}
}

func speedReportTitle(source string, issuesOnly bool) string {
	if source == speedConfirmationRetrySource {
		return "Speed-test: проблема подтверждена"
	}
	if issuesOnly {
		return "Speed-test: есть проблемы"
	}
	return "Speed-test завершён"
}

func formatSpeedHistoryLine(result speedtest.Result, threshold float64) string {
	prefix := formatCheckedAt(result.CheckedAt)
	if result.Offline {
		diagnostics := formatSpeedResultDiagnosticsHTML(result)
		if diagnostics == "" {
			return fmt.Sprintf("• %s · 🔴 <b>недоступна</b>", htmlCode(prefix))
		}
		return fmt.Sprintf("• %s · 🔴 <b>недоступна</b> · %s", htmlCode(prefix), diagnostics)
	}
	if result.Error != "" {
		return fmt.Sprintf("• %s · ❌ %s", htmlCode(prefix), htmlEscape(compactText(result.Error, 120)))
	}
	marker := "✅"
	if effective := resultThreshold(result, threshold); effective > 0 && result.Mbps < effective {
		marker = "⚠️"
	}
	ttfb := ""
	if result.TTFBMs > 0 {
		ttfb = fmt.Sprintf(" · TTFB %d ms", result.TTFBMs)
	}
	value := formatMbps(result.Mbps)
	if marker == "⚠️" {
		value += " · порог " + formatMbps(resultThreshold(result, threshold))
	}
	return fmt.Sprintf("• %s · %s <b>%s</b>%s", htmlCode(prefix), marker, value, ttfb)
}

func formatSpeedResultDiagnosticsHTML(result speedtest.Result) string {
	var hostCheck checker.HostCheckDetails
	var pingCheck checker.PingCheckDetails
	if result.HostCheck != nil {
		hostCheck = *result.HostCheck
	}
	if result.PingCheck != nil {
		pingCheck = *result.PingCheck
	}
	return formatHostDiagnosticsHTML(hostCheck, pingCheck)
}
