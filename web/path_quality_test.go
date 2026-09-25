package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xray-checker/paneltelemetry"
	"xray-checker/pathquality"
	"xray-checker/verdictlog"
)

type fakePathQuality struct {
	from, to time.Time
	location *time.Location
	kept     []string
}

func (f *fakePathQuality) Report(from, to time.Time, location *time.Location, keep func(pathquality.NodeInput) bool) pathquality.Report {
	f.from, f.to, f.location = from, to, location
	for _, node := range []pathquality.NodeInput{{StableID: "own", Subscription: "Main"}, {StableID: "other", Subscription: "Other"}} {
		if keep == nil || keep(node) {
			f.kept = append(f.kept, node.StableID)
		}
	}
	return pathquality.Report{PeakStart: 18, PeakEnd: 24}
}

func TestPathQualityHandlerReadsThePeriodZoneAndSubscription(t *testing.T) {
	service := &fakePathQuality{}
	handler := AdminPathQualityHandler(service)

	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/path-quality?days=7&tz=Europe/Moscow&subscription=Main", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}
	if got := service.to.Sub(service.from); got != 7*24*time.Hour {
		t.Fatalf("period = %s, want seven days", got)
	}
	if service.location == nil || service.location.String() != "Europe/Moscow" {
		t.Fatalf("location = %v, want Europe/Moscow", service.location)
	}
	if len(service.kept) != 1 || service.kept[0] != "own" {
		t.Fatalf("kept = %v, want the one subscription", service.kept)
	}

	for _, query := range []string{"?days=0", "?days=91", "?tz=Mars/Olympus"} {
		response = httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/path-quality"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, response.Code)
		}
	}
	response = httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodPost, "/api/v1/admin/path-quality", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405: the report changes nothing", response.Code)
	}
}

type fakeVerdictJournal struct {
	filter verdictlog.Filter
}

func (f *fakeVerdictJournal) Query(filter verdictlog.Filter) ([]verdictlog.Entry, error) {
	f.filter = filter
	return nil, nil
}

func TestDiagnosticVerdictsHandlerPassesTheFilterAndAnswersAnEmptyList(t *testing.T) {
	journal := &fakeVerdictJournal{}
	handler := AdminDiagnosticVerdictsHandler(journal)
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/diagnostic-verdicts?stableId=node&source=availability&limit=50&from=2026-09-01T00:00:00Z", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}
	if journal.filter.StableID != "node" || journal.filter.Source != verdictlog.SourceAvailability || journal.filter.Limit != 50 || journal.filter.From.IsZero() {
		t.Fatalf("filter = %+v", journal.filter)
	}
	var envelope struct {
		Data []verdictlog.Entry `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data == nil {
		t.Fatalf("body = %s, want an empty list rather than null", response.Body.String())
	}
	for _, query := range []string{"?source=elsewhere", "?limit=0", "?from=yesterday"} {
		response = httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/diagnostic-verdicts"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, response.Code)
		}
	}
}

func TestPanelTelemetryHandlerReportsANotConfiguredTelemetryAsDisabled(t *testing.T) {
	response := httptest.NewRecorder()
	AdminPanelTelemetryHandler(nil)(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/remnawave/telemetry", nil))
	var envelope struct {
		Data paneltelemetry.State `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.Enabled {
		t.Fatalf("body = %s, want a disabled state", response.Body.String())
	}
}
