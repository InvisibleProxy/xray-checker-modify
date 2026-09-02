package telegram

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"xray-checker/models"
	"xray-checker/speedtest"
)

type proxyCandidate struct {
	Proxy   *models.ProxyConfig
	Latency time.Duration
}

func (s *Service) latestSpeedResult(stableID string) *speedtest.Result {
	for _, result := range s.speedManager.Snapshot().Results {
		if result.StableID == stableID {
			resultCopy := result
			return &resultCopy
		}
	}
	return nil
}

func (s *Service) sortedProxies() []*models.ProxyConfig {
	all := s.proxyChecker.GetProxies()
	proxies := make([]*models.ProxyConfig, 0, len(all))
	for _, proxy := range all {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if !s.proxyChecker.MonitoringEnabled(proxy.StableID) {
			continue
		}
		proxies = append(proxies, proxy)
	}
	sort.Slice(proxies, func(i, j int) bool {
		return strings.ToLower(proxies[i].Name) < strings.ToLower(proxies[j].Name)
	})
	return proxies
}

func (s *Service) proxyCandidates() []proxyCandidate {
	proxies := s.proxyChecker.GetProxies()
	var candidates []proxyCandidate
	knownStatuses := 0

	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if !s.proxyChecker.MonitoringEnabled(proxy.StableID) {
			continue
		}

		online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		if err != nil {
			continue
		}
		knownStatuses++
		if online {
			candidates = append(candidates, proxyCandidate{Proxy: proxy, Latency: latency})
		}
	}

	if len(candidates) == 0 && knownStatuses == 0 {
		for _, proxy := range proxies {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			if !s.proxyChecker.MonitoringEnabled(proxy.StableID) {
				continue
			}
			candidates = append(candidates, proxyCandidate{Proxy: proxy})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i].Latency
		right := candidates[j].Latency
		if left == 0 && right != 0 {
			return false
		}
		if right == 0 && left != 0 {
			return true
		}
		return left < right
	})
	return candidates
}

func (s *Service) httpClientFor(proxy *models.ProxyConfig, timeoutSec int) (*http.Client, error) {
	proxyURL, err := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", s.startPort+proxy.Index))
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
		Timeout: time.Duration(timeoutSec) * time.Second,
	}, nil
}

func (s *Service) findProxy(query string) (*models.ProxyConfig, []*models.ProxyConfig) {
	query = strings.ToLower(strings.TrimSpace(query))
	var matches []*models.ProxyConfig

	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if !s.proxyChecker.MonitoringEnabled(proxy.StableID) {
			continue
		}
		stableID := strings.ToLower(proxy.StableID)
		name := strings.ToLower(proxy.Name)
		if stableID == query || strings.HasPrefix(stableID, query) {
			return proxy, []*models.ProxyConfig{proxy}
		}
		if strings.Contains(name, query) {
			matches = append(matches, proxy)
		}
	}

	if len(matches) == 1 {
		return matches[0], matches
	}
	return nil, matches
}

func (s *Service) activeMutedNodeIDs(values []string) []string {
	active := s.activeNodeIDs()
	if len(active) == 0 {
		return normalizeNodeIDs(values)
	}
	return filterActiveNodeIDs(values, active)
}

func (s *Service) activeNodeIDs() map[string]bool {
	if s.proxyChecker == nil {
		return nil
	}

	proxies := s.proxyChecker.GetProxies()
	if len(proxies) == 0 {
		return nil
	}

	active := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if proxy.StableID != "" {
			active[proxy.StableID] = true
		}
	}
	return active
}

func (s *Service) monitoredNodeIDs() map[string]bool {
	active := s.activeNodeIDs()
	for stableID := range active {
		if !s.proxyChecker.MonitoringEnabled(stableID) {
			delete(active, stableID)
		}
	}
	return active
}

func filterActiveNodeIDs(values []string, active map[string]bool) []string {
	values = normalizeNodeIDs(values)
	if len(active) == 0 {
		return values
	}

	result := values[:0]
	for _, value := range values {
		if active[value] {
			result = append(result, value)
		}
	}
	return result
}

func sameNodeIDs(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mutedNodeSet(groups ...[]string) map[string]bool {
	result := make(map[string]bool)
	for _, values := range groups {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				result[value] = true
			}
		}
	}
	return result
}

func mutedSpeedNodeSet(cfg Config) map[string]bool {
	return mutedNodeSet(cfg.MutedNodeIDs, cfg.MutedSpeedNodeIDs)
}

func mutedAlertNodeSet(cfg Config) map[string]bool {
	return mutedNodeSet(cfg.MutedNodeIDs, cfg.MutedAlertNodeIDs)
}
