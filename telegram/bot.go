package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/models"
	"xray-checker/speedtest"
)

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
		s.sendFormattedCommandReplyWithMarkup(msg, s.formatHelpMessage(cfg), backToMenuMarkup())
	case "start", "menu":
		s.sendMenu(msg)
	case "status", "statuses":
		s.sendFormattedCommandReplyWithMarkup(msg, s.formatStatusMessage(), statusMarkup())
	case "speed", "speedresult", "speedhistory":
		s.sendFormattedCommandReplyWithMarkup(msg, s.formatSpeedHistoryMessage(strings.Join(args, " ")), backToMenuMarkup())
	case "speedtest":
		if !s.isAdmin(msg, cfg) {
			s.sendCommandReply(msg, "<b>Нет доступа</b>\n\nSpeed-test может запускать только администратор.")
			return
		}
		s.handleSpeedTestCommand(msg, args)
	default:
		s.sendCommandReply(msg, "<b>Неизвестная команда</b>\n\nИспользуйте /help или откройте /menu.")
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
		s.editFormattedCommandMessage(cb.Message, s.formatStatusMessage(), statusMarkup())
	case data == "status:refresh":
		s.handleStatusRefreshCallback(cb)
	case data == "issues":
		s.answerCallback(cb.ID, "")
		s.editFormattedCommandMessage(cb.Message, s.formatIssuesSummaryMessage(), backToMenuMarkup())
	case data == "nodes:list":
		s.answerCallback(cb.ID, "")
		s.editFormattedCommandMessage(cb.Message, s.formatNodeListMessage(), s.nodeListMarkup())
	case data == "help":
		s.answerCallback(cb.ID, "")
		s.editFormattedCommandMessage(cb.Message, s.formatHelpMessage(cfg), backToMenuMarkup())
	case data == "speed:list":
		s.answerCallback(cb.ID, "")
		s.editFormattedCommandMessage(cb.Message, s.formatRecentSpeedOverviewMessage(), s.speedHistoryMarkup())
	case strings.HasPrefix(data, "node:check:"):
		stableID := strings.TrimPrefix(data, "node:check:")
		s.handleNodeAvailabilityCheckCallback(cb, stableID)
	case strings.HasPrefix(data, "node:test:"):
		stableID := strings.TrimPrefix(data, "node:test:")
		s.handleNodeSpeedTestCallback(cb, stableID)
	case strings.HasPrefix(data, "node:"):
		s.answerCallback(cb.ID, "")
		stableID := strings.TrimPrefix(data, "node:")
		s.editFormattedCommandMessage(cb.Message, s.formatNodeDetailsMessage(stableID), s.nodeDetailMarkup(stableID, s.isAdminUser(cb.From, cfg)))
	case strings.HasPrefix(data, "speed:"):
		s.answerCallback(cb.ID, "")
		query := strings.TrimPrefix(data, "speed:")
		s.editFormattedCommandMessage(cb.Message, s.formatSpeedHistoryMessage(query), backToMenuMarkup())
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

	req := s.newTelegramSpeedTestRunRequest(cb.Message)
	req.ProxyIDs = []string{proxy.StableID}
	req.OnlyOnline = false
	if err := s.runSpeedTest(req, "telegram"); err != nil {
		s.answerCallback(cb.ID, "Не запущено")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())), s.nodeDetailMarkup(proxy.StableID, true))
		}
		return
	}

	s.answerCallback(cb.ID, "Speed-test запущен")
	if cb.Message != nil {
		s.editCommandMessage(cb.Message, fmt.Sprintf("<b>Speed-test запущен</b>\n\nНода: <b>%s</b>\nОтчёт придёт после завершения проверки.", htmlEscape(proxy.Name)), s.nodeDetailMarkup(proxy.StableID, true))
	}
}

