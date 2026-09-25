package telegram

import (
	"strings"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/paneltelemetry"
	"xray-checker/speedtest"
)

type recordingPanel struct {
	status paneltelemetry.Status
	before []time.Time
}

func (r *recordingPanel) NodeStatus(_ string, before time.Time) (paneltelemetry.Status, bool) {
	r.before = append(r.before, before)
	return r.status, true
}

// The bot's node card quotes the panel like the alerts do, and every place the
// bot quotes it compares the online count with the same moment the admin node
// card does: when the node's failure began, not when the alert state last
// restarted its own timer.
func TestPanelLineSharesTheFailureStartAcrossTheBot(t *testing.T) {
	proxy := &models.ProxyConfig{Protocol: "vless", Server: "31.76.38.179", Port: 443, Name: "Германия #2", UUID: "uuid"}
	proxy.StableID = proxy.GenerateStableID()
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	failingSince := time.Now().Add(-time.Hour).Truncate(time.Second)
	failure := checker.FailureDetails{Code: checker.FailureCodeProxyTimeout, Summary: checker.FailureSummary(checker.FailureCodeProxyTimeout)}
	if !proxyChecker.RestoreProxyFailureStatus(proxy.StableID, failingSince, checker.HostCheckDetails{}, checker.PingCheckDetails{}, failure) {
		t.Fatal("failed to seed the proxy failure")
	}
	service := NewService("", proxyChecker, speedtest.NewManager(proxyChecker, 10000, "", speedtest.TestConfig{}), 10000)
	panel := &recordingPanel{status: paneltelemetry.Status{
		Name: "Germany-02", Connected: true, UsersOnline: 1, UsersOnlineBefore: 14, HasBefore: true,
		XrayUptime: 26 * time.Minute, FetchedAt: time.Now(),
	}}
	service.SetPanelTelemetry(panel)
	// The alert state's own clock restarted when a lost ping reclassified the
	// failure; the panel baseline must not follow it.
	service.alerts[proxy.StableID] = nodeAlertState{
		FailCount: 5, WasDown: true, Status: checker.AvailabilityStateProxyFailure,
		ProxyFailureSince: time.Now().Add(-5 * time.Minute),
	}

	want := "<b>Панель</b>: 🟢 связь есть · онлайн 1 (до сбоя 14) · xray 26 мин"
	if card := service.formatNodeDetails(proxy.StableID); !strings.Contains(card, want) {
		t.Fatalf("node card lacks the panel line:\n%s", card)
	}
	if card := service.formatNodeDetailsMessage(proxy.StableID); !strings.Contains(card.RichHTML, "<p>"+want+"</p>") {
		t.Fatalf("rich node card lacks the panel line:\n%s", card.RichHTML)
	}
	if line := formatPanelStatusHTML(service.nodePanelStatus(proxy)); line != want {
		t.Fatalf("alert panel line = %q, want %q", line, want)
	}
	if lines := service.speedPanelLines([]speedtest.Result{{StableID: proxy.StableID, Offline: true}}, 100); lines[proxy.StableID] != want {
		t.Fatalf("speed report panel line = %q, want %q", lines[proxy.StableID], want)
	}
	if len(panel.before) == 0 {
		t.Fatal("the panel was never read")
	}
	for _, before := range panel.before {
		if !before.Equal(failingSince) {
			t.Fatalf("panel compared with %s, want the failure start %s (all: %v)", before, failingSince, panel.before)
		}
	}
}
