package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/speedtest"
)

type nodeAlertState struct {
	FailCount         int
	WasDown           bool
	Status            checker.AvailabilityState
	DownSince         time.Time
	ProxyFailureSince time.Time
	LastAlert         time.Time
	AlertCount        int
	NextAlert         time.Time
	LastDiagnostics   time.Time
	HostCheck         checker.HostCheckDetails
	PingCheck         checker.PingCheckDetails
	Failure           checker.FailureDetails
	RecoveryPending   bool
	RecoveredAt       time.Time
	RecoveryLatency   time.Duration
}

type nodeAlertStateFile struct {
	Version      int                                `json:"version"`
	UpdatedAt    time.Time                          `json:"updatedAt"`
	Nodes        map[string]persistedNodeAlertState `json:"nodes"`
	SpeedRetries []persistedSpeedRetry              `json:"speedRetries,omitempty"`
}

type pendingSpeedRetry struct {
	Kind      string
	Request   speedtest.RunRequest
	StableIDs []string
	DueAt     time.Time
}

type persistedSpeedRetry struct {
	Kind      string               `json:"kind,omitempty"`
	StableIDs []string             `json:"stableIds"`
	Config    speedtest.TestConfig `json:"config"`
	DueAt     time.Time            `json:"dueAt"`
}

type speedRetryKey struct {
	Kind     string
	StableID string
}

type persistedNodeAlertState struct {
	FailCount         int                 `json:"failCount"`
	WasDown           bool                `json:"wasDown"`
	Status            string              `json:"status,omitempty"`
	DownSince         time.Time           `json:"downSince"`
	ProxyFailureSince time.Time           `json:"proxyFailureSince,omitempty"`
	LastAlert         time.Time           `json:"lastAlert"`
	AlertCount        int                 `json:"alertCount"`
	NextAlert         time.Time           `json:"nextAlert"`
	LastDiagnostics   time.Time           `json:"lastDiagnostics"`
	HostCheck         *persistedHostCheck `json:"hostCheck,omitempty"`
	PingCheck         *persistedPingCheck `json:"pingCheck,omitempty"`
	FailureCode       string              `json:"failureCode,omitempty"`
	FailureSummary    string              `json:"failureSummary,omitempty"`
	FailureDetail     string              `json:"failureDetail,omitempty"`
	RecoveryPending   bool                `json:"recoveryPending,omitempty"`
	RecoveredAt       time.Time           `json:"recoveredAt,omitempty"`
	RecoveryLatency   int64               `json:"recoveryLatencyMs,omitempty"`
}

type persistedHostCheck struct {
	Checked   bool      `json:"checked"`
	Online    bool      `json:"online"`
	LatencyMs int64     `json:"latencyMs"`
	CheckedAt time.Time `json:"checkedAt"`
	Target    string    `json:"target,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type persistedPingCheck struct {
	Checked   bool      `json:"checked"`
	Online    bool      `json:"online"`
	LatencyMs int64     `json:"latencyMs"`
	CheckedAt time.Time `json:"checkedAt"`
	Target    string    `json:"target,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func (s *Service) loadAlertStateWithWarn() {
	if err := s.loadAlertState(); err != nil {
		logger.Warn("Failed to load Telegram node alert state: %v", err)
	}
}

