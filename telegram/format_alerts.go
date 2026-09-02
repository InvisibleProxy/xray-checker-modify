package telegram

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

func formatHostDiagnosticsHTML(hostCheck checker.HostCheckDetails, pingCheck checker.PingCheckDetails) string {
	var parts []string
	if hostCheck.Checked {
		parts = append(parts, formatTCPCheckHTML(hostCheck))
	}
	if pingCheck.Checked {
		parts = append(parts, formatPingCheckHTML(pingCheck))
	}
	return strings.Join(parts, " · ")
}

func formatTCPCheckHTML(hostCheck checker.HostCheckDetails) string {
	if hostCheck.Online {
		if hostCheck.Latency > 0 {
			return fmt.Sprintf("TCP 🟢 %d ms", hostCheck.Latency.Milliseconds())
		}
		return "TCP 🟢"
	}
	return "TCP 🔴"
}

func formatPingCheckHTML(pingCheck checker.PingCheckDetails) string {
	if pingCheck.Online {
		if pingCheck.Latency > 0 {
			return fmt.Sprintf("Ping 🟢 %d ms", pingCheck.Latency.Milliseconds())
		}
		return "Ping 🟢"
	}
	return "Ping 🔴"
}

func formatProxyLineHTML(proxy *models.ProxyConfig, details checker.ProxyStatusDetails) string {
	latencyText := "—"
	if details.Latency > 0 {
		latencyText = fmt.Sprintf("%d ms", details.Latency.Milliseconds())
	}
	status := details.EffectiveStatus()
	if status == checker.AvailabilityStateOnline {
		return fmt.Sprintf("• 🟢 <b>%s</b> · %s", htmlEscape(proxy.Name), htmlEscape(latencyText))
	}
	marker := "🔴"
	since := details.DownSince
	durationLabel := "простой"
	if status == checker.AvailabilityStateProxyFailure {
		marker = "🟡"
		since = details.ProxyFailureSince
		durationLabel = "proxy failure"
	}
	parts := []string{marker + " <b>" + htmlEscape(proxy.Name) + "</b>"}
	if !since.IsZero() {
		parts = append(parts, durationLabel+" "+htmlEscape(formatDuration(time.Since(since))))
	}
	if diagnostics := formatHostDiagnosticsHTML(details.HostCheck, details.PingCheck); diagnostics != "" {
		parts = append(parts, diagnostics)
	}
	return "• " + strings.Join(parts, " · ")
}

func formatProxyRichItem(proxy *models.ProxyConfig, details checker.ProxyStatusDetails) string {
	status := details.EffectiveStatus()
	if status == checker.AvailabilityStateOnline {
		latencyText := "—"
		if details.Latency > 0 {
			latencyText = fmt.Sprintf("%d ms", details.Latency.Milliseconds())
		}
		return fmt.Sprintf("<li>🟢 <b>%s</b> — %s</li>", htmlEscape(proxy.Name), htmlEscape(latencyText))
	}
	marker := "🔴"
	since := details.DownSince
	durationLabel := "простой"
	if status == checker.AvailabilityStateProxyFailure {
		marker = "🟡"
		since = details.ProxyFailureSince
		durationLabel = "proxy failure"
	}
	parts := []string{marker + " <b>" + htmlEscape(proxy.Name) + "</b>"}
	if !since.IsZero() {
		parts = append(parts, durationLabel+" "+htmlEscape(formatDuration(time.Since(since))))
	}
	if diagnostics := formatHostDiagnosticsHTML(details.HostCheck, details.PingCheck); diagnostics != "" {
		parts = append(parts, diagnostics)
	}
	return "<li>" + strings.Join(parts, " · ") + "</li>"
}

