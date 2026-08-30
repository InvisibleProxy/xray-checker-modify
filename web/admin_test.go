package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xray-checker/backup"
	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/nodearchive"
	"xray-checker/nodemerge"
	"xray-checker/speedtest"
)

type nodeMergeServiceStub struct {
	previewRequest [2]string
	stageRequest   [3]string
	preview        nodemerge.Preview
	previewErr     error
	stage          nodemerge.StageResult
	stageErr       error
}

func (s *nodeMergeServiceStub) Preview(sourceStableID, targetStableID string) (nodemerge.Preview, error) {
	s.previewRequest = [2]string{sourceStableID, targetStableID}
	return s.preview, s.previewErr
}

func (s *nodeMergeServiceStub) Stage(sourceStableID, targetStableID, confirmationToken string) (nodemerge.StageResult, error) {
	s.stageRequest = [3]string{sourceStableID, targetStableID, confirmationToken}
	return s.stage, s.stageErr
}

func TestAdminTemplateExposesRowAndGroupCheckRunActions(t *testing.T) {
	var rendered bytes.Buffer
	if err := RenderAdmin(&rendered); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	html := rendered.String()
	for _, marker := range []string{
		`id="selection-check"`,
		`id="selection-run"`,
		`id="select-all-nodes"`,
		`selectAll.indeterminate = selectedVisible > 0 && selectedVisible < visibleIDs.length`,
		`filteredProxies().forEach((proxy) => {`,
		`data-check-id="${escapeHtml(proxy.stableId)}"`,
		`data-run-id="${escapeHtml(proxy.stableId)}"`,
		`id="toggle-maintenance"`,
		`function renderMaintenanceControl()`,
		`button.dataset.maintenanceId = proxy.stableId`,
		`checkDisabled: state.availabilityCheckRunning || maintenanceUpdating`,
		`runDisabled: Boolean(run && run.running) || maintenanceUpdating`,
		`const ids = [...new Set(stableIds.filter(Boolean))]`,
		`request("/nodes-overview/maintenance"`,
		`proxy.maintenance`,
		`data-node-toggle="${escapeHtml(proxy.stableId)}"`,
		`data-node-toggle-area="${escapeHtml(proxy.stableId)}"`,
		`class="node-card-panel"`,
		`data-chart-range="7d"`,
		`data-node-metric-chart="${escapeHtml(proxy.stableId)}"`,
		`data-chart-view-select="availability"`,
		"request(`${endpoint}?${query.toString()}`)",
		`preserveAspectRatio="none"`,
		`style="mask-type: alpha"`,
		`class="chart-area"`,
		`class="chart-gap-bridge"`,
		`ordered.forEach((result) => {`,
		`const singlePointTimes = new Set(successful.length === 1`,
		`class="chart-error-band"`,
		`class="chart-last-marker"`,
		`class="chart-cursor"`,
		`function availabilityChartScale(results)`,
		`chartPercentile(latencies, 0.90)`,
		`.node-chart-stats {`,
		`grid-template-columns: repeat(3, minmax(0, 1fr)) minmax(220px, 1.35fr)`,
		`.node-chart-stats .node-detail-stat strong`,
		`min-height: 2.5em`,
		`class="node-detail-grid node-chart-stats"`,
		`class="chart-outlier-marker"`,
		`class="legend-peak"`,
		`Peaks above ${escapeHtml(formatLatencyMs(availabilityScale.yMax))} are pinned to the top`,
		`id="node-speed-tooltip"`,
		`function bindMetricChartInteractions(root = $("nodes"))`,
		`function animateNodePanelClose(stableId)`,
		`openNodeStableIds: new Set()`,
		`dashboardCardHistory: new Map()`,
		`function applyNodeCard(card, proxy)`,
		`const sameOrder = proxies.length > 0`,
		`function setDashboardRange(stableId, range)`,
		`function measurementStats(results)`,
		`!isLowSpeed(result)`,
		`${measurements.failed} failed · ${measurements.successPercent}`,
		`id="tab-incidents"`,
		`id="incidents"`,
		`proxy.failureSummary`,
		`data-merge-node="${escapeHtml(node.stableId || "")}"`,
		`id="node-merge-dialog"`,
		`id="node-merge-target"`,
		`function matchingActiveMergeTargets(source)`,
		`function openNodeMergeDialog(sourceStableId)`,
		`function previewNodeMerge()`,
		`function stageNodeMerge()`,
		`function renderNodeMergeNotice()`,
		`pendingNodeMergeKey`,
		`Node merge completed successfully`,
		`Merge applied`,
		`.merge-notice[hidden]`,
		`request("/nodes-overview/merge/preview"`,
		`confirmationToken: preview.confirmationToken`,
		`mergedFromStableIds`,
		`.filter((node) => node.active === true)`,
		`No active nodes to refresh`,
		`function nodeDiagnosticsHTML(proxy)`,
		`function updateNodeDiagnostics(card, proxy)`,
		`function diagnosticAgentBusy(stableId, agentId)`,
		`data-diagnose-id="${escapeHtml(proxy.stableId)}"`,
		`data-diagnostic-start`,
		`.node-diagnostics [data-node-diagnostics-maintenance][hidden]`,
		`request("/diagnostic-sessions"`,
		`data-cancel-diagnostic`,
		`/diagnostic-sessions/export?sessionId=`,
		`it does not change Availability`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("admin template does not contain %q", marker)
		}
	}
	if strings.Contains(html, `targets.length !== 1`) {
		t.Fatal("admin template still blocks multiple compatible merge targets")
	}
	if strings.Contains(html, `data-maintenance-id=`) {
		t.Fatal("admin template still exposes maintenance in node card actions")
	}
	if strings.Contains(html, `diagnostics.outerHTML = nodeDiagnosticsHTML(proxy)`) {
		t.Fatal("admin polling still replaces the remote diagnostics controls")
	}
}

