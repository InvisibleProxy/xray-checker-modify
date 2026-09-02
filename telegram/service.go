// Package telegram is the bot-facing side of the checker: it turns availability
// and speed-test results into alerts, and exposes a small operator interface as
// bot commands and inline keyboards.
//
// The package is split by concern rather than by layer, because almost every
// change starts from "which message is wrong":
//
//	service.go       Service, its dependencies and lifecycle
//	config.go        Config/AdminConfig, defaults, env overrides
//	state.go         node alert state and its on-disk representation
//	nodealerts.go    availability alerts: down, reminders, recovery
//	speedalerts.go   speed-test reports and their confirmation retries
//	bot.go           update polling, commands, callbacks, authorization
//	transport.go     Telegram API wire types and send/edit calls
//	nodes.go         node lookup, mute sets and result filtering
//	format*.go       message rendering, one file per family of messages
//	markup.go        inline keyboards
//	text.go          escaping, truncation and unit formatting
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xray-checker/agentautomation"
	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/speedtest"
)

type Service struct {
	proxyChecker   *checker.ProxyChecker
	speedManager   *speedtest.Manager
	startPort      int
	statePath      string
	alertStatePath string

	mu           sync.RWMutex
	nodeNotifyMu sync.Mutex
	stateSaveMu  sync.Mutex
	config       Config
	alerts       map[string]nodeAlertState
	menuMessages map[string]int
	statusCheck  bool
	lastAlertRun time.Time
	lastUpdateID int
	richMessages int

	stopCh   chan struct{}
	stopOnce sync.Once

	speedRetryMu        sync.Mutex
	speedRetryWG        sync.WaitGroup
	speedRetryPending   map[speedRetryKey]bool
	speedRetryTimers    map[uint64]*time.Timer
	speedRetryEntries   map[uint64]pendingSpeedRetry
	speedRetrySeq       uint64
	speedRetryDelay     time.Duration
	speedRetryBusy      time.Duration
	speedRunFunc        func(speedtest.RunRequest, string) error
	speedReportSendFunc func(string, int, formattedMessage)
	availabilityCheck   func([]string) error
	nodeAlertSendFunc   func(Config, formattedMessage) error
	projectMaintenance  atomic.Bool
	speedDiagnostics    SpeedDiagnosticAutomation
}

type SpeedDiagnosticAutomation interface {
	Enabled() bool
	AlertWait() time.Duration
	StartSpeedDiagnostics(speedtest.RunReport, float64) map[string]agentautomation.Handle
	Annotations(map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic
	Await(context.Context, map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic
}

func NewService(statePath string, proxyChecker *checker.ProxyChecker, speedManager *speedtest.Manager, startPort int) *Service {
	alertStatePath := ""
	if statePath != "" {
		alertStatePath = filepath.Join(filepath.Dir(statePath), "node_alert_state.json")
	}

	return &Service{
		proxyChecker:      proxyChecker,
		speedManager:      speedManager,
		startPort:         startPort,
		statePath:         statePath,
		alertStatePath:    alertStatePath,
		config:            DefaultConfig(),
		alerts:            make(map[string]nodeAlertState),
		menuMessages:      make(map[string]int),
		stopCh:            make(chan struct{}),
		speedRetryPending: make(map[speedRetryKey]bool),
		speedRetryTimers:  make(map[uint64]*time.Timer),
		speedRetryEntries: make(map[uint64]pendingSpeedRetry),
		speedRetryDelay:   speedConfirmationRetryDelay,
		speedRetryBusy:    speedConfirmationRetryBusyDelay,
	}
}

func (s *Service) SetAvailabilityCheckFunc(check func([]string) error) {
	s.availabilityCheck = check
}

func (s *Service) SetSpeedDiagnosticAutomation(automation SpeedDiagnosticAutomation) {
	s.speedDiagnostics = automation
}

func (s *Service) SetProjectMaintenance(enabled bool) {
	s.projectMaintenance.Store(enabled)
}

func (s *Service) ProjectMaintenanceEnabled() bool {
	return s.projectMaintenance.Load()
}

func (s *Service) Load() error {
	cfg := DefaultConfig()
	if s.statePath == "" {
		applyEnvDefaults(&cfg)
		disableInvalidEnabledConfig(&cfg)
		s.setConfig(cfg)
		s.loadAlertStateWithWarn()
		return nil
	}

	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvDefaults(&cfg)
			disableInvalidEnabledConfig(&cfg)
			s.setConfig(cfg)
			s.loadAlertStateWithWarn()
			return nil
		}
		return err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	applyLegacyAlertRepeat(data, &cfg)
	applyEnvOverrides(&cfg)
	cfg.Normalize()
	disableInvalidEnabledConfig(&cfg)
	s.setConfig(cfg)
	s.loadAlertStateWithWarn()
	return nil
}

func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *Service) AdminConfig() AdminConfig {
	cfg := s.Config()
	mutedNodeIDs := s.activeMutedNodeIDs(cfg.MutedNodeIDs)
	mutedSpeedNodeIDs := s.activeMutedNodeIDs(cfg.MutedSpeedNodeIDs)
	mutedAlertNodeIDs := s.activeMutedNodeIDs(cfg.MutedAlertNodeIDs)
	return AdminConfig{
		Enabled:                      cfg.Enabled,
		CommandPollingEnabled:        cfg.CommandPollingEnabled,
		SpeedReportsEnabled:          cfg.SpeedReportsEnabled,
		SpeedReportMode:              cfg.SpeedReportMode,
		LowSpeedThresholdMbps:        cfg.LowSpeedThresholdMbps,
		SpeedReportLimit:             cfg.SpeedReportLimit,
		NodeAlertsEnabled:            cfg.NodeAlertsEnabled,
		AlertCheckMinutes:            cfg.AlertCheckMinutes,
		AlertAfterFailures:           cfg.AlertAfterFailures,
		AlertRepeatMinutes:           cfg.AlertRepeatMinutes,
		AlertDiagnosticsMinutes:      cfg.AlertDiagnosticsMinutes,
		AlertReminderScheduleMinutes: append([]int(nil), cfg.AlertReminderScheduleMinutes...),
		AlertMaxReminderMinutes:      cfg.AlertMaxReminderMinutes,
		GroupOfflineReminders:        cfg.GroupOfflineReminders,
		NotifyRecovery:               cfg.NotifyRecovery,
		MutedNodeIDs:                 mutedNodeIDs,
		MutedSpeedNodeIDs:            mutedSpeedNodeIDs,
		MutedAlertNodeIDs:            mutedAlertNodeIDs,
		BotTokenConfigured:           cfg.BotToken != "",
		ChatConfigured:               cfg.ChatID != "",
		MessageThreadConfigured:      cfg.MessageThreadID > 0,
		AdminUserCount:               len(cfg.AdminUserIDs),
	}
}

