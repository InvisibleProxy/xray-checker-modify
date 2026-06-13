package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/models"
	"xray-checker/speedtest"
)

const (
	defaultTimeoutSec                  = 20
	defaultSpeedReportLimit            = 10
	defaultAlertCheckMinutes           = 5
	minAlertAfterFailures              = 2
	defaultAlertAfterFailures          = minAlertAfterFailures
	defaultAlertDiagnosticsMinutes     = 60
	defaultAlertMaxReminderMinutes     = 1440
	defaultAlertReminderScheduleString = "15,60,180,360,720"
	maxSpeedReportLimit                = 50
	menuSpeedButtonLimit               = 8
	maxDiagnosticsRefreshConcurrency   = 4
)

var defaultAlertReminderScheduleMinutes = parseMinuteSchedule(defaultAlertReminderScheduleString)

type Config struct {
	Enabled                      bool     `json:"enabled"`
	BotToken                     string   `json:"botToken"`
	ChatID                       string   `json:"chatId"`
	MessageThreadID              int      `json:"messageThreadId"`
	AdminUserIDs                 []int64  `json:"adminUserIds"`
	CommandPollingEnabled        bool     `json:"commandPollingEnabled"`
	SpeedReportsEnabled          bool     `json:"speedReportsEnabled"`
	SpeedReportMode              string   `json:"speedReportMode"`
	LowSpeedThresholdMbps        float64  `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit             int      `json:"speedReportLimit"`
	NodeAlertsEnabled            bool     `json:"nodeAlertsEnabled"`
	AlertCheckMinutes            int      `json:"alertCheckMinutes"`
	AlertAfterFailures           int      `json:"alertAfterFailures"`
	AlertRepeatMinutes           int      `json:"alertRepeatMinutes,omitempty"`
	AlertDiagnosticsMinutes      int      `json:"alertDiagnosticsMinutes"`
	AlertReminderScheduleMinutes []int    `json:"alertReminderScheduleMinutes"`
	AlertMaxReminderMinutes      int      `json:"alertMaxReminderMinutes"`
	GroupOfflineReminders        bool     `json:"groupOfflineReminders"`
	NotifyRecovery               bool     `json:"notifyRecovery"`
	MutedNodeIDs                 []string `json:"mutedNodeIds,omitempty"`
	TimeoutSec                   int      `json:"timeoutSec"`
}

type AdminConfig struct {
	Enabled                      bool     `json:"enabled"`
	CommandPollingEnabled        bool     `json:"commandPollingEnabled"`
	SpeedReportsEnabled          bool     `json:"speedReportsEnabled"`
	SpeedReportMode              string   `json:"speedReportMode"`
	LowSpeedThresholdMbps        float64  `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit             int      `json:"speedReportLimit"`
	NodeAlertsEnabled            bool     `json:"nodeAlertsEnabled"`
	AlertCheckMinutes            int      `json:"alertCheckMinutes"`
	AlertAfterFailures           int      `json:"alertAfterFailures"`
	AlertRepeatMinutes           int      `json:"alertRepeatMinutes,omitempty"`
	AlertDiagnosticsMinutes      int      `json:"alertDiagnosticsMinutes"`
	AlertReminderScheduleMinutes []int    `json:"alertReminderScheduleMinutes"`
	AlertMaxReminderMinutes      int      `json:"alertMaxReminderMinutes"`
	GroupOfflineReminders        bool     `json:"groupOfflineReminders"`
	NotifyRecovery               bool     `json:"notifyRecovery"`
	MutedNodeIDs                 []string `json:"mutedNodeIds,omitempty"`
	BotTokenConfigured           bool     `json:"botTokenConfigured"`
	ChatConfigured               bool     `json:"chatConfigured"`
	MessageThreadConfigured      bool     `json:"messageThreadConfigured"`
	AdminUserCount               int      `json:"adminUserCount"`
}

func DefaultConfig() Config {
	return Config{
		CommandPollingEnabled:        true,
		SpeedReportsEnabled:          true,
		SpeedReportMode:              "always",
		SpeedReportLimit:             defaultSpeedReportLimit,
		NodeAlertsEnabled:            true,
		AlertCheckMinutes:            defaultAlertCheckMinutes,
		AlertAfterFailures:           defaultAlertAfterFailures,
		AlertDiagnosticsMinutes:      defaultAlertDiagnosticsMinutes,
		AlertReminderScheduleMinutes: append([]int(nil), defaultAlertReminderScheduleMinutes...),
		AlertMaxReminderMinutes:      defaultAlertMaxReminderMinutes,
		GroupOfflineReminders:        true,
		NotifyRecovery:               true,
		TimeoutSec:                   defaultTimeoutSec,
	}
}

func (c *Config) Normalize() {
	c.BotToken = strings.TrimSpace(c.BotToken)
	c.ChatID = strings.TrimSpace(c.ChatID)

	if c.SpeedReportMode == "" {
		c.SpeedReportMode = "always"
	}
	if c.SpeedReportMode != "always" && c.SpeedReportMode != "issues" && c.SpeedReportMode != "disabled" {
		c.SpeedReportMode = "always"
	}
	if c.SpeedReportLimit <= 0 {
		c.SpeedReportLimit = defaultSpeedReportLimit
	}
	if c.SpeedReportLimit > maxSpeedReportLimit {
		c.SpeedReportLimit = maxSpeedReportLimit
	}
	if c.AlertCheckMinutes <= 0 {
		c.AlertCheckMinutes = defaultAlertCheckMinutes
	}
	if c.AlertAfterFailures < minAlertAfterFailures {
		c.AlertAfterFailures = defaultAlertAfterFailures
	}
	if c.AlertDiagnosticsMinutes <= 0 {
		if c.AlertRepeatMinutes > 0 {
			c.AlertDiagnosticsMinutes = c.AlertRepeatMinutes
		} else {
			c.AlertDiagnosticsMinutes = defaultAlertDiagnosticsMinutes
		}
	}
	c.AlertReminderScheduleMinutes = normalizeMinuteSchedule(c.AlertReminderScheduleMinutes)
	if len(c.AlertReminderScheduleMinutes) == 0 {
		c.AlertReminderScheduleMinutes = append([]int(nil), defaultAlertReminderScheduleMinutes...)
	}
	if c.AlertMaxReminderMinutes <= 0 {
		if c.AlertRepeatMinutes > 0 {
			c.AlertMaxReminderMinutes = c.AlertRepeatMinutes
		} else {
			c.AlertMaxReminderMinutes = defaultAlertMaxReminderMinutes
		}
	}
	if c.AlertMaxReminderMinutes < c.AlertReminderScheduleMinutes[len(c.AlertReminderScheduleMinutes)-1] {
		c.AlertMaxReminderMinutes = c.AlertReminderScheduleMinutes[len(c.AlertReminderScheduleMinutes)-1]
	}
	c.MutedNodeIDs = normalizeNodeIDs(c.MutedNodeIDs)
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = defaultTimeoutSec
	}
}

type Service struct {
	proxyChecker   *checker.ProxyChecker
	speedManager   *speedtest.Manager
	startPort      int
	statePath      string
	alertStatePath string

	mu           sync.RWMutex
	config       Config
	alerts       map[string]nodeAlertState
	menuMessages map[string]int
	statusCheck  bool
	lastAlertRun time.Time
	lastUpdateID int

	stopCh   chan struct{}
	stopOnce sync.Once
}

type nodeAlertState struct {
	FailCount       int
	WasDown         bool
	DownSince       time.Time
	LastAlert       time.Time
	AlertCount      int
	NextAlert       time.Time
	LastDiagnostics time.Time
	HostCheck       checker.HostCheckDetails
	PingCheck       checker.PingCheckDetails
}

type nodeAlertStateFile struct {
	Version   int                                `json:"version"`
	UpdatedAt time.Time                          `json:"updatedAt"`
	Nodes     map[string]persistedNodeAlertState `json:"nodes"`
}

