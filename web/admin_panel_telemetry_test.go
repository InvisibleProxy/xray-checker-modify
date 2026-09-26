package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/paneltelemetry"
)

type recordingAdminPanel struct {
	before map[string]time.Time
}

func (r *recordingAdminPanel) NodeStatus(server string, before time.Time) (paneltelemetry.Status, bool) {
	r.before[server] = before
	return paneltelemetry.Status{
		Name: "Germany-02", Connected: true, UsersOnline: 1, UsersOnlineBefore: 14, HasBefore: true,
		XrayUptime: 26 * time.Minute, FetchedAt: time.Now(),
	}, true
}

// The node card gets the same panel facts the Telegram bot quotes, compared
// with the start of the node's failure, and only for the deployment's own
// subscription: a node added from the panel belongs to another service.
func TestAdminProxiesCarryThePanelViewOfOwnNodes(t *testing.T) {
	own := &models.ProxyConfig{StableID: "own", Name: "Германия #2", Protocol: "vless", Server: "31.59.178.215", Port: 443, UUID: "a"}
	added := &models.ProxyConfig{StableID: "added", Name: "Added", SourceID: "src-1", Protocol: "vless", Server: "added.example", Port: 443, UUID: "b"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{own, added}, 10000, "", 1, "", "", 1, 0, "status")
	failingSince := time.Now().Add(-time.Hour).Truncate(time.Second)
	if !proxyChecker.RestoreProxyFailureStatus(own.StableID, failingSince, checker.HostCheckDetails{}, checker.PingCheckDetails{}) {
		t.Fatal("failed to seed the proxy failure")
	}
	panel := &recordingAdminPanel{before: map[string]time.Time{}}

	rec := httptest.NewRecorder()
	AdminProxiesHandler(proxyChecker, 10000, panel).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies", nil))
	var envelope struct {
		Data []AdminProxyInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := make(map[string]AdminProxyInfo, len(envelope.Data))
	for _, info := range envelope.Data {
		byID[info.StableID] = info
	}
	view := byID["own"].Panel
	if view == nil || view.Link != paneltelemetry.LinkConnected || view.UsersOnline != 1 ||
		view.UsersOnlineBefore == nil || *view.UsersOnlineBefore != 14 ||
		view.XrayUptime == nil || *view.XrayUptime != (paneltelemetry.Uptime{Value: 26, Unit: paneltelemetry.UptimeMinutes}) {
		t.Fatalf("own node panel = %+v", view)
	}
	if !panel.before["31.59.178.215"].Equal(failingSince) {
		t.Fatalf("panel compared with %s, want the failure start %s", panel.before["31.59.178.215"], failingSince)
	}
	if byID["added"].Panel != nil {
		t.Fatalf("a node added from the panel got the panel view: %+v", byID["added"].Panel)
	}
	if _, asked := panel.before["added.example"]; asked {
		t.Fatal("the panel was asked about a node added from the panel")
	}

	rec = httptest.NewRecorder()
	AdminProxiesHandler(proxyChecker, 10000, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies", nil))
	envelope.Data = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, info := range envelope.Data {
		if info.Panel != nil {
			t.Fatalf("telemetry off, yet %s has a panel view", info.StableID)
		}
	}
}
