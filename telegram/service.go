package telegram

import (
	"context"
	"encoding/json"
	"fmt"
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
	defaultTimeoutSec         = 20
	defaultSpeedReportLimit   = 10
	defaultAlertAfterFailures = 1
	defaultAlertRepeatMinutes = 60
	maxSpeedReportLimit       = 50
	menuSpeedButtonLimit      = 8
)

type Config struct {
	Enabled              bool    `json:"enabled"`
	BotToken             string  `json:"botToken"`
	ChatID               string  `json:"chatId"`
	MessageThreadID      int     `json:"messageThreadId"`
	AdminUserIDs         []int64 `json:"adminUserIds"`
	CommandPollingEnabled bool   `json:"commandPollingEnabled"`
	SpeedReportsEnabled  bool    `json:"speedReportsEnabled"`
	SpeedReportMode      string  `json:"speedReportMode"`
	LowSpeedThresholdMbps float64 `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit      int     `json:"speedReportLimit"`
	NodeAlertsEnabled     bool    `json:"nodeAlertsEnabled"`
	AlertAfterFailures    int     `json:"alertAfterFailures"`
	AlertRepeatMinutes    int     `json:"alertRepeatMinutes"`
	NotifyRecovery        bool    `json:"notifyRecovery"`
	TimeoutSec            int     `json:"timeoutSec"`
}

type AdminConfig struct {
	Enabled                 bool    `json:"enabled"`
	CommandPollingEnabled   bool    `json:"commandPollingEnabled"`
	SpeedReportsEnabled    bool    `json:"speedReportsEnabled"`
	SpeedReportMode        string  `json:"speedReportMode"`
	LowSpeedThresholdMbps  float64 `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit       int     `json:"speedReportLimit"`
	NodeAlertsEnabled      bool    `json:"nodeAlertsEnabled"`
	AlertAfterFailures     int     `json:"alertAfterFailures"`
	AlertRepeatMinutes     int     `json:"alertRepeatMinutes"`
	NotifyRecovery         bool    `json:"notifyRecovery"`
	BotTokenConfigured     bool    `json:"botTokenConfigured"`
	ChatConfigured         bool    `json:"chatConfigured"`
	MessageThreadConfigured bool    `json:"messageThreadConfigured"`
	AdminUserCount         int     `json:"adminUserCount"`
}

func DefaultConfig() Config {
	return Config{
		CommandPollingEnabled: true,
		SpeedReportsEnabled:  true,
		SpeedReportMode:      "always",
		SpeedReportLimit:     defaultSpeedReportLimit,
		NodeAlertsEnabled:    true,
		AlertAfterFailures:   defaultAlertAfterFailures,
		AlertRepeatMinutes:   defaultAlertRepeatMinutes,
		NotifyRecovery:       true,
		TimeoutSec:           defaultTimeoutSec,
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
	if c.AlertAfterFailures <= 0 {
		c.AlertAfterFailures = defaultAlertAfterFailures
	}
	if c.AlertRepeatMinutes <= 0 {
		c.AlertRepeatMinutes = defaultAlertRepeatMinutes
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = defaultTimeoutSec
	}
}

type Service struct {
	proxyChecker *checker.ProxyChecker
	speedManager *speedtest.Manager
	startPort    int
	statePath    string

	mu           sync.RWMutex
	config       Config
	alerts       map[string]nodeAlertState
	lastUpdateID int

	stopCh   chan struct{}
	stopOnce sync.Once
}

type nodeAlertState struct {
	FailCount int
	WasDown   bool
	LastAlert time.Time
}

type proxyCandidate struct {
	Proxy   *models.ProxyConfig
	Latency time.Duration
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
	MessageThreadID int   `json:"message_thread_id"`
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
	return &Service{
		proxyChecker: proxyChecker,
		speedManager: speedManager,
		startPort:    startPort,
		statePath:    statePath,
		config:       DefaultConfig(),
		alerts:       make(map[string]nodeAlertState),
		stopCh:       make(chan struct{}),
	}
}

func (s *Service) Load() error {
	cfg := DefaultConfig()
	if s.statePath == "" {
		applyEnvDefaults(&cfg)
		disableInvalidEnabledConfig(&cfg)
		s.setConfig(cfg)
		return nil
	}

	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvDefaults(&cfg)
			disableInvalidEnabledConfig(&cfg)
			s.setConfig(cfg)
			return nil
		}
		return err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	applyEnvOverrides(&cfg)
	cfg.Normalize()
	disableInvalidEnabledConfig(&cfg)
	s.setConfig(cfg)
	return nil
}