type persistedNodeAlertState struct {
	FailCount       int                 `json:"failCount"`
	WasDown         bool                `json:"wasDown"`
	DownSince       time.Time           `json:"downSince"`
	LastAlert       time.Time           `json:"lastAlert"`
	AlertCount      int                 `json:"alertCount"`
	NextAlert       time.Time           `json:"nextAlert"`
	LastDiagnostics time.Time           `json:"lastDiagnostics"`
	HostCheck       *persistedHostCheck `json:"hostCheck,omitempty"`
	PingCheck       *persistedPingCheck `json:"pingCheck,omitempty"`
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

type proxyCandidate struct {
	Proxy   *models.ProxyConfig
	Latency time.Duration
}

type nodeDownAlert struct {
	Proxy     *models.ProxyConfig
	State     nodeAlertState
	NextAfter time.Duration
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

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

type update struct {
	UpdateID      int            `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

type message struct {
	MessageID       int    `json:"message_id"`
	MessageThreadID int    `json:"message_thread_id"`
	Text            string `json:"text"`
	Chat            chat   `json:"chat"`
	From            *user  `json:"from"`
}

type chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type user struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type callbackQuery struct {
	ID      string   `json:"id"`
	From    *user    `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

func NewService(statePath string, proxyChecker *checker.ProxyChecker, speedManager *speedtest.Manager, startPort int) *Service {
	alertStatePath := ""
	if statePath != "" {
		alertStatePath = filepath.Join(filepath.Dir(statePath), "node_alert_state.json")
	}

	return &Service{
		proxyChecker:   proxyChecker,
		speedManager:   speedManager,
		startPort:      startPort,
		statePath:      statePath,
		alertStatePath: alertStatePath,
		config:         DefaultConfig(),
		alerts:         make(map[string]nodeAlertState),
		menuMessages:   make(map[string]int),
		stopCh:         make(chan struct{}),
	}
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
	current := normalizeNodeIDs(cfg.MutedNodeIDs)
	pruned := s.activeMutedNodeIDs(current)
	if strings.Join(current, "\x00") == strings.Join(pruned, "\x00") {
		return nil
	}

	cfg.MutedNodeIDs = pruned
	if err := s.saveEditableConfig(cfg); err != nil {
		return err
	}
	s.setConfig(cfg)
	return nil
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
	go s.pollingLoop()
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
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
	return s.sendText(context.Background(), "Xray Checker Telegram notifications are configured.")
}

func (s *Service) NotifySpeedTest(report speedtest.RunReport) {
	cfg := s.Config()
	report = filterMutedRunReport(report, cfg)
	failed, slow, issuesOnly, shouldSend := speedReportDecision(report, cfg)
	if !shouldSend {
		return
	}

	text := s.formatSpeedReport(report, cfg, failed, slow, issuesOnly)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	if _, err := s.sendHTMLToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, text, backToMenuMarkup()); err != nil {
		logger.Warn("Failed to send Telegram speed-test report: %v", err)
	}
}

func speedReportDecision(report speedtest.RunReport, cfg Config) (failed int, slow int, issuesOnly bool, shouldSend bool) {
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.SpeedReportsEnabled || cfg.SpeedReportMode == "disabled" {
		return 0, 0, false, false
	}

	results := filterMutedSpeedResults(report.Results, cfg)
	if len(results) == 0 {
		return 0, 0, false, false
	}

	failed, slow = countSpeedIssues(results, cfg.LowSpeedThresholdMbps)
	issuesOnly = report.Source == "schedule"
	if issuesOnly && failed == 0 && slow == 0 {
		return failed, slow, issuesOnly, false
	}
	if cfg.SpeedReportMode == "issues" && failed == 0 && slow == 0 {
		return failed, slow, issuesOnly, false
	}
	return failed, slow, issuesOnly, true
}

func (s *Service) NotifyNodeStatuses() {
	cfg := s.Config()
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.NodeAlertsEnabled {
		return
	}

	now := time.Now()
	if !s.shouldRunNodeAlertCheck(cfg, now) {
		return
	}

	proxies := s.proxyChecker.GetProxies()
	active := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		active[proxy.StableID] = true
	}
	muted := mutedNodeSet(cfg.MutedNodeIDs)

	stateChanged := false
	s.mu.Lock()
	for stableID := range s.alerts {
		if !active[stableID] {
			delete(s.alerts, stableID)
			stateChanged = true
		}
	}
	s.mu.Unlock()

	var recoveryMessages []string
	var refreshRequests []diagnosticsRefreshRequest
	var dueAlertProxies []*models.ProxyConfig
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		isMuted := muted[proxy.StableID]

		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		isDown := err != nil || !details.Online

		var shouldSendDownAlert bool
		var shouldRefreshDiagnostics bool
		s.mu.Lock()
		state := s.alerts[proxy.StableID]
		previous := state
		if isDown {
			state.FailCount++
			state.WasDown = true
			if details.HostCheck.Checked {
				state.HostCheck = details.HostCheck
			}
			if details.PingCheck.Checked {
				state.PingCheck = details.PingCheck
			}
			state.LastDiagnostics = latestDiagnosticsAt(state.LastDiagnostics, state.HostCheck, state.PingCheck)
			if state.DownSince.IsZero() {
				state.DownSince = details.DownSince
				if state.DownSince.IsZero() {
					state.DownSince = now
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
				recoveryMessages = append(recoveryMessages, formatNodeRecovery(proxy, details.Latency, state.DownSince, now))
			}
			state = nodeAlertState{}
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

		if isDown && shouldRefreshDiagnostics {
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
		if result.Details.Online {
			continue
		}
		if s.updateAlertDiagnostics(result.StableID, result.Details.HostCheck, result.Details.PingCheck) {
			stateChanged = true
		}
	}

	downAlerts := make([]nodeDownAlert, 0, len(dueAlertProxies))
	for _, proxy := range dueAlertProxies {
		if alert, ok := s.pendingNodeDownAlert(proxy, cfg, now); ok {
			downAlerts = append(downAlerts, alert)
		}
	}

	for _, text := range recoveryMessages {
		_ = s.sendNodeAlertMessage(cfg, text)
	}

	if cfg.GroupOfflineReminders && len(downAlerts) > 1 {
		if err := s.sendNodeAlertMessage(cfg, formatNodeDownGroup(downAlerts, now)); err == nil {
			if s.confirmNodeDownAlertsSent(downAlerts, time.Now(), cfg) {
				stateChanged = true
			}
		}
	} else {
		for _, alert := range downAlerts {
			if err := s.sendNodeAlertMessage(cfg, formatNodeDown(alert.Proxy, alert.State, now)); err == nil {
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
}

func (s *Service) sendNodeAlertMessage(cfg Config, text string) error {
	if text == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.sendHTMLToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, text, ""); err != nil {
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
	if err == nil && details.Online {
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
		if state == (nodeAlertState{}) || !state.WasDown {
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

func (s *Service) pollingLoop() {
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		cfg := s.Config()
		if !cfg.Enabled || !cfg.CommandPollingEnabled || cfg.BotToken == "" {
			if !s.wait(5 * time.Second) {
				return
			}
			continue
		}

		values := url.Values{}
		values.Set("timeout", "15")
		values.Set("offset", strconv.Itoa(s.nextUpdateOffset()))
		values.Set("allowed_updates", `["message","callback_query"]`)

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec+20)*time.Second)
		result, err := s.doAPI(ctx, "getUpdates", values)
		cancel()
		if err != nil {
			logger.Warn("Telegram polling failed: %v", err)
			if !s.wait(10 * time.Second) {
				return
			}
			continue
		}

		var updates []update
		if err := json.Unmarshal(result, &updates); err != nil {
			logger.Warn("Failed to parse Telegram updates: %v", err)
			continue
		}

		for _, upd := range updates {
			s.markUpdateSeen(upd.UpdateID)
			s.handleUpdate(upd)
		}
	}
}

func (s *Service) handleUpdate(upd update) {
	if upd.CallbackQuery != nil {
		s.handleCallback(upd.CallbackQuery)
		return
	}

	if upd.Message == nil {
		return
	}

	msg := upd.Message
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return
	}

	cfg := s.Config()
	cmd, args := parseCommand(text)
	if cmd == "id" {
		s.sendCommandReply(msg, formatIDReply(msg))
		return
	}

	if !s.isChatAllowed(msg, cfg) {
		return
	}

	switch cmd {
	case "help":
		s.sendCommandReplyWithMarkup(msg, s.formatHelp(cfg), backToMenuMarkup())
	case "start", "menu":
		s.sendMenu(msg)
	case "status", "statuses":
		s.sendCommandReplyWithMarkup(msg, s.formatStatus(), statusMarkup())
	case "speed", "speedresult", "speedhistory":
		s.sendCommandReplyWithMarkup(msg, s.formatSpeedHistory(strings.Join(args, " ")), backToMenuMarkup())
	case "speedtest":
		if !s.isAdmin(msg, cfg) {
			s.sendCommandReply(msg, "This command is admin-only.")
			return
		}
		s.handleSpeedTestCommand(msg, args)
	default:
		s.sendCommandReply(msg, "Unknown command. Use /help.")
	}
}

func (s *Service) handleCallback(cb *callbackQuery) {
	if cb == nil {
		return
	}
	cfg := s.Config()
	data := strings.TrimSpace(cb.Data)

	if data == "id" {
		s.answerCallback(cb.ID, "")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, formatIDReplyFor(cb.Message, cb.From), backToMenuMarkup())
		}
		return
	}

	if cb.Message == nil || !s.isChatAllowedFor(cb.Message.Chat.ID, cb.From, cfg) {
		s.answerCallback(cb.ID, "Нет доступа")
		return
	}

	switch {
	case data == "back_to_menu" || data == "menu" || data == "menu:refresh":
		s.answerCallback(cb.ID, "Обновлено")
		s.editMenuMessage(cb.Message, cb.From)
	case data == "status":
		s.answerCallback(cb.ID, "")
		s.editCommandMessage(cb.Message, s.formatStatus(), statusMarkup())
	case data == "status:refresh":
		s.handleStatusRefreshCallback(cb)
	case data == "issues":
		s.answerCallback(cb.ID, "")
		s.editCommandMessage(cb.Message, s.formatIssuesSummary(), backToMenuMarkup())
	case data == "nodes:list":
		s.answerCallback(cb.ID, "")
		s.editCommandMessage(cb.Message, s.formatNodeList(), s.nodeListMarkup())
	case data == "help":
		s.answerCallback(cb.ID, "")
		s.editCommandMessage(cb.Message, s.formatHelp(cfg), backToMenuMarkup())
	case data == "speed:list":
		s.answerCallback(cb.ID, "")
		s.editCommandMessage(cb.Message, s.formatRecentSpeedOverview(), s.speedHistoryMarkup())
	case strings.HasPrefix(data, "node:test:"):
		stableID := strings.TrimPrefix(data, "node:test:")
		s.handleNodeSpeedTestCallback(cb, stableID)
	case strings.HasPrefix(data, "node:"):
		s.answerCallback(cb.ID, "")
		stableID := strings.TrimPrefix(data, "node:")
		s.editCommandMessage(cb.Message, s.formatNodeDetails(stableID), s.nodeDetailMarkup(stableID, s.isAdminUser(cb.From, cfg)))
	case strings.HasPrefix(data, "speed:"):
		s.answerCallback(cb.ID, "")
		query := strings.TrimPrefix(data, "speed:")
		s.editCommandMessage(cb.Message, s.formatSpeedHistory(query), backToMenuMarkup())
	case data == "speedtest:online":
		s.handleSpeedTestCallback(cb, true)
	case data == "speedtest:all":
		s.handleSpeedTestCallback(cb, false)
	default:
		s.answerCallback(cb.ID, "Неизвестное действие")
	}
}

func (s *Service) handleNodeSpeedTestCallback(cb *callbackQuery, stableID string) {
	cfg := s.Config()
	if !s.isAdminUser(cb.From, cfg) {
		s.answerCallback(cb.ID, "Только для администратора")
		return
	}
	proxy, matches := s.findProxy(stableID)
	if proxy == nil {
		s.answerCallback(cb.ID, "Нода не найдена")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, formatProxySearchMiss(matches), s.nodeListMarkup())
		}
		return
	}

	req := speedtest.RunRequest{
		ProxyIDs:   []string{proxy.StableID},
		OnlyOnline: false,
	}
	if err := s.speedManager.Run(req, "telegram"); err != nil {
		s.answerCallback(cb.ID, "Не запущено")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())), s.nodeDetailMarkup(proxy.StableID, true))
		}
		return
	}

	s.answerCallback(cb.ID, "Speed-test запущен")
	if cb.Message != nil {
		s.editCommandMessage(cb.Message, fmt.Sprintf("<b>Speed-test запущен</b>\n\nНода: <b>%s</b>\nОтчет будет отправлен после завершения проверки.", htmlEscape(proxy.Name)), s.nodeDetailMarkup(proxy.StableID, true))
	}
}

