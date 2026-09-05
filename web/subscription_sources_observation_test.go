package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"xray-checker/checker"
	"xray-checker/models"
	"xray-checker/observation"
	"xray-checker/subsource"
)

func decodeSourcesResponse(t *testing.T, body []byte) AdminSubscriptionSourcesResponse {
	t.Helper()
	var envelope struct {
		Data AdminSubscriptionSourcesResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return envelope.Data
}

// The API never returns a real subscription URL, so a client that saves a row
// back — toggling a source re-sends the whole thing — would otherwise store the
// mask as the URL and leave a source that can never be fetched again.
func TestSavingASourceWithAMaskedURLKeepsTheStoredOne(t *testing.T) {
	store := subsource.NewStore(filepath.Join(t.TempDir(), "subscription_sources.json"))
	handler := AdminSubscriptionSourcesHandler(store, nil, nil)

	rec := httptest.NewRecorder()
	body := `{"url":"https://panel.example/sub/super-secret-token","name":"Third-party panel","profile":"happ","enabled":true}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscription/sources", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("add status = %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeSourcesResponse(t, rec.Body.Bytes())
	if len(created.Sources) != 1 {
		t.Fatalf("sources = %+v, want one", created.Sources)
	}
	masked := created.Sources[0].URL
	if masked == "" || !strings.Contains(masked, "…") {
		t.Fatalf("URL %q was not masked", masked)
	}

	// Exactly what the panel holds for that row: the mask it was shown.
	rec = httptest.NewRecorder()
	toggle := `{"id":"` + created.Sources[0].ID + `","url":"` + masked + `","profile":"happ","enabled":false}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/admin/subscription/sources", strings.NewReader(toggle)))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d: %s", rec.Code, rec.Body.String())
	}

	stored := store.List()
	if len(stored) != 1 {
		t.Fatalf("stored = %+v, want one", stored)
	}
	if stored[0].URL != "https://panel.example/sub/super-secret-token" {
		t.Fatalf("stored URL = %q, want the original one kept", stored[0].URL)
	}
	if stored[0].Enabled {
		t.Fatal("the toggle did not apply")
	}
}

