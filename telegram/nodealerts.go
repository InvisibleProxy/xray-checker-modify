package telegram

import (
	"context"
	"strings"
	"sync"
	"time"

	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/models"
)

type nodeDownAlert struct {
	Proxy     *models.ProxyConfig
	State     nodeAlertState
	NextAfter time.Duration
}

type nodeDownIncidentGroup struct {
	Scope        string
	Subscription string
	Alerts       []nodeDownAlert
	Total        int
	Cause        checker.FailureDetails
}

type nodeRecoveryAlert struct {
	StableID    string
	RecoveredAt time.Time
	Message     formattedMessage
}

type diagnosticsRefreshRequest struct {
	StableID string
	Name     string
}

type diagnosticsRefreshResult struct {
	StableID string
	Name     string
	Details  checker.ProxyStatusDetails
	Err      error
}

func (s *Service) NotifyNodeStatuses() bool {
	if s.ProjectMaintenanceEnabled() {
		return false
	}
	s.nodeNotifyMu.Lock()
	defer s.nodeNotifyMu.Unlock()

	cfg := s.Config()
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.NodeAlertsEnabled {
		return false
	}

	now := time.Now()
	if !s.shouldRunNodeAlertCheck(cfg, now) {
		return false
	}

	proxies := s.proxyChecker.GetProxies()
	active := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if s.proxyChecker.AvailabilityAccounted(proxy.StableID) && s.speaksAbout(proxy.StableID) {
			active[proxy.StableID] = true
		}
	}
	muted := s.alertMuteSet(cfg)

	stateChanged := false
	s.mu.Lock()
	for stableID := range s.alerts {
		if !active[stableID] {
			delete(s.alerts, stableID)
			stateChanged = true
		}
	}
	s.mu.Unlock()

	var recoveryAlerts []nodeRecoveryAlert
	var refreshRequests []diagnosticsRefreshRequest
	var dueAlertProxies []*models.ProxyConfig
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		// Alerting needs a verdict to alert about, and a node the bot speaks
		// about at all.
		if !s.proxyChecker.AvailabilityAccounted(proxy.StableID) || !s.speaksAbout(proxy.StableID) {
			continue
		}
		isMuted := muted[proxy.StableID]

		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil {
			continue
		}
		status := details.EffectiveStatus()
		hasIssue := status != checker.AvailabilityStateOnline

		var shouldSendDownAlert bool
		var shouldRefreshDiagnostics bool
		s.mu.Lock()
		state := s.alerts[proxy.StableID]
		previous := state
		if hasIssue {
			state.Status = status
			state.RecoveryPending = false
			state.RecoveredAt = time.Time{}
			state.RecoveryLatency = 0
			state.FailCount++
			state.WasDown = true
			if details.HostCheck.Checked {
				state.HostCheck = details.HostCheck
			}
			if details.PingCheck.Checked {
				state.PingCheck = details.PingCheck
			}
			if details.Failure.Code != "" {
				state.Failure = details.Failure
			}
			state.LastDiagnostics = latestDiagnosticsAt(state.LastDiagnostics, state.HostCheck, state.PingCheck)
			if status == checker.AvailabilityStateOffline {
				state.ProxyFailureSince = time.Time{}
				if state.DownSince.IsZero() {
					state.DownSince = details.DownSince
					if state.DownSince.IsZero() {
						state.DownSince = now
					}
				}
			} else {
				state.DownSince = time.Time{}
				if state.ProxyFailureSince.IsZero() {
					state.ProxyFailureSince = details.ProxyFailureSince
					if state.ProxyFailureSince.IsZero() {
						state.ProxyFailureSince = now
					}
				}
			}
			shouldRefreshDiagnostics = shouldRefreshNodeDiagnostics(state, cfg, now)
			if !isMuted && state.FailCount >= cfg.AlertAfterFailures {
				if state.NextAlert.IsZero() {
					if state.LastAlert.IsZero() {
						state.NextAlert = now
					} else {
						state.NextAlert = nextAlertAt(state.LastAlert, state.AlertCount, cfg)
					}
				}
				if !now.Before(state.NextAlert) {
					shouldSendDownAlert = true
				}
			}
		} else {
			if shouldNotifyNodeRecovery(state, cfg, isMuted) {
				if !state.RecoveryPending {
					state.RecoveryPending = true
					state.RecoveredAt = now
					state.RecoveryLatency = details.Latency
				}
				recoveryAlerts = append(recoveryAlerts, nodeRecoveryAlert{
					StableID:    proxy.StableID,
					RecoveredAt: state.RecoveredAt,
					Message:     formatNodeRecoveryMessage(proxy, state.RecoveryLatency, state, state.RecoveredAt),
				})
			} else {
				state = nodeAlertState{}
			}
		}
		if previous != state {
			stateChanged = true
		}
		if state == (nodeAlertState{}) {
			delete(s.alerts, proxy.StableID)
		} else {
			s.alerts[proxy.StableID] = state
		}
		s.mu.Unlock()

		if hasIssue && shouldRefreshDiagnostics {
			refreshRequests = append(refreshRequests, diagnosticsRefreshRequest{
				StableID: proxy.StableID,
				Name:     proxy.Name,
			})
		}

		if shouldSendDownAlert {
			dueAlertProxies = append(dueAlertProxies, proxy)
		}
	}

	for _, result := range s.refreshHostDiagnostics(refreshRequests) {
		if result.Err != nil {
			logger.Warn("Failed to refresh host diagnostics for %s: %v", result.Name, result.Err)
			continue
		}
		if result.Details.EffectiveStatus() == checker.AvailabilityStateOnline {
			continue
		}
		if s.updateAlertDiagnostics(result.StableID, result.Details) {
			stateChanged = true
		}
	}
	if s.ProjectMaintenanceEnabled() {
		return false
	}

	downAlerts := make([]nodeDownAlert, 0, len(dueAlertProxies))
	for _, proxy := range dueAlertProxies {
		if alert, ok := s.pendingNodeDownAlert(proxy, cfg, now); ok {
			downAlerts = append(downAlerts, alert)
		}
	}

	for _, alert := range recoveryAlerts {
		if err := s.sendNodeAlertMessageWithMarkup(cfg, alert.Message, nodeAlertMarkup(alert.StableID)); err == nil {
			if s.confirmNodeRecoverySent(alert.StableID, alert.RecoveredAt) {
				stateChanged = true
			}
		}
	}

	massGroups, remainingDownAlerts := partitionMassNodeDownAlerts(downAlerts, proxies, active, muted)
	for _, group := range massGroups {
		if err := s.sendNodeAlertMessage(cfg, formatMassNodeDownMessage(group, now)); err == nil {
			if s.confirmNodeDownAlertsSent(group.Alerts, time.Now(), cfg) {
				stateChanged = true
			}
		}
	}
	if cfg.GroupOfflineReminders && len(remainingDownAlerts) > 1 {
		if err := s.sendNodeAlertMessage(cfg, formatNodeDownGroupMessage(remainingDownAlerts, now)); err == nil {
			if s.confirmNodeDownAlertsSent(remainingDownAlerts, time.Now(), cfg) {
				stateChanged = true
			}
		}
	} else {
		for _, alert := range remainingDownAlerts {
			markup := issuesMarkup()
			if alert.Proxy != nil {
				markup = nodeAlertMarkup(alert.Proxy.StableID)
			}
			if err := s.sendNodeAlertMessageWithMarkup(cfg, formatNodeDownMessage(alert.Proxy, alert.State, now), markup); err == nil {
				if s.confirmNodeDownAlertsSent([]nodeDownAlert{alert}, time.Now(), cfg) {
					stateChanged = true
				}
			}
		}
	}

	if stateChanged {
		if err := s.saveAlertState(); err != nil {
			logger.Warn("Failed to save Telegram node alert state: %v", err)
		}
	}
	return true
}