func (s *Service) handleSpeedTestCallback(cb *callbackQuery, onlyOnline bool) {
	cfg := s.Config()
	if !s.isAdminUser(cb.From, cfg) {
		s.answerCallback(cb.ID, "Только для администратора")
		return
	}
	req := speedtest.RunRequest{OnlyOnline: onlyOnline}
	if err := s.speedManager.Run(req, "telegram"); err != nil {
		s.answerCallback(cb.ID, "Не запущено")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())), backToMenuMarkup())
		}
		return
	}
	s.answerCallback(cb.ID, "Speed test запущен")
	if cb.Message != nil {
		s.editCommandMessage(cb.Message, "<b>Speed-test запущен</b>\n\nОтчет будет отправлен после завершения проверки.", backToMenuMarkup())
	}
}

func (s *Service) handleStatusRefreshCallback(cb *callbackQuery) {
	if !s.beginStatusCheck() {
		s.answerCallback(cb.ID, "Проверка уже идет")
		return
	}

	s.answerCallback(cb.ID, "Проверка запущена")
	if cb.Message != nil {
		s.editCommandMessage(cb.Message, formatStatusRefreshStarted(), statusRefreshMarkup())
	}

	msg := cb.Message
	go func() {
		defer s.endStatusCheck()

		offlineBefore := s.offlineStableIDs()
		s.proxyChecker.CheckAllProxies()
		s.refreshHostDiagnosticsForStillOffline(offlineBefore)

		if msg != nil {
			s.editCommandMessage(msg, s.formatStatus(), statusMarkup())
		}
	}()
}

func (s *Service) offlineStableIDs() map[string]bool {
	result := make(map[string]bool)
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err == nil && !details.Online {
			result[proxy.StableID] = true
		}
	}
	return result
}

func (s *Service) refreshHostDiagnosticsForStillOffline(stableIDs map[string]bool) {
	if len(stableIDs) == 0 {
		return
	}

	requests := make([]diagnosticsRefreshRequest, 0, len(stableIDs))
	for stableID := range stableIDs {
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(stableID)
		if err != nil || details.Online {
			continue
		}
		requests = append(requests, diagnosticsRefreshRequest{
			StableID: stableID,
			Name:     stableID,
		})
	}

	stateChanged := false
	for _, result := range s.refreshHostDiagnostics(requests) {
		if result.Err != nil {
			logger.Warn("Failed to refresh host diagnostics for %s: %v", result.Name, result.Err)
			continue
		}
		if result.Details.Online {
			continue
		}
		if s.updateAlertDiagnostics(result.StableID, result.Details.HostCheck, result.Details.PingCheck) {
			stateChanged = true
		}
	}
	if stateChanged {
		if err := s.saveAlertState(); err != nil {
			logger.Warn("Failed to save Telegram node alert state: %v", err)
		}
	}
}

func (s *Service) updateAlertDiagnostics(stableID string, hostCheck checker.HostCheckDetails, pingCheck checker.PingCheckDetails) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.alerts[stableID]
	if state == (nodeAlertState{}) {
		return false
	}

	previous := state
	if hostCheck.Checked {
		state.HostCheck = hostCheck
	}
	if pingCheck.Checked {
		state.PingCheck = pingCheck
	}
	state.LastDiagnostics = latestDiagnosticsAt(state.LastDiagnostics, state.HostCheck, state.PingCheck)
	if previous == state {
		return false
	}

	s.alerts[stableID] = state
	return true
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

func (s *Service) sendMenu(msg *message) {
	s.sendMenuToMessage(msg, msg.From)
}

func (s *Service) sendMenuToMessage(msg *message, from *user) {
	cfg := s.Config()
	threadID := replyThreadID(msg, cfg)
	userID := userIDFrom(from)
	text := s.formatMenu(cfg, s.isAdminUser(from, cfg))
	replyMarkup := mainMenuMarkup(s.isAdminUser(from, cfg))
	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	if messageID, ok := s.lastMenuMessageID(msg.Chat.ID, threadID, userID); ok {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
		err := s.editTextWithMarkup(ctx, chatID, messageID, text, replyMarkup)
		cancel()
		if err == nil || isMessageNotModified(err) {
			return
		}
		s.forgetMenuMessage(msg.Chat.ID, threadID, userID)
		logger.Warn("Failed to edit previous Telegram menu message: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	sent, err := s.sendHTMLToWithMarkup(ctx, chatID, threadID, text, replyMarkup)
	if err != nil {
		logger.Warn("Failed to send Telegram menu: %v", err)
		return
	}
	if sent != nil && sent.MessageID > 0 {
		s.rememberMenuMessage(msg.Chat.ID, threadID, userID, sent.MessageID)
	}
}

func (s *Service) editMenuMessage(msg *message, from *user) {
	cfg := s.Config()
	if s.editCommandMessage(msg, s.formatMenu(cfg, s.isAdminUser(from, cfg)), mainMenuMarkup(s.isAdminUser(from, cfg))) {
		s.rememberMenuMessage(msg.Chat.ID, msg.MessageThreadID, userIDFrom(from), msg.MessageID)
	}
}

func (s *Service) handleSpeedTestCommand(msg *message, args []string) {
	req := speedtest.RunRequest{
		OnlyOnline: true,
	}

	var queryParts []string
	for _, arg := range args {
		switch {
		case arg == "all":
			req.OnlyOnline = false
		case arg == "online":
			req.OnlyOnline = true
		case strings.HasPrefix(arg, "protocol:"):
			req.Protocol = strings.TrimPrefix(arg, "protocol:")
		case strings.HasPrefix(arg, "sub:"):
			req.SubName = strings.TrimPrefix(arg, "sub:")
		default:
			queryParts = append(queryParts, arg)
		}
	}

	if len(queryParts) > 0 {
		proxy, matches := s.findProxy(strings.Join(queryParts, " "))
		if proxy == nil {
			s.sendCommandReply(msg, formatProxySearchMiss(matches))
			return
		}
		req.ProxyIDs = []string{proxy.StableID}
		req.OnlyOnline = false
	}

	if err := s.speedManager.Run(req, "telegram"); err != nil {
		s.sendCommandReply(msg, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())))
		return
	}
	s.sendCommandReply(msg, "<b>Speed-test запущен</b>\n\nОтчет будет отправлен после завершения проверки.")
}