func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *Service) AdminConfig() AdminConfig {
	cfg := s.Config()
	return AdminConfig{
		Enabled:                 cfg.Enabled,
		CommandPollingEnabled:   cfg.CommandPollingEnabled,
		SpeedReportsEnabled:    cfg.SpeedReportsEnabled,
		SpeedReportMode:        cfg.SpeedReportMode,
		LowSpeedThresholdMbps:  cfg.LowSpeedThresholdMbps,
		SpeedReportLimit:       cfg.SpeedReportLimit,
		NodeAlertsEnabled:      cfg.NodeAlertsEnabled,
		AlertAfterFailures:     cfg.AlertAfterFailures,
		AlertRepeatMinutes:     cfg.AlertRepeatMinutes,
		NotifyRecovery:         cfg.NotifyRecovery,
		BotTokenConfigured:     cfg.BotToken != "",
		ChatConfigured:         cfg.ChatID != "",
		MessageThreadConfigured: cfg.MessageThreadID > 0,
		AdminUserCount:         len(cfg.AdminUserIDs),
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
	cfg.AlertAfterFailures = input.AlertAfterFailures
	cfg.AlertRepeatMinutes = input.AlertRepeatMinutes
	cfg.NotifyRecovery = input.NotifyRecovery
	cfg.Normalize()
	if cfg.Enabled && cfg.BotToken == "" {
		return fmt.Errorf("bot token is required when Telegram is enabled; set TELEGRAM_BOT_TOKEN")
	}

	if err := s.saveEditableConfig(cfg); err != nil {
		return err
	}
	s.setConfig(cfg)
	go s.syncBotCommands()
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
	go func() {
		if s.wait(2 * time.Second) {
			s.syncBotCommands()
		}
	}()
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
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.SpeedReportsEnabled || cfg.SpeedReportMode == "disabled" {
		return
	}

	failed, slow := countSpeedIssues(report.Results, cfg.LowSpeedThresholdMbps)
	if cfg.SpeedReportMode == "issues" && failed == 0 && slow == 0 {
		return
	}

	text := s.formatSpeedReport(report, cfg, failed, slow)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	if err := s.sendText(ctx, text); err != nil {
		logger.Warn("Failed to send Telegram speed-test report: %v", err)
	}
}

func (s *Service) NotifyNodeStatuses() {
	cfg := s.Config()
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.NodeAlertsEnabled {
		return
	}

	now := time.Now()
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		isDown := err != nil || !online

		var messageText string
		s.mu.Lock()
		state := s.alerts[proxy.StableID]
		if isDown {
			state.FailCount++
			state.WasDown = true
			if state.FailCount >= cfg.AlertAfterFailures && shouldRepeatAlert(state.LastAlert, cfg.AlertRepeatMinutes, now) {
				state.LastAlert = now
				messageText = formatNodeDown(proxy, state.FailCount)
			}
		} else {
			if state.WasDown && cfg.NotifyRecovery {
				messageText = formatNodeRecovery(proxy, latency)
			}
			state = nodeAlertState{}
		}
		s.alerts[proxy.StableID] = state
		s.mu.Unlock()

		if messageText == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
		if err := s.sendText(ctx, messageText); err != nil {
			logger.Warn("Failed to send Telegram node alert: %v", err)
		}
		cancel()
	}
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
		s.sendCommandReplyWithMarkup(msg, s.formatStatus(), backToMenuMarkup())
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
			s.sendCommandReplyWithMarkup(cb.Message, formatIDReplyFor(cb.Message, cb.From), backToMenuMarkup())
		}
		return
	}

	if cb.Message == nil || !s.isChatAllowedFor(cb.Message.Chat.ID, cb.From, cfg) {
		s.answerCallback(cb.ID, "Нет доступа")
		return
	}

	switch {
	case data == "menu" || data == "menu:refresh":
		s.answerCallback(cb.ID, "Обновлено")
		s.sendMenuToMessage(cb.Message, cb.From)
	case data == "status":
		s.answerCallback(cb.ID, "")
		s.sendCommandReplyWithMarkup(cb.Message, s.formatStatus(), backToMenuMarkup())
	case data == "issues":
		s.answerCallback(cb.ID, "")
		s.sendCommandReplyWithMarkup(cb.Message, s.formatIssuesSummary(), backToMenuMarkup())
	case data == "help":
		s.answerCallback(cb.ID, "")
		s.sendCommandReplyWithMarkup(cb.Message, s.formatHelp(cfg), backToMenuMarkup())
	case data == "speed:list":
		s.answerCallback(cb.ID, "")
		s.sendCommandReplyWithMarkup(cb.Message, s.formatRecentSpeedOverview(), s.speedHistoryMarkup())
	case strings.HasPrefix(data, "speed:"):
		s.answerCallback(cb.ID, "")
		query := strings.TrimPrefix(data, "speed:")
		s.sendCommandReplyWithMarkup(cb.Message, s.formatSpeedHistory(query), backToMenuMarkup())
	case data == "speedtest:online":
		s.handleSpeedTestCallback(cb, true)
	case data == "speedtest:all":
		s.handleSpeedTestCallback(cb, false)
	default:
		s.answerCallback(cb.ID, "Неизвестное действие")
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
			s.sendCommandReplyWithMarkup(cb.Message, fmt.Sprintf("Speed test was not started: %v", err), backToMenuMarkup())
		}
		return
	}
	s.answerCallback(cb.ID, "Speed test запущен")
	if cb.Message != nil {
		s.sendCommandReplyWithMarkup(cb.Message, "Speed test started. The report will be sent when it finishes.", backToMenuMarkup())
	}
}

