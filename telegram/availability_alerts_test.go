package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/diagnostics"
	"xray-checker/models"
	"xray-checker/paneltelemetry"
	"xray-checker/speedtest"
)

type fakeAvailabilityDiagnostics struct {
	enabled     bool
	wait        time.Duration
	awaited     []string
	read        []string
	annotations map[string]speedtest.AgentDiagnostic
}

func (f *fakeAvailabilityDiagnostics) AvailabilityEnabled() bool { return f.enabled }

func (f *fakeAvailabilityDiagnostics) AlertWait() time.Duration { return f.wait }

func (f *fakeAvailabilityDiagnostics) AvailabilityAnnotations(stableIDs []string) map[string]speedtest.AgentDiagnostic {
	f.read = append(f.read, stableIDs...)
	return f.annotations
}

func (f *fakeAvailabilityDiagnostics) AwaitAvailability(_ context.Context, stableIDs []string) map[string]speedtest.AgentDiagnostic {
	f.awaited = append(f.awaited, stableIDs...)
	return f.annotations
}

func availabilityAlerts() []nodeDownAlert {
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

func taskOf(kind string) *speedtest.AgentProbeTask {
	return &speedtest.AgentProbeTask{Kind: kind}
}

// Each alert carries the answer to its own question: the proxy failure the
// tunnel verdict, the unreachable node the offline one. An answer asked about
// another kind of failure is left off, so "the tunnel works from elsewhere" is
// never printed under "unreachable".
func TestDownAlertsCarryTheAgentVerdictForTheirOwnKindOfFailure(t *testing.T) {
	checkedAt := time.Date(2026, 9, 1, 1, 3, 0, 0, time.UTC)
	probes := &fakeAvailabilityDiagnostics{enabled: true, wait: time.Second, annotations: map[string]speedtest.AgentDiagnostic{
		"node-pf":  {State: speedtest.AgentDiagnosticReproduced, AgentName: "DE-01", RemoteStatus: "proxy_failure", CheckedAt: checkedAt, Task: taskOf(diagnostics.AutomationKindProxyFailure)},
		"node-off": {State: speedtest.AgentDiagnosticNotReproduced, AgentName: "NL-03", RemoteStatus: "online", CheckedAt: checkedAt, Task: taskOf(diagnostics.AutomationKindOffline)},
	}}
	service := &Service{}
	service.SetAvailabilityDiagnostics(probes)

	alerts := service.attachAvailabilityDiagnostics(availabilityAlerts())
	if len(probes.awaited) != 2 {
		t.Fatalf("awaited = %v, want both failing nodes", probes.awaited)
	}
	if alerts[0].Agent == nil || alerts[1].Agent == nil {
		t.Fatalf("agents = %+v / %+v, want a verdict on both alerts", alerts[0].Agent, alerts[1].Agent)
	}
	message := formatNodeDownAlertMessage(alerts[0], checkedAt.Add(time.Minute))
	if !strings.Contains(message.HTML, "Агент DE-01") || !strings.Contains(message.HTML, "туннель не работает и у него") ||
		!strings.Contains(message.HTML, formatCheckedAt(checkedAt)) {
		t.Fatalf("proxy-failure alert = %q, want the agent's verdict with its time", message.HTML)
	}
	offline := formatNodeDownAlertMessage(alerts[1], checkedAt.Add(time.Minute))
	if !strings.Contains(offline.HTML, "Агент NL-03") || !strings.Contains(offline.HTML, "блокировка IP или маршрута") {
		t.Fatalf("offline alert = %q, want the blocked-path reading", offline.HTML)
	}

	// The offline node's entry still holds the answer to its earlier proxy
	// failure: it must not be shown under the offline alert.
	probes.annotations["node-off"] = speedtest.AgentDiagnostic{State: speedtest.AgentDiagnosticNotReproduced, AgentName: "NL-03", Task: taskOf(diagnostics.AutomationKindProxyFailure)}
	if alerts := service.attachAvailabilityDiagnostics(availabilityAlerts()); alerts[1].Agent != nil {
		t.Fatalf("offline alert carries a proxy-failure verdict: %+v", alerts[1].Agent)
	}
}

func TestOfflineVerdictWordingSeparatesADeadHostFromADeadService(t *testing.T) {
	for _, test := range []struct {
		name   string
		agent  speedtest.AgentDiagnostic
		expect string
	}{
		{"dead everywhere", speedtest.AgentDiagnostic{State: speedtest.AgentDiagnosticReproduced, RemoteStatus: "offline", Task: taskOf(diagnostics.AutomationKindOffline)}, "отказ хоста или хостера"},
		{"host answers, tunnel does not", speedtest.AgentDiagnostic{State: speedtest.AgentDiagnosticReproduced, RemoteStatus: "proxy_failure", Task: taskOf(diagnostics.AutomationKindOffline)}, "упал сервис на ноде"},
		{"works from the agent", speedtest.AgentDiagnostic{State: speedtest.AgentDiagnosticNotReproduced, RemoteStatus: "online", Task: taskOf(diagnostics.AutomationKindOffline)}, "недоступна только с пути checker-а"},
		{"tunnel works for the agent", speedtest.AgentDiagnostic{State: speedtest.AgentDiagnosticNotReproduced, Task: taskOf(diagnostics.AutomationKindProxyFailure)}, "сбой на пути checker-а"},
	} {
		if line := formatAvailabilityAgentHTML(&test.agent); !strings.Contains(line, test.expect) {
			t.Errorf("%s: %q, want %q", test.name, line, test.expect)
		}
	}
}

// With the wait switched off the alert reads what is there, and with the
// triggers off it reads nothing at all.
func TestAvailabilityAlertReadsWithoutWaitingOrNotAtAll(t *testing.T) {
	noWait := &fakeAvailabilityDiagnostics{enabled: true, annotations: map[string]speedtest.AgentDiagnostic{
		"node-pf": {State: speedtest.AgentDiagnosticRunning, AgentName: "DE-01", Task: taskOf(diagnostics.AutomationKindProxyFailure)},
	}}
	service := &Service{}
	service.SetAvailabilityDiagnostics(noWait)
	alerts := service.attachAvailabilityDiagnostics(availabilityAlerts())
	if len(noWait.awaited) != 0 || len(noWait.read) != 2 || alerts[0].Agent == nil {
		t.Fatalf("awaited %v, read %v, agent %+v; want one read without waiting", noWait.awaited, noWait.read, alerts[0].Agent)
	}
	if line := formatNodeAgentDiagnosticHTML(alerts[0].Agent); !strings.Contains(line, "проверка идёт") || strings.Contains(line, " · ") {
		t.Fatalf("running verdict = %q, want no time for a probe still in flight", line)
	}

	disabled := &fakeAvailabilityDiagnostics{}
	service.SetAvailabilityDiagnostics(disabled)
	if alerts := service.attachAvailabilityDiagnostics(availabilityAlerts()); alerts[0].Agent != nil || len(disabled.read)+len(disabled.awaited) != 0 {
		t.Fatalf("disabled triggers were consulted: %+v", disabled)
	}
}

// The panel's view goes under the alert: whether it still reaches the node,
// how many clients the node lost, and memory and load for the known failure of
// a leaking transport.
func TestDownAlertCarriesThePanelView(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 3, 0, 0, time.UTC)
	alert := availabilityAlerts()[0]
	alert.Panel = &paneltelemetry.Status{
		Connected: true, UsersOnline: 3, UsersOnlineBefore: 41, HasBefore: true,
		MemoryUsedPercent: 88, Load1: 1.9, CPUs: 2, RxMbps: 120, TxMbps: 80, XrayUptime: 12 * 24 * time.Hour,
	}
	message := formatNodeDownAlertMessage(alert, now)
	for _, want := range []string{"Панель", "онлайн 3 (до сбоя 41)", "память 88%", "load 1.90/2 CPU", "xray 12 д"} {
		if !strings.Contains(message.HTML, want) {
			t.Errorf("alert = %q, want %q", message.HTML, want)
		}
	}
	alert.Panel = &paneltelemetry.Status{Connected: false, Message: "connect ECONNREFUSED"}
	if line := formatPanelStatusHTML(alert.Panel); !strings.Contains(line, "панель не видит ноду") || !strings.Contains(line, "ECONNREFUSED") {
		t.Fatalf("panel line = %q, want the lost control connection", line)
	}
}