func TestAdminTemplateSeparatesGlobalSettingsFromNodeControls(t *testing.T) {
	var rendered bytes.Buffer
	if err := RenderAdmin(&rendered); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	html := rendered.String()
	dashboardStart := strings.Index(html, `id="dashboard-view"`)
	settingsStart := strings.Index(html, `id="settings-view"`)
	nodesOverviewStart := strings.Index(html, `id="nodes-overview-view"`)
	if dashboardStart < 0 || settingsStart <= dashboardStart || nodesOverviewStart <= settingsStart {
		t.Fatalf("admin views are missing or out of order: dashboard=%d settings=%d nodes=%d", dashboardStart, settingsStart, nodesOverviewStart)
	}

	dashboard := html[dashboardStart:settingsStart]
	for _, marker := range []string{
		`aria-label="Node controls"`,
		`id="settings-tab-run"`,
		`id="settings-tab-filters"`,
		`id="settings-tab-schedule"`,
		`id="node-url"`,
		`id="toggle-maintenance"`,
		`id="mute-scope"`,
		`id="run"`,
	} {
		if !strings.Contains(dashboard, marker) {
			t.Errorf("dashboard node controls do not contain %q", marker)
		}
	}
	for _, marker := range []string{
		`id="refresh-subscription"`,
		`id="history-retention-days"`,
		`id="telegram-enabled"`,
		`id="download-backup"`,
	} {
		if strings.Contains(dashboard, marker) {
			t.Errorf("dashboard node controls still contain global setting %q", marker)
		}
	}

	settings := html[settingsStart:nodesOverviewStart]
	if !strings.Contains(html[:dashboardStart], `id="tab-settings"`) {
		t.Error("admin section navigation does not contain the Settings tab")
	}
	for _, marker := range []string{
		`aria-label="Global settings"`,
		`id="global-settings-pane-subscription"`,
		`id="refresh-subscription"`,
		`id="global-settings-pane-history"`,
		`id="history-retention-days"`,
		`id="global-settings-pane-telegram"`,
		`id="telegram-enabled"`,
		`id="global-settings-pane-agents"`,
		`id="create-diagnostic-agent"`,
		`id="diagnostic-agents-list"`,
		`id="global-settings-pane-backup"`,
		`id="download-backup"`,
	} {
		if !strings.Contains(settings, marker) {
			t.Errorf("global settings view does not contain %q", marker)
		}
	}
	if strings.Contains(settings, `id="mute-scope"`) {
		t.Error("global Telegram settings still contain selected-node mute controls")
	}
	for _, marker := range []string{
		`function switchGlobalSettingsPane(pane)`,
		`$("settings-view").hidden = tab !== "settings"`,
		`$("save-history-retention").addEventListener("click", saveHistoryRetention)`,
		`proxyIds: Array.isArray(schedule.proxyIds) ? schedule.proxyIds : []`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("admin template does not contain global settings behavior %q", marker)
		}
	}
}

func TestAdminTemplateColorsAvailabilityDiagnosticsIndependently(t *testing.T) {
	var rendered bytes.Buffer
	if err := RenderAdmin(&rendered); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	html := rendered.String()
	for _, marker := range []string{
		`function nodeAvailabilityDetailsHTML(proxy)`,
		`availability-diagnostic ${proxy.hostCheckOnline ? "ok" : "error"}`,
		`availability-diagnostic ${proxy.pingCheckOnline ? "ok" : "error"}`,
		`class="availability-separator"`,
		`${nodeAvailabilityDetailsHTML(proxy)}`,
		`availability.innerHTML = nodeAvailabilityDetailsHTML(proxy)`,
		`.node-detail-stat .availability-diagnostic.ok`,
		`.node-detail-stat .availability-diagnostic.error`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("admin template missing independently colored availability marker %q", marker)
		}
	}
}