func (s *Service) UpdateAdminConfig(input AdminConfig) error {
	cfg := s.Config()
	cfg.Enabled = input.Enabled
	cfg.CommandPollingEnabled = input.CommandPollingEnabled
	cfg.SpeedReportsEnabled = input.SpeedReportsEnabled
	cfg.SpeedReportMode = input.SpeedReportMode
	cfg.LowSpeedThresholdMbps = input.LowSpeedThresholdMbps
	cfg.SpeedReportLimit = input.SpeedReportLimit
	cfg.NodeAlertsEnabled = input.NodeAlertsEnabled
	cfg.AlertCheckMinutes = input.AlertCheckMinutes
	cfg.AlertAfterFailures = input.AlertAfterFailures
	cfg.AlertRepeatMinutes = input.AlertRepeatMinutes
	cfg.AlertDiagnosticsMinutes = input.AlertDiagnosticsMinutes
	cfg.AlertReminderScheduleMinutes = append([]int(nil), input.AlertReminderScheduleMinutes...)
	cfg.AlertMaxReminderMinutes = input.AlertMaxReminderMinutes
	cfg.GroupOfflineReminders = input.GroupOfflineReminders
	cfg.NotifyRecovery = input.NotifyRecovery
	cfg.MutedNodeIDs = s.activeMutedNodeIDs(input.MutedNodeIDs)
	cfg.MutedSpeedNodeIDs = s.activeMutedNodeIDs(input.MutedSpeedNodeIDs)
	cfg.MutedAlertNodeIDs = s.activeMutedNodeIDs(input.MutedAlertNodeIDs)
	cfg.Normalize()
	if cfg.Enabled && cfg.BotToken == "" {
		return fmt.Errorf("bot token is required when Telegram is enabled; set TELEGRAM_BOT_TOKEN")
	}

	if err := s.saveEditableConfig(cfg); err != nil {
		return err
	}
	s.setConfig(cfg)
	return nil
}

