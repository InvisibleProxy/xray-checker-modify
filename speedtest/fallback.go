package speedtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"xray-checker/logger"
	"xray-checker/models"

	"gopkg.in/yaml.v3"
)

const (
	countryFallbackCatalogVersion = 1
	fallbackHealthVersion         = 1
	fallbackProbeMaxBytes         = int64(64 * 1024)
	fallbackProbeTimeoutSec       = 8
	maxFallbackAttempts           = 2
	fallbackNodeCooldown          = 10 * time.Minute
	fallbackGlobalCooldown        = 30 * time.Minute
	fallbackFailureWindow         = 5 * time.Minute
	fallbackGlobalFailureCount    = 3
	fallbackGlobalFailureNodes    = 2
)

var fallbackEndpointIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type countryTestURLCatalog struct {
	Version   int                           `yaml:"version"`
	Countries map[string][]fallbackEndpoint `yaml:"countries"`
}

type fallbackEndpoint struct {
	ID       string `yaml:"id"`
	Provider string `yaml:"provider,omitempty"`
	City     string `yaml:"city,omitempty"`
	URL      string `yaml:"url"`
	Priority int    `yaml:"priority"`
	Enabled  *bool  `yaml:"enabled,omitempty"`
	Source   string `yaml:"source,omitempty"`
}

func (e fallbackEndpoint) enabled() bool {
	return e.Enabled == nil || *e.Enabled
}

type fallbackHealthState struct {
	Version   int                                      `json:"version"`
	UpdatedAt time.Time                                `json:"updatedAt"`
	Endpoints map[string]fallbackEndpointHealth        `json:"endpoints,omitempty"`
	Nodes     map[string]map[string]fallbackNodeHealth `json:"nodes,omitempty"`
}

type fallbackEndpointHealth struct {
	Successes           int                  `json:"successes,omitempty"`
	Failures            int                  `json:"failures,omitempty"`
	ConsecutiveFailures int                  `json:"consecutiveFailures,omitempty"`
	LastSuccessAt       time.Time            `json:"lastSuccessAt,omitempty"`
	LastFailureAt       time.Time            `json:"lastFailureAt,omitempty"`
	CooldownUntil       time.Time            `json:"cooldownUntil,omitempty"`
	RecentFailures      map[string]time.Time `json:"recentFailures,omitempty"`
}

type fallbackNodeHealth struct {
	LastSuccessAt time.Time `json:"lastSuccessAt,omitempty"`
	LastFailureAt time.Time `json:"lastFailureAt,omitempty"`
	CooldownUntil time.Time `json:"cooldownUntil,omitempty"`
}

func (m *Manager) ConfigureCountryFallbacks(catalogPath string, healthPath string) {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()
	m.fallbackCatalogPath = strings.TrimSpace(catalogPath)
	m.fallbackHealthPath = strings.TrimSpace(healthPath)
}

func (m *Manager) LoadCountryFallbacks() error {
	catalogErr := m.reloadCountryFallbackCatalog(true)
	healthErr := m.loadFallbackHealth()
	return errors.Join(catalogErr, healthErr)
}

func (m *Manager) SetCountryResolver(resolver func(stableID string) string) {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()
	m.countryResolver = resolver
}

func (m *Manager) reloadCountryFallbackCatalog(force bool) error {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()

	path := m.fallbackCatalogPath
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.fallbackCatalog = countryTestURLCatalog{Version: countryFallbackCatalogVersion}
			m.fallbackCatalogModTime = time.Time{}
			m.fallbackCatalogLoaded = true
			return nil
		}
		return fmt.Errorf("stat country Test URL catalog: %w", err)
	}
	if !force && m.fallbackCatalogLoaded && info.ModTime().Equal(m.fallbackCatalogModTime) {
		return nil
	}

	catalog, err := readCountryTestURLCatalog(path)
	if err != nil {
		return err
	}
	m.fallbackCatalog = catalog
	m.fallbackCatalogModTime = info.ModTime()
	m.fallbackCatalogLoaded = true
	logger.Info("Loaded country Test URL catalog: %d countries, %d endpoints", len(catalog.Countries), catalogEndpointCount(catalog))
	return nil
}