func formatNodeDown(proxy *models.ProxyConfig, state nodeAlertState, now time.Time) string {
	proxyFailure := nodeAlertStatus(state) == checker.AvailabilityStateProxyFailure
	title := "⚠️ Нода недоступна"
	marker := "🔴"
	durationLabel := "Простой"
	if proxyFailure {
		title = "⚠️ Proxy failure"
		marker = "🟡"
		durationLabel = "Proxy failure"
	}
	if state.AlertCount > 1 {
		if proxyFailure {
			title = "⚠️ Proxy failure продолжается"
		} else {
			title = "⚠️ Нода всё ещё недоступна"
		}
	}

	lines := []string{
		fmt.Sprintf("<b>%s</b>", htmlEscape(title)),
		fmt.Sprintf("%s <b>%s</b>", marker, htmlEscape(proxy.Name)),
	}
	if since := nodeAlertIssueSince(state); !since.IsZero() {
		lines = append(lines, fmt.Sprintf("%s: <b>%s</b> · с %s", durationLabel, htmlEscape(formatDuration(now.Sub(since))), htmlEscape(formatCheckedAt(since))))
	}
	if failure := formatFailureHTML(state.Failure); failure != "" {
		lines = append(lines, failure)
	}
	if diagnostics := formatHostDiagnosticsHTML(state.HostCheck, state.PingCheck); diagnostics != "" {
		lines = append(lines, diagnostics)
	} else {
		lines = append(lines, "Диагностики пока нет")
	}
	if nextAfter := state.NextAlert.Sub(now); nextAfter > 0 {
		lines = append(lines, fmt.Sprintf("Следующее напоминание через <b>%s</b>", htmlEscape(formatDuration(nextAfter))))
	}
	return strings.Join(lines, "\n")
}

func formatNodeDownMessage(proxy *models.ProxyConfig, state nodeAlertState, now time.Time) formattedMessage {
	fallback := formatNodeDown(proxy, state, now)
	title := "Нода недоступна"
	marker := "🔴"
	durationLabel := "Простой"
	if nodeAlertStatus(state) == checker.AvailabilityStateProxyFailure {
		title = "Proxy failure"
		marker = "🟡"
		durationLabel = "Proxy failure"
	}
	if state.AlertCount > 1 {
		if nodeAlertStatus(state) == checker.AvailabilityStateProxyFailure {
			title = "Proxy failure продолжается"
		} else {
			title = "Нода всё ещё недоступна"
		}
	}
	var rich strings.Builder
	fmt.Fprintf(&rich, "<h2>⚠️ %s</h2><p>%s <b>%s</b></p>", htmlEscape(title), marker, htmlEscape(proxy.Name))
	if since := nodeAlertIssueSince(state); !since.IsZero() {
		fmt.Fprintf(&rich, "<p>%s: <b>%s</b> · с %s</p>", durationLabel, htmlEscape(formatDuration(now.Sub(since))), htmlEscape(formatCheckedAt(since)))
	}
	if failure := formatFailureHTML(state.Failure); failure != "" {
		fmt.Fprintf(&rich, "<p>%s</p>", failure)
	}
	if diagnostics := formatHostDiagnosticsHTML(state.HostCheck, state.PingCheck); diagnostics != "" {
		fmt.Fprintf(&rich, "<blockquote>%s</blockquote>", diagnostics)
	}
	rich.WriteString("<details><summary>Технические данные</summary><table bordered>")
	fmt.Fprintf(&rich, "<tr><th>StableID</th><td><code>%s</code></td></tr><tr><th>Протокол</th><td>%s</td></tr><tr><th>Провалов подряд</th><td>%d</td></tr>",
		htmlEscape(proxy.StableID), htmlEscape(strings.ToUpper(proxy.Protocol)), state.FailCount)
	if proxy.SubName != "" {
		fmt.Fprintf(&rich, "<tr><th>Подписка</th><td>%s</td></tr>", htmlEscape(proxy.SubName))
	}
	rich.WriteString("</table></details>")
	return formattedMessage{HTML: fallback, RichHTML: rich.String()}
}

