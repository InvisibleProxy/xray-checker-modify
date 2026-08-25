package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xray-checker/remnawave"
)

type fakeAdminRemnawaveService struct {
	snapshot remnawave.Snapshot
	updated  remnawave.Settings
	syncs    int
}

func (f *fakeAdminRemnawaveService) Snapshot() remnawave.Snapshot {
	return f.snapshot
}

func (f *fakeAdminRemnawaveService) UpdateSettings(settings remnawave.Settings) (remnawave.Snapshot, error) {
	f.updated = settings
	f.snapshot.Settings = settings
	return f.snapshot, nil
}

func (f *fakeAdminRemnawaveService) SyncNow(context.Context) (remnawave.Snapshot, error) {
	f.syncs++
	return f.snapshot, nil
}

func TestAdminRemnawaveHandlerGetsAndUpdatesSanitizedSettings(t *testing.T) {
	service := &fakeAdminRemnawaveService{snapshot: remnawave.Snapshot{
		Connection: remnawave.ConnectionInfo{Enabled: true, Configured: true, APITokenConfigured: true},
	}}
	handler := AdminRemnawaveHandler(service)

	get := httptest.NewRequest(http.MethodGet, "/api/v1/admin/remnawave", nil)
	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusOK || strings.Contains(getRecorder.Body.String(), "api-token-secret") {
		t.Fatalf("GET status/body = %d %s", getRecorder.Code, getRecorder.Body.String())
	}

	putBody := `{"policy":{"enabled":true,"outageMinutes":15,"minimumFailures":3,"recoveryMinutes":5,"messages":{"singleLocation":{"enabled":true,"template":"Недоступна: {location}"},"multipleLocations":{"enabled":true,"template":"Недоступны: {locations}"},"allLocations":{"enabled":true,"template":"Все недоступны"},"partialSingleLocation":{"enabled":true,"template":"Часть недоступна: {location}"},"partialMultipleLocations":{"enabled":true,"template":"Частично недоступны: {locations}"},"healthy":{"enabled":false,"template":"Всё стабильно"},"partialFallback":"Недоступно: {unavailable}/{total}","partialAvailabilityFallback":"Частично недоступно: {affected}/{total}"}},"squadPairs":[{"internalSquadUuid":"internal-1","externalSquadUuid":"external-1"}],"nodeMappings":{"stable-1":{"hostUuid":"host-1","groupKey":"de","publicLabel":"Германия"}}}`
	put := httptest.NewRequest(http.MethodPut, "/api/v1/admin/remnawave", strings.NewReader(putBody))
	putRecorder := httptest.NewRecorder()
	handler.ServeHTTP(putRecorder, put)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status/body = %d %s", putRecorder.Code, putRecorder.Body.String())
	}
	if !service.updated.Policy.Enabled || service.updated.NodeMappings["stable-1"].HostUUID != "host-1" ||
		service.updated.Policy.Messages.SingleLocation.Template != "Недоступна: {location}" ||
		service.updated.Policy.Messages.PartialSingleLocation.Template != "Часть недоступна: {location}" {
		t.Fatalf("updated settings = %+v", service.updated)
	}
	var envelope struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(putRecorder.Body.Bytes(), &envelope); err != nil || !envelope.Success {
		t.Fatalf("invalid PUT response: %v %s", err, putRecorder.Body.String())
	}
}

func TestAdminRemnawaveHandlersRejectWrongMethodsAndTrailingJSON(t *testing.T) {
	service := &fakeAdminRemnawaveService{}
	settingsHandler := AdminRemnawaveHandler(service)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/remnawave", strings.NewReader(`{} {}`))
	recorder := httptest.NewRecorder()
	settingsHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d", recorder.Code)
	}

	syncHandler := AdminRemnawaveSyncHandler(service)
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/remnawave/sync", nil)
	recorder = httptest.NewRecorder()
	syncHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("sync GET status = %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/remnawave/sync", nil)
	recorder = httptest.NewRecorder()
	syncHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.syncs != 1 {
		t.Fatalf("sync POST status/calls = %d/%d", recorder.Code, service.syncs)
	}
}