func (s *Service) formatHelp(cfg Config) string {
	var lines []string
	lines = append(lines,
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Команды</b>",
		"Главное управление доступно через кнопки под сообщением.",
		"",
		"• <code>/start</code> — открыть главное меню",
		"• <code>/status</code> — статусы нод",
		"• <code>/speed &lt;id или имя&gt;</code> — история замеров ноды",
		"• <code>/id</code> — ID чата, топика и пользователя",
	)
	if len(cfg.AdminUserIDs) > 0 {
		lines = append(lines,
			"• <code>/speedtest</code> — speed-test online-нод",
			"• <code>/speedtest all</code> — speed-test всех нод",
			"• <code>/speedtest &lt;id или имя&gt;</code> — speed-test одной ноды",
		)
	}
	return strings.Join(lines, "\n")
}

func (s *Service) formatMenu(cfg Config, isAdmin bool) string {
	total, online, offline := s.nodeCounts()
	speedReports := "выключены"
	if cfg.SpeedReportsEnabled && cfg.SpeedReportMode != "disabled" {
		speedReports = "включены"
		if cfg.SpeedReportMode == "issues" {
			speedReports = "только проблемы"
		}
	}
	alerts := "выключены"
	if cfg.NodeAlertsEnabled {
		alerts = fmt.Sprintf(
			"проверка %d мин, после %d провалов, диагностика %d мин, напоминания %s мин, максимум %d мин",
			cfg.AlertCheckMinutes,
			cfg.AlertAfterFailures,
			cfg.AlertDiagnosticsMinutes,
			formatIntList(cfg.AlertReminderScheduleMinutes),
			cfg.AlertMaxReminderMinutes,
		)
	}
	adminText := "нет"
	if isAdmin {
		adminText = "да"
	}
	thresholdText := "не задан"
	if cfg.LowSpeedThresholdMbps > 0 {
		thresholdText = fmt.Sprintf("%.2f Mbps", cfg.LowSpeedThresholdMbps)
	}

	return strings.Join([]string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Панель управления</b>",
		fmt.Sprintf("Ноды: <b>%d</b> всего | <b>%d</b> online | <b>%d</b> offline", total, online, offline),
		"",
		"<b>Speed-test</b>",
		fmt.Sprintf("Отчеты: <b>%s</b>", htmlEscape(speedReports)),
		fmt.Sprintf("Порог низкой скорости: <b>%s</b>", htmlEscape(thresholdText)),
		"",
		"<b>Оповещения</b>",
		fmt.Sprintf("Недоступность нод: <b>%s</b>", htmlEscape(alerts)),
		"",
		"<b>Доступ</b>",
		fmt.Sprintf("Текущий пользователь админ: <b>%s</b>", htmlEscape(adminText)),
		"",
		"Выберите действие:",
	}, "\n")
}

func (s *Service) formatNodeList() string {
	proxies := s.sortedProxies()
	total, online, offline := s.nodeCounts()
	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Ноды</b>",
		fmt.Sprintf("Всего: <b>%d</b> | Online: <b>%d</b> | Offline: <b>%d</b>", total, online, offline),
		"",
		"Выберите ноду кнопкой ниже, чтобы открыть статус, последние замеры и действия.",
	}
	if len(proxies) == 0 {
		lines = append(lines, "", "Ноды не найдены.")
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatNodeDetails(stableID string) string {
	proxy, matches := s.findProxy(stableID)
	if proxy == nil {
		return formatProxySearchMiss(matches)
	}
	details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	status := "ONLINE"
	if err != nil || !details.Online {
		status = "OFFLINE"
	}
	latencyText := "n/a"
	if details.Latency > 0 {
		latencyText = fmt.Sprintf("%d ms", details.Latency.Milliseconds())
	}

	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Нода</b>",
		fmt.Sprintf("<b>%s</b>", htmlEscape(proxy.Name)),
		fmt.Sprintf("ID: %s", htmlCode(proxy.StableID)),
		fmt.Sprintf("Статус: <b>%s</b> · %s", htmlEscape(status), htmlEscape(latencyText)),
		fmt.Sprintf("Протокол: %s", htmlCode(proxy.Protocol)),
	}
	if status == "OFFLINE" && !details.DownSince.IsZero() {
		lines = append(lines, fmt.Sprintf("Недоступна с: <b>%s</b>", htmlEscape(formatCheckedAt(details.DownSince))))
		lines = append(lines, fmt.Sprintf("Простой: <b>%s</b>", htmlEscape(formatDuration(time.Since(details.DownSince)))))
	}
	if status == "OFFLINE" {
		if diagnostics := formatHostDiagnosticsHTML(details.HostCheck, details.PingCheck); diagnostics != "" {
			lines = append(lines, fmt.Sprintf("Диагностика: %s", diagnostics))
		}
	}
	if proxy.SubName != "" {
		lines = append(lines, fmt.Sprintf("Подписка: <b>%s</b>", htmlEscape(proxy.SubName)))
	}
	if proxy.Server != "" {
		lines = append(lines, fmt.Sprintf("Сервер: %s", htmlCode(fmt.Sprintf("%s:%d", proxy.Server, proxy.Port))))
	}

	history := s.speedManager.ResultHistory(proxy.StableID)
	if len(history) == 0 {
		if result := s.latestSpeedResult(proxy.StableID); result != nil {
			history = []speedtest.Result{*result}
		}
	}
	lines = append(lines, "", "<b>Последние замеры</b>")
	if len(history) == 0 {
		lines = append(lines, "Пока нет результатов speed-test.")
	} else {
		cfg := s.Config()
		for _, result := range limitResults(history, 5) {
			lines = append(lines, formatSpeedHistoryLine(result, cfg.LowSpeedThresholdMbps))
		}
	}
	return trimMessage(strings.Join(lines, "\n"))
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
	proxies := s.proxyChecker.GetProxies()
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
	}
	sort.Slice(proxies, func(i, j int) bool {
		return strings.ToLower(proxies[i].Name) < strings.ToLower(proxies[j].Name)
	})
	return proxies
}

func (s *Service) lastMenuMessageID(chatID int64, threadID int, userID int64) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	messageID, ok := s.menuMessages[menuMessageKey(chatID, threadID, userID)]
	return messageID, ok
}

func (s *Service) rememberMenuMessage(chatID int64, threadID int, userID int64, messageID int) {
	if messageID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.menuMessages[menuMessageKey(chatID, threadID, userID)] = messageID
}

func (s *Service) forgetMenuMessage(chatID int64, threadID int, userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.menuMessages, menuMessageKey(chatID, threadID, userID))
}

func menuMessageKey(chatID int64, threadID int, userID int64) string {
	return fmt.Sprintf("%d:%d:%d", chatID, threadID, userID)
}

func userIDFrom(from *user) int64 {
	if from == nil {
		return 0
	}
	return from.ID
}

func replyThreadID(msg *message, cfg Config) int {
	if msg == nil {
		return 0
	}
	if msg.MessageThreadID > 0 {
		return msg.MessageThreadID
	}
	if cfg.MessageThreadID <= 0 || cfg.ChatID == "" {
		return 0
	}
	if cfg.ChatID != strconv.FormatInt(msg.Chat.ID, 10) {
		return 0
	}
	return cfg.MessageThreadID
}

func (s *Service) nodeCounts() (total int, online int, offline int) {
	proxies := s.proxyChecker.GetProxies()
	total = len(proxies)
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		ok, _, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		if err != nil || !ok {
			offline++
			continue
		}
		online++
	}
	return total, online, offline
}

func (s *Service) formatStatus() string {
	proxies := s.sortedProxies()

	var onlineLines []string
	var offlineLines []string
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		online := err == nil && details.Online
		line := formatProxyLineHTML(proxy, online, details.Latency, details.DownSince, details.HostCheck, details.PingCheck)
		if !online {
			offlineLines = append(offlineLines, line)
		} else {
			onlineLines = append(onlineLines, line)
		}
	}

	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Статусы нод</b>",
		fmt.Sprintf("Всего: <b>%d</b> | Online: <b>%d</b> | Offline: <b>%d</b>", len(proxies), len(onlineLines), len(offlineLines)),
	}
	if len(offlineLines) > 0 {
		lines = append(lines, "", "<b>Недоступны</b>")
		lines = append(lines, limitLines(offlineLines, 12)...)
	}
	if len(onlineLines) > 0 {
		lines = append(lines, "", "<b>Онлайн</b>")
		lines = append(lines, limitLines(onlineLines, 12)...)
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func formatStatusRefreshStarted() string {
	return strings.Join([]string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Статусы нод</b>",
		"Проверяю доступность нод. Сообщение обновится после завершения.",
	}, "\n")
}