func (s *Service) PruneInactiveMutedNodes() error {
	cfg := s.Config()
	currentAll := normalizeNodeIDs(cfg.MutedNodeIDs)
	currentSpeed := normalizeNodeIDs(cfg.MutedSpeedNodeIDs)
	currentAlert := normalizeNodeIDs(cfg.MutedAlertNodeIDs)
	prunedAll := s.activeMutedNodeIDs(currentAll)
	prunedSpeed := s.activeMutedNodeIDs(currentSpeed)
	prunedAlert := s.activeMutedNodeIDs(currentAlert)
	configChanged := !sameNodeIDs(currentAll, prunedAll) || !sameNodeIDs(currentSpeed, prunedSpeed) || !sameNodeIDs(currentAlert, prunedAlert)
	if configChanged {
		cfg.MutedNodeIDs = prunedAll
		cfg.MutedSpeedNodeIDs = prunedSpeed
		cfg.MutedAlertNodeIDs = prunedAlert
		if err := s.saveEditableConfig(cfg); err != nil {
			return err
		}
		s.setConfig(cfg)
	}

	active := make(map[string]bool)
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy != nil {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			if s.proxyChecker.MonitoringEnabled(proxy.StableID) {
				active[proxy.StableID] = true
			}
		}
	}
	var inactiveRetries []string
	s.speedRetryMu.Lock()
	for key := range s.speedRetryPending {
		if !active[key.StableID] {
			inactiveRetries = append(inactiveRetries, key.StableID)
		}
	}
	s.speedRetryMu.Unlock()
	if len(inactiveRetries) > 0 {
		s.clearSpeedRetry(speedRetryKindConfirmation, inactiveRetries)
		if err := s.saveAlertState(); err != nil {
			return err
		}
	}
	return nil
}

// ClearMonitoringState removes alert/recovery counters and pending speed-test
// confirmations when an operator places a node into maintenance. Telegram mute
// preferences are intentionally preserved for when monitoring resumes.
func (s *Service) ClearMonitoringState(stableID string) error {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return fmt.Errorf("stableId is required")
	}
	s.mu.Lock()
	_, hadAlert := s.alerts[stableID]
	delete(s.alerts, stableID)
	s.mu.Unlock()
	retryChanged := s.clearSpeedRetry(speedRetryKindConfirmation, []string{stableID})
	if !hadAlert && !retryChanged {
		return nil
	}
	return s.saveAlertState()
}

// ClearAllMonitoringState closes the project maintenance boundary. Telegram
// configuration and mute preferences remain intact, while alert counters and
// persisted confirmation retries are discarded as stale operational state.
func (s *Service) ClearAllMonitoringState() error {
	s.mu.Lock()
	s.alerts = make(map[string]nodeAlertState)
	s.lastAlertRun = time.Time{}
	s.mu.Unlock()

	s.speedRetryMu.Lock()
	for _, timer := range s.speedRetryTimers {
		if timer.Stop() {
			s.speedRetryWG.Done()
		}
	}
	s.speedRetryPending = make(map[speedRetryKey]bool)
	s.speedRetryTimers = make(map[uint64]*time.Timer)
	s.speedRetryEntries = make(map[uint64]pendingSpeedRetry)
	s.speedRetryMu.Unlock()
	return s.saveAlertState()
}

func (s *Service) UpdateConfig(cfg Config) error {
	cfg.Normalize()
	if cfg.Enabled && cfg.BotToken == "" {
		return fmt.Errorf("bot token is required when Telegram is enabled")
	}

	if err := s.saveConfig(cfg); err != nil {
		return err
	}
	s.setConfig(cfg)
	return nil
}

func (s *Service) Start() {
	s.startRestoredSpeedRetries()
	go s.pollingLoop()
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.speedRetryMu.Lock()
		for _, timer := range s.speedRetryTimers {
			if timer.Stop() {
				s.speedRetryWG.Done()
			}
		}
		s.speedRetryTimers = make(map[uint64]*time.Timer)
		s.speedRetryMu.Unlock()
		if err := s.saveAlertState(); err != nil {
			logger.Warn("Failed to save pending speed-test retries on shutdown: %v", err)
		}
		s.speedRetryWG.Wait()
	})
}

func (s *Service) SendTestMessage() error {
	cfg := s.Config()
	if !cfg.Enabled {
		return fmt.Errorf("Telegram is disabled")
	}
	if cfg.ChatID == "" {
		return fmt.Errorf("chat ID is required for test messages")
	}
	content := formattedMessage{
		HTML:     "✅ <b>Telegram подключён</b>\n\nТестовое сообщение доставлено. Уведомления настроены.",
		RichHTML: "<h2>✅ Telegram подключён</h2><p>Тестовое сообщение доставлено. Уведомления настроены.</p>",
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	_, err := s.sendFormattedToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, content, "")
	return err
}

func (s *Service) beginStatusCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusCheck {
		return false
	}
	s.statusCheck = true
	return true
}

func (s *Service) endStatusCheck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCheck = false
}

func (s *Service) wait(duration time.Duration) bool {
	select {
	case <-time.After(duration):
		return true
	case <-s.stopCh:
		return false
	}
}