func (s *Service) loadAlertState() error {
	if s.alertStatePath == "" {
		return nil
	}

	data, err := os.ReadFile(s.alertStatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var stateFile nodeAlertStateFile
	if err := json.Unmarshal(data, &stateFile); err != nil {
		return err
	}
	active := make(map[string]bool)
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if s.proxyChecker.MonitoringEnabled(proxy.StableID) {
			active[proxy.StableID] = true
		}
	}

	loaded := make(map[string]nodeAlertState)
	for stableID, persisted := range stateFile.Nodes {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" || !active[stableID] {
			continue
		}

		state := persisted.toNodeAlertState()
		if !state.WasDown && state.FailCount <= 0 && state.DownSince.IsZero() && state.ProxyFailureSince.IsZero() && state.LastAlert.IsZero() {
			continue
		}
		loaded[stableID] = state
	}
	if len(loaded) > 0 {
		s.mu.Lock()
		for stableID, state := range loaded {
			s.alerts[stableID] = state
		}
		s.mu.Unlock()
	}

	type restoredSpeedRetry struct {
		stableIDs []string
		config    speedtest.TestConfig
		dueAt     time.Time
	}
	loadNow := time.Now()
	retryStateMigrated := false
	normalizedRetries := make([]restoredSpeedRetry, 0, len(stateFile.SpeedRetries))
	for _, persisted := range stateFile.SpeedRetries {
		rawKind := strings.TrimSpace(persisted.Kind)
		if rawKind != "" && rawKind != speedRetryKindConfirmation && rawKind != legacySpeedRetryKindLowSpeed && rawKind != legacySpeedRetryKindDeadline {
			continue
		}
		dueAt := persisted.DueAt
		if dueAt.IsZero() {
			dueAt = loadNow.Add(s.configuredSpeedRetryDelay())
		}
		if rawKind == legacySpeedRetryKindDeadline {
			// Old deadline retries were due five minutes after the failed run.
			// Extending their stored due time keeps the new confirmation at thirty
			// minutes from the original run and the immediate rewrite makes the
			// migration idempotent across restarts.
			if !persisted.DueAt.IsZero() {
				dueAt = dueAt.Add(speedConfirmationRetryDelay - legacyDeadlineRetryDelay)
			}
		}
		if rawKind != speedRetryKindConfirmation {
			retryStateMigrated = true
		}
		normalizedRetries = append(normalizedRetries, restoredSpeedRetry{
			stableIDs: persisted.StableIDs,
			config:    persisted.Config,
			dueAt:     dueAt,
		})
	}
	sort.SliceStable(normalizedRetries, func(i, j int) bool {
		return normalizedRetries[i].dueAt.Before(normalizedRetries[j].dueAt)
	})

	restoredRetries := 0
	s.speedRetryMu.Lock()
	for _, persisted := range normalizedRetries {
		var ids []string
		for _, rawStableID := range persisted.stableIDs {
			stableID := strings.TrimSpace(rawStableID)
			key := speedRetryKey{Kind: speedRetryKindConfirmation, StableID: stableID}
			if stableID == "" || !active[stableID] || s.speedRetryPending[key] {
				continue
			}
			s.speedRetryPending[key] = true
			ids = append(ids, stableID)
		}
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		s.speedRetrySeq++
		s.speedRetryEntries[s.speedRetrySeq] = pendingSpeedRetry{
			Kind: speedRetryKindConfirmation,
			Request: speedtest.RunRequest{
				ProxyIDs: append([]string(nil), ids...),
				Config:   persisted.config,
			},
			StableIDs: append([]string(nil), ids...),
			DueAt:     persisted.dueAt,
		}
		restoredRetries += len(ids)
	}
	s.speedRetryMu.Unlock()
	if retryStateMigrated {
		if err := s.saveAlertState(); err != nil {
			return fmt.Errorf("persist migrated speed-test retry state: %w", err)
		}
	}

	restored := 0
	for stableID, state := range loaded {
		if !state.WasDown || state.RecoveryPending {
			continue
		}
		switch nodeAlertStatus(state) {
		case checker.AvailabilityStateOffline:
			if !state.DownSince.IsZero() && s.proxyChecker.RestoreOfflineStatus(stableID, state.DownSince, state.HostCheck, state.PingCheck, state.Failure) {
				restored++
			}
		case checker.AvailabilityStateProxyFailure:
			if !state.ProxyFailureSince.IsZero() && s.proxyChecker.RestoreProxyFailureStatus(stableID, state.ProxyFailureSince, state.HostCheck, state.PingCheck, state.Failure) {
				restored++
			}
		}
	}

	logger.Info("Loaded Telegram state: %d alert nodes, %d availability issue statuses, %d pending speed-test retries", len(loaded), restored, restoredRetries)
	return nil
}