func TestWebTemplatesCopyNodeAddressesAndRefreshLiveData(t *testing.T) {
	var dashboard bytes.Buffer
	if err := RenderIndex(&dashboard, PageData{
		CheckInterval:     30,
		ShowServerDetails: true,
		Endpoints: []EndpointInfo{{
			Name:       "Test node",
			StableID:   "node-1",
			ServerInfo: "203.0.113.7:443",
			Server:     "203.0.113.7",
			ServerPort: 443,
		}},
	}); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	for _, marker := range []string{
		`@click.stop="copyIP(proxy.server)"`,
		`203.0.113.7`,
		`serverPort:`,
		`autoRefresh: localStorage.getItem('autoRefresh') !== 'false'`,
		`refreshInFlight: false`,
		`const proxy = this.proxies.find(p => p.stableId === updated.stableId)`,
		`clearInterval(this.countdownInterval)`,
	} {
		if !strings.Contains(dashboard.String(), marker) {
			t.Fatalf("dashboard template does not contain %q", marker)
		}
	}

	var admin bytes.Buffer
	if err := RenderAdmin(&admin); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	for _, marker := range []string{
		`data-copy-ip="${escapeHtml(address)}"`,
		`function copyIPButton(value, marker = "")`,
		`function copyToClipboard(value)`,
		`if (state.loadRunning) return`,
		`if (state.speedPollRunning || state.loadRunning`,
		`document.addEventListener("visibilitychange"`,
		`Automatic refresh failed: ${err.message}`,
	} {
		if !strings.Contains(admin.String(), marker) {
			t.Fatalf("admin template does not contain %q", marker)
		}
	}
}

func TestWebTemplatesExposeSharedEnglishRussianLocalization(t *testing.T) {
	var admin bytes.Buffer
	if err := RenderAdmin(&admin); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	var dashboard bytes.Buffer
	if err := RenderIndex(&dashboard, PageData{}); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}

	for name, rendered := range map[string]string{
		"admin":     admin.String(),
		"dashboard": dashboard.String(),
	} {
		for _, marker := range []string{
			`localization.js`,
			`data-language="en"`,
			`data-language="ru"`,
			`aria-label="Switch language"`,
		} {
			if !strings.Contains(rendered, marker) {
				t.Fatalf("%s template does not contain %q", name, marker)
			}
		}
	}

	asset, err := staticFiles.ReadFile("static/localization.js")
	if err != nil {
		t.Fatalf("read localization asset: %v", err)
	}
	localization := string(asset)
	for _, marker := range []string{
		`xray-checker-language`,
		`new MutationObserver`,
		`document.documentElement.lang = language`,
		`"Nodes Overview": "Nodes Overview"`,
		`"Check": "Check", "Run": "Run"`,
		`"Announce locations": "Локации Announce"`,
		`"Loading speed-test history…": "Загрузка speedtest history…"`,
		`"Loading availability history…": "Загрузка availability history…"`,
		`Пики выше $1 ms отмечены у верхней границы`,
		`"IP / server copied!": "IP / сервер скопирован!"`,
	} {
		if !strings.Contains(localization, marker) {
			t.Fatalf("localization asset does not contain %q", marker)
		}
	}
}

func TestLocalizationAssetUsesRevalidatingCachePolicy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/static/localization.js?v=1", nil)
	recorder := httptest.NewRecorder()

	StaticHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if !strings.Contains(recorder.Body.String(), "xray-checker-language") {
		t.Fatal("localization asset body is missing language storage key")
	}
}

