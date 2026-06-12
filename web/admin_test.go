package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