func (s *Service) saveAlertState() error {
	s.stateSaveMu.Lock()
	defer s.stateSaveMu.Unlock()
	if s.alertStatePath == "" {
		return nil
	}

	// A nil checker is a supported state for this service, as the guards in
	// NotifySpeedTest and the status formatter already assume. Without one there
	// is no active set to filter against, so nothing is persisted.
	active := make(map[string]bool)
	if s.proxyChecker != nil {
		for _, proxy := range s.proxyChecker.GetProxies() {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			if s.proxyChecker.MonitoringEnabled(proxy.StableID) {
				active[proxy.StableID] = true
			}
		}
	}

	nodes := make(map[string]persistedNodeAlertState)
	s.mu.RLock()
	for stableID, state := range s.alerts {
		if !active[stableID] {
			continue
		}
		if !state.WasDown && state.FailCount <= 0 && state.DownSince.IsZero() && state.ProxyFailureSince.IsZero() && state.LastAlert.IsZero() {
			continue
		}
		nodes[stableID] = persistedNodeAlertStateFrom(state)
	}
	s.mu.RUnlock()

	var retries []persistedSpeedRetry
	s.speedRetryMu.Lock()
	for _, entry := range s.speedRetryEntries {
		var ids []string
		for _, stableID := range entry.StableIDs {
			if active[stableID] {
				ids = append(ids, stableID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		retries = append(retries, persistedSpeedRetry{
			Kind:      normalizedSpeedRetryKind(entry.Kind),
			StableIDs: ids,
			Config:    entry.Request.Config,
			DueAt:     entry.DueAt,
		})
	}
	s.speedRetryMu.Unlock()
	sort.Slice(retries, func(i, j int) bool { return retries[i].DueAt.Before(retries[j].DueAt) })

	if len(nodes) == 0 && len(retries) == 0 {
		if err := os.Remove(s.alertStatePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(s.alertStatePath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(nodeAlertStateFile{
		Version:      1,
		UpdatedAt:    time.Now(),
		Nodes:        nodes,
		SpeedRetries: retries,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.alertStatePath, data, 0600)
}

func persistedNodeAlertStateFrom(state nodeAlertState) persistedNodeAlertState {
	persisted := persistedNodeAlertState{
		FailCount:         state.FailCount,
		WasDown:           state.WasDown,
		Status:            string(nodeAlertStatus(state)),
		DownSince:         state.DownSince,
		ProxyFailureSince: state.ProxyFailureSince,
		LastAlert:         state.LastAlert,
		AlertCount:        state.AlertCount,
		NextAlert:         state.NextAlert,
		LastDiagnostics:   state.LastDiagnostics,
		RecoveryPending:   state.RecoveryPending,
		RecoveredAt:       state.RecoveredAt,
		RecoveryLatency:   state.RecoveryLatency.Milliseconds(),
		FailureCode:       state.Failure.Code,
		FailureSummary:    state.Failure.Summary,
		FailureDetail:     state.Failure.Detail,
	}
	if state.HostCheck.Checked {
		hostCheck := persistedHostCheckFrom(state.HostCheck)
		persisted.HostCheck = &hostCheck
	}
	if state.PingCheck.Checked {
		pingCheck := persistedPingCheckFrom(state.PingCheck)
		persisted.PingCheck = &pingCheck
	}
	return persisted
}

func (p persistedNodeAlertState) toNodeAlertState() nodeAlertState {
	state := nodeAlertState{
		FailCount:         p.FailCount,
		WasDown:           p.WasDown,
		Status:            checker.AvailabilityState(strings.TrimSpace(p.Status)),
		DownSince:         p.DownSince,
		ProxyFailureSince: p.ProxyFailureSince,
		LastAlert:         p.LastAlert,
		AlertCount:        p.AlertCount,
		NextAlert:         p.NextAlert,
		LastDiagnostics:   p.LastDiagnostics,
		RecoveryPending:   p.RecoveryPending,
		RecoveredAt:       p.RecoveredAt,
		RecoveryLatency:   time.Duration(p.RecoveryLatency) * time.Millisecond,
		Failure: checker.FailureDetails{
			Code:    p.FailureCode,
			Summary: p.FailureSummary,
			Detail:  p.FailureDetail,
		},
	}
	switch state.Status {
	case checker.AvailabilityStateOnline, checker.AvailabilityStateProxyFailure, checker.AvailabilityStateOffline:
	default:
		if state.WasDown {
			state.Status = checker.AvailabilityStateOffline
		} else {
			state.Status = ""
		}
	}
	if state.Failure.Code != "" && state.Failure.Summary == "" {
		state.Failure.Summary = checker.FailureSummary(state.Failure.Code)
	}
	if p.HostCheck != nil {
		state.HostCheck = p.HostCheck.toHostCheckDetails()
	}
	if p.PingCheck != nil {
		state.PingCheck = p.PingCheck.toPingCheckDetails()
	}
	if state.AlertCount <= 0 && !state.LastAlert.IsZero() {
		state.AlertCount = 1
	}
	state.LastDiagnostics = latestDiagnosticsAt(state.LastDiagnostics, state.HostCheck, state.PingCheck)
	return state
}

func persistedHostCheckFrom(hostCheck checker.HostCheckDetails) persistedHostCheck {
	return persistedHostCheck{
		Checked:   hostCheck.Checked,
		Online:    hostCheck.Online,
		LatencyMs: hostCheck.Latency.Milliseconds(),
		CheckedAt: hostCheck.CheckedAt,
		Target:    hostCheck.Target,
		Error:     hostCheck.Error,
	}
}

func (p persistedHostCheck) toHostCheckDetails() checker.HostCheckDetails {
	return checker.HostCheckDetails{
		Checked:   p.Checked,
		Online:    p.Online,
		Latency:   time.Duration(p.LatencyMs) * time.Millisecond,
		CheckedAt: p.CheckedAt,
		Target:    p.Target,
		Error:     p.Error,
	}
}

func persistedPingCheckFrom(pingCheck checker.PingCheckDetails) persistedPingCheck {
	return persistedPingCheck{
		Checked:   pingCheck.Checked,
		Online:    pingCheck.Online,
		LatencyMs: pingCheck.Latency.Milliseconds(),
		CheckedAt: pingCheck.CheckedAt,
		Target:    pingCheck.Target,
		Error:     pingCheck.Error,
	}
}

func (p persistedPingCheck) toPingCheckDetails() checker.PingCheckDetails {
	return checker.PingCheckDetails{
		Checked:   p.Checked,
		Online:    p.Online,
		Latency:   time.Duration(p.LatencyMs) * time.Millisecond,
		CheckedAt: p.CheckedAt,
		Target:    p.Target,
		Error:     p.Error,
	}
}

func (s *Service) saveConfig(cfg Config) error {
	return s.writeConfig(cfg)
}

func (s *Service) saveEditableConfig(cfg Config) error {
	cfg.BotToken = ""
	cfg.ChatID = ""
	cfg.MessageThreadID = 0
	cfg.AdminUserIDs = nil
	return s.writeConfig(cfg)
}

func (s *Service) writeConfig(cfg Config) error {
	if s.statePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.statePath, data, 0600)
}

func (s *Service) setConfig(cfg Config) {
	cfg.Normalize()
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	if s.speedManager != nil {
		s.speedManager.SetLowSpeedThresholdMbps(cfg.LowSpeedThresholdMbps)
	}
}