func readCountryTestURLCatalog(path string) (countryTestURLCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return countryTestURLCatalog{}, fmt.Errorf("read country Test URL catalog: %w", err)
	}
	return parseCountryTestURLCatalog(data)
}

func parseCountryTestURLCatalog(data []byte) (countryTestURLCatalog, error) {
	var raw countryTestURLCatalog
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return countryTestURLCatalog{}, fmt.Errorf("parse country Test URL catalog: %w", err)
	}
	if raw.Version != countryFallbackCatalogVersion {
		return countryTestURLCatalog{}, fmt.Errorf("unsupported country Test URL catalog version %d", raw.Version)
	}

	normalized := countryTestURLCatalog{
		Version:   countryFallbackCatalogVersion,
		Countries: make(map[string][]fallbackEndpoint, len(raw.Countries)),
	}
	seenCountries := make(map[string]bool, len(raw.Countries))
	seenIDs := make(map[string]bool)
	for rawCode, endpoints := range raw.Countries {
		code := strings.ToUpper(strings.TrimSpace(rawCode))
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return countryTestURLCatalog{}, fmt.Errorf("invalid ISO country code %q", rawCode)
		}
		if seenCountries[code] {
			return countryTestURLCatalog{}, fmt.Errorf("duplicate country code %q", code)
		}
		seenCountries[code] = true

		seenURLs := make(map[string]bool)
		for _, endpoint := range endpoints {
			endpoint.ID = strings.ToLower(strings.TrimSpace(endpoint.ID))
			endpoint.Provider = strings.TrimSpace(endpoint.Provider)
			endpoint.City = strings.TrimSpace(endpoint.City)
			endpoint.URL = strings.TrimSpace(endpoint.URL)
			endpoint.Source = strings.TrimSpace(endpoint.Source)
			if !fallbackEndpointIDPattern.MatchString(endpoint.ID) {
				return countryTestURLCatalog{}, fmt.Errorf("invalid endpoint id %q for country %s", endpoint.ID, code)
			}
			if seenIDs[endpoint.ID] {
				return countryTestURLCatalog{}, fmt.Errorf("duplicate endpoint id %q", endpoint.ID)
			}
			seenIDs[endpoint.ID] = true
			if endpoint.Priority < 0 {
				return countryTestURLCatalog{}, fmt.Errorf("negative priority for endpoint %q", endpoint.ID)
			}
			if _, err := normalizeNodeTestURL(endpoint.URL); err != nil {
				return countryTestURLCatalog{}, fmt.Errorf("endpoint %q: %w", endpoint.ID, err)
			}
			if seenURLs[endpoint.URL] {
				return countryTestURLCatalog{}, fmt.Errorf("duplicate URL for country %s: %s", code, endpoint.URL)
			}
			seenURLs[endpoint.URL] = true
			normalized.Countries[code] = append(normalized.Countries[code], endpoint)
		}
		sort.SliceStable(normalized.Countries[code], func(i, j int) bool {
			left := normalized.Countries[code][i]
			right := normalized.Countries[code][j]
			if left.Priority != right.Priority {
				return left.Priority < right.Priority
			}
			return left.ID < right.ID
		})
	}
	return normalized, nil
}

func catalogEndpointCount(catalog countryTestURLCatalog) int {
	total := 0
	for _, endpoints := range catalog.Countries {
		total += len(endpoints)
	}
	return total
}

func (m *Manager) loadFallbackHealth() error {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()

	if m.fallbackHealthPath == "" {
		m.ensureFallbackHealthLocked()
		return nil
	}
	data, err := os.ReadFile(m.fallbackHealthPath)
	if err != nil {
		if os.IsNotExist(err) {
			m.ensureFallbackHealthLocked()
			return nil
		}
		return fmt.Errorf("read Test URL health: %w", err)
	}
	var state fallbackHealthState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse Test URL health: %w", err)
	}
	if state.Version > fallbackHealthVersion {
		return fmt.Errorf("unsupported Test URL health version %d", state.Version)
	}
	state.Version = fallbackHealthVersion
	m.fallbackHealth = state
	m.ensureFallbackHealthLocked()
	return nil
}

