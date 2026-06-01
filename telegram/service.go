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
	UpdateID int      `json:"update_id"`
	Message  *message `json:"message"`
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
		values.Set("allowed_updates", `["message"]`)

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
	case "help", "start":
		s.sendCommandReply(msg, s.formatHelp(cfg))
	case "status", "statuses":
		s.sendCommandReply(msg, s.formatStatus())
	case "speed", "speedresult", "speedhistory":
		s.sendCommandReply(msg, s.formatSpeedHistory(strings.Join(args, " ")))
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
	threadID := msg.MessageThreadID
	if threadID == 0 {
		threadID = s.Config().MessageThreadID
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
	defer cancel()
	if err := s.sendTextTo(ctx, strconv.FormatInt(msg.Chat.ID, 10), threadID, text); err != nil {
		logger.Warn("Failed to send Telegram command reply: %v", err)
	}
}

func (s *Service) sendTextTo(ctx context.Context, chatID string, threadID int, text string) error {
	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("text", trimMessage(text))
	values.Set("disable_web_page_preview", "true")
	if threadID > 0 {
		values.Set("message_thread_id", strconv.Itoa(threadID))
	}

	_, err := s.doAPI(ctx, "sendMessage", values)
	return err
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
	if cfg.ChatID == "" {
		return true
	}
	if cfg.ChatID == strconv.FormatInt(msg.Chat.ID, 10) {
		return true
	}
	return s.isAdmin(msg, cfg)
}

func (s *Service) isAdmin(msg *message, cfg Config) bool {
	if msg.From == nil {
		return false
	}
	for _, id := range cfg.AdminUserIDs {
		if id == msg.From.ID {
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
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.BotToken = v
	}
	if v := os.Getenv("TELEGRAM_CHAT_ID"); v != "" {
		cfg.ChatID = v
	}
	if v := os.Getenv("TELEGRAM_MESSAGE_THREAD_ID"); v != "" {
		cfg.MessageThreadID, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("TELEGRAM_ADMIN_IDS"); v != "" {
		cfg.AdminUserIDs = parseInt64List(v)
	}
	cfg.Normalize()
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
	var lines []string
	lines = append(lines, fmt.Sprintf("Chat ID: %d", msg.Chat.ID))
	if msg.MessageThreadID > 0 {
		lines = append(lines, fmt.Sprintf("Topic ID: %d", msg.MessageThreadID))
	}
	if msg.From != nil {
		lines = append(lines, fmt.Sprintf("User ID: %d", msg.From.ID))
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