func (s *Service) formatIssuesSummary() string {
	cfg := s.Config()
	muted := mutedNodeSet(cfg.MutedNodeIDs)
	var offlineLines []string
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if muted[proxy.StableID] {
			continue
		}
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err != nil || !details.Online {
			offlineLines = append(offlineLines, formatProxyLineHTML(proxy, false, details.Latency, details.DownSince, details.HostCheck, details.PingCheck))
		}
	}

	speedLines := speedIssuesHTML(filterMutedSpeedResults(s.speedManager.Snapshot().Results, cfg), cfg.LowSpeedThresholdMbps)
	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Проблемные ноды</b>",
	}
	if len(offlineLines) == 0 && len(speedLines) == 0 {
		lines = append(lines, "", "Проблем не найдено.")
		return strings.Join(lines, "\n")
	}
	if len(offlineLines) > 0 {
		lines = append(lines, "", "<b>Недоступны</b>")
		lines = append(lines, limitLines(offlineLines, 12)...)
	}
	if len(speedLines) > 0 {
		lines = append(lines, "", "<b>Speed-test ниже порога или с ошибками</b>")
		lines = append(lines, limitLines(speedLines, cfg.SpeedReportLimit)...)
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatSpeedHistory(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "<b>История замеров</b>\n\nИспользование: <code>/speed &lt;id или имя&gt;</code>"
	}

	proxy, matches := s.findProxy(query)
	if proxy == nil {
		return formatProxySearchMiss(matches)
	}

	history := s.speedManager.ResultHistory(proxy.StableID)
	if len(history) == 0 {
		for _, result := range s.speedManager.Snapshot().Results {
			if result.StableID == proxy.StableID {
				history = []speedtest.Result{result}
				break
			}
		}
	}
	if len(history) == 0 {
		return fmt.Sprintf("<b>История замеров</b>\n\nДля ноды <b>%s</b> пока нет результатов speed-test.", htmlEscape(proxy.Name))
	}

	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>История замеров</b>",
		fmt.Sprintf("<b>%s</b>", htmlEscape(proxy.Name)),
		fmt.Sprintf("ID: <code>%s</code>", htmlEscape(proxy.StableID)),
	}
	cfg := s.Config()
	for _, result := range limitResults(history, 5) {
		lines = append(lines, formatSpeedHistoryLine(result, cfg.LowSpeedThresholdMbps))
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatRecentSpeedOverview() string {
	results := s.speedManager.Snapshot().Results
	if len(results) == 0 {
		return "<b>Последние замеры</b>\n\nПока нет результатов speed-test."
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].CheckedAt.After(results[j].CheckedAt)
	})

	cfg := s.Config()
	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Последние замеры speed-test</b>",
	}
	for _, result := range limitResults(results, 10) {
		status := fmt.Sprintf("<b>%.2f Mbps</b>", result.Mbps)
		if result.Offline {
			status = "<b>OFFLINE</b>"
		} else if result.Error != "" {
			status = "<b>FAILED</b>"
		} else if cfg.LowSpeedThresholdMbps > 0 && result.Mbps < cfg.LowSpeedThresholdMbps {
			status = fmt.Sprintf("<b>LOW %.2f Mbps</b>", result.Mbps)
		}
		lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  <code>%s</code> · %s · %s", htmlEscape(result.Name), htmlEscape(result.StableID), status, htmlEscape(formatCheckedAt(result.CheckedAt))))
	}
	lines = append(lines, "", "Нажмите на ноду ниже, чтобы открыть историю.")
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatSpeedReport(report speedtest.RunReport, cfg Config, failed int, slow int, issuesOnly bool) string {
	successful := 0
	for _, result := range report.Results {
		if !result.Offline && result.Error == "" {
			successful++
		}
	}

	issues := speedIssuesHTML(report.Results, cfg.LowSpeedThresholdMbps)
	if issuesOnly {
		lines := []string{
			"<b>InvisibleProxyChecker</b>",
			"",
			"<b>Speed-test по расписанию: проблемы</b>",
			fmt.Sprintf("Завершен: %s", htmlCode(report.FinishedAt.Format("2006-01-02 15:04:05"))),
		}
		if cfg.LowSpeedThresholdMbps > 0 {
			lines = append(lines, fmt.Sprintf("Порог низкой скорости: <b>%.2f Mbps</b>", cfg.LowSpeedThresholdMbps))
		}
		lines = append(lines, "", "<b>Требует внимания</b>")
		lines = append(lines, limitLines(issues, cfg.SpeedReportLimit)...)
		return trimMessage(strings.Join(lines, "\n"))
	}

	lines := []string{
		"<b>InvisibleProxyChecker</b>",
		"",
		"<b>Speed-test завершен</b>",
		fmt.Sprintf("Источник: <b>%s</b>", htmlEscape(reportSourceLabel(report.Source))),
		fmt.Sprintf("Завершен: %s", htmlCode(report.FinishedAt.Format("2006-01-02 15:04:05"))),
		"",
		"<b>Сводка</b>",
		fmt.Sprintf("Проверено: <b>%d</b> · Успешно: <b>%d</b> · Низкая скорость: <b>%d</b> · Ошибки: <b>%d</b>", report.Selected, successful, slow, failed),
	}
	if cfg.LowSpeedThresholdMbps > 0 {
		lines = append(lines, fmt.Sprintf("Порог низкой скорости: <b>%.2f Mbps</b>", cfg.LowSpeedThresholdMbps))
	}

	if len(issues) > 0 {
		lines = append(lines, "", "<b>Требует внимания</b>")
		lines = append(lines, limitLines(issues, cfg.SpeedReportLimit)...)
	}

	top := successfulResults(report.Results)
	if len(top) > 0 {
		lines = append(lines, "", "<b>Лучшие результаты</b>")
		for _, result := range limitResults(top, cfg.SpeedReportLimit) {
			lines = append(lines, formatSpeedResultHTML(result, cfg.LowSpeedThresholdMbps))
		}
	}

	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) sendText(ctx context.Context, text string) error {
	cfg := s.Config()
	return s.sendTextTo(ctx, cfg.ChatID, cfg.MessageThreadID, text)
}

func (s *Service) sendCommandReply(msg *message, text string) {
	s.sendCommandReplyWithMarkup(msg, text, "")
}

func (s *Service) sendCommandReplyWithMarkup(msg *message, text string, replyMarkup string) {
	cfg := s.Config()
	threadID := replyThreadID(msg, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.sendHTMLToWithMarkup(ctx, strconv.FormatInt(msg.Chat.ID, 10), threadID, text, replyMarkup); err != nil {
		logger.Warn("Failed to send Telegram command reply: %v", err)
	}
}

func (s *Service) editCommandMessage(msg *message, text string, replyMarkup string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
	defer cancel()
	if err := s.editTextWithMarkup(ctx, strconv.FormatInt(msg.Chat.ID, 10), msg.MessageID, text, replyMarkup); err != nil {
		if isMessageNotModified(err) {
			return true
		}
		logger.Warn("Failed to edit Telegram command message: %v", err)
		return false
	}
	return true
}

func (s *Service) sendTextTo(ctx context.Context, chatID string, threadID int, text string) error {
	return s.sendTextToWithMarkup(ctx, chatID, threadID, text, "")
}

func (s *Service) sendTextToWithMarkup(ctx context.Context, chatID string, threadID int, text string, replyMarkup string) error {
	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("text", trimMessage(text))
	values.Set("disable_web_page_preview", "true")
	if threadID > 0 {
		values.Set("message_thread_id", strconv.Itoa(threadID))
	}
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}

	_, err := s.doAPI(ctx, "sendMessage", values)
	return err
}

func (s *Service) sendHTMLToWithMarkup(ctx context.Context, chatID string, threadID int, text string, replyMarkup string) (*message, error) {
	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("text", trimMessage(text))
	values.Set("parse_mode", "HTML")
	values.Set("disable_web_page_preview", "true")
	if threadID > 0 {
		values.Set("message_thread_id", strconv.Itoa(threadID))
	}
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}

	result, err := s.doAPI(ctx, "sendMessage", values)
	if err != nil {
		return nil, err
	}
	var sent message
	if err := json.Unmarshal(result, &sent); err != nil {
		return nil, nil
	}
	return &sent, nil
}

func isMessageNotModified(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}

func (s *Service) editTextWithMarkup(ctx context.Context, chatID string, messageID int, text string, replyMarkup string) error {
	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("message_id", strconv.Itoa(messageID))
	values.Set("text", trimMessage(text))
	values.Set("parse_mode", "HTML")
	values.Set("disable_web_page_preview", "true")
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}

	_, err := s.doAPI(ctx, "editMessageText", values)
	return err
}