func partitionMassNodeDownAlerts(alerts []nodeDownAlert, proxies []*models.ProxyConfig, muted map[string]bool) ([]nodeDownIncidentGroup, []nodeDownAlert) {
	alertable := make([]*models.ProxyConfig, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || muted[proxy.StableID] {
			continue
		}
		alertable = append(alertable, proxy)
	}
	grouped := make(map[string]bool)
	var groups []nodeDownIncidentGroup
	if group, ok := buildMassNodeDownGroup("global", "", alerts, len(alertable)); ok {
		groups = append(groups, group)
		for _, alert := range group.Alerts {
			grouped[alert.Proxy.StableID] = true
		}
	} else {
		totals := make(map[string]int)
		bySubscription := make(map[string][]nodeDownAlert)
		for _, proxy := range alertable {
			totals[proxy.SubName]++
		}
		for _, alert := range alerts {
			if alert.Proxy != nil {
				bySubscription[alert.Proxy.SubName] = append(bySubscription[alert.Proxy.SubName], alert)
			}
		}
		var subscriptions []string
		for subscription := range bySubscription {
			subscriptions = append(subscriptions, subscription)
		}
		sort.Strings(subscriptions)
		for _, subscription := range subscriptions {
			scope := "subscription:" + subscription
			if subscription == "" {
				scope = "subscription:(unnamed)"
			}
			group, ok := buildMassNodeDownGroup(scope, subscription, bySubscription[subscription], totals[subscription])
			if !ok {
				continue
			}
			groups = append(groups, group)
			for _, alert := range group.Alerts {
				grouped[alert.Proxy.StableID] = true
			}
		}
	}
	remaining := make([]nodeDownAlert, 0, len(alerts))
	for _, alert := range alerts {
		if alert.Proxy == nil || !grouped[alert.Proxy.StableID] {
			remaining = append(remaining, alert)
		}
	}
	return groups, remaining
}

func buildMassNodeDownGroup(scope, subscription string, alerts []nodeDownAlert, total int) (nodeDownIncidentGroup, bool) {
	required := (total + 1) / 2
	if required < 3 {
		required = 3
	}
	if total == 0 || len(alerts) < required {
		return nodeDownIncidentGroup{}, false
	}
	byCause := make(map[string][]nodeDownAlert)
	for _, alert := range alerts {
		if alert.Proxy == nil {
			continue
		}
		code := alert.State.Failure.Code
		if code == "" {
			code = checker.FailureCodeUnknown
		}
		byCause[code] = append(byCause[code], alert)
	}
	var codes []string
	for code := range byCause {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	selectedCode := ""
	for _, code := range codes {
		if len(byCause[code]) >= required && (selectedCode == "" || len(byCause[code]) > len(byCause[selectedCode])) {
			selectedCode = code
		}
	}
	if selectedCode == "" {
		return nodeDownIncidentGroup{}, false
	}
	selected := append([]nodeDownAlert(nil), byCause[selectedCode]...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].Proxy.Name < selected[j].Proxy.Name })
	cause := selected[0].State.Failure
	if cause.Code == "" {
		cause.Code = selectedCode
	}
	if cause.Summary == "" {
		cause.Summary = checker.FailureSummary(cause.Code)
	}
	if scope == "global" && telegramLikelySharedCheckEndpoint(selectedCode, selected) {
		cause = checker.FailureDetails{
			Code:    checker.FailureCodeCheckEndpoint,
			Summary: "Вероятен общий сбой проверочного endpoint",
			Detail:  "одинаковая ошибка проверки при доступных TCP-портах разных нод",
		}
	}
	return nodeDownIncidentGroup{
		Scope:        scope,
		Subscription: subscription,
		Alerts:       selected,
		Total:        total,
		Cause:        cause,
	}, true
}

func telegramLikelySharedCheckEndpoint(code string, alerts []nodeDownAlert) bool {
	switch code {
	case checker.FailureCodeDNS, checker.FailureCodeHTTPStatus, checker.FailureCodeProxyTimeout, checker.FailureCodeTLS:
	default:
		return false
	}
	servers := make(map[string]bool)
	for _, alert := range alerts {
		if alert.Proxy == nil || !alert.State.HostCheck.Checked || !alert.State.HostCheck.Online {
			return false
		}
		servers[alert.Proxy.Server] = true
	}
	return len(servers) >= 2
}