func (m *Manager) ensureFallbackHealthLocked() {
	if m.fallbackHealth.Version == 0 {
		m.fallbackHealth.Version = fallbackHealthVersion
	}
	if m.fallbackHealth.Endpoints == nil {
		m.fallbackHealth.Endpoints = make(map[string]fallbackEndpointHealth)
	}
	if m.fallbackHealth.Nodes == nil {
		m.fallbackHealth.Nodes = make(map[string]map[string]fallbackNodeHealth)
	}
}

func (m *Manager) persistFallbackHealth() error {
	m.fallbackMu.Lock()
	path := m.fallbackHealthPath
	if path == "" {
		m.fallbackMu.Unlock()
		return nil
	}
	m.ensureFallbackHealthLocked()
	m.fallbackHealth.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(m.fallbackHealth, "", "  ")
	m.fallbackMu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0644)
}

func (m *Manager) testProxyWithFallback(proxy *models.ProxyConfig, cfg TestConfig, source string) Result {
	primary := m.executeTestAttempt(proxy, cfg, source)
	threshold := m.fallbackLowSpeedThreshold()
	if primary.Offline || (primary.Error == "" && (threshold <= 0 || primary.Mbps >= threshold)) {
		return primary
	}

	countryCode := m.resolveClaimedCountry(proxy)
	if countryCode == "" {
		return primary
	}
	candidates := m.fallbackCandidates(proxy.StableID, countryCode, cfg.URL, time.Now())
	if len(candidates) > maxFallbackAttempts {
		candidates = candidates[:maxFallbackAttempts]
	}
	for _, endpoint := range candidates {
		probeConfig := cfg
		probeConfig.URL = endpoint.URL
		probeConfig.MaxBytes = fallbackProbeMaxBytes
		if probeConfig.TimeoutSec <= 0 || probeConfig.TimeoutSec > fallbackProbeTimeoutSec {
			probeConfig.TimeoutSec = fallbackProbeTimeoutSec
		}
		probe := m.executeTestAttempt(proxy, probeConfig, source)
		if !successfulSpeedResult(probe) {
			m.recordFallbackFailure(proxy.StableID, endpoint.ID, time.Now())
			continue
		}

		fallbackConfig := cfg
		fallbackConfig.URL = endpoint.URL
		result := m.executeTestAttempt(proxy, fallbackConfig, source)
		if !successfulSpeedResult(result) {
			m.recordFallbackFailure(proxy.StableID, endpoint.ID, time.Now())
			continue
		}
		m.recordFallbackSuccess(proxy.StableID, endpoint.ID, time.Now())
		result.FallbackUsed = true
		result.FallbackID = endpoint.ID
		result.FallbackProvider = endpoint.Provider
		result.FallbackCity = endpoint.City
		result.FallbackCountryCode = countryCode
		result.PrimaryURL = primary.URL
		result.PrimaryError = primary.Error
		result.TelegramAlertSuppressed = threshold <= 0 || result.Mbps >= threshold
		return result
	}
	return primary
}

// SetLowSpeedThresholdMbps keeps country fallback selection aligned with the
// threshold used by Telegram reports and their delayed confirmation workflow.
func (m *Manager) SetLowSpeedThresholdMbps(threshold float64) {
	if threshold < 0 {
		threshold = 0
	}
	m.fallbackMu.Lock()
	m.lowSpeedThresholdMbps = threshold
	m.fallbackMu.Unlock()
}

func (m *Manager) fallbackLowSpeedThreshold() float64 {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()
	return m.lowSpeedThresholdMbps
}

func successfulSpeedResult(result Result) bool {
	return !result.Offline && result.Error == "" && result.DownloadedBytes > 0
}

func (m *Manager) executeTestAttempt(proxy *models.ProxyConfig, cfg TestConfig, source string) Result {
	if m.testAttempt != nil {
		return m.testAttempt(proxy, cfg, source)
	}
	return m.testProxy(proxy, cfg, source)
}

func (m *Manager) resolveClaimedCountry(proxy *models.ProxyConfig) string {
	if proxy == nil {
		return ""
	}
	m.fallbackMu.Lock()
	resolver := m.countryResolver
	m.fallbackMu.Unlock()
	if resolver == nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(resolver(proxy.StableID)))
}

