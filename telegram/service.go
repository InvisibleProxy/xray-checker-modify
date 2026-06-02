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
	defaultTimeoutSec         = 20
	defaultSpeedReportLimit   = 10
	defaultAlertAfterFailures = 1
	defaultAlertRepeatMinutes = 60
	maxSpeedReportLimit       = 50
	menuSpeedButtonLimit      = 8
)

type Config struct {
	Enabled               bool    `json:"enabled"`
	BotToken              string  `json:"botToken"`
	ChatID                string  `json:"chatId"`
	MessageThreadID       int     `json:"messageThreadId"`
	AdminUserIDs          []int64 `json:"adminUserIds"`
	CommandPollingEnabled bool    `json:"commandPollingEnabled"`
	SpeedReportsEnabled   bool    `json:"speedReportsEnabled"`
	SpeedReportMode       string  `json:"speedReportMode"`
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
	SpeedReportsEnabled     bool    `json:"speedReportsEnabled"`
	SpeedReportMode         string  `json:"speedReportMode"`
	LowSpeedThresholdMbps   float64 `json:"lowSpeedThresholdMbps"`
	SpeedReportLimit        int     `json:"speedReportLimit"`
	NodeAlertsEnabled       bool    `json:"nodeAlertsEnabled"`
	AlertAfterFailures      int     `json:"alertAfterFailures"`
	AlertRepeatMinutes      int     `json:"alertRepeatMinutes"`
	NotifyRecovery          bool    `json:"notifyRecovery"`
	BotTokenConfigured      bool    `json:"botTokenConfigured"`
	ChatConfigured          bool    `json:"chatConfigured"`
	MessageThreadConfigured bool    `json:"messageThreadConfigured"`
	AdminUserCount          int     `json:"adminUserCount"`
}

func DefaultConfig() Config {
	return Config{
		CommandPollingEnabled: true,
		SpeedReportsEnabled:   true,
		SpeedReportMode:       "always",
		SpeedReportLimit:      defaultSpeedReportLimit,
		NodeAlertsEnabled:     true,
		AlertAfterFailures:    defaultAlertAfterFailures,
		AlertRepeatMinutes:    defaultAlertRepeatMinutes,
		NotifyRecovery:        true,
		TimeoutSec:            defaultTimeoutSec,
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
	menuMessages map[string]int
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
	return &Service{
		proxyChecker: proxyChecker,
		speedManager: speedManager,
		startPort:    startPort,
		statePath:    statePath,
		config:       DefaultConfig(),
		alerts:       make(map[string]nodeAlertState),
		menuMessages: make(map[string]int),
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
		SpeedReportsEnabled:     cfg.SpeedReportsEnabled,
		SpeedReportMode:         cfg.SpeedReportMode,
		LowSpeedThresholdMbps:   cfg.LowSpeedThresholdMbps,
		SpeedReportLimit:        cfg.SpeedReportLimit,
		NodeAlertsEnabled:       cfg.NodeAlertsEnabled,
		AlertAfterFailures:      cfg.AlertAfterFailures,
		AlertRepeatMinutes:      cfg.AlertRepeatMinutes,
		NotifyRecovery:          cfg.NotifyRecovery,
		BotTokenConfigured:      cfg.BotToken != "",
		ChatConfigured:          cfg.ChatID != "",
		MessageThreadConfigured: cfg.MessageThreadID > 0,
		AdminUserCount:          len(cfg.AdminUserIDs),
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

	if _, err := s.sendHTMLToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, text, backToMenuMarkup()); err != nil {
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
		s.editCommandMessage(cb.Message, s.formatStatus(), backToMenuMarkup())
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

func (s *Service) sendMenu(msg *message) {
	s.sendMenuToMessage(msg, msg.From)
}

func (s *Service) sendMenuToMessage(msg *message, from *user) {
	cfg := s.Config()
	threadID := msg.MessageThreadID
	if threadID == 0 {
		threadID = cfg.MessageThreadID
	}
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
		alerts = fmt.Sprintf("после %d проверок", cfg.AlertAfterFailures)
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
	online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
	status := "ONLINE"
	if err != nil || !online {
		status = "OFFLINE"
	}
	latencyText := "n/a"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
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
		online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		line := formatProxyLineHTML(proxy, online, latency)
		if err != nil || !online {
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

func (s *Service) formatIssuesSummary() string {
	cfg := s.Config()
	var offlineLines []string
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		online, latency, err := s.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
		if err != nil || !online {
			offlineLines = append(offlineLines, formatProxyLineHTML(proxy, online, latency))
		}
	}

	speedLines := speedIssuesHTML(s.speedManager.Snapshot().Results, cfg.LowSpeedThresholdMbps)
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
		if result.Error != "" {
			status = "<b>FAILED</b>"
		} else if cfg.LowSpeedThresholdMbps > 0 && result.Mbps < cfg.LowSpeedThresholdMbps {
			status = fmt.Sprintf("<b>LOW %.2f Mbps</b>", result.Mbps)
		}
		lines = append(lines, fmt.Sprintf("• <b>%s</b>\n  <code>%s</code> · %s · %s", htmlEscape(result.Name), htmlEscape(result.StableID), status, htmlEscape(formatCheckedAt(result.CheckedAt))))
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

	issues := speedIssuesHTML(report.Results, cfg.LowSpeedThresholdMbps)
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
	threadID := msg.MessageThreadID
	if threadID == 0 {
		threadID = s.Config().MessageThreadID
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
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

func speedIssuesHTML(results []speedtest.Result, threshold float64) []string {
	var lines []string
	for _, result := range results {
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
	if threshold <= 0 || result.Mbps >= threshold {
		return fmt.Sprintf("• <b>%s</b>\n  ✅ <b>%.2f Mbps</b>", htmlEscape(result.Name), result.Mbps)
	}

	ttfbText := ""
	if result.TTFBMs > 0 {
		ttfbText = fmt.Sprintf(" · TTFB %d ms", result.TTFBMs)
	}
	return fmt.Sprintf("• <b>%s</b>\n  ⚠️ <b>%.2f Mbps</b> · %s · %d ms%s", htmlEscape(result.Name), result.Mbps, htmlEscape(formatBytes(result.DownloadedBytes)), result.DurationMs, ttfbText)
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
	if result.Error != "" {
		return fmt.Sprintf("• %s\n  <b>FAILED</b> · %s", htmlCode(prefix), htmlEscape(result.Error))
	}
	marker := "OK"
	if threshold > 0 && result.Mbps < threshold {
		marker = "LOW"
	}
	return fmt.Sprintf("• %s\n  <b>%s %.2f Mbps</b> · %s · %d ms · TTFB %d ms", htmlCode(prefix), marker, result.Mbps, htmlEscape(formatBytes(result.DownloadedBytes)), result.DurationMs, result.TTFBMs)
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

func formatProxyLineHTML(proxy *models.ProxyConfig, online bool, latency time.Duration) string {
	status := "OFFLINE"
	if online {
		status = "ONLINE"
	}
	latencyText := "n/a"
	if latency > 0 {
		latencyText = fmt.Sprintf("%d ms", latency.Milliseconds())
	}
	return fmt.Sprintf("• <b>%s</b> <b>%s</b>\n  %s · %s · %s", htmlEscape(status), htmlEscape(proxy.Name), htmlCode(proxy.StableID), htmlCode(proxy.Protocol), htmlEscape(latencyText))
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