func (s *Service) NotifyNodeRecoveries(stableIDs []string) {
	if len(stableIDs) == 0 || s.ProjectMaintenanceEnabled() {
		return
	}

	s.nodeNotifyMu.Lock()
	defer s.nodeNotifyMu.Unlock()

	cfg := s.Config()
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.NodeAlertsEnabled {
		return
	}

	muted := s.alertMuteSet(cfg)
	seen := make(map[string]bool, len(stableIDs))
	recoveryAlerts := make([]nodeRecoveryAlert, 0, len(stableIDs))
	stateChanged := false
	for _, rawStableID := range stableIDs {
		stableID := strings.TrimSpace(rawStableID)
		if stableID == "" || seen[stableID] {
			continue
		}
		seen[stableID] = true

		proxy, ok := s.proxyChecker.GetProxyByStableID(stableID)
		if !ok || !s.speaksAbout(stableID) {
			continue
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(stableID)
		if err != nil || !details.Online {
			continue
		}

		alert, shouldSend, changed := s.prepareNodeRecovery(proxy, details, cfg, muted[stableID])
		if shouldSend {
			recoveryAlerts = append(recoveryAlerts, alert)
		}
		if changed {
			stateChanged = true
		}
	}
	if s.ProjectMaintenanceEnabled() {
		return
	}

	for _, alert := range recoveryAlerts {
		if err := s.sendNodeAlertMessageWithMarkup(cfg, alert.Message, nodeAlertMarkup(alert.StableID)); err == nil {
			if s.confirmNodeRecoverySent(alert.StableID, alert.RecoveredAt) {
				stateChanged = true
			}
		}
	}
	if stateChanged {
		if err := s.saveAlertState(); err != nil {
			logger.Warn("Failed to save Telegram node alert state: %v", err)
		}
	}
}

func (s *Service) prepareNodeRecovery(proxy *models.ProxyConfig, details checker.ProxyStatusDetails, cfg Config, isMuted bool) (nodeRecoveryAlert, bool, bool) {
	if proxy == nil || proxy.StableID == "" || !details.Online {
		return nodeRecoveryAlert{}, false, false
	}
	recoveredAt := details.CheckedAt
	if recoveredAt.IsZero() {
		recoveredAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.alerts[proxy.StableID]
	if !exists {
		return nodeRecoveryAlert{}, false, false
	}
	previous := state
	if !shouldNotifyNodeRecovery(state, cfg, isMuted) {
		delete(s.alerts, proxy.StableID)
		return nodeRecoveryAlert{}, false, true
	}
	if !state.RecoveryPending {
		state.RecoveryPending = true
		state.RecoveredAt = recoveredAt
		state.RecoveryLatency = details.Latency
		s.alerts[proxy.StableID] = state
	}
	return nodeRecoveryAlert{
		StableID:    proxy.StableID,
		RecoveredAt: state.RecoveredAt,
		Message:     formatNodeRecoveryMessage(proxy, state.RecoveryLatency, state, state.RecoveredAt),
	}, true, previous != state
}

func (s *Service) sendNodeAlertMessage(cfg Config, content formattedMessage) error {
	return s.sendNodeAlertMessageWithMarkup(cfg, content, issuesMarkup())
}

// sendNodeAlertMessageWithMarkup attaches the actions that belong to an alert.
// Without them, the only way to react to a node going down at night is to open
// the menu and find it by hand.
func (s *Service) sendNodeAlertMessageWithMarkup(cfg Config, content formattedMessage, replyMarkup string) error {
	if content.HTML == "" && content.RichHTML == "" {
		return nil
	}
	if s.nodeAlertSendFunc != nil {
		return s.nodeAlertSendFunc(cfg, content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.sendFormattedToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, content, replyMarkup); err != nil {
		logger.Warn("Failed to send Telegram node alert: %v", err)
		return err
	}
	return nil
}

func (s *Service) pendingNodeDownAlert(proxy *models.ProxyConfig, cfg Config, now time.Time) (nodeDownAlert, bool) {
	if proxy == nil || proxy.StableID == "" {
		return nodeDownAlert{}, false
	}

	details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err == nil && details.EffectiveStatus() == checker.AvailabilityStateOnline {
		return nodeDownAlert{}, false
	}

	s.mu.RLock()
	state := s.alerts[proxy.StableID]
	s.mu.RUnlock()
	if state == (nodeAlertState{}) {
		return nodeDownAlert{}, false
	}
	if state.FailCount < cfg.AlertAfterFailures || state.NextAlert.IsZero() || now.Before(state.NextAlert) {
		return nodeDownAlert{}, false
	}

	alertState := state
	alertState.LastAlert = now
	alertState.AlertCount++
	alertState.NextAlert = nextAlertAt(now, alertState.AlertCount, cfg)

	return nodeDownAlert{
		Proxy:     proxy,
		State:     alertState,
		NextAfter: alertState.NextAlert.Sub(now),
	}, true
}

func shouldNotifyNodeRecovery(state nodeAlertState, cfg Config, isMuted bool) bool {
	if isMuted || !cfg.NotifyRecovery || !state.WasDown {
		return false
	}
	return nodeDownAlertWasSent(state)
}

func nodeAlertStatus(state nodeAlertState) checker.AvailabilityState {
	if state.Status != "" {
		return state.Status
	}
	if state.WasDown {
		return checker.AvailabilityStateOffline
	}
	return ""
}

func nodeAlertIssueSince(state nodeAlertState) time.Time {
	if nodeAlertStatus(state) == checker.AvailabilityStateProxyFailure {
		return state.ProxyFailureSince
	}
	return state.DownSince
}

func nodeDownAlertWasSent(state nodeAlertState) bool {
	return state.AlertCount > 0 || !state.LastAlert.IsZero()
}

func (s *Service) confirmNodeDownAlertsSent(alerts []nodeDownAlert, sentAt time.Time, cfg Config) bool {
	if len(alerts) == 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, alert := range alerts {
		if alert.Proxy == nil || alert.Proxy.StableID == "" {
			continue
		}
		state := s.alerts[alert.Proxy.StableID]
		if state == (nodeAlertState{}) || !state.WasDown || nodeAlertStatus(state) != nodeAlertStatus(alert.State) {
			continue
		}

		previous := state
		state.LastAlert = sentAt
		state.AlertCount++
		state.NextAlert = nextAlertAt(sentAt, state.AlertCount, cfg)
		if previous != state {
			s.alerts[alert.Proxy.StableID] = state
			changed = true
		}
	}
	return changed
}

func (s *Service) confirmNodeRecoverySent(stableID string, recoveredAt time.Time) bool {
	if stableID == "" || recoveredAt.IsZero() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.alerts[stableID]
	if !ok || !state.RecoveryPending || !state.RecoveredAt.Equal(recoveredAt) {
		return false
	}
	delete(s.alerts, stableID)
	return true
}

func (s *Service) refreshHostDiagnostics(requests []diagnosticsRefreshRequest) []diagnosticsRefreshResult {
	if len(requests) == 0 {
		return nil
	}

	workers := maxDiagnosticsRefreshConcurrency
	if workers > len(requests) {
		workers = len(requests)
	}
	if workers <= 0 {
		workers = 1
	}

	jobs := make(chan diagnosticsRefreshRequest)
	results := make(chan diagnosticsRefreshResult, len(requests))
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := range jobs {
				details, err := s.proxyChecker.RefreshHostDiagnosticsByStableID(req.StableID)
				results <- diagnosticsRefreshResult{
					StableID: req.StableID,
					Name:     req.Name,
					Details:  details,
					Err:      err,
				}
			}
		}()
	}

	for _, req := range requests {
		jobs <- req
	}
	close(jobs)
	wg.Wait()
	close(results)

	collected := make([]diagnosticsRefreshResult, 0, len(requests))
	for result := range results {
		collected = append(collected, result)
	}
	return collected
}

func (s *Service) shouldRunNodeAlertCheck(cfg Config, now time.Time) bool {
	if cfg.AlertCheckMinutes <= 0 {
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	interval := time.Duration(cfg.AlertCheckMinutes) * time.Minute
	if !s.lastAlertRun.IsZero() && now.Sub(s.lastAlertRun) < interval {
		return false
	}
	s.lastAlertRun = now
	return true
}

func shouldRefreshNodeDiagnostics(state nodeAlertState, cfg Config, now time.Time) bool {
	if cfg.AlertDiagnosticsMinutes <= 0 {
		return false
	}
	if !state.HostCheck.Checked || !state.PingCheck.Checked {
		return true
	}
	lastDiagnostics := latestDiagnosticsAt(state.LastDiagnostics, state.HostCheck, state.PingCheck)
	if lastDiagnostics.IsZero() {
		return true
	}
	return now.Sub(lastDiagnostics) >= time.Duration(cfg.AlertDiagnosticsMinutes)*time.Minute
}

func latestDiagnosticsAt(current time.Time, hostCheck checker.HostCheckDetails, pingCheck checker.PingCheckDetails) time.Time {
	latest := current
	if hostCheck.Checked && hostCheck.CheckedAt.After(latest) {
		latest = hostCheck.CheckedAt
	}
	if pingCheck.Checked && pingCheck.CheckedAt.After(latest) {
		latest = pingCheck.CheckedAt
	}
	return latest
}

func nextAlertAt(from time.Time, alertCount int, cfg Config) time.Time {
	return from.Add(time.Duration(nextAlertIntervalMinutes(alertCount, cfg)) * time.Minute)
}

func nextAlertIntervalMinutes(alertCount int, cfg Config) int {
	if alertCount <= 0 {
		return 0
	}
	index := alertCount - 1
	if index >= 0 && index < len(cfg.AlertReminderScheduleMinutes) {
		return cfg.AlertReminderScheduleMinutes[index]
	}
	return cfg.AlertMaxReminderMinutes
}