func (s *Service) answerCallback(callbackID string, text string) {
	if callbackID == "" {
		return
	}
	values := url.Values{}
	values.Set("callback_query_id", callbackID)
	if text != "" {
		values.Set("text", text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.doAPI(ctx, "answerCallbackQuery", values); err != nil {
		logger.Warn("Failed to answer Telegram callback: %v", err)
	}
}

func (s *Service) doAPI(ctx context.Context, method string, values url.Values) (json.RawMessage, error) {
	cfg := s.Config()
	if cfg.BotToken == "" {
		return nil, fmt.Errorf("Telegram bot token is empty")
	}

	endpoint := "https://api.telegram.org/bot" + cfg.BotToken + "/" + method
	candidates := s.proxyCandidates()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available Xray node for Telegram API")
	}

	var lastErr error
	for _, candidate := range candidates {
		client, err := s.httpClientFor(candidate.Proxy, cfg.TimeoutSec)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %v", candidate.Proxy.Name, err)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%s: %v", candidate.Proxy.Name, readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("%s: HTTP %d: %s", candidate.Proxy.Name, resp.StatusCode, string(body))
			continue
		}

		var apiResp apiResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			lastErr = fmt.Errorf("%s: invalid Telegram response: %v", candidate.Proxy.Name, err)
			continue
		}
		if !apiResp.OK {
			lastErr = fmt.Errorf("%s: Telegram API error: %s", candidate.Proxy.Name, apiResp.Description)
			continue
		}
		return apiResp.Result, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("Telegram API request failed")
	}
	return nil, lastErr
}

func (s *Service) proxyCandidates() []proxyCandidate {
	proxies := s.proxyChecker.GetProxies()
	var candidates []proxyCandidate
	knownStatuses := 0

	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
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

func (s *Service) isChatAllowed(msg *message, cfg Config) bool {
	if msg == nil {
		return false
	}
	return s.isChatAllowedFor(msg.Chat.ID, msg.From, cfg)
}

func (s *Service) isChatAllowedFor(chatID int64, from *user, cfg Config) bool {
	if cfg.ChatID == "" {
		return true
	}
	if cfg.ChatID == strconv.FormatInt(chatID, 10) {
		return true
	}
	return s.isAdminUser(from, cfg)
}

func (s *Service) isAdmin(msg *message, cfg Config) bool {
	if msg == nil {
		return false
	}
	return s.isAdminUser(msg.From, cfg)
}

func (s *Service) isAdminUser(from *user, cfg Config) bool {
	if from == nil {
		return false
	}
	for _, id := range cfg.AdminUserIDs {
		if id == from.ID {
			return true
		}
	}
	return false
}

func (s *Service) nextUpdateOffset() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastUpdateID
}

func (s *Service) markUpdateSeen(updateID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if updateID >= s.lastUpdateID {
		s.lastUpdateID = updateID + 1
	}
}

func (s *Service) wait(duration time.Duration) bool {
	select {
	case <-time.After(duration):
		return true
	case <-s.stopCh:
		return false
	}
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
	if len(stateFile.Nodes) == 0 {
		return nil
	}

	active := make(map[string]bool)
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		active[proxy.StableID] = true
	}

	loaded := make(map[string]nodeAlertState)
	for stableID, persisted := range stateFile.Nodes {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" || !active[stableID] {
			continue
		}

		state := persisted.toNodeAlertState()
		if !state.WasDown && state.FailCount <= 0 && state.DownSince.IsZero() && state.LastAlert.IsZero() {
			continue
		}
		loaded[stableID] = state
	}
	if len(loaded) == 0 {
		return nil
	}

	s.mu.Lock()
	for stableID, state := range loaded {
		s.alerts[stableID] = state
	}
	s.mu.Unlock()

	restored := 0
	for stableID, state := range loaded {
		if state.WasDown && !state.DownSince.IsZero() {
			if s.proxyChecker.RestoreOfflineStatus(stableID, state.DownSince, state.HostCheck, state.PingCheck) {
				restored++
			}
		}
	}

	logger.Info("Loaded Telegram node alert state: %d nodes, restored %d offline statuses", len(loaded), restored)
	return nil
}

func (s *Service) saveAlertState() error {
	if s.alertStatePath == "" {
		return nil
	}

	active := make(map[string]bool)
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		active[proxy.StableID] = true
	}

	nodes := make(map[string]persistedNodeAlertState)
	s.mu.RLock()
	for stableID, state := range s.alerts {
		if !active[stableID] {
			continue
		}
		if !state.WasDown && state.FailCount <= 0 && state.DownSince.IsZero() && state.LastAlert.IsZero() {
			continue
		}
		nodes[stableID] = persistedNodeAlertStateFrom(state)
	}
	s.mu.RUnlock()

	if len(nodes) == 0 {
		if err := os.Remove(s.alertStatePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(s.alertStatePath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(nodeAlertStateFile{
		Version:   1,
		UpdatedAt: time.Now(),
		Nodes:     nodes,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.alertStatePath, data, 0600)
}

func persistedNodeAlertStateFrom(state nodeAlertState) persistedNodeAlertState {
	persisted := persistedNodeAlertState{
		FailCount:       state.FailCount,
		WasDown:         state.WasDown,
		DownSince:       state.DownSince,
		LastAlert:       state.LastAlert,
		AlertCount:      state.AlertCount,
		NextAlert:       state.NextAlert,
		LastDiagnostics: state.LastDiagnostics,
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
		FailCount:       p.FailCount,
		WasDown:         p.WasDown,
		DownSince:       p.DownSince,
		LastAlert:       p.LastAlert,
		AlertCount:      p.AlertCount,
		NextAlert:       p.NextAlert,
		LastDiagnostics: p.LastDiagnostics,
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
}

func applyLegacyAlertRepeat(data []byte, cfg *Config) {
	if cfg.AlertRepeatMinutes <= 0 {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if _, ok := raw["alertDiagnosticsMinutes"]; !ok {
		cfg.AlertDiagnosticsMinutes = cfg.AlertRepeatMinutes
	}
	if _, ok := raw["alertMaxReminderMinutes"]; !ok {
		cfg.AlertMaxReminderMinutes = cfg.AlertRepeatMinutes
	}
}

func applyEnvDefaults(cfg *Config) {
	if v := os.Getenv("TELEGRAM_ENABLED"); v != "" {
		cfg.Enabled = parseBool(v)
	}
	applyEnvSensitive(cfg)
	cfg.Normalize()
}

func applyEnvOverrides(cfg *Config) {
	if v, ok := os.LookupEnv("TELEGRAM_ENABLED"); ok && parseBool(v) {
		cfg.Enabled = true
	}
	applyEnvSensitive(cfg)
}

func applyEnvSensitive(cfg *Config) {
	if v, ok := os.LookupEnv("TELEGRAM_BOT_TOKEN"); ok {
		cfg.BotToken = v
	}
	if v, ok := os.LookupEnv("TELEGRAM_CHAT_ID"); ok {
		cfg.ChatID = v
	}
	if v, ok := os.LookupEnv("TELEGRAM_MESSAGE_THREAD_ID"); ok {
		cfg.MessageThreadID, _ = strconv.Atoi(v)
	}
	if v, ok := os.LookupEnv("TELEGRAM_ADMIN_IDS"); ok {
		cfg.AdminUserIDs = parseInt64List(v)
	}
}

func disableInvalidEnabledConfig(cfg *Config) {
	if !cfg.Enabled {
		return
	}
	if cfg.BotToken == "" {
		logger.Warn("Telegram is enabled but bot token is empty; disabling Telegram")
		cfg.Enabled = false
	}
}

func parseBool(value string) bool {
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return parsed
}

func parseInt64List(value string) []int64 {
	var result []int64
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err == nil {
			result = append(result, id)
		}
	}
	return result
}

func normalizeMinuteSchedule(values []int) []int {
	seen := make(map[int]bool)
	var result []int
	for _, value := range values {
		if value <= 0 || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func parseMinuteSchedule(value string) []int {
	var result []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		minutes, err := strconv.Atoi(part)
		if err == nil {
			result = append(result, minutes)
		}
	}
	return normalizeMinuteSchedule(result)
}

func normalizeNodeIDs(values []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
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

func mutedNodeSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result[value] = true
		}
	}
	return result
}

func formatIntList(values []int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func parseCommand(text string) (string, []string) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", nil
	}
	cmd := strings.TrimPrefix(parts[0], "/")
	if idx := strings.Index(cmd, "@"); idx >= 0 {
		cmd = cmd[:idx]
	}
	return strings.ToLower(cmd), parts[1:]
}

func countSpeedIssues(results []speedtest.Result, threshold float64) (failed int, slow int) {
	for _, result := range results {
		if result.Offline {
			failed++
			continue
		}
		if result.Error != "" {
			failed++
			continue
		}
		if threshold > 0 && result.Mbps < threshold {
			slow++
		}
	}
	return failed, slow
}

func filterMutedRunReport(report speedtest.RunReport, cfg Config) speedtest.RunReport {
	report.Results = filterMutedSpeedResults(report.Results, cfg)
	report.Selected = len(report.Results)
	return report
}

func filterMutedSpeedResults(results []speedtest.Result, cfg Config) []speedtest.Result {
	if len(results) == 0 || len(cfg.MutedNodeIDs) == 0 {
		return results
	}
	muted := mutedNodeSet(cfg.MutedNodeIDs)
	filtered := make([]speedtest.Result, 0, len(results))
	for _, result := range results {
		if result.StableID != "" && muted[result.StableID] {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func speedIssuesHTML(results []speedtest.Result, threshold float64) []string {
	var lines []string
	for _, result := range results {
		if result.Offline {
			diagnostics := formatSpeedResultDiagnosticsHTML(result)
			if diagnostics == "" {
				diagnostics = "Диагностика: нет данных"
			}
			lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  ⚠️ <b>Нода offline</b> · %s\n  %s", htmlEscape(result.Name), htmlCode(result.StableID), diagnostics))
			continue
		}
		if result.Error != "" {
			lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  ❌ <b>Ошибка</b> · %s\n  %s", htmlEscape(result.Name), htmlCode(result.StableID), htmlEscape(result.Error)))
			continue
		}
		if threshold > 0 && result.Mbps < threshold {
			lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  ⚠️ <b>%.2f Mbps</b> · порог %.2f Mbps · %s\n  %s · %d ms", htmlEscape(result.Name), result.Mbps, threshold, htmlCode(result.StableID), htmlEscape(formatBytes(result.DownloadedBytes)), result.DurationMs))
		}
	}
	return lines
}

func successfulResults(results []speedtest.Result) []speedtest.Result {
	var successful []speedtest.Result
	for _, result := range results {
		if !result.Offline && result.Error == "" {
			successful = append(successful, result)
		}
	}
	sort.Slice(successful, func(i, j int) bool {
		return successful[i].Mbps > successful[j].Mbps
	})
	return successful
}

func mainMenuMarkup(isAdmin bool) string {
	rows := [][]inlineKeyboardButton{
		{
			{Text: "🖥 Ноды", CallbackData: "nodes:list"},
			{Text: "📈 Замеры", CallbackData: "speed:list"},
		},
		{
			{Text: "⚠️ Проблемы", CallbackData: "issues"},
			{Text: "📊 Все статусы", CallbackData: "status"},
		},
	}
	if isAdmin {
		rows = append(rows, []inlineKeyboardButton{
			{Text: "Speed-test online", CallbackData: "speedtest:online"},
		})
		rows = append(rows, []inlineKeyboardButton{
			{Text: "Speed-test all", CallbackData: "speedtest:all"},
		})
	}
	rows = append(rows, []inlineKeyboardButton{
		{Text: "Обновить", CallbackData: "back_to_menu"},
	})
	return encodeMarkup(rows)
}

func backToMenuMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{{Text: "Меню", CallbackData: "back_to_menu"}},
	})
}

func statusMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{{Text: "Обновить", CallbackData: "status:refresh"}},
		{{Text: "Меню", CallbackData: "back_to_menu"}},
	})
}

func statusRefreshMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{{Text: "Проверка идет...", CallbackData: "status:refresh"}},
		{{Text: "Меню", CallbackData: "back_to_menu"}},
	})
}

func (s *Service) nodeListMarkup() string {
	var rows [][]inlineKeyboardButton
	for _, proxy := range s.sortedProxies() {
		online, _, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		status := "🔴"
		if err == nil && online {
			status = "🟢"
		}
		rows = append(rows, []inlineKeyboardButton{{
			Text:         status + " " + shortButtonText(proxy.Name),
			CallbackData: "node:" + proxy.StableID,
		}})
	}
	rows = append(rows, []inlineKeyboardButton{{Text: "Меню", CallbackData: "back_to_menu"}})
	return encodeMarkup(rows)
}

func (s *Service) nodeDetailMarkup(stableID string, isAdmin bool) string {
	var rows [][]inlineKeyboardButton
	if isAdmin {
		rows = append(rows, []inlineKeyboardButton{{
			Text:         "Speed-test этой ноды",
			CallbackData: "node:test:" + stableID,
		}})
	}
	rows = append(rows, []inlineKeyboardButton{
		{Text: "Ноды", CallbackData: "nodes:list"},
		{Text: "Меню", CallbackData: "back_to_menu"},
	})
	return encodeMarkup(rows)
}

func (s *Service) speedHistoryMarkup() string {
	results := s.speedManager.Snapshot().Results
	sort.Slice(results, func(i, j int) bool {
		return results[i].CheckedAt.After(results[j].CheckedAt)
	})

	var rows [][]inlineKeyboardButton
	var row []inlineKeyboardButton
	for _, result := range limitResults(results, menuSpeedButtonLimit) {
		if result.StableID == "" {
			continue
		}
		row = append(row, inlineKeyboardButton{
			Text:         shortButtonText(result.Name),
			CallbackData: "speed:" + result.StableID,
		})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []inlineKeyboardButton{{Text: "Меню", CallbackData: "back_to_menu"}})
	return encodeMarkup(rows)
}

func encodeMarkup(rows [][]inlineKeyboardButton) string {
	data, err := json.Marshal(inlineKeyboardMarkup{InlineKeyboard: rows})
	if err != nil {
		return ""
	}
	return string(data)
}

func shortButtonText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "Нода"
	}
	runes := []rune(text)
	if len(runes) <= 24 {
		return text
	}
	return string(runes[:21]) + "..."
}

func limitLines(lines []string, limit int) []string {
	if limit <= 0 || len(lines) <= limit {
		return lines
	}
	result := append([]string{}, lines[:limit]...)
	result = append(result, fmt.Sprintf("...and %d more", len(lines)-limit))
	return result
}

func limitResults(results []speedtest.Result, limit int) []speedtest.Result {
	if limit <= 0 || len(results) <= limit {
		return results
	}
	return results[:limit]
}

func formatSpeedResultHTML(result speedtest.Result, threshold float64) string {
	if result.Offline {
		diagnostics := formatSpeedResultDiagnosticsHTML(result)
		if diagnostics == "" {
			diagnostics = "Диагностика: нет данных"
		}
		return fmt.Sprintf("• <b>%s</b>\n  ⚠️ <b>Нода offline</b> · %s", htmlEscape(result.Name), diagnostics)
	}

	if threshold <= 0 || result.Mbps >= threshold {
		return fmt.Sprintf("• <b>%s</b>\n  ✅ <b>%.2f Mbps</b>", htmlEscape(result.Name), result.Mbps)
	}

	ttfbText := ""
	if result.TTFBMs > 0 {
		ttfbText = fmt.Sprintf(" · TTFB %d ms", result.TTFBMs)
	}
	return fmt.Sprintf("• <b>%s</b>\n  ⚠️ <b>LOW %.2f Mbps</b> · %s · %d ms%s", htmlEscape(result.Name), result.Mbps, htmlEscape(formatBytes(result.DownloadedBytes)), result.DurationMs, ttfbText)
}

func reportSourceLabel(source string) string {
	switch source {
	case "manual":
		return "админ-панель"
	case "telegram":
		return "Telegram"
	case "schedule":
		return "расписание"
	default:
		if source == "" {
			return "неизвестно"
		}
		return source
	}
}

func formatSpeedHistoryLine(result speedtest.Result, threshold float64) string {
	prefix := formatCheckedAt(result.CheckedAt)
	if result.Offline {
		diagnostics := formatSpeedResultDiagnosticsHTML(result)
		if diagnostics == "" {
			return fmt.Sprintf("• %s\n  <b>OFFLINE</b>", htmlCode(prefix))
		}
		return fmt.Sprintf("• %s\n  <b>OFFLINE</b> · %s", htmlCode(prefix), diagnostics)
	}
	if result.Error != "" {
		return fmt.Sprintf("• %s\n  <b>FAILED</b> · %s", htmlCode(prefix), htmlEscape(result.Error))
	}
	marker := "OK"
	if threshold > 0 && result.Mbps < threshold {
		marker = "LOW"
	}
	return fmt.Sprintf("• %s\n  <b>%s %.2f Mbps</b> · %s · %d ms · TTFB %d ms", htmlCode(prefix), marker, result.Mbps, htmlEscape(formatBytes(result.DownloadedBytes)), result.DurationMs, result.TTFBMs)
}

func formatSpeedResultDiagnosticsHTML(result speedtest.Result) string {
	var hostCheck checker.HostCheckDetails
	var pingCheck checker.PingCheckDetails
	if result.HostCheck != nil {
		hostCheck = *result.HostCheck
	}
	if result.PingCheck != nil {
		pingCheck = *result.PingCheck
	}
	return formatHostDiagnosticsHTML(hostCheck, pingCheck)
}