func TestAdminTemplateExposesRemnawaveMessageConstructor(t *testing.T) {
	var rendered bytes.Buffer
	if err := RenderAdmin(&rendered); err != nil {
		t.Fatalf("RenderAdmin() error = %v", err)
	}
	html := rendered.String()
	for _, marker := range []string{
		`id="remnawave-message-constructor"`,
		`id="remnawave-message-single-template"`,
		`id="remnawave-message-multiple-template"`,
		`id="remnawave-message-all-template"`,
		`id="remnawave-message-partial-single-template"`,
		`id="remnawave-message-partial-multiple-template"`,
		`id="remnawave-message-maintenance-single-template"`,
		`id="remnawave-message-maintenance-multiple-template"`,
		`id="remnawave-message-healthy-template"`,
		`id="remnawave-message-fallback-template"`,
		`id="remnawave-message-partial-fallback-template"`,
		`id="remnawave-message-maintenance-fallback-template"`,
		`id="remnawave-message-maintenance-mixed-fallback-template"`,
		`data-remnawave-token="{location}"`,
		`data-remnawave-token="{locations}"`,
		`data-remnawave-token="{affected}"`,
		`function renderRemnawaveMessagePreviews()`,
		`function insertRemnawaveTemplateToken(targetID, token)`,
		`singleLocation: remnawaveScenarioFromForm("single")`,
		`partialSingleLocation: remnawaveScenarioFromForm("partial-single")`,
		`partialMultipleLocations: remnawaveScenarioFromForm("partial-multiple")`,
		`maintenanceSingleLocation: remnawaveScenarioFromForm("maintenance-single")`,
		`maintenanceMultipleLocations: remnawaveScenarioFromForm("maintenance-multiple")`,
		`partialFallback: $("remnawave-message-fallback-template").value.trim()`,
		`partialAvailabilityFallback: $("remnawave-message-partial-fallback-template").value.trim()`,
		`maintenanceFallback: $("remnawave-message-maintenance-fallback-template").value.trim()`,
		`maintenanceMixedFallback: $("remnawave-message-maintenance-mixed-fallback-template").value.trim()`,
		`id="remnawave-add-location"`,
		`id="remnawave-locations"`,
		`function remnawaveLocationRowsFromForm()`,
		`function renderRemnawaveLocations(input)`,
		`data-remnawave-location-key`,
		`data-remnawave-member-node`,
		`data-remnawave-member-host`,
		`const locations = Object.create(null);`,
		`locations,`,
		`function activeSpeedResults()`,
		`activeIDs.has(result.stableId)`,
		`function monitoringSpeedResults()`,
		`const results = activeSpeedResults();`,
		`!result.maintenanceProbe && !maintenanceIDs.has(result.stableId)`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("admin template does not contain %q", marker)
		}
	}
	if strings.Contains(html, `nodeMappings,`) || strings.Contains(html, `remnawave-node-mappings`) {
		t.Fatal("admin template still serializes the legacy server-first Remnawave mapping")
	}
}

func TestAdminNodeMergePreviewAndStageHandlers(t *testing.T) {
	service := &nodeMergeServiceStub{
		preview: nodemerge.Preview{
			Source:            nodemerge.NodeSnapshot{StableID: "retired", ResultCount: 490},
			Target:            nodemerge.NodeSnapshot{StableID: "active"},
			MergedResultCount: 490,
			ConfirmationToken: "confirmed-candidate",
			RestartRequired:   true,
			IdentityWarnings:  []string{"Node name changed", "Port changed from 443 to 8443"},
		},
		stage: nodemerge.StageResult{
			SourceStableID: "retired", TargetStableID: "active", RestartRequired: true,
			Message: "Node merge staged; restart the application to apply it",
		},
	}

	previewRecorder := httptest.NewRecorder()
	previewRequest := httptest.NewRequest(http.MethodPost, "/api/v1/admin/nodes-overview/merge/preview", strings.NewReader(`{"sourceStableId":"retired","targetStableId":"active"}`))
	AdminNodesOverviewMergePreviewHandler(service).ServeHTTP(previewRecorder, previewRequest)
	if previewRecorder.Code != http.StatusOK || service.previewRequest != [2]string{"retired", "active"} {
		t.Fatalf("preview response %d %s, request=%v", previewRecorder.Code, previewRecorder.Body.String(), service.previewRequest)
	}
	if !strings.Contains(previewRecorder.Body.String(), `"confirmationToken":"confirmed-candidate"`) || !strings.Contains(previewRecorder.Body.String(), `"mergedResultCount":490`) || !strings.Contains(previewRecorder.Body.String(), `"identityWarnings":["Node name changed","Port changed from 443 to 8443"]`) {
		t.Fatalf("unexpected preview response: %s", previewRecorder.Body.String())
	}

	stageRecorder := httptest.NewRecorder()
	stageRequest := httptest.NewRequest(http.MethodPost, "/api/v1/admin/nodes-overview/merge", strings.NewReader(`{"sourceStableId":"retired","targetStableId":"active","confirmationToken":"confirmed-candidate"}`))
	AdminNodesOverviewMergeHandler(service).ServeHTTP(stageRecorder, stageRequest)
	if stageRecorder.Code != http.StatusOK || service.stageRequest != [3]string{"retired", "active", "confirmed-candidate"} {
		t.Fatalf("stage response %d %s, request=%v", stageRecorder.Code, stageRecorder.Body.String(), service.stageRequest)
	}
	if !strings.Contains(stageRecorder.Body.String(), `"restartRequired":true`) {
		t.Fatalf("unexpected stage response: %s", stageRecorder.Body.String())
	}
}

