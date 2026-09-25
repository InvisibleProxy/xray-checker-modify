package telegram

import (
	"fmt"
	"strings"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/paneltelemetry"
	"xray-checker/speedtest"
)

// agentRateRatio is how many times the checker's rate the agent got, from the
// numbers the probe carries, or zero when either is missing.
func agentRateRatio(diagnostic *speedtest.AgentDiagnostic) float64 {
	if diagnostic == nil || diagnostic.Task == nil || diagnostic.Task.ObservedMbps <= 0 || diagnostic.Mbps <= 0 {
		return 0
	}
	return float64(diagnostic.Mbps) / diagnostic.Task.ObservedMbps
}

// formatAvailabilityAgentHTML words an agent's answer under a down alert.
//
// The wording names what to do next rather than who agreed with whom. For a
// node the checker cannot reach at all, "it works from elsewhere" means the
// checker's path is blocked — for a checker inside a filtered network, the IP or
// the route to it — and "it fails from elsewhere too" means the host or its
// hoster; those send the operator to different consoles.
func formatAvailabilityAgentHTML(agent *speedtest.AgentDiagnostic) string {
	if agent == nil {
		return ""
	}
	name := strings.TrimSpace(agent.AgentName)
	if name == "" {
		name = agent.AgentID
	}
	prefix := "Агент"
	if name != "" {
		prefix += " " + compactText(name, 48)
	}
	offline := agent.Task != nil && agent.Task.Kind == diagnostics.AutomationKindOffline
	verdict := "результат неизвестен"
	switch agent.State {
	case speedtest.AgentDiagnosticRunning:
		verdict = "проверка идёт"
	case speedtest.AgentDiagnosticNotReproduced:
		switch {
		case agent.Detail == diagnostics.ReasonAlternativeWorked && offline:
			verdict = "у него туннель работает через резервный endpoint → нода жива, недоступна только с пути checker-а"
		case agent.Detail == diagnostics.ReasonAlternativeWorked:
			verdict = "у него туннель работает через резервный endpoint → отказал проверочный endpoint, не нода"
		case offline:
			verdict = "у него нода работает → недоступна только с пути checker-а: вероятна блокировка IP или маршрута"
		default:
			verdict = "у него туннель работает → сбой на пути checker-а, а не в ноде"
		}
	case speedtest.AgentDiagnosticReproduced:
		switch {
		case offline && agent.RemoteStatus == string(diagnostics.ProbeStatusProxyFailure):
			verdict = "у него хост отвечает, но туннель не работает → вероятно, упал сервис на ноде"
		case offline:
			verdict = "недоступна и у него → вероятен отказ хоста или хостера"
		default:
			verdict = "туннель не работает и у него → проблема в ноде или её конфигурации"
		}
	case speedtest.AgentDiagnosticUnreliable, speedtest.AgentDiagnosticUnavailable:
		verdict = localizedSpeedDiagnosticDetail(agent.Detail)
	}
	return "<b>" + htmlEscape(prefix) + "</b>: " + htmlEscape(verdict)
}

// formatPanelStatusHTML is the panel's view of a node on one line.
//
// The online count comes first: it is the one number here about the clients
// themselves, and a count that fell when the failure began says the outage is
// theirs too, whatever the checker's own vantage point made of it.
func formatPanelStatusHTML(status *paneltelemetry.Status) string {
	if status == nil {
		return ""
	}
	parts := make([]string, 0, 6)
	switch {
	case status.Disabled:
		parts = append(parts, "нода отключена в панели")
	case !status.Connected && status.Connecting:
		parts = append(parts, "🟡 панель переподключается к ноде")
	case !status.Connected:
		line := "🔴 панель не видит ноду"
		if status.Message != "" {
			line += " (" + status.Message + ")"
		}
		parts = append(parts, line)
	default:
		parts = append(parts, "🟢 связь есть")
	}
	online := fmt.Sprintf("онлайн %d", status.UsersOnline)
	if status.HasBefore && status.UsersOnlineBefore != status.UsersOnline {
		online += fmt.Sprintf(" (до сбоя %d)", status.UsersOnlineBefore)
	}
	parts = append(parts, online)
	if status.MemoryUsedPercent > 0 {
		parts = append(parts, fmt.Sprintf("память %.0f%%", status.MemoryUsedPercent))
	}
	if status.Load1 > 0 {
		load := fmt.Sprintf("load %.2f", status.Load1)
		if status.CPUs > 0 {
			load += fmt.Sprintf("/%d CPU", status.CPUs)
		}
		parts = append(parts, load)
	}
	if status.RxMbps > 0 || status.TxMbps > 0 {
		parts = append(parts, fmt.Sprintf("трафик ↓%.0f ↑%.0f Мбит/с", status.RxMbps, status.TxMbps))
	}
	if status.XrayUptime > 0 {
		parts = append(parts, "xray "+formatUptime(status.XrayUptime))
	}
	return "<b>Панель</b>: " + htmlEscape(strings.Join(parts, " · "))
}

// formatUptime is a coarse age: an operator reads "restarted an hour ago" or
// "up for twelve days", not minutes.
func formatUptime(value time.Duration) string {
	switch {
	case value >= 48*time.Hour:
		return fmt.Sprintf("%d д", int(value/(24*time.Hour)))
	case value >= time.Hour:
		return fmt.Sprintf("%d ч", int(value/time.Hour))
	default:
		return fmt.Sprintf("%d мин", int(value/time.Minute))
	}
}
