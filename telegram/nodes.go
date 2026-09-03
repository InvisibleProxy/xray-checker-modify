package telegram

import (
	"fmt"
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

// orderedProxyCandidates puts the node that last carried a Telegram reply in
// front of the latency ordering. Reusing it keeps the pooled TLS connection
// warm instead of handshaking through a different node on every call.
func (s *Service) orderedProxyCandidates() []proxyCandidate {
	candidates := s.proxyCandidates()
	if len(candidates) < 2 {
		return candidates
	}

	s.mu.RLock()
	preferred := s.lastWorkingProxyID
	s.mu.RUnlock()
	if preferred == "" {
		return candidates
	}

	for i, candidate := range candidates {
		if candidate.Proxy == nil || candidate.Proxy.StableID != preferred {
			continue
		}
		if i == 0 {
			return candidates
		}
		reordered := make([]proxyCandidate, 0, len(candidates))
		reordered = append(reordered, candidate)
		reordered = append(reordered, candidates[:i]...)
		reordered = append(reordered, candidates[i+1:]...)
		return reordered
	}
	return candidates
}

func (s *Service) rememberWorkingProxy(proxy *models.ProxyConfig) {
	if proxy == nil || proxy.StableID == "" {
		return
	}
	s.mu.Lock()
	s.lastWorkingProxyID = proxy.StableID
	s.mu.Unlock()
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

// alertMuteSet and speedMuteSet combine the operator's permanent mute lists
// from the admin UI with the expiring mutes an admin set from the bot.
func (s *Service) alertMuteSet(cfg Config) map[string]bool {
	return s.withTemporaryMutes(mutedAlertNodeSet(cfg), muteScopeAlerts)
}

func (s *Service) speedMuteSet(cfg Config) map[string]bool {
	return s.withTemporaryMutes(mutedSpeedNodeSet(cfg), muteScopeSpeed)
}

func (s *Service) withTemporaryMutes(base map[string]bool, scope string) map[string]bool {
	if base == nil {
		base = make(map[string]bool)
	}
	for stableID := range s.activeTemporaryMutes(time.Now(), scope) {
		base[stableID] = true
	}
	return base
}

func normalizeMuteScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case muteScopeAll:
		return muteScopeAll
	case muteScopeAlerts:
		return muteScopeAlerts
	case muteScopeSpeed:
		return muteScopeSpeed
	default:
		return ""
	}
}

func muteScopeCovers(scope string, wanted string) bool {
	return scope == muteScopeAll || scope == wanted
}

func (s *Service) activeTemporaryMutes(now time.Time, scope string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.mutes) == 0 {
		return nil
	}
	result := make(map[string]bool)
	for stableID, mute := range s.mutes {
		if mute.Until.After(now) && muteScopeCovers(mute.Scope, scope) {
			result[stableID] = true
		}
	}
	return result
}

// muteNodeFor silences a node until a deadline. A zero duration means the
// operator asked for a permanent mute, which belongs in the editable config so
// the admin UI shows it and can lift it.
func (s *Service) muteNodeFor(stableID string, scope string, duration time.Duration) error {
	stableID = strings.TrimSpace(stableID)
	scope = normalizeMuteScope(scope)
	if stableID == "" || scope == "" {
		return fmt.Errorf("stableId and mute scope are required")
	}
	if duration <= 0 {
		return s.muteNodePermanently(stableID, scope)
	}

	s.mu.Lock()
	if s.mutes == nil {
		s.mutes = make(map[string]nodeMute)
	}
	s.mutes[stableID] = nodeMute{Scope: scope, Until: time.Now().Add(duration)}
	s.mu.Unlock()
	return s.saveAlertState()
}

func (s *Service) muteNodePermanently(stableID string, scope string) error {
	cfg := s.Config()
	switch scope {
	case muteScopeAll:
		cfg.MutedNodeIDs = appendNodeID(cfg.MutedNodeIDs, stableID)
	case muteScopeAlerts:
		cfg.MutedAlertNodeIDs = appendNodeID(cfg.MutedAlertNodeIDs, stableID)
	case muteScopeSpeed:
		cfg.MutedSpeedNodeIDs = appendNodeID(cfg.MutedSpeedNodeIDs, stableID)
	default:
		return fmt.Errorf("unknown mute scope %q", scope)
	}
	cfg.Normalize()
	if err := s.saveEditableConfig(cfg); err != nil {
		return err
	}
	s.setConfig(cfg)

	// A permanent mute supersedes any countdown that was already running.
	s.mu.Lock()
	_, hadTemporary := s.mutes[stableID]
	delete(s.mutes, stableID)
	s.mu.Unlock()
	if hadTemporary {
		return s.saveAlertState()
	}
	return nil
}

// unmuteNode lifts both halves at once: the operator asked for notifications
// back, not for a guess about which list currently holds the node.
func (s *Service) unmuteNode(stableID string) error {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return fmt.Errorf("stableId is required")
	}

	s.mu.Lock()
	_, hadTemporary := s.mutes[stableID]
	delete(s.mutes, stableID)
	s.mu.Unlock()

	cfg := s.Config()
	permanent := removeNodeID(cfg.MutedNodeIDs, stableID)
	alerts := removeNodeID(cfg.MutedAlertNodeIDs, stableID)
	speed := removeNodeID(cfg.MutedSpeedNodeIDs, stableID)
	configChanged := !sameNodeIDs(cfg.MutedNodeIDs, permanent) ||
		!sameNodeIDs(cfg.MutedAlertNodeIDs, alerts) ||
		!sameNodeIDs(cfg.MutedSpeedNodeIDs, speed)
	if configChanged {
		cfg.MutedNodeIDs = permanent
		cfg.MutedAlertNodeIDs = alerts
		cfg.MutedSpeedNodeIDs = speed
		if err := s.saveEditableConfig(cfg); err != nil {
			return err
		}
		s.setConfig(cfg)
	}
	if hadTemporary {
		return s.saveAlertState()
	}
	return nil
}

type nodeMuteStatus struct {
	Scope     string
	Until     time.Time
	Permanent bool
}

func (s *Service) nodeMuteStatusFor(stableID string, cfg Config) nodeMuteStatus {
	if stableID == "" {
		return nodeMuteStatus{}
	}
	permanent := mutedNodeSet(cfg.MutedNodeIDs)
	alerts := mutedNodeSet(cfg.MutedAlertNodeIDs)
	speed := mutedNodeSet(cfg.MutedSpeedNodeIDs)
	switch {
	case permanent[stableID]:
		return nodeMuteStatus{Scope: muteScopeAll, Permanent: true}
	case alerts[stableID] && speed[stableID]:
		return nodeMuteStatus{Scope: muteScopeAll, Permanent: true}
	case alerts[stableID]:
		return nodeMuteStatus{Scope: muteScopeAlerts, Permanent: true}
	case speed[stableID]:
		return nodeMuteStatus{Scope: muteScopeSpeed, Permanent: true}
	}

	s.mu.RLock()
	mute, ok := s.mutes[stableID]
	s.mu.RUnlock()
	if ok && mute.Until.After(time.Now()) {
		return nodeMuteStatus{Scope: mute.Scope, Until: mute.Until}
	}
	return nodeMuteStatus{}
}

func (s nodeMuteStatus) Muted() bool {
	return s.Scope != ""
}

func appendNodeID(values []string, stableID string) []string {
	for _, value := range values {
		if value == stableID {
			return values
		}
	}
	return append(append([]string(nil), values...), stableID)
}

func removeNodeID(values []string, stableID string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != stableID {
			result = append(result, value)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