func (s *Service) sendMenu(msg *message) {
	s.sendMenuToMessage(msg, msg.From)
}

func (s *Service) sendMenuToMessage(msg *message, from *user) {
	cfg := s.Config()
	s.sendCommandReplyWithMarkup(msg, s.formatMenu(cfg, s.isAdminUser(from, cfg)), mainMenuMarkup(s.isAdminUser(from, cfg)))
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
		s.sendCommandReply(msg, fmt.Sprintf("Speed test was not started: %v", err))
		return
	}
	s.sendCommandReply(msg, "Speed test started. The report will be sent when it finishes.")
}

func (s *Service) formatHelp(cfg Config) string {
	var lines []string
	lines = append(lines,
		"Xray Checker bot commands:",
		"/menu - open the action menu",
		"/status - show node status summary",
		"/speed <stableId or name> - show recent speed results for a node",
		"/id - show chat, topic and user IDs",
	)
	if len(cfg.AdminUserIDs) > 0 {
		lines = append(lines,
			"/speedtest - run speed test for online nodes",
			"/speedtest all - run speed test for all nodes",
			"/speedtest <stableId or name> - run speed test for one node",
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
		alerts = fmt.Sprintf("после %d проверок", cfg.AlertAfterFailures)
	}
	adminText := "нет"
	if isAdmin {
		adminText = "да"
	}

	return strings.Join([]string{
		"Invisible Proxy",
		"",
		"Панель управления Xray Checker",
		fmt.Sprintf("Ноды: %d всего | %d online | %d offline", total, online, offline),
		fmt.Sprintf("Speed-test отчеты: %s", speedReports),
		fmt.Sprintf("Оповещения: %s", alerts),
		fmt.Sprintf("Администратор: %s", adminText),
		"",
		"Выберите действие:",
	}, "\n")
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
	proxies := s.proxyChecker.GetProxies()
	sort.Slice(proxies, func(i, j int) bool {
		return strings.ToLower(proxies[i].Name) < strings.ToLower(proxies[j].Name)
	})

	var onlineLines []string
	var offlineLines []string
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		line := formatProxyLine(proxy, online, latency)
		if err != nil || !online {
			offlineLines = append(offlineLines, line)
		} else {
			onlineLines = append(onlineLines, line)
		}
	}

	lines := []string{
		"Node statuses",
		fmt.Sprintf("Total: %d, online: %d, offline: %d", len(proxies), len(onlineLines), len(offlineLines)),
	}
	if len(offlineLines) > 0 {
		lines = append(lines, "", "Offline:")
		lines = append(lines, limitLines(offlineLines, 20)...)
	}
	if len(onlineLines) > 0 {
		lines = append(lines, "", "Online:")
		lines = append(lines, limitLines(onlineLines, 20)...)
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatIssuesSummary() string {
	cfg := s.Config()
	var offlineLines []string
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		if err != nil || !online {
			offlineLines = append(offlineLines, formatProxyLine(proxy, online, latency))
		}
	}

	speedLines := speedIssues(s.speedManager.Snapshot().Results, cfg.LowSpeedThresholdMbps)
	lines := []string{"Проблемные ноды"}
	if len(offlineLines) == 0 && len(speedLines) == 0 {
		lines = append(lines, "", "Проблем не найдено.")
		return strings.Join(lines, "\n")
	}
	if len(offlineLines) > 0 {
		lines = append(lines, "", "Недоступны:")
		lines = append(lines, limitLines(offlineLines, 20)...)
	}
	if len(speedLines) > 0 {
		lines = append(lines, "", "Speed-test:")
		lines = append(lines, limitLines(speedLines, cfg.SpeedReportLimit)...)
	}
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatSpeedHistory(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "Usage: /speed <stableId or name>"
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
		return fmt.Sprintf("No speed-test results for %s yet.", proxy.Name)
	}

	lines := []string{
		fmt.Sprintf("Recent speed results for %s", proxy.Name),
		fmt.Sprintf("ID: %s", proxy.StableID),
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
		return "Пока нет результатов speed-test."
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].CheckedAt.After(results[j].CheckedAt)
	})

	cfg := s.Config()
	lines := []string{"Последние замеры speed-test:"}
	for _, result := range limitResults(results, 10) {
		status := fmt.Sprintf("%.2f Mbps", result.Mbps)
		if result.Error != "" {
			status = "FAILED"
		} else if cfg.LowSpeedThresholdMbps > 0 && result.Mbps < cfg.LowSpeedThresholdMbps {
			status = fmt.Sprintf("LOW %.2f Mbps", result.Mbps)
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", result.Name, status))
	}
	lines = append(lines, "", "Нажмите на ноду ниже, чтобы открыть историю.")
	return trimMessage(strings.Join(lines, "\n"))
}

