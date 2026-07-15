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

	"xray-checker/backup"
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
