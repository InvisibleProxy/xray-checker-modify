package checker

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"strings"
)

const (
	FailureCodeConfiguration      = "configuration"
	FailureCodeDNS                = "dns"
	FailureCodeTCPRefused         = "tcp_refused"
	FailureCodeTCPTimeout         = "tcp_timeout"
	FailureCodeHostUnreachable    = "host_unreachable"
	FailureCodeProxyHandshake     = "proxy_handshake"
	FailureCodeProxyTimeout       = "proxy_timeout"
	FailureCodeTLS                = "tls"
	FailureCodeHTTPStatus         = "http_status"
	FailureCodeSourceIPUnchanged  = "source_ip_unchanged"
	FailureCodeDownloadIncomplete = "download_incomplete"
	FailureCodeCheckEndpoint      = "check_endpoint"
	FailureCodeUnknown            = "unknown"
)

// FailureDetails is a stable, user-facing classification of a failed proxy
// check. Detail may contain compact transport text, while Summary is safe for
// dashboards and Telegram notifications.
type FailureDetails struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

func failureFromError(err error) FailureDetails {
	if err == nil {
		return FailureDetails{}
	}
	detail := compactFailureDetail(err.Error())
	lower := strings.ToLower(detail)

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || strings.Contains(lower, "no such host") || strings.Contains(lower, "server misbehaving") {
		return failureDetails(FailureCodeDNS, detail)
	}
	var tlsErr tls.RecordHeaderError
	if errors.As(err, &tlsErr) || strings.Contains(lower, "tls") || strings.Contains(lower, "certificate") || strings.Contains(lower, "x509") {
		return failureDetails(FailureCodeTLS, detail)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failureDetails(FailureCodeProxyTimeout, detail)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return failureDetails(FailureCodeProxyTimeout, detail)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		lower = strings.ToLower(urlErr.Err.Error())
	}
	if strings.Contains(lower, "socks") || strings.Contains(lower, "proxyconnect") || strings.Contains(lower, "proxy connect") {
		return failureDetails(FailureCodeProxyHandshake, detail)
	}
	if strings.Contains(lower, "connection refused") {
		return failureDetails(FailureCodeTCPRefused, detail)
	}
	if strings.Contains(lower, "network is unreachable") || strings.Contains(lower, "no route to host") || strings.Contains(lower, "host is unreachable") {
		return failureDetails(FailureCodeHostUnreachable, detail)
	}
	return failureDetails(FailureCodeUnknown, detail)
}

func failureFromCheckResult(method, message string) FailureDetails {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch method {
	case "ip":
		return failureDetails(FailureCodeSourceIPUnchanged, message)
	case "status":
		return failureDetails(FailureCodeHTTPStatus, message)
	case "download":
		if strings.Contains(lower, "http status") {
			return failureDetails(FailureCodeHTTPStatus, message)
		}
		return failureDetails(FailureCodeDownloadIncomplete, message)
	default:
		return failureDetails(FailureCodeUnknown, message)
	}
}

// DiagnoseFailure combines the proxy-check failure with direct host
// diagnostics. The result states what is proven and avoids claiming that ICMP
// failure alone means that the host is offline.
func DiagnoseFailure(checkFailure FailureDetails, hostCheck HostCheckDetails, pingCheck PingCheckDetails) FailureDetails {
	if hostCheck.Checked && !hostCheck.Online {
		hostFailure := failureFromHostCheck(hostCheck)
		if hostFailure.Code == FailureCodeDNS {
			return hostFailure
		}
		if hostFailure.Code == FailureCodeTCPRefused {
			return hostFailure
		}
		if hostFailure.Code == FailureCodeTCPTimeout && pingCheck.Checked && pingCheck.Online {
			hostFailure.Summary = "Хост отвечает, но TCP-порт недоступен"
			return hostFailure
		}
		if pingCheck.Checked && !pingCheck.Online {
			return FailureDetails{
				Code:    FailureCodeHostUnreachable,
				Summary: "Хост или маршрут недоступен",
				Detail:  joinFailureDetails(hostCheck.Error, pingCheck.Error),
			}
		}
		return hostFailure
	}
	if checkFailure.Code == "" {
		return failureDetails(FailureCodeUnknown, "proxy check failed")
	}
	return checkFailure
}

func failureFromHostCheck(hostCheck HostCheckDetails) FailureDetails {
	detail := compactFailureDetail(hostCheck.Error)
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "no such host") || strings.Contains(lower, "server misbehaving") || strings.Contains(lower, "resolved to no") {
		return failureDetails(FailureCodeDNS, detail)
	}
	if strings.Contains(lower, "connection refused") {
		return failureDetails(FailureCodeTCPRefused, detail)
	}
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") {
		return failureDetails(FailureCodeTCPTimeout, detail)
	}
	if strings.Contains(lower, "network is unreachable") || strings.Contains(lower, "no route to host") || strings.Contains(lower, "host is unreachable") {
		return failureDetails(FailureCodeHostUnreachable, detail)
	}
	return failureDetails(FailureCodeHostUnreachable, detail)
}

func failureDetails(code, detail string) FailureDetails {
	return FailureDetails{Code: code, Summary: FailureSummary(code), Detail: compactFailureDetail(detail)}
}

// FailureSummary returns stable Russian UI text for a diagnostic code.
func FailureSummary(code string) string {
	switch code {
	case FailureCodeConfiguration:
		return "Ошибка конфигурации проверки"
	case FailureCodeDNS:
		return "DNS не разрешил адрес"
	case FailureCodeTCPRefused:
		return "TCP-порт отклоняет соединение"
	case FailureCodeTCPTimeout:
		return "TCP-порт не отвечает"
	case FailureCodeHostUnreachable:
		return "Хост или маршрут недоступен"
	case FailureCodeProxyHandshake:
		return "Ошибка SOCKS/Xray соединения"
	case FailureCodeProxyTimeout:
		return "Проверка через прокси превысила таймаут"
	case FailureCodeTLS:
		return "Ошибка TLS или сертификата"
	case FailureCodeHTTPStatus:
		return "Проверочный URL вернул ошибочный HTTP-статус"
	case FailureCodeSourceIPUnchanged:
		return "Трафик не вышел через прокси"
	case FailureCodeDownloadIncomplete:
		return "Проверочная загрузка не завершилась"
	case FailureCodeCheckEndpoint:
		return "Общий проверочный endpoint недоступен"
	default:
		return "Причина не определена"
	}
}

func compactFailureDetail(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	const maxRunes = 240
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes-1]) + "…"
}

func joinFailureDetails(parts ...string) string {
	var filtered []string
	for _, part := range parts {
		if part = compactFailureDetail(part); part != "" {
			filtered = append(filtered, part)
		}
	}
	return compactFailureDetail(strings.Join(filtered, "; "))
}