func (s *Service) formatSpeedReport(report speedtest.RunReport, cfg Config, failed int, slow int) string {
	successful := 0
	for _, result := range report.Results {
		if result.Error == "" {
			successful++
		}
	}

	lines := []string{
		"Speed test report",
		fmt.Sprintf("Source: %s", report.Source),
		fmt.Sprintf("Finished: %s", report.FinishedAt.Format("2006-01-02 15:04:05")),
		fmt.Sprintf("Selected: %d, successful: %d, low: %d, failed: %d", report.Selected, successful, slow, failed),
	}
	if cfg.LowSpeedThresholdMbps > 0 {
		lines = append(lines, fmt.Sprintf("Low speed threshold: %.2f Mbps", cfg.LowSpeedThresholdMbps))
	}

	issues := speedIssues(report.Results, cfg.LowSpeedThresholdMbps)
	if len(issues) > 0 {
		lines = append(lines, "", "Attention:")
		lines = append(lines, limitLines(issues, cfg.SpeedReportLimit)...)
	}

	top := successfulResults(report.Results)
	if len(top) > 0 {
		lines = append(lines, "", "Top results:")
		for _, result := range limitResults(top, cfg.SpeedReportLimit) {
			lines = append(lines, formatSpeedResult(result, cfg.LowSpeedThresholdMbps))
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
	threadID := msg.MessageThreadID
	if threadID == 0 {
		threadID = s.Config().MessageThreadID
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
	defer cancel()
	if err := s.sendTextToWithMarkup(ctx, strconv.FormatInt(msg.Chat.ID, 10), threadID, text, replyMarkup); err != nil {
		logger.Warn("Failed to send Telegram command reply: %v", err)
	}
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

func (s *Service) syncBotCommands() {
	cfg := s.Config()
	if !cfg.Enabled || !cfg.CommandPollingEnabled || cfg.BotToken == "" {
		return
	}
	commands := []map[string]string{
		{"command": "start", "description": "Открыть меню"},
		{"command": "menu", "description": "Открыть меню"},
		{"command": "status", "description": "Статусы серверов"},
		{"command": "speed", "description": "Последние замеры ноды"},
		{"command": "speedtest", "description": "Запустить speed-test"},
		{"command": "id", "description": "Показать ID чата, топика и пользователя"},
		{"command": "help", "description": "Справка по командам"},
	}
	data, err := json.Marshal(commands)
	if err != nil {
		return
	}
	values := url.Values{}
	values.Set("commands", string(data))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.doAPI(ctx, "setMyCommands", values); err != nil {
		logger.Warn("Failed to update Telegram bot commands: %v", err)
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

func speedIssues(results []speedtest.Result, threshold float64) []string {
	var lines []string
	for _, result := range results {
		if result.Error != "" {
			lines = append(lines, fmt.Sprintf("- FAILED %s: %s", result.Name, result.Error))
			continue
		}
		if threshold > 0 && result.Mbps < threshold {
			lines = append(lines, fmt.Sprintf("- LOW %s: %.2f Mbps", result.Name, result.Mbps))
		}
	}
	return lines
}

func successfulResults(results []speedtest.Result) []speedtest.Result {
	var successful []speedtest.Result
	for _, result := range results {
		if result.Error == "" {
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
			{Text: "📊 Статусы", CallbackData: "status"},
			{Text: "📈 Замеры", CallbackData: "speed:list"},
		},
		{
			{Text: "⚠️ Проблемы", CallbackData: "issues"},
			{Text: "🆔 ID", CallbackData: "id"},
		},
	}
	if isAdmin {
		rows = append(rows, []inlineKeyboardButton{
			{Text: "🚀 Speed-test online", CallbackData: "speedtest:online"},
		})
		rows = append(rows, []inlineKeyboardButton{
			{Text: "🧪 Speed-test all", CallbackData: "speedtest:all"},
		})
	}
	rows = append(rows, []inlineKeyboardButton{
		{Text: "🔄 Обновить", CallbackData: "menu:refresh"},
		{Text: "ℹ️ Помощь", CallbackData: "help"},
	})
	return encodeMarkup(rows)
}

func backToMenuMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{{Text: "⬅️ Меню", CallbackData: "menu"}},
	})
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
	rows = append(rows, []inlineKeyboardButton{{Text: "⬅️ Меню", CallbackData: "menu"}})
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

func formatSpeedResult(result speedtest.Result, threshold float64) string {
	marker := "OK"
	if threshold > 0 && result.Mbps < threshold {
		marker = "LOW"
	}
	return fmt.Sprintf("- %s %s: %.2f Mbps, %s, %d ms", marker, result.Name, result.Mbps, formatBytes(result.DownloadedBytes), result.DurationMs)
}

func formatSpeedHistoryLine(result speedtest.Result, threshold float64) string {
	prefix := result.CheckedAt.Format("2006-01-02 15:04:05")
	if result.Error != "" {
		return fmt.Sprintf("- %s: FAILED, %s", prefix, result.Error)
	}
	marker := "OK"
	if threshold > 0 && result.Mbps < threshold {
		marker = "LOW"
	}
	return fmt.Sprintf("- %s: %s %.2f Mbps, %s, %d ms, TTFB %d ms", prefix, marker, result.Mbps, formatBytes(result.DownloadedBytes), result.DurationMs, result.TTFBMs)
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

func formatNodeDown(proxy *models.ProxyConfig, failures int) string {
	return strings.Join([]string{
		"Node unavailable",
		fmt.Sprintf("Name: %s", proxy.Name),
		fmt.Sprintf("ID: %s", proxy.StableID),
		fmt.Sprintf("Protocol: %s", proxy.Protocol),
		fmt.Sprintf("Subscription: %s", proxy.SubName),
		fmt.Sprintf("Consecutive failed checks: %d", failures),
	}, "\n")
}

func formatNodeRecovery(proxy *models.ProxyConfig, latency time.Duration) string {
	latencyText := "n/a"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}
	return strings.Join([]string{
		"Node recovered",
		fmt.Sprintf("Name: %s", proxy.Name),
		fmt.Sprintf("ID: %s", proxy.StableID),
		fmt.Sprintf("Latency: %s", latencyText),
	}, "\n")
}

func shouldRepeatAlert(lastAlert time.Time, repeatMinutes int, now time.Time) bool {
	if lastAlert.IsZero() {
		return true
	}
	return now.Sub(lastAlert) >= time.Duration(repeatMinutes)*time.Minute
}

func formatIDReply(msg *message) string {
	return formatIDReplyFor(msg, msg.From)
}

func formatIDReplyFor(msg *message, from *user) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Chat ID: %d", msg.Chat.ID))
	if msg.MessageThreadID > 0 {
		lines = append(lines, fmt.Sprintf("Topic ID: %d", msg.MessageThreadID))
	}
	if from != nil {
		lines = append(lines, fmt.Sprintf("User ID: %d", from.ID))
	}
	return strings.Join(lines, "\n")
}

func formatProxySearchMiss(matches []*models.ProxyConfig) string {
	if len(matches) == 0 {
		return "Node not found."
	}
	var lines []string
	lines = append(lines, "Several nodes matched. Use stableId:")
	for _, proxy := range matches {
		if len(lines) >= 11 {
			lines = append(lines, fmt.Sprintf("...and %d more", len(matches)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", proxy.StableID, proxy.Name))
	}
	return strings.Join(lines, "\n")
}

func trimMessage(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= 3900 {
		return text
	}
	return text[:3900] + "\n...truncated"
}