func formatMassNodeDownMessage(group nodeDownIncidentGroup, now time.Time) formattedMessage {
	scope := "все подписки"
	if group.Subscription != "" {
		scope = "подписка " + group.Subscription
	} else if strings.HasPrefix(group.Scope, "subscription:") {
		scope = "подписка без имени"
	}
	summary := group.Cause.Summary
	if summary == "" {
		summary = checker.FailureSummary(group.Cause.Code)
	}
	lines := []string{
		"<b>🚨 Массовый сбой нод</b>",
		fmt.Sprintf("Область: <b>%s</b>", htmlEscape(scope)),
		fmt.Sprintf("Затронуто: <b>%d из %d</b>", len(group.Alerts), group.Total),
		fmt.Sprintf("Причина: <b>%s</b> · %s", htmlEscape(summary), htmlCode(group.Cause.Code)),
		"",
	}
	var items []string
	for _, alert := range group.Alerts {
		duration := "—"
		if since := nodeAlertIssueSince(alert.State); !since.IsZero() {
			duration = formatDuration(now.Sub(since))
		}
		marker := "🔴"
		durationLabel := "простой"
		if nodeAlertStatus(alert.State) == checker.AvailabilityStateProxyFailure {
			marker = "🟡"
			durationLabel = "proxy failure"
		}
		lines = append(lines, fmt.Sprintf("• %s <b>%s</b> · %s %s", marker, htmlEscape(alert.Proxy.Name), durationLabel, htmlEscape(duration)))
		items = append(items, fmt.Sprintf("<li>%s <b>%s</b> — %s %s</li>", marker, htmlEscape(alert.Proxy.Name), durationLabel, htmlEscape(duration)))
	}
	fallback := trimHTMLMessage(strings.Join(lines, "\n"))
	rich := fmt.Sprintf("<h2>🚨 Массовый сбой нод</h2><p>%s · <b>%d из %d</b></p><blockquote>Причина: <b>%s</b> · <code>%s</code></blockquote><ul>%s</ul>",
		htmlEscape(scope), len(group.Alerts), group.Total, htmlEscape(summary), htmlEscape(group.Cause.Code), strings.Join(items, ""))
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func formatNodeDownGroup(alerts []nodeDownAlert, now time.Time) string {
	lines := []string{
		fmt.Sprintf("<b>⚠️ Проблемы у %d нод</b>", len(alerts)),
		"",
	}
	for _, alert := range alerts {
		state := alert.State
		duration := "—"
		if since := nodeAlertIssueSince(state); !since.IsZero() {
			duration = formatDuration(now.Sub(since))
		}
		durationLabel := "простой"
		if nodeAlertStatus(state) == checker.AvailabilityStateProxyFailure {
			durationLabel = "proxy failure"
		}
		parts := []string{fmt.Sprintf("%s %s", durationLabel, htmlEscape(duration))}
		if alert.State.Failure.Summary != "" {
			parts = append(parts, "причина "+htmlEscape(alert.State.Failure.Summary))
		}
		diagnostics := formatHostDiagnosticsHTML(state.HostCheck, state.PingCheck)
		if diagnostics == "" {
			diagnostics = "Диагностика: нет данных"
		}
		parts = append(parts, diagnostics)
		lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  %s", htmlEscape(alert.Proxy.Name), strings.Join(parts, " · ")))
	}
	return trimHTMLMessage(strings.Join(lines, "\n"))
}

func formatNodeDownGroupMessage(alerts []nodeDownAlert, now time.Time) formattedMessage {
	fallback := formatNodeDownGroup(alerts, now)
	var items []string
	var details []string
	for _, alert := range alerts {
		state := alert.State
		duration := "—"
		if since := nodeAlertIssueSince(state); !since.IsZero() {
			duration = formatDuration(now.Sub(since))
		}
		diagnostics := formatHostDiagnosticsHTML(state.HostCheck, state.PingCheck)
		if diagnostics == "" {
			diagnostics = "диагностики нет"
		}
		cause := state.Failure.Summary
		if cause == "" {
			cause = "причина не определена"
		}
		marker := "🔴"
		if nodeAlertStatus(state) == checker.AvailabilityStateProxyFailure {
			marker = "🟡"
		}
		items = append(items, fmt.Sprintf("<li>%s <b>%s</b> — %s · %s · %s</li>", marker, htmlEscape(alert.Proxy.Name), htmlEscape(duration), htmlEscape(cause), diagnostics))
		details = append(details, fmt.Sprintf("<li><b>%s</b> — <code>%s</code> · %s</li>", htmlEscape(alert.Proxy.Name), htmlEscape(alert.Proxy.StableID), htmlEscape(strings.ToUpper(alert.Proxy.Protocol))))
	}
	rich := fmt.Sprintf("<h2>⚠️ Проблемы у нод: %d</h2><ul>%s</ul><details><summary>Технические данные</summary><ul>%s</ul></details>",
		len(alerts), strings.Join(items, ""), strings.Join(details, ""))
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func formatNodeRecovery(proxy *models.ProxyConfig, latency time.Duration, downSince time.Time, now time.Time) string {
	latencyText := "—"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}
	lines := []string{
		fmt.Sprintf("✅ <b>%s</b> снова доступна", htmlEscape(proxy.Name)),
		fmt.Sprintf("Задержка: <b>%s</b>", htmlEscape(latencyText)),
	}
	if !downSince.IsZero() {
		lines = append(lines, fmt.Sprintf("Простой: <b>%s</b>", htmlEscape(formatDuration(now.Sub(downSince)))))
	}
	return strings.Join(lines, "\n")
}

func formatNodeRecoveryMessage(proxy *models.ProxyConfig, latency time.Duration, previous nodeAlertState, recoveredAt time.Time) formattedMessage {
	since := nodeAlertIssueSince(previous)
	proxyFailure := nodeAlertStatus(previous) == checker.AvailabilityStateProxyFailure
	if !proxyFailure {
		fallback := formatNodeRecovery(proxy, latency, since, recoveredAt)
		latencyText := "—"
		if latency > 0 {
			latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
		}
		downtime := "—"
		if !since.IsZero() {
			downtime = formatDuration(recoveredAt.Sub(since))
		}
		rich := fmt.Sprintf("<h2>✅ Нода снова доступна</h2><p><b>%s</b></p><table bordered><tr><th>Простой</th><td>%s</td></tr><tr><th>Задержка</th><td>%s</td></tr></table><details><summary>StableID</summary><p><code>%s</code></p></details>",
			htmlEscape(proxy.Name), htmlEscape(downtime), htmlEscape(latencyText), htmlEscape(proxy.StableID))
		return formattedMessage{HTML: fallback, RichHTML: rich}
	}

	latencyText := "—"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}
	duration := "—"
	if !since.IsZero() {
		duration = formatDuration(recoveredAt.Sub(since))
	}
	fallback := fmt.Sprintf("✅ <b>%s</b>: proxy снова работает\nProxy failure: <b>%s</b>\nЗадержка: <b>%s</b>", htmlEscape(proxy.Name), htmlEscape(duration), htmlEscape(latencyText))
	rich := fmt.Sprintf("<h2>✅ Proxy снова работает</h2><p><b>%s</b></p><table bordered><tr><th>Proxy failure</th><td>%s</td></tr><tr><th>Задержка</th><td>%s</td></tr></table><details><summary>StableID</summary><p><code>%s</code></p></details>",
		htmlEscape(proxy.Name), htmlEscape(duration), htmlEscape(latencyText), htmlEscape(proxy.StableID))
	return formattedMessage{HTML: fallback, RichHTML: rich}
}

func formatFailureHTML(failure checker.FailureDetails) string {
	if failure.Code == "" && failure.Summary == "" {
		return ""
	}
	summary := failure.Summary
	if summary == "" {
		summary = checker.FailureSummary(failure.Code)
	}
	result := "Причина: <b>" + htmlEscape(summary) + "</b>"
	if failure.Code != "" {
		result += " · " + htmlCode(failure.Code)
	}
	return result
}