func TestAdminNodeMergeHandlerReportsStaleCandidateAsConflict(t *testing.T) {
	service := &nodeMergeServiceStub{stageErr: fmt.Errorf("node merge candidate changed since preview; review it again")}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/nodes-overview/merge", strings.NewReader(`{"sourceStableId":"retired","targetStableId":"active","confirmationToken":"stale"}`))
	AdminNodesOverviewMergeHandler(service).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, recorder.Code, recorder.Body.String())
	}
}

func TestAdminSpeedTestHistoryHandlerFiltersByTimeRange(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root := t.TempDir()
	schedulePath := filepath.Join(root, "speedtest_schedule.json")
	results := []speedtest.Result{
		{StableID: "node-1", Name: "Node one", Mbps: 30, CheckedAt: now.Add(-time.Hour)},
		{StableID: "node-1", Name: "Node one", Mbps: 20, CheckedAt: now.Add(-48 * time.Hour)},
		{StableID: "node-1", Name: "Node one", Mbps: 10, CheckedAt: now.Add(-10 * 24 * time.Hour)},
	}
	state := map[string]any{
		"version":   1,
		"updatedAt": now,
		"lastRun":   map[string]any{},
		"results":   map[string]speedtest.Result{"node-1": results[0]},
		"history":   map[string][]speedtest.Result{"node-1": results},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "speedtest_results.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := speedtest.NewManager(proxyChecker, 10000, schedulePath, speedtest.TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatalf("load speed history: %v", err)
	}
	handler := AdminSpeedTestHistoryHandler(manager)
	rec := httptest.NewRecorder()
	path := fmt.Sprintf(
		"/api/v1/admin/speed-tests/history?stableId=node-1&from=%s&to=%s",
		now.Add(-3*24*time.Hour).Format(time.RFC3339),
		now.Format(time.RFC3339),
	)
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool               `json:"success"`
		Data    []speedtest.Result `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Success || len(body.Data) != 2 || body.Data[0].Mbps != 30 || body.Data[1].Mbps != 20 {
		t.Fatalf("unexpected filtered history: %+v", body)
	}
}

func TestAdminSpeedTestHistoryHandlerRejectsInvalidRange(t *testing.T) {
	manager := speedtest.NewManager(nil, 10000, "", speedtest.TestConfig{})
	handler := AdminSpeedTestHistoryHandler(manager)
	tests := []string{
		"/api/v1/admin/speed-tests/history?stableId=node-1&from=not-a-time",
		"/api/v1/admin/speed-tests/history?stableId=node-1&from=2026-08-15T12:00:00Z&to=2026-08-15T11:00:00Z",
	}
	for _, path := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected status %d, got %d: %s", path, http.StatusBadRequest, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminAvailabilityHistoryHandlerFiltersByTimeRange(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "node_registry.json")
	state := nodearchive.StateFile{
		Version: 1,
		Nodes:   map[string]nodearchive.NodeRecord{"node-1": {StableID: "node-1", Active: true}},
		AvailabilityHistory: map[string][]nodearchive.AvailabilitySample{
			"node-1": {
				{CheckedAt: now.Add(-time.Hour), Online: true, LatencyMs: 25},
				{CheckedAt: now.Add(-48 * time.Hour), Online: false, FailureCode: checker.FailureCodeProxyTimeout},
				{CheckedAt: now.Add(-10 * 24 * time.Hour), Online: true, LatencyMs: 40},
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := nodearchive.NewStore(path, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("load availability history: %v", err)
	}

	rec := httptest.NewRecorder()
	requestPath := fmt.Sprintf(
		"/api/v1/admin/availability/history?stableId=node-1&from=%s&to=%s",
		now.Add(-3*24*time.Hour).Format(time.RFC3339),
		now.Format(time.RFC3339),
	)
	AdminAvailabilityHistoryHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool                             `json:"success"`
		Data    []nodearchive.AvailabilitySample `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Success || len(body.Data) != 2 || body.Data[0].LatencyMs != 25 || body.Data[1].Online {
		t.Fatalf("unexpected filtered history: %+v", body)
	}
}