func (s *Service) handleSpeedTestCallback(cb *callbackQuery, onlyOnline bool) {
	cfg := s.Config()
	if !s.isAdminUser(cb.From, cfg) {
		s.answerCallback(cb.ID, "Только для администратора")
		return
	}
	req := s.newTelegramSpeedTestRunRequest(cb.Message)
	req.OnlyOnline = onlyOnline
	if err := s.runSpeedTest(req, "telegram"); err != nil {
		s.answerCallback(cb.ID, "Не запущено")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())), backToMenuMarkup())
		}
		return
	}
	s.answerCallback(cb.ID, "Speed-test запущен")
	if cb.Message != nil {
		s.editCommandMessage(cb.Message, "<b>Speed-test запущен</b>\n\nОтчёт придёт после завершения проверки.", backToMenuMarkup())
	}
}

func (s *Service) handleNodeAvailabilityCheckCallback(cb *callbackQuery, stableID string) {
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
	if !s.beginStatusCheck() {
		s.answerCallback(cb.ID, "Проверка уже идёт")
		return
	}

	s.answerCallback(cb.ID, "Проверка запущена")
	if cb.Message != nil {
		s.editFormattedCommandMessage(cb.Message, formatNodeAvailabilityCheckStartedMessage(proxy), s.nodeDetailMarkup(proxy.StableID, true))
	}

	msg := cb.Message
	go func() {
		defer s.endStatusCheck()
		if err := s.runAvailabilityCheck([]string{proxy.StableID}); err != nil {
			if msg != nil {
				s.editCommandMessage(msg, fmt.Sprintf("<b>Проверка не выполнена</b>\n\n%s", htmlEscape(err.Error())), s.nodeDetailMarkup(proxy.StableID, true))
			}
			return
		}
		if msg != nil {
			s.editFormattedCommandMessage(msg, s.formatNodeDetailsMessage(proxy.StableID), s.nodeDetailMarkup(proxy.StableID, true))
		}
	}()
}

func (s *Service) handleStatusRefreshCallback(cb *callbackQuery) {
	if !s.beginStatusCheck() {
		s.answerCallback(cb.ID, "Проверка уже идёт")
		return
	}

	s.answerCallback(cb.ID, "Проверка запущена")
	if cb.Message != nil {
		s.editFormattedCommandMessage(cb.Message, formatStatusRefreshStartedMessage(), statusRefreshMarkup())
	}

	msg := cb.Message
	go func() {
		defer s.endStatusCheck()

		offlineBefore := s.offlineStableIDs()
		if err := s.runAvailabilityCheck(nil); err != nil {
			if msg != nil {
				s.editCommandMessage(msg, fmt.Sprintf("<b>Проверка не выполнена</b>\n\n%s", htmlEscape(err.Error())), statusMarkup())
			}
			return
		}
		s.refreshHostDiagnosticsForStillOffline(offlineBefore)

		if msg != nil {
			s.editFormattedCommandMessage(msg, s.formatStatusMessage(), statusMarkup())
		}
	}()
}

func (s *Service) runAvailabilityCheck(stableIDs []string) error {
	if s.availabilityCheck != nil {
		return s.availabilityCheck(stableIDs)
	}
	if len(stableIDs) == 0 {
		return s.proxyChecker.CheckAllProxies()
	}
	_, err := s.proxyChecker.CheckProxiesByStableIDs(stableIDs)
	return err
}

func (s *Service) offlineStableIDs() map[string]bool {
	result := make(map[string]bool)
	for _, proxy := range s.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		if err == nil && details.IsOffline() {
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
		if err != nil || !details.IsOffline() {
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
		if result.Details.EffectiveStatus() == checker.AvailabilityStateOnline {
			continue
		}
		if s.updateAlertDiagnostics(result.StableID, result.Details) {
			stateChanged = true
		}
	}
	if stateChanged {
		if err := s.saveAlertState(); err != nil {
			logger.Warn("Failed to save Telegram node alert state: %v", err)
		}
	}
}

func (s *Service) updateAlertDiagnostics(stableID string, details checker.ProxyStatusDetails) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.alerts[stableID]
	if state == (nodeAlertState{}) {
		return false
	}

	previous := state
	status := details.EffectiveStatus()
	if status == checker.AvailabilityStateOnline {
		return false
	}
	if previousStatus := nodeAlertStatus(state); previousStatus != "" && previousStatus != status {
		state.Status = status
		if status == checker.AvailabilityStateOffline {
			state.ProxyFailureSince = time.Time{}
			state.DownSince = details.DownSince
			if state.DownSince.IsZero() {
				state.DownSince = time.Now()
			}
		} else {
			state.DownSince = time.Time{}
			state.ProxyFailureSince = details.ProxyFailureSince
			if state.ProxyFailureSince.IsZero() {
				state.ProxyFailureSince = time.Now()
			}
		}
	} else if state.Status == "" {
		state.Status = status
	}
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
	if previous == state {
		return false
	}

	s.alerts[stableID] = state
	return true
}

