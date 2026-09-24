package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/speedtest"
)

type fakeProxyFailureDiagnostics struct {
	enabled     bool
	wait        time.Duration
	awaited     []string
	read        []string
	annotations map[string]speedtest.AgentDiagnostic
}

func (f *fakeProxyFailureDiagnostics) ProxyFailureEnabled() bool { return f.enabled }

func (f *fakeProxyFailureDiagnostics) AlertWait() time.Duration { return f.wait }

func (f *fakeProxyFailureDiagnostics) ProxyFailureAnnotations(stableIDs []string) map[string]speedtest.AgentDiagnostic {
	f.read = append(f.read, stableIDs...)
	return f.annotations
}

func (f *fakeProxyFailureDiagnostics) AwaitProxyFailure(_ context.Context, stableIDs []string) map[string]speedtest.AgentDiagnostic {
	f.awaited = append(f.awaited, stableIDs...)
	return f.annotations
}

func proxyFailureAlerts() []nodeDownAlert {
	return []nodeDownAlert{
		{
			Proxy: &models.ProxyConfig{StableID: "node-pf", Name: "Germany", Protocol: "vless"},
			State: nodeAlertState{Status: checker.AvailabilityStateProxyFailure, WasDown: true, FailCount: 2},
		},
		{
			Proxy: &models.ProxyConfig{StableID: "node-off", Name: "Finland", Protocol: "vless"},
			State: nodeAlertState{Status: checker.AvailabilityStateOffline, WasDown: true, FailCount: 2},
		},
	}
}

// Only a proxy failure is asked about: an offline node was never probed, and
// whatever an entry holds for it describes some other outage.
func TestProxyFailureAlertCarriesTheAgentVerdictAndLeavesOfflineAlone(t *testing.T) {
	checkedAt := time.Date(2026, 9, 1, 1, 3, 0, 0, time.UTC)
	probes := &fakeProxyFailureDiagnostics{enabled: true, wait: time.Second, annotations: map[string]speedtest.AgentDiagnostic{
		"node-pf":  {State: speedtest.AgentDiagnosticReproduced, AgentName: "DE-01", RemoteStatus: "proxy_failure", CheckedAt: checkedAt},
		"node-off": {State: speedtest.AgentDiagnosticNotReproduced, AgentName: "NL-03"},
	}}
	service := &Service{}
	service.SetProxyFailureDiagnostics(probes)

	alerts := service.attachProxyFailureDiagnostics(proxyFailureAlerts())
	if len(probes.awaited) != 1 || probes.awaited[0] != "node-pf" {
		t.Fatalf("awaited = %v, want only the proxy-failure node", probes.awaited)
	}
	if alerts[1].Agent != nil {
		t.Fatalf("offline alert carries an agent verdict: %+v", alerts[1].Agent)
	}
	message := formatNodeDownAlertMessage(alerts[0], checkedAt.Add(time.Minute))
	if !strings.Contains(message.HTML, "Агент DE-01") || !strings.Contains(message.HTML, "проблема воспроизведена") ||
		!strings.Contains(message.HTML, formatCheckedAt(checkedAt)) {
		t.Fatalf("proxy-failure alert = %q, want the agent's verdict with its time", message.HTML)
	}
	group := formatNodeDownGroupMessage(alerts, checkedAt.Add(time.Minute))
	if strings.Count(group.HTML, "Агент") != 1 || strings.Count(group.RichHTML, "Агент") != 1 {
		t.Fatalf("group alert = %q, want one agent line for the one proxy failure", group.HTML)
	}
}

// With the wait switched off the alert reads what is there, and with the
// trigger off it reads nothing at all.
func TestProxyFailureAlertReadsWithoutWaitingOrNotAtAll(t *testing.T) {
	noWait := &fakeProxyFailureDiagnostics{enabled: true, annotations: map[string]speedtest.AgentDiagnostic{
		"node-pf": {State: speedtest.AgentDiagnosticRunning, AgentName: "DE-01"},
	}}
	service := &Service{}
	service.SetProxyFailureDiagnostics(noWait)
	alerts := service.attachProxyFailureDiagnostics(proxyFailureAlerts())
	if len(noWait.awaited) != 0 || len(noWait.read) != 1 || alerts[0].Agent == nil {
		t.Fatalf("awaited %v, read %v, agent %+v; want one read without waiting", noWait.awaited, noWait.read, alerts[0].Agent)
	}
	if line := formatNodeAgentDiagnosticHTML(alerts[0].Agent); !strings.Contains(line, "проверка идёт") || strings.Contains(line, " · ") {
		t.Fatalf("running verdict = %q, want no time for a probe still in flight", line)
	}

	disabled := &fakeProxyFailureDiagnostics{}
	service.SetProxyFailureDiagnostics(disabled)
	if alerts := service.attachProxyFailureDiagnostics(proxyFailureAlerts()); alerts[0].Agent != nil || len(disabled.read)+len(disabled.awaited) != 0 {
		t.Fatalf("disabled trigger was consulted: %+v", disabled)
	}
}