func TestAdminScheduleHandlerAppliesRetentionToAvailabilityHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "node_registry.json")
	state := nodearchive.StateFile{
		Version: 1,
		Nodes:   map[string]nodearchive.NodeRecord{"node-1": {StableID: "node-1", Active: true}},
		AvailabilityHistory: map[string][]nodearchive.AvailabilitySample{
			"node-1": {
				{CheckedAt: now.Add(-20 * 24 * time.Hour), Online: true, LatencyMs: 20},
				{CheckedAt: now.Add(-40 * 24 * time.Hour), Online: true, LatencyMs: 40},
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := nodearchive.NewStore(path, nil)
	if err := store.SetAvailabilityHistoryRetentionDays(90); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}

	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := speedtest.NewManager(proxyChecker, 10000, "", speedtest.TestConfig{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/schedules",
		strings.NewReader(`{"enabled":false,"intervalSec":7200,"historyRetentionDays":30,"config":{}}`),
	)
	AdminScheduleHandler(manager, store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := manager.Schedule().HistoryRetentionDays; got != 30 {
		t.Fatalf("speed-test retention = %d days, want 30", got)
	}
	if got := store.AvailabilityHistory("node-1", time.Time{}, time.Time{}); len(got) != 1 || got[0].LatencyMs != 20 {
		t.Fatalf("availability history after schedule update = %+v, want one retained sample", got)
	}
}

func TestAdminIncidentsHandlerReturnsPersistedJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node_registry.json")
	state := nodearchive.StateFile{
		Version: 1,
		Nodes:   map[string]nodearchive.NodeRecord{},
		Incidents: []nodearchive.IncidentRecord{{
			ID: "incident-1", Kind: "node", Status: "active", Scope: "node:one",
			StableIDs: []string{"one"}, AffectedCount: 1, TotalCount: 1,
			CauseCode: checker.FailureCodeTCPRefused, CauseSummary: checker.FailureSummary(checker.FailureCodeTCPRefused),
			StartedAt: time.Now(),
		}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := nodearchive.NewStore(path, nil)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/incidents?limit=10", nil)
	AdminIncidentsHandler(store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "incident-1") {
		t.Fatalf("incidents response %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminSubscriptionRefreshHandler(t *testing.T) {
	handler := AdminSubscriptionRefreshHandler(func(request AdminSubscriptionRefreshRequest) (AdminSubscriptionRefreshResult, error) {
		if !request.Force || request.ConfirmationToken != "candidate-token" {
			t.Fatalf("refresh request was not decoded: %+v", request)
		}
		return AdminSubscriptionRefreshResult{
			Updated: true,
			Count:   2,
			Message: "Configuration updated",
		}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscription/refresh", strings.NewReader(`{"force":true,"confirmationToken":"candidate-token"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var body struct {
		Success bool                           `json:"success"`
		Data    AdminSubscriptionRefreshResult `json:"data"`
		Error   string                         `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Success || !body.Data.Updated || body.Data.Count != 2 || body.Error != "" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestAdminSubscriptionRefreshHandlerRejectsInvalidMethod(t *testing.T) {
	handler := AdminSubscriptionRefreshHandler(func(AdminSubscriptionRefreshRequest) (AdminSubscriptionRefreshResult, error) {
		return AdminSubscriptionRefreshResult{}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/subscription/refresh", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
}

func TestAdminSubscriptionRefreshHandlerReportsRunningRefresh(t *testing.T) {
	handler := AdminSubscriptionRefreshHandler(func(AdminSubscriptionRefreshRequest) (AdminSubscriptionRefreshResult, error) {
		return AdminSubscriptionRefreshResult{}, fmt.Errorf("subscription refresh already running")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscription/refresh", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestAdminBackupHandlerDownloadsArchive(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "node_registry.json"), []byte(`{"version":1,"nodes":{}}`), 0600); err != nil {
		t.Fatalf("write backup data: %v", err)
	}
	handler := AdminBackupHandler(backup.NewCreator(dataDir, "test-version"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/backup", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("unexpected content type: %s", got)
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.Contains(disposition, "xray-checker-backup-") {
		t.Fatalf("unexpected content disposition: %s", disposition)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("unexpected cache control: %s", got)
	}

	archiveData := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if err != nil {
		t.Fatalf("open downloaded archive: %v", err)
	}
	entries := make(map[string]bool, len(zr.File))
	for _, file := range zr.File {
		entries[file.Name] = true
	}
	if !entries["data/node_registry.json"] || !entries["manifest.json"] {
		t.Fatalf("unexpected archive entries: %+v", entries)
	}
}

func TestAdminBackupHandlerRejectsInvalidMethod(t *testing.T) {
	handler := AdminBackupHandler(backup.NewCreator(t.TempDir(), "test-version"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
}

func TestAdminBackupRestoreHandlerStagesArchive(t *testing.T) {
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "node_registry.json"), []byte(`{"version":1,"nodes":{"restored":{}}}`), 0600); err != nil {
		t.Fatalf("write source backup data: %v", err)
	}
	var archive bytes.Buffer
	if _, err := backup.NewCreator(sourceDir, "test-version").Create(&archive); err != nil {
		t.Fatalf("create backup archive: %v", err)
	}

	var requestBody bytes.Buffer
	multipartWriter := multipart.NewWriter(&requestBody)
	part, err := multipartWriter.CreateFormFile("backup", "backup.zip")
	if err != nil {
		t.Fatalf("create upload part: %v", err)
	}
	if _, err := part.Write(archive.Bytes()); err != nil {
		t.Fatalf("write upload part: %v", err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatalf("close multipart upload: %v", err)
	}

	targetDir := t.TempDir()
	handler := AdminBackupRestoreHandler(backup.NewRestorer(targetDir))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup/restore", &requestBody)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("X-Xray-Checker-Action", "restore-backup")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool                 `json:"success"`
		Data    backup.RestoreResult `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode restore response: %v", err)
	}
	if !body.Success || !body.Data.RestartRequired || len(body.Data.Files) != 1 {
		t.Fatalf("unexpected restore response: %+v", body)
	}
	if _, err := os.Stat(filepath.Join(targetDir, ".restore-pending", "node_registry.json")); err != nil {
		t.Fatalf("restore archive was not staged: %v", err)
	}
}

func TestAdminBackupRestoreHandlerRequiresArchive(t *testing.T) {
	handler := AdminBackupRestoreHandler(backup.NewRestorer(t.TempDir()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup/restore", nil)
	req.Header.Set("X-Xray-Checker-Action", "restore-backup")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestAdminBackupRestoreHandlerRequiresConfirmationHeader(t *testing.T) {
	handler := AdminBackupRestoreHandler(backup.NewRestorer(t.TempDir()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup/restore", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, rec.Code)
	}
}

func TestAdminBackupRestoreHandlerRunsTransactionGuardBeforeUpload(t *testing.T) {
	handler := AdminBackupRestoreHandler(backup.NewRestorer(t.TempDir()), func() (func(), error) {
		return nil, fmt.Errorf("backup restore cannot be staged while a node merge is pending")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/backup/restore", nil)
	req.Header.Set("X-Xray-Checker-Action", "restore-backup")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "node merge is pending") {
		t.Fatalf("expected guarded conflict, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminReadHandlersRejectMutatingMethods(t *testing.T) {
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := speedtest.NewManager(proxyChecker, 10000, "", speedtest.TestConfig{})
	tests := []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{name: "proxies", handler: AdminProxiesHandler(proxyChecker, 10000), path: "/api/v1/admin/proxies"},
		{name: "speed snapshot", handler: AdminSpeedTestSnapshotHandler(manager), path: "/api/v1/admin/speed-tests"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			tt.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
			}
		})
	}
}

func TestAdminProxyCheckHandlerChecksSelectedNodes(t *testing.T) {
	proxy := &models.ProxyConfig{
		StableID: "node-1",
		Name:     "Node one",
		Protocol: "vless",
		Server:   "node.example.com",
		Port:     443,
		UUID:     "uuid",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	if !proxyChecker.RestoreOfflineStatus(proxy.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{}, checker.PingCheckDetails{}) {
		t.Fatal("failed to seed offline status")
	}

	var checked []string
	handler := AdminProxyCheckHandler(func(stableIDs []string) error {
		checked = append([]string(nil), stableIDs...)
		return nil
	}, proxyChecker, 10000)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies/check", strings.NewReader(`{"stableIds":["node-1"]}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if len(checked) != 1 || checked[0] != proxy.StableID {
		t.Fatalf("checked IDs = %v, want [%s]", checked, proxy.StableID)
	}
	if !strings.Contains(rec.Body.String(), `"stableId":"node-1"`) {
		t.Fatalf("response does not contain updated proxy: %s", rec.Body.String())
	}
}

func TestAdminSpeedTestRunChecksAvailabilityBeforeFiltering(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node one", Protocol: "vless", Server: "node.example.com", Port: 443, UUID: "uuid"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	if !proxyChecker.RestoreProxyFailureStatus(proxy.StableID, time.Now().Add(-time.Minute), checker.HostCheckDetails{Checked: true, Online: true}, checker.PingCheckDetails{}) {
		t.Fatal("failed to seed proxy-failure status")
	}
	manager := speedtest.NewManager(proxyChecker, 10000, "", speedtest.TestConfig{})
	called := false
	handler := AdminSpeedTestRunHandler(manager, func(stableIDs []string) error {
		called = true
		if len(stableIDs) != 1 || stableIDs[0] != proxy.StableID {
			t.Fatalf("availability IDs = %v", stableIDs)
		}
		return nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/speed-tests/run", strings.NewReader(`{"proxyIds":["node-1"],"config":{}}`))
	handler.ServeHTTP(rec, req)
	if !called {
		t.Fatal("manual speed-test did not run availability check first")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no proxies selected") {
		t.Fatalf("expected unhealthy node to be filtered, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminProxyCheckHandlerRequiresSelection(t *testing.T) {
	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	handler := AdminProxyCheckHandler(func([]string) error { return nil }, proxyChecker, 10000)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies/check", strings.NewReader(`{"stableIds":[]}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestAdminNodeMaintenanceHandlerUpdatesSelectedStableID(t *testing.T) {
	var gotStableID string
	var gotEnabled bool
	handler := AdminNodeMaintenanceHandler(func(stableID string, enabled bool) (nodearchive.NodeRecord, error) {
		gotStableID = stableID
		gotEnabled = enabled
		return nodearchive.NodeRecord{StableID: stableID, Active: true, Maintenance: enabled}, nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/nodes-overview/maintenance", strings.NewReader(`{"stableId":"node-1","enabled":true}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if gotStableID != "node-1" || !gotEnabled {
		t.Fatalf("maintenance update = %q/%v", gotStableID, gotEnabled)
	}
	if !strings.Contains(rec.Body.String(), `"maintenance":true`) {
		t.Fatalf("response does not include maintenance state: %s", rec.Body.String())
	}
}

func TestConfigStatusHandlerReturnsOKDuringMaintenance(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node one"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	if err := proxyChecker.SetMaintenanceMode(proxy.StableID, true); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/config/node-1", nil)
	ConfigStatusHandler(proxyChecker).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "Maintenance" {
		t.Fatalf("maintenance config status = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Xray-Checker-Status") != "maintenance" {
		t.Fatalf("maintenance response header = %q", rec.Header().Get("X-Xray-Checker-Status"))
	}
}

func TestConfigStatusHandlerReturnsProxyFailureWithoutOffline(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "Node one"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	if !proxyChecker.RestoreProxyFailureStatus(
		proxy.StableID,
		time.Now().Add(-time.Minute),
		checker.HostCheckDetails{Checked: true, Online: true},
		checker.PingCheckDetails{Checked: true, Online: true},
	) {
		t.Fatal("failed to seed proxy-failure status")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/config/node-1", nil)
	ConfigStatusHandler(proxyChecker).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "Proxy failure" {
		t.Fatalf("proxy-failure response = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Xray-Checker-Status"); got != string(checker.AvailabilityStateProxyFailure) {
		t.Fatalf("proxy-failure response header = %q", got)
	}
}

func TestAdminRetiredNodeDeleteRollsBackArchiveWhenSpeedHistorySaveFails(t *testing.T) {
	root := t.TempDir()
	speedDir := filepath.Join(root, "speed")
	if err := os.Mkdir(speedDir, 0700); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	result := speedtest.Result{StableID: "retired", Name: "Retired", Mbps: 50, CheckedAt: checkedAt}
	state := map[string]any{
		"version":   1,
		"updatedAt": checkedAt,
		"lastRun":   map[string]any{},
		"results":   map[string]speedtest.Result{"retired": result},
		"history":   map[string][]speedtest.Result{"retired": {result}},
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(speedDir, "speedtest_results.json"), stateData, 0600); err != nil {
		t.Fatal(err)
	}

	proxyChecker := checker.NewProxyChecker(nil, 10000, "", 1, "", "", 1, 0, "status")
	manager := speedtest.NewManager(proxyChecker, 10000, filepath.Join(speedDir, "speedtest_schedule.json"), speedtest.TestConfig{})
	if err := manager.Load(); err != nil {
		t.Fatalf("load speed history: %v", err)
	}
	store := nodearchive.NewStore(filepath.Join(root, "node_registry.json"), proxyChecker)
	if err := store.SyncSpeedHistory(manager.AllResultHistory()); err != nil {
		t.Fatalf("seed retired node: %v", err)
	}

	movedSpeedDir := filepath.Join(root, "speed-loaded")
	if err := os.Rename(speedDir, movedSpeedDir); err != nil {
		t.Fatalf("move speed state directory: %v", err)
	}
	if err := os.WriteFile(speedDir, []byte("block persistence"), 0600); err != nil {
		t.Fatalf("create persistence blocker: %v", err)
	}

	handler := AdminNodesOverviewDeleteHandler(store, manager)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/nodes-overview/delete", strings.NewReader(`{"stableId":"retired"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}
	if _, err := store.ArchivedRecord("retired"); err != nil {
		t.Fatalf("retired archive record was not rolled back: %v", err)
	}
	if history := manager.ResultHistory("retired"); len(history) != 1 {
		t.Fatalf("speed history was not rolled back: %+v", history)
	}
}
