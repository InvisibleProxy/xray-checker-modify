package remnawave

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHTTPClientUsesMinimalEndpointsAndBearerToken(t *testing.T) {
	requests := make([]string, 0)
	var patched struct {
		UUID               string            `json:"uuid"`
		ResponseHeadersAdd map[string]string `json:"responseHeadersAdd"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/hosts":
			_, _ = w.Write([]byte(`{"response":[{"uuid":"host-1","remark":"DE","address":"de.example","port":443,"inbound":{"configProfileInboundUuid":"inbound-1"}}]}`))
		case "GET /api/internal-squads":
			_, _ = w.Write([]byte(`{"response":{"internalSquads":[{"uuid":"internal-1","name":"Users 1","inbounds":[{"uuid":"inbound-1","rawInbound":{"credentials":"must-not-be-modeled"}}]}]}}`))
		case "GET /api/external-squads":
			_, _ = w.Write([]byte(`{"response":{"externalSquads":[{"uuid":"external-1","name":"Plan 1","responseHeadersAdd":{"x-test":"keep"}}]}}`))
		case "PATCH /api/external-squads":
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Fatalf("decode PATCH: %v", err)
			}
			_, _ = w.Write([]byte(`{"response":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL+"/api", "test-token", time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	hosts, err := client.GetHosts(context.Background())
	if err != nil || len(hosts) != 1 || hosts[0].UUID != "host-1" {
		t.Fatalf("GetHosts = %+v, %v", hosts, err)
	}
	internal, err := client.GetInternalSquads(context.Background())
	if err != nil || len(internal) != 1 || len(internal[0].Inbounds) != 1 {
		t.Fatalf("GetInternalSquads = %+v, %v", internal, err)
	}
	external, err := client.GetExternalSquads(context.Background())
	if err != nil || len(external) != 1 || external[0].ResponseHeadersAdd["x-test"] != "keep" {
		t.Fatalf("GetExternalSquads = %+v, %v", external, err)
	}
	if err := client.UpdateExternalHeaders(context.Background(), "external-1", map[string]string{"x-test": "keep", "announce": "rwEncodeBase64:test"}); err != nil {
		t.Fatalf("UpdateExternalHeaders: %v", err)
	}
	if patched.UUID != "external-1" || patched.ResponseHeadersAdd["x-test"] != "keep" || patched.ResponseHeadersAdd["announce"] == "" {
		t.Fatalf("PATCH body = %+v", patched)
	}
	wantRequests := []string{"GET /api/hosts", "GET /api/internal-squads", "GET /api/external-squads", "PATCH /api/external-squads"}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests = %#v, want %#v", requests, wantRequests)
	}
}

func TestHTTPClientRejectsCredentialedURL(t *testing.T) {
	if _, err := NewHTTPClient("https://user:password@example.com", "token", time.Second); err == nil {
		t.Fatal("credentialed API URL was accepted")
	}
}

func TestHTTPClientRedactsTokenFromAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"rejected secret-token"}`))
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "secret-token", time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	_, err = client.GetHosts(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token was not redacted from error: %v", err)
	}
}