func (m *Manager) fallbackCandidates(stableID string, countryCode string, primaryURL string, now time.Time) []fallbackEndpoint {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()
	m.ensureFallbackHealthLocked()

	endpoints := m.fallbackCatalog.Countries[strings.ToUpper(strings.TrimSpace(countryCode))]
	candidates := make([]fallbackEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if !endpoint.enabled() || endpoint.URL == strings.TrimSpace(primaryURL) {
			continue
		}
		globalHealth := m.fallbackHealth.Endpoints[endpoint.ID]
		if globalHealth.CooldownUntil.After(now) {
			continue
		}
		nodeHealth := m.fallbackHealth.Nodes[stableID][endpoint.ID]
		if nodeHealth.CooldownUntil.After(now) {
			continue
		}
		candidates = append(candidates, endpoint)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		leftNode := m.fallbackHealth.Nodes[stableID][left.ID]
		rightNode := m.fallbackHealth.Nodes[stableID][right.ID]
		if !leftNode.LastSuccessAt.Equal(rightNode.LastSuccessAt) {
			return leftNode.LastSuccessAt.After(rightNode.LastSuccessAt)
		}
		leftGlobal := m.fallbackHealth.Endpoints[left.ID]
		rightGlobal := m.fallbackHealth.Endpoints[right.ID]
		leftKnown := leftGlobal.Successes > 0
		rightKnown := rightGlobal.Successes > 0
		if leftKnown != rightKnown {
			return leftKnown
		}
		leftReliability := fallbackReliability(leftGlobal)
		rightReliability := fallbackReliability(rightGlobal)
		if leftReliability != rightReliability {
			return leftReliability > rightReliability
		}
		if !leftGlobal.LastSuccessAt.Equal(rightGlobal.LastSuccessAt) {
			return leftGlobal.LastSuccessAt.After(rightGlobal.LastSuccessAt)
		}
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		return left.ID < right.ID
	})
	return candidates
}

func fallbackReliability(health fallbackEndpointHealth) float64 {
	total := health.Successes + health.Failures
	if total == 0 {
		return 0
	}
	return float64(health.Successes) / float64(total)
}

func (m *Manager) recordFallbackFailure(stableID string, endpointID string, now time.Time) {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()
	m.ensureFallbackHealthLocked()

	endpoint := m.fallbackHealth.Endpoints[endpointID]
	endpoint.Failures++
	endpoint.ConsecutiveFailures++
	endpoint.LastFailureAt = now
	if endpoint.RecentFailures == nil {
		endpoint.RecentFailures = make(map[string]time.Time)
	}
	for id, failedAt := range endpoint.RecentFailures {
		if failedAt.Before(now.Add(-fallbackFailureWindow)) {
			delete(endpoint.RecentFailures, id)
		}
	}
	endpoint.RecentFailures[stableID] = now
	if endpoint.ConsecutiveFailures >= fallbackGlobalFailureCount && len(endpoint.RecentFailures) >= fallbackGlobalFailureNodes {
		endpoint.CooldownUntil = now.Add(fallbackGlobalCooldown)
	}
	m.fallbackHealth.Endpoints[endpointID] = endpoint

	if m.fallbackHealth.Nodes[stableID] == nil {
		m.fallbackHealth.Nodes[stableID] = make(map[string]fallbackNodeHealth)
	}
	node := m.fallbackHealth.Nodes[stableID][endpointID]
	node.LastFailureAt = now
	node.CooldownUntil = now.Add(fallbackNodeCooldown)
	m.fallbackHealth.Nodes[stableID][endpointID] = node
}

func (m *Manager) recordFallbackSuccess(stableID string, endpointID string, now time.Time) {
	m.fallbackMu.Lock()
	defer m.fallbackMu.Unlock()
	m.ensureFallbackHealthLocked()

	endpoint := m.fallbackHealth.Endpoints[endpointID]
	endpoint.Successes++
	endpoint.ConsecutiveFailures = 0
	endpoint.LastSuccessAt = now
	endpoint.CooldownUntil = time.Time{}
	endpoint.RecentFailures = nil
	m.fallbackHealth.Endpoints[endpointID] = endpoint

	if m.fallbackHealth.Nodes[stableID] == nil {
		m.fallbackHealth.Nodes[stableID] = make(map[string]fallbackNodeHealth)
	}
	node := m.fallbackHealth.Nodes[stableID][endpointID]
	node.LastSuccessAt = now
	node.CooldownUntil = time.Time{}
	m.fallbackHealth.Nodes[stableID][endpointID] = node
}
