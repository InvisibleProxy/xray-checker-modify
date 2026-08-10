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
	"xray-checker/speedtest"
)

func TestAdminSubscriptionRefreshHandler(t *testing.T) {
	handler := AdminSubscriptionRefreshHandler(func() (AdminSubscriptionRefreshResult, error) {
		return AdminSubscriptionRefreshResult{
			Updated: true,
			Count:   2,
			Message: "Configuration updated",
		}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscription/refresh", nil)
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
	handler := AdminSubscriptionRefreshHandler(func() (AdminSubscriptionRefreshResult, error) {
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
	handler := AdminSubscriptionRefreshHandler(func() (AdminSubscriptionRefreshResult, error) {
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
