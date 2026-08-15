package speedtest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

func TestCountryTestURLExampleIsCompleteAndValid(t *testing.T) {
	catalog, err := readCountryTestURLCatalog(filepath.Join("..", "country-test-urls.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"DE", "NL", "EE", "FI", "US"} {
		if len(catalog.Countries[code]) < 2 {
			t.Fatalf("country %s has %d endpoints, want at least 2", code, len(catalog.Countries[code]))
		}
	}
	if got := catalogEndpointCount(catalog); got < 25 {
		t.Fatalf("example endpoint count = %d, want at least 25", got)
	}
}

func TestCountryTestURLCatalogRejectsUnknownFields(t *testing.T) {
	_, err := parseCountryTestURLCatalog([]byte(`
version: 1
countries:
  DE:
    - id: test
      url: https://example.com/test.bin
      priority: 10
      typo: true
`))
	if err == nil {
		t.Fatal("catalog with unknown field was accepted")
	}
}

func TestFallbackTriesNextHealthyEndpointAndSuppressesTelegramAlert(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve-one
      provider: First
      city: Frankfurt
      url: https://first.example.test/100mb.bin
      priority: 10
    - id: reserve-two
      provider: Second
      city: Berlin
      url: https://second.example.test/100mb.bin
      priority: 20
`)
	manager.SetCountryResolver(func(stableID string) string {
		if stableID == proxy.StableID {
			return "DE"
		}
		return ""
	})

	type attempt struct {
		url      string
		maxBytes int64
	}
	var attempts []attempt
	manager.testAttempt = func(_ *models.ProxyConfig, cfg TestConfig, source string) Result {
		attempts = append(attempts, attempt{url: cfg.URL, maxBytes: cfg.MaxBytes})
		result := Result{StableID: proxy.StableID, Name: proxy.Name, URL: cfg.URL, Source: source, CheckedAt: time.Now()}
		switch cfg.URL {
		case "https://primary.example.test/100mb.bin":
			result.Error = "HTTP status 503"
		case "https://first.example.test/100mb.bin":
			result.Error = "connection refused"
		case "https://second.example.test/100mb.bin":
			result.DownloadedBytes = cfg.MaxBytes
			result.Mbps = 50
		}
		return result
	}

	result := manager.testProxyWithFallback(proxy, TestConfig{
		URL:        "https://primary.example.test/100mb.bin",
		MaxBytes:   1024 * 1024,
		TimeoutSec: 30,
	}, "schedule")

	if !result.FallbackUsed || result.FallbackID != "reserve-two" {
		t.Fatalf("fallback result = %+v, want reserve-two", result)
	}
	if result.URL != "https://second.example.test/100mb.bin" {
		t.Fatalf("result URL = %q", result.URL)
	}
	if result.PrimaryURL != "https://primary.example.test/100mb.bin" || result.PrimaryError != "HTTP status 503" {
		t.Fatalf("primary diagnostics = %q / %q", result.PrimaryURL, result.PrimaryError)
	}
	if !result.TelegramAlertSuppressed {
		t.Fatal("successful fallback did not suppress Telegram alert")
	}
	if len(attempts) != 4 {
		t.Fatalf("attempts = %+v, want primary, failed probe, successful probe and full fallback", attempts)
	}
	if attempts[1].maxBytes != fallbackProbeMaxBytes || attempts[2].maxBytes != fallbackProbeMaxBytes {
		t.Fatalf("fallback probes did not use %d bytes: %+v", fallbackProbeMaxBytes, attempts)
	}
	if attempts[3].maxBytes != 1024*1024 {
		t.Fatalf("full fallback maxBytes = %d, want 1 MiB", attempts[3].maxBytes)
	}
}

func TestLowSpeedDoesNotSwitchToFallback(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve
      url: https://reserve.example.test/100mb.bin
      priority: 10
`)
	manager.SetCountryResolver(func(string) string { return "DE" })
	attempts := 0
	manager.testAttempt = func(_ *models.ProxyConfig, cfg TestConfig, source string) Result {
		attempts++
		return Result{StableID: proxy.StableID, URL: cfg.URL, DownloadedBytes: cfg.MaxBytes, Mbps: 1, Source: source}
	}

	result := manager.testProxyWithFallback(proxy, TestConfig{
		URL:        "https://primary.example.test/100mb.bin",
		MaxBytes:   1024,
		TimeoutSec: 30,
	}, "schedule")
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want primary only", attempts)
	}
	if result.FallbackUsed {
		t.Fatal("low speed switched to fallback")
	}
}

func TestFallbackHealthPersistsLastSuccessfulEndpoint(t *testing.T) {
	dir := t.TempDir()
	healthPath := filepath.Join(dir, "speedtest_url_health.json")
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.ConfigureCountryFallbacks("", healthPath)
	manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve-one
      url: https://first.example.test/100mb.bin
      priority: 10
    - id: reserve-two
      url: https://second.example.test/100mb.bin
      priority: 20
`)
	manager.recordFallbackSuccess(proxy.StableID, "reserve-two", time.Now())
	if err := manager.persistFallbackHealth(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(healthPath); err != nil {
		t.Fatal(err)
	}

	reloaded := newFallbackTestManager(proxy)
	reloaded.ConfigureCountryFallbacks("", healthPath)
	reloaded.fallbackCatalog = manager.fallbackCatalog
	if err := reloaded.LoadCountryFallbacks(); err != nil {
		t.Fatal(err)
	}
	candidates := reloaded.fallbackCandidates(proxy.StableID, "DE", "https://primary.example.test/100mb.bin", time.Now())
	if len(candidates) != 2 || candidates[0].ID != "reserve-two" {
		t.Fatalf("reloaded candidates = %+v, want reserve-two first", candidates)
	}
}

func TestFallbackFailureStartsPerNodeCooldown(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve-one
      url: https://first.example.test/100mb.bin
      priority: 10
    - id: reserve-two
      url: https://second.example.test/100mb.bin
      priority: 20
`)
	now := time.Now()
	manager.recordFallbackFailure(proxy.StableID, "reserve-one", now)
	candidates := manager.fallbackCandidates(proxy.StableID, "DE", "https://primary.example.test/100mb.bin", now.Add(time.Minute))
	if len(candidates) != 1 || candidates[0].ID != "reserve-two" {
		t.Fatalf("candidates during cooldown = %+v, want reserve-two only", candidates)
	}
}

func newFallbackTestManager(proxy *models.ProxyConfig) *Manager {
	proxyChecker := checker.NewProxyChecker([]*models.ProxyConfig{proxy}, 10000, "", 1, "", "", 1, 0, "status")
	manager := NewManager(proxyChecker, 10000, "", TestConfig{})
	manager.ensureFallbackHealthLocked()
	return manager
}

func mustParseFallbackCatalog(t *testing.T, value string) countryTestURLCatalog {
	t.Helper()
	catalog, err := parseCountryTestURLCatalog([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