func formatBytes(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	mb := float64(bytes) / 1024 / 1024
	if mb >= 1 {
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%.0f KB", float64(bytes)/1024)
}

func formatDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}

	totalMinutes := int(value / time.Minute)
	if totalMinutes <= 0 {
		return "<1 мин"
	}

	days := totalMinutes / (24 * 60)
	hours := (totalMinutes % (24 * 60)) / 60
	minutes := totalMinutes % 60

	if days > 0 {
		return fmt.Sprintf("%d д %d ч %d мин", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%d ч %d мин", hours, minutes)
	}
	return fmt.Sprintf("%d мин", minutes)
}

func formatHostDiagnosticsHTML(hostCheck checker.HostCheckDetails, pingCheck checker.PingCheckDetails) string {
	var parts []string
	if hostCheck.Checked {
		parts = append(parts, formatTCPCheckHTML(hostCheck))
	}
	if pingCheck.Checked {
		parts = append(parts, formatPingCheckHTML(pingCheck))
	}
	return strings.Join(parts, " · ")
}

func formatTCPCheckHTML(hostCheck checker.HostCheckDetails) string {
	if hostCheck.Online {
		if hostCheck.Latency > 0 {
			return fmt.Sprintf("TCP 🟢 %d ms", hostCheck.Latency.Milliseconds())
		}
		return "TCP 🟢"
	}
	return "TCP 🔴"
}

func formatPingCheckHTML(pingCheck checker.PingCheckDetails) string {
	if pingCheck.Online {
		if pingCheck.Latency > 0 {
			return fmt.Sprintf("Ping 🟢 %d ms", pingCheck.Latency.Milliseconds())
		}
		return "Ping 🟢"
	}
	return "Ping 🔴"
}

func formatProxyLine(proxy *models.ProxyConfig, online bool, latency time.Duration) string {
	status := "OFFLINE"
	if online {
		status = "ONLINE"
	}
	latencyText := "n/a"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}
	return fmt.Sprintf("- %s %s [%s] (%s, %s)", status, proxy.Name, proxy.StableID, proxy.Protocol, latencyText)
}

func formatProxyLineHTML(proxy *models.ProxyConfig, online bool, latency time.Duration, downSince time.Time, hostCheck checker.HostCheckDetails, pingCheck checker.PingCheckDetails) string {
	status := "OFFLINE"
	if online {
		status = "ONLINE"
	}
	latencyText := "n/a"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}

	parts := []string{htmlCode(proxy.StableID), htmlCode(proxy.Protocol), htmlEscape(latencyText)}
	if !online && !downSince.IsZero() {
		parts = append(parts, fmt.Sprintf("с %s", htmlEscape(formatCheckedAt(downSince))))
		parts = append(parts, fmt.Sprintf("простой %s", htmlEscape(formatDuration(time.Since(downSince)))))
	}
	if !online {
		if diagnostics := formatHostDiagnosticsHTML(hostCheck, pingCheck); diagnostics != "" {
			parts = append(parts, diagnostics)
		}
	}
	return fmt.Sprintf("• <b>%s</b> <b>%s</b>\n  %s", htmlEscape(status), htmlEscape(proxy.Name), strings.Join(parts, " · "))
}

func formatNodeDown(proxy *models.ProxyConfig, state nodeAlertState, now time.Time) string {
	title := "⚠️ Нода недоступна"
	if state.AlertCount > 1 {
		title = "⚠️ Нода всё ещё недоступна"
	}

	lines := []string{
		fmt.Sprintf("<b>%s</b>", htmlEscape(title)),
		"",
		fmt.Sprintf("<b>%s</b>", htmlEscape(proxy.Name)),
		fmt.Sprintf("ID: %s", htmlCode(proxy.StableID)),
		fmt.Sprintf("Протокол: %s", htmlCode(proxy.Protocol)),
	}
	if proxy.SubName != "" {
		lines = append(lines, fmt.Sprintf("Подписка: <b>%s</b>", htmlEscape(proxy.SubName)))
	}
	if !state.DownSince.IsZero() {
		lines = append(lines, fmt.Sprintf("Недоступна с: <b>%s</b>", htmlEscape(formatCheckedAt(state.DownSince))))
		lines = append(lines, fmt.Sprintf("Простой: <b>%s</b>", htmlEscape(formatDuration(now.Sub(state.DownSince)))))
	}
	if diagnostics := formatHostDiagnosticsHTML(state.HostCheck, state.PingCheck); diagnostics != "" {
		lines = append(lines, fmt.Sprintf("Диагностика: %s", diagnostics))
	} else {
		lines = append(lines, "Диагностика: <b>нет данных</b>")
	}
	if nextAfter := state.NextAlert.Sub(now); nextAfter > 0 {
		lines = append(lines, fmt.Sprintf("Следующее напоминание: через <b>%s</b>", htmlEscape(formatDuration(nextAfter))))
	}
	lines = append(lines, fmt.Sprintf("Провалов подряд: <b>%d</b>", state.FailCount))
	return strings.Join(lines, "\n")
}

func formatNodeDownGroup(alerts []nodeDownAlert, now time.Time) string {
	lines := []string{
		fmt.Sprintf("<b>⚠️ Недоступны %d ноды</b>", len(alerts)),
		"",
	}
	for _, alert := range alerts {
		state := alert.State
		downtime := "n/a"
		if !state.DownSince.IsZero() {
			downtime = formatDuration(now.Sub(state.DownSince))
		}
		parts := []string{fmt.Sprintf("простой %s", htmlEscape(downtime))}
		diagnostics := formatHostDiagnosticsHTML(state.HostCheck, state.PingCheck)
		if diagnostics == "" {
			diagnostics = "Диагностика: нет данных"
		}
		parts = append(parts, diagnostics)
		if alert.NextAfter > 0 {
			parts = append(parts, fmt.Sprintf("следующее через %s", htmlEscape(formatDuration(alert.NextAfter))))
		}
		lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  %s", htmlEscape(alert.Proxy.Name), strings.Join(parts, " · ")))
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func formatNodeRecovery(proxy *models.ProxyConfig, latency time.Duration, downSince time.Time, now time.Time) string {
	latencyText := "n/a"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}
	lines := []string{
		"<b>✅ Нода снова доступна</b>",
		"",
		fmt.Sprintf("<b>%s</b>", htmlEscape(proxy.Name)),
		fmt.Sprintf("ID: %s", htmlCode(proxy.StableID)),
		fmt.Sprintf("Задержка: <b>%s</b>", htmlEscape(latencyText)),
	}
	if !downSince.IsZero() {
		lines = append(lines, fmt.Sprintf("Была недоступна с: <b>%s</b>", htmlEscape(formatCheckedAt(downSince))))
		lines = append(lines, fmt.Sprintf("Простой: <b>%s</b>", htmlEscape(formatDuration(now.Sub(downSince)))))
	}
	return strings.Join(lines, "\n")
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

func formatIDReply(msg *message) string {
	return formatIDReplyFor(msg, msg.From)
}

func formatIDReplyFor(msg *message, from *user) string {
	var lines []string
	lines = append(lines, "<b>InvisibleProxyChecker</b>", "", "<b>Telegram IDs</b>")
	lines = append(lines, fmt.Sprintf("Chat ID: %s", htmlCode(strconv.FormatInt(msg.Chat.ID, 10))))
	if msg.MessageThreadID > 0 {
		lines = append(lines, fmt.Sprintf("Topic ID: %s", htmlCode(strconv.Itoa(msg.MessageThreadID))))
	}
	if from != nil {
		lines = append(lines, fmt.Sprintf("User ID: %s", htmlCode(strconv.FormatInt(from.ID, 10))))
	}
	return strings.Join(lines, "\n")
}

func formatProxySearchMiss(matches []*models.ProxyConfig) string {
	if len(matches) == 0 {
		return "<b>Нода не найдена</b>\n\nПроверьте stable ID или часть имени."
	}
	var lines []string
	lines = append(lines, "<b>Найдено несколько нод</b>", "", "Уточните запрос по stable ID:")
	for _, proxy := range matches {
		if len(lines) >= 11 {
			lines = append(lines, fmt.Sprintf("...and %d more", len(matches)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("• %s — <b>%s</b>", htmlCode(proxy.StableID), htmlEscape(proxy.Name)))
	}
	return strings.Join(lines, "\n")
}

func htmlEscape(value string) string {
	return html.EscapeString(value)
}

func htmlCode(value string) string {
	return "<code>" + htmlEscape(value) + "</code>"
}

func formatCheckedAt(value time.Time) string {
	if value.IsZero() {
		return "n/a"
	}
	return value.Format("2006-01-02 15:04:05")
}

func trimMessage(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= 3900 {
		return text
	}
	return text[:3900] + "\n...truncated"
}