// A mode change does not alter what is fetched, so it must reach the checker at
// once instead of waiting for the next subscription refresh.
func TestSavingASourceAppliesItsObservationPolicyImmediately(t *testing.T) {
	store := subsource.NewStore(filepath.Join(t.TempDir(), "subscription_sources.json"))
	applied := 0
	handler := AdminSubscriptionSourcesHandler(store, nil, func() { applied++ })

	rec := httptest.NewRecorder()
	body := `{"url":"https://panel.example/sub/token","profile":"happ","enabled":true,"mode":"availability","unlisted":true}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscription/sources", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if applied != 1 {
		t.Fatalf("policy was applied %d times, want once", applied)
	}

	response := decodeSourcesResponse(t, rec.Body.Bytes())
	if len(response.Sources) != 1 {
		t.Fatalf("sources = %+v", response.Sources)
	}
	source := response.Sources[0]
	if source.Mode != string(observation.ModeAvailability) || !source.Unlisted {
		t.Fatalf("source = %+v, want the observation settings echoed back", source)
	}
	if len(response.Modes) == 0 {
		t.Fatal("the panel was given no modes to choose from")
	}
	for _, mode := range response.Modes {
		if mode.ID == "" || mode.Label == "" {
			t.Fatalf("mode %+v is missing its identity or label", mode)
		}
	}
}

// An unlisted source is watched for the operator, not published as their own
// service: it belongs neither on the public list nor behind a /config endpoint
// an external uptime check could bind to.
func TestUnlistedSourceStaysOffThePublicSurfaces(t *testing.T) {
	own := &models.ProxyConfig{StableID: "own", Name: "Own", Protocol: "vless", Server: "own.example", Port: 443, UUID: "a"}
	foreign := &models.ProxyConfig{StableID: "foreign", Name: "Foreign", Protocol: "vless", Server: "foreign.example", Port: 443, UUID: "b", SourceID: "src-foreign"}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{own, foreign}, 10000, "", 1, "", "", 1, 0, "status")
	proxyChecker.SetSourcePolicies(map[string]observation.Policy{
		"src-foreign": observation.PolicyFor(observation.ModeFull, true),
	})

	rec := httptest.NewRecorder()
	APIPublicProxiesHandler(proxyChecker).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/public/proxies", nil))
	if body := rec.Body.String(); strings.Contains(body, "Foreign") {
		t.Fatalf("public list exposed an unlisted source: %s", body)
	} else if !strings.Contains(body, "Own") {
		t.Fatalf("public list lost the deployment's own node: %s", body)
	}

	rec = httptest.NewRecorder()
	ConfigStatusHandler(proxyChecker).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config/foreign", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/config for an unlisted node = %d, want 404", rec.Code)
	}

	RegisterConfigEndpoints([]*models.ProxyConfig{own, foreign}, proxyChecker, 10000)
	endpointsMu.RLock()
	registered := append([]EndpointInfo(nil), registeredEndpoints...)
	endpointsMu.RUnlock()
	if len(registered) != 1 || registered[0].StableID != "own" {
		t.Fatalf("status page endpoints = %+v, want only the deployment's own node", registered)
	}
}

// The panel gets both names: the one it shows and the one the subscription
// sends, so a renamed node still says where it came from.
func TestAdminProxyInfoCarriesBothNames(t *testing.T) {
	proxy := &models.ProxyConfig{
		StableID: "node-1", Name: "proxy-01-nl-edge-host-01", GroupName: "Auto-select",
		Protocol: "vless", Server: "node.example", Port: 443, UUID: "a",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")

	decode := func(t *testing.T) AdminProxyInfo {
		t.Helper()
		rec := httptest.NewRecorder()
		AdminProxiesHandler(proxyChecker, 10000).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies", nil))
		var envelope struct {
			Data []AdminProxyInfo `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(envelope.Data) != 1 {
			t.Fatalf("proxies = %+v, want one", envelope.Data)
		}
		return envelope.Data[0]
	}

	unnamed := decode(t)
	if unnamed.Name != proxy.Name || unnamed.DisplayName != "" || unnamed.SourceName != "" {
		t.Fatalf("unnamed node = %+v, want only the subscription name", unnamed)
	}
	if unnamed.GroupName != "Auto-select" {
		t.Fatalf("group name = %q, want the config remarks surfaced", unnamed.GroupName)
	}

	proxyChecker.ApplyDisplayNames(map[string]string{proxy.StableID: "Нидерланды · узел 1"})
	named := decode(t)
	if named.Name != "Нидерланды · узел 1" {
		t.Fatalf("name = %q, want the operator's label", named.Name)
	}
	if named.DisplayName != "Нидерланды · узел 1" || named.SourceName != proxy.Name {
		t.Fatalf("renamed node = %+v, want both names returned", named)
	}
}

// The panel holds no subscription URL to compare against, so the API is the
// only thing that can tell it which nodes came from the deployment's own feed
// and which were added to enrich the picture. Its filters default on that.
func TestAdminProxiesMarkTheEnvironmentsOwnSubscription(t *testing.T) {
	own := &models.ProxyConfig{
		StableID: "own", Name: "Own", SubName: "Home panel",
		Protocol: "vless", Server: "own.example", Port: 443, UUID: "a",
	}
	added := &models.ProxyConfig{
		StableID: "added", Name: "Added", SubName: "Third-party panel", SourceID: "src-1",
		Protocol: "vless", Server: "added.example", Port: 443, UUID: "b",
	}
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{own, added}, 10000, "", 1, "", "", 1, 0, "status")

	rec := httptest.NewRecorder()
	AdminProxiesHandler(proxyChecker, 10000).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies", nil))
	var envelope struct {
		Data []AdminProxyInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(envelope.Data) != 2 {
		t.Fatalf("proxies = %+v, want two", envelope.Data)
	}
	sourced := make(map[string]bool, len(envelope.Data))
	for _, info := range envelope.Data {
		sourced[info.StableID] = info.EnvSource
	}
	if !sourced["own"] {
		t.Fatal("a node from the environment subscription was not marked as the deployment's own")
	}
	if sourced["added"] {
		t.Fatal("a node from a panel-added source was marked as the environment's")
	}
}
