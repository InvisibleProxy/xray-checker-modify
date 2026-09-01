package speedtest

import (
	"os"
	"path/filepath"
	"sync"
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
	if !result.FallbackAttempted || result.FallbackAttempts != 2 || result.FallbackExhausted {
		t.Fatalf("fallback outcome = attempted:%v attempts:%d exhausted:%v", result.FallbackAttempted, result.FallbackAttempts, result.FallbackExhausted)
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

func TestLowSpeedSwitchesToHealthyFallback(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.SetLowSpeedThresholdMbps(10)
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
		mbps := 1.0
		if cfg.URL == "https://reserve.example.test/100mb.bin" {
			mbps = 25
		}
		return Result{StableID: proxy.StableID, URL: cfg.URL, DownloadedBytes: cfg.MaxBytes, Mbps: mbps, Source: source}
	}

	result := manager.testProxyWithFallback(proxy, TestConfig{
		URL:        "https://primary.example.test/100mb.bin",
		MaxBytes:   1024,
		TimeoutSec: 30,
	}, "schedule")
	if attempts != 3 {
		t.Fatalf("attempt count = %d, want primary, probe and full fallback", attempts)
	}
	if !result.FallbackUsed || result.URL != "https://reserve.example.test/100mb.bin" || result.Mbps != 25 {
		t.Fatalf("fallback result = %+v", result)
	}
	if !result.FallbackAttempted || result.FallbackAttempts != 1 || result.FallbackExhausted || result.PrimaryMbps != 1 {
		t.Fatalf("fallback outcome = %+v", result)
	}
	if result.PrimaryURL != "https://primary.example.test/100mb.bin" || result.PrimaryError != "" {
		t.Fatalf("primary diagnostics = %q / %q", result.PrimaryURL, result.PrimaryError)
	}
	if !result.TelegramAlertSuppressed {
		t.Fatal("healthy fallback did not suppress the automated Telegram report")
	}
}

func TestLowSpeedFallbackRemainsEligibleForConfirmation(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.SetLowSpeedThresholdMbps(10)
	manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve
      url: https://reserve.example.test/100mb.bin
      priority: 10
`)
	manager.SetCountryResolver(func(string) string { return "DE" })
	manager.testAttempt = func(_ *models.ProxyConfig, cfg TestConfig, source string) Result {
		mbps := 2.0
		if cfg.URL == "https://reserve.example.test/100mb.bin" {
			mbps = 3
		}
		return Result{StableID: proxy.StableID, URL: cfg.URL, DownloadedBytes: cfg.MaxBytes, Mbps: mbps, Source: source}
	}

	result := manager.testProxyWithFallback(proxy, TestConfig{
		URL:        "https://primary.example.test/100mb.bin",
		MaxBytes:   1024,
		TimeoutSec: 30,
	}, "schedule")
	if !result.FallbackUsed || result.Mbps != 3 {
		t.Fatalf("fallback result = %+v", result)
	}
	if !result.FallbackAttempted || result.FallbackAttempts != 1 || result.FallbackExhausted || result.PrimaryMbps != 2 {
		t.Fatalf("fallback outcome = %+v", result)
	}
	if result.TelegramAlertSuppressed {
		t.Fatal("low-speed fallback was suppressed from delayed confirmation")
	}
}

func TestFailedFallbackKeepsPrimaryLowSpeedForConfirmation(t *testing.T) {
	proxy := &models.ProxyConfig{StableID: "node-1", Name: "DE Node", Protocol: "vless"}
	manager := newFallbackTestManager(proxy)
	manager.SetLowSpeedThresholdMbps(10)
	manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve
      url: https://reserve.example.test/100mb.bin
      priority: 10
`)
	manager.SetCountryResolver(func(string) string { return "DE" })
	manager.testAttempt = func(_ *models.ProxyConfig, cfg TestConfig, source string) Result {
		result := Result{StableID: proxy.StableID, URL: cfg.URL, Source: source}
		if cfg.URL == "https://reserve.example.test/100mb.bin" {
			result.Error = "connection refused"
			return result
		}
		result.DownloadedBytes = cfg.MaxBytes
		result.Mbps = 2
		return result
	}

	result := manager.testProxyWithFallback(proxy, TestConfig{
		URL:        "https://primary.example.test/100mb.bin",
		MaxBytes:   1024,
		TimeoutSec: 30,
	}, "schedule")
	if result.FallbackUsed || result.Error != "" || result.Mbps != 2 {
		t.Fatalf("result = %+v, want original low-speed result", result)
	}
	if !result.FallbackAttempted || result.FallbackAttempts != 1 || !result.FallbackExhausted || result.PrimaryMbps != 2 {
		t.Fatalf("exhausted fallback outcome = %+v", result)
	}
}

func TestFailedFallbackKeepsPrimaryTechnicalErrorAndMarksExhaustion(t *testing.T) {
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
	manager.testAttempt = func(_ *models.ProxyConfig, cfg TestConfig, source string) Result {
		return Result{StableID: proxy.StableID, URL: cfg.URL, Source: source, Error: "connection refused"}
	}

	result := manager.testProxyWithFallback(proxy, TestConfig{
		URL: "https://primary.example.test/100mb.bin", MaxBytes: 1024, TimeoutSec: 30,
	}, "schedule")
	if result.Error != "connection refused" || !result.FallbackAttempted || result.FallbackAttempts != 1 || !result.FallbackExhausted {
		t.Fatalf("technical fallback result = %+v", result)
	}
}