func (s *Service) sendMenu(msg *message) {
	s.sendMenuToMessage(msg, msg.From)
}

func (s *Service) sendMenuToMessage(msg *message, from *user) {
	cfg := s.Config()
	threadID := replyThreadID(msg, cfg)
	userID := userIDFrom(from)
	content := s.formatMenuMessage(cfg, s.isAdminUser(from, cfg))
	replyMarkup := mainMenuMarkup(s.isAdminUser(from, cfg))
	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	if messageID, ok := s.lastMenuMessageID(msg.Chat.ID, threadID, userID); ok {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
		err := s.editFormattedWithMarkup(ctx, chatID, messageID, content, replyMarkup)
		cancel()
		if err == nil || isMessageNotModified(err) {
			return
		}
		s.forgetMenuMessage(msg.Chat.ID, threadID, userID)
		logger.Warn("Failed to edit previous Telegram menu message: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	sent, err := s.sendFormattedToWithMarkup(ctx, chatID, threadID, content, replyMarkup)
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
	if s.editFormattedCommandMessage(msg, s.formatMenuMessage(cfg, s.isAdminUser(from, cfg)), mainMenuMarkup(s.isAdminUser(from, cfg))) {
		s.rememberMenuMessage(msg.Chat.ID, msg.MessageThreadID, userIDFrom(from), msg.MessageID)
	}
}

func (s *Service) handleSpeedTestCommand(msg *message, args []string) {
	req := s.newTelegramSpeedTestRunRequest(msg)
	req.OnlyOnline = true

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

	if err := s.runSpeedTest(req, "telegram"); err != nil {
		s.sendCommandReply(msg, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())))
		return
	}
	s.sendCommandReply(msg, "<b>Speed-test запущен</b>\n\nОтчёт придёт после завершения проверки.")
}

func (s *Service) newSpeedTestRunRequest() speedtest.RunRequest {
	return speedtest.RunRequest{Config: s.speedManager.Schedule().Config}
}

func (s *Service) newTelegramSpeedTestRunRequest(msg *message) speedtest.RunRequest {
	req := s.newSpeedTestRunRequest()
	if msg == nil {
		return req
	}
	cfg := s.Config()
	req.ReportTarget = speedtest.ReportTarget{
		ChatID:          strconv.FormatInt(msg.Chat.ID, 10),
		MessageThreadID: replyThreadID(msg, cfg),
	}
	return req
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

func (s *Service) isChatAllowed(msg *message, cfg Config) bool {
	if msg == nil {
		return false
	}
	return s.isChatAllowedFor(msg.Chat.ID, msg.From, cfg)
}

func (s *Service) isChatAllowedFor(chatID int64, from *user, cfg Config) bool {
	if cfg.ChatID == "" {
		return false
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
			lines = append(lines, fmt.Sprintf("…и ещё %d", len(matches)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("• %s — <b>%s</b>", htmlCode(proxy.StableID), htmlEscape(proxy.Name)))
	}
	return strings.Join(lines, "\n")
}

func formatProxySearchMissRich(matches []*models.ProxyConfig) string {
	if len(matches) == 0 {
		return "<h2>Нода не найдена</h2><p>Проверьте StableID или часть имени.</p>"
	}
	var items []string
	for _, proxy := range matches {
		items = append(items, fmt.Sprintf("<li><b>%s</b> — <code>%s</code></li>", htmlEscape(proxy.Name), htmlEscape(proxy.StableID)))
	}
	items = limitRichItems(items, 10)
	return "<h2>Найдено несколько нод</h2><p>Уточните запрос по StableID.</p><ul>" + strings.Join(items, "") + "</ul>"
}