func TestDisabledLowSpeedThresholdDoesNotSwitchToFallback(t *testing.T) {
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
	if attempts != 1 || result.FallbackUsed {
		t.Fatalf("attempts = %d, result = %+v", attempts, result)
	}
}

func TestSequencedFallbackWaitsForAllPrimaryTests(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		primaryResult Result
	}{
		{
			name:          "low speed",
			source:        ManualSource,
			primaryResult: Result{DownloadedBytes: 1024, Mbps: 2},
		},
		{
			name:          "context deadline exceeded",
			source:        ManualSource,
			primaryResult: Result{Error: "context deadline exceeded"},
		},
		{
			name:          "telegram low speed",
			source:        TelegramSource,
			primaryResult: Result{DownloadedBytes: 1024, Mbps: 2},
		},
		{
			// A confirmation retry decides whether the alert is sent, so a
			// fallback sharing bandwidth with another node's primary attempt
			// turns directly into a wrong verdict.
			name:          "confirmation retry low speed",
			source:        ConfirmationRetrySource,
			primaryResult: Result{DownloadedBytes: 1024, Mbps: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxies := []*models.ProxyConfig{
				{StableID: "needs-fallback", Name: "Needs fallback", Protocol: "vless"},
				{StableID: "slow-primary", Name: "Slow primary", Protocol: "vless"},
			}
			proxyChecker := checker.NewProxyChecker(proxies, 10000, "", 1, "", "", 1, 0, "status")
			manager := NewManager(proxyChecker, 10000, "", TestConfig{})
			manager.SetLowSpeedThresholdMbps(10)
			manager.fallbackCatalog = mustParseFallbackCatalog(t, `
version: 1
countries:
  DE:
    - id: reserve
      url: https://reserve.example.test/100mb.bin
      priority: 10
`)
			manager.SetCountryResolver(func(string) string { return "DE" })

			primaryFallbackDone := make(chan struct{})
			primarySlowStarted := make(chan struct{})
			releaseSlowPrimary := make(chan struct{})
			fallbackStarted := make(chan struct{}, 1)
			var closeFallbackPrimary sync.Once
			var closeSlowPrimary sync.Once
			manager.testAttempt = func(proxy *models.ProxyConfig, cfg TestConfig, source string) Result {
				result := Result{
					StableID:  proxy.StableID,
					Name:      proxy.Name,
					URL:       cfg.URL,
					Source:    source,
					CheckedAt: time.Now(),
				}
				if cfg.URL == "https://reserve.example.test/100mb.bin" {
					select {
					case fallbackStarted <- struct{}{}:
					default:
					}
					result.DownloadedBytes = cfg.MaxBytes
					result.Mbps = 25
					return result
				}
				if proxy.StableID == "needs-fallback" {
					result.DownloadedBytes = tt.primaryResult.DownloadedBytes
					result.Mbps = tt.primaryResult.Mbps
					result.Error = tt.primaryResult.Error
					closeFallbackPrimary.Do(func() { close(primaryFallbackDone) })
					return result
				}

				closeSlowPrimary.Do(func() { close(primarySlowStarted) })
				<-releaseSlowPrimary
				result.DownloadedBytes = cfg.MaxBytes
				result.Mbps = 25
				return result
			}

			reports := make(reportRecorder, 1)
			manager.SetReporter(reports)
			if err := manager.Run(RunRequest{
				Config: TestConfig{
					URL:         "https://primary.example.test/100mb.bin",
					MaxBytes:    1024,
					TimeoutSec:  30,
					Concurrency: 2,
				},
			}, tt.source); err != nil {
				t.Fatal(err)
			}

			select {
			case <-primaryFallbackDone:
			case <-time.After(time.Second):
				t.Fatal("fallback-eligible primary test did not finish")
			}
			select {
			case <-primarySlowStarted:
			case <-time.After(time.Second):
				t.Fatal("second primary test did not start")
			}
			select {
			case <-fallbackStarted:
				t.Fatal("fallback started before all primary tests finished")
			case <-time.After(50 * time.Millisecond):
			}

			close(releaseSlowPrimary)
			select {
			case <-fallbackStarted:
			case <-time.After(time.Second):
				t.Fatal("queued fallback did not start after the primary phase")
			}

			var report RunReport
			select {
			case report = <-reports:
			case <-time.After(time.Second):
				t.Fatal("manual speed-test report was not delivered")
			}
			if len(report.Results) != 2 {
				t.Fatalf("report results = %+v, want one final result per node", report.Results)
			}
			for _, result := range report.Results {
				if result.StableID == "needs-fallback" && (!result.FallbackUsed || result.URL != "https://reserve.example.test/100mb.bin") {
					t.Fatalf("fallback result = %+v", result)
				}
			}
			for _, proxy := range proxies {
				if history := manager.ResultHistory(proxy.StableID); len(history) != 1 {
					t.Fatalf("history for %s = %+v, want one final result", proxy.StableID, history)
				}
			}
		})
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

// Scheduled sweeps stay parallel on purpose; every interactive or
// alert-deciding source must sequence its phases.
func TestUsesSequencedFallbackCoversAlertDecidingSources(t *testing.T) {
	for source, want := range map[string]bool{
		ManualSource:            true,
		TelegramSource:          true,
		ConfirmationRetrySource: true,
		ScheduleSource:          false,
		"":                      false,
	} {
		if got := usesSequencedFallback(source); got != want {
			t.Errorf("usesSequencedFallback(%q) = %v, want %v", source, got, want)
		}
	}
}
