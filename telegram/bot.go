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
	commandsPublished := false
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		cfg := s.Config()
		if !cfg.Enabled || !cfg.CommandPollingEnabled || cfg.BotToken == "" {
			commandsPublished = false
			if !s.wait(pollDisabledInterval) {
				return
			}
			continue
		}

		if !commandsPublished {
			commandsPublished = s.publishBotCommands(cfg)
		}

		values := url.Values{}
		values.Set("timeout", strconv.Itoa(pollTimeoutSec))
		values.Set("offset", strconv.Itoa(s.nextUpdateOffset()))
		values.Set("allowed_updates", `["message","callback_query"]`)

		// The long poll needs a budget of its own: the send timeout would cut it
		// off mid-poll and waste the entire cycle.
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(pollTimeoutSec+cfg.TimeoutSec)*time.Second)
		result, err := s.doAPI(ctx, "getUpdates", values)
		cancel()
		if err != nil {
			if !s.wait(pollBackoffFor(err)) {
				return
			}
			continue
		}

		var updates []update
		if err := json.Unmarshal(result, &updates); err != nil {
			logger.Warn("Failed to parse Telegram updates: %v", err)
			if !s.wait(pollNetworkBackoff) {
				return
			}
			continue
		}

		for _, upd := range updates {
			s.markUpdateSeen(upd.UpdateID)
			s.handleUpdateSafely(upd)
		}
	}
}

// pollBackoffFor logs the failure and reports how long to wait. A rate limit
// names its own pause, and a conflict means a second process is consuming the
// same token — retrying quietly would only hide it.
func pollBackoffFor(err error) time.Duration {
	if wait := retryAfterFor(err); wait > 0 {
		logger.Warn("Telegram asked to slow down; pausing polling for %s: %v", wait, err)
		if wait > maxPollBackoff {
			return maxPollBackoff
		}
		return wait
	}
	if isPollingConflict(err) {
		logger.Warn("Another getUpdates consumer is using this bot token; only one checker instance may poll it: %v", err)
		return pollConflictBackoff
	}
	logger.Warn("Telegram polling failed: %v", err)
	return pollNetworkBackoff
}

func (s *Service) publishBotCommands(cfg Config) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if err := s.syncBotCommands(ctx); err != nil {
		logger.Warn("Failed to publish Telegram bot commands: %v", err)
		return false
	}
	return true
}

// handleUpdateSafely keeps a malformed update from taking the process down.
// Command text and callback data are the only externally supplied input this
// binary parses, and a panic here would stop Xray and user traffic with it.
func (s *Service) handleUpdateSafely(upd update) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("Recovered from panic while handling Telegram update %d: %v", upd.UpdateID, recovered)
		}
	}()
	s.handleUpdate(upd)
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
		// /id has to answer before the chat check, otherwise nobody could learn
		// the IDs needed to configure the bot. That makes it the one command a
		// stranger can reach, and every reply spends a request through a
		// monitored node, so it is rate limited instead.
		if s.allowIDReply(msg, time.Now()) {
			s.sendFormattedCommandReplyWithMarkup(msg, formatIDReplyMessage(msg, msg.From), backToMenuMarkup())
		}
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
	case "nodes":
		s.sendFormattedCommandReplyWithMarkup(msg, s.formatNodeListMessage(1), s.nodeListMarkup(1))
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

// allowIDReply throttles the only command a stranger can reach, per user and
// globally, so the bot cannot be turned into a traffic sink on your own nodes.
func (s *Service) allowIDReply(msg *message, now time.Time) bool {
	if msg == nil {
		return false
	}

	s.throttleMu.Lock()
	defer s.throttleMu.Unlock()
	if !s.lastAnyReply.IsZero() && now.Sub(s.lastAnyReply) < idReplyGlobalInterval {
		return false
	}
	if s.lastIDReply == nil {
		s.lastIDReply = make(map[int64]time.Time)
	}
	// Fall back to the chat when the sender is anonymous, so a channel post
	// cannot bypass the per-sender window.
	key := msg.Chat.ID
	if msg.From != nil {
		key = msg.From.ID
	}
	if last, ok := s.lastIDReply[key]; ok && now.Sub(last) < idReplyPerUserInterval {
		return false
	}
	for id, seen := range s.lastIDReply {
		if now.Sub(seen) >= idReplyPerUserInterval {
			delete(s.lastIDReply, id)
		}
	}
	s.lastIDReply[key] = now
	s.lastAnyReply = now
	return true
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
			s.editFormattedCommandMessage(cb.Message, formatIDReplyMessage(cb.Message, cb.From), backToMenuMarkup())
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
	case data == "nodes:list" || strings.HasPrefix(data, "nodes:list:"):
		s.answerCallback(cb.ID, "")
		page := parsePage(strings.TrimPrefix(strings.TrimPrefix(data, "nodes:list"), ":"))
		s.editFormattedCommandMessage(cb.Message, s.formatNodeListMessage(page), s.nodeListMarkup(page))
	case data == "help":
		s.answerCallback(cb.ID, "")
		s.editFormattedCommandMessage(cb.Message, s.formatHelpMessage(cfg), backToMenuMarkup())
	case data == "speed:list" || strings.HasPrefix(data, "speed:list:"):
		s.answerCallback(cb.ID, "")
		// The message lists every node grouped by status; only the keyboard
		// pages, so the text stays the same across pages.
		page := parsePage(strings.TrimPrefix(strings.TrimPrefix(data, "speed:list"), ":"))
		s.editFormattedCommandMessage(cb.Message, s.formatRecentSpeedOverviewMessage(), s.speedHistoryMarkup(page))
	case data == "noop":
		s.answerCallback(cb.ID, "")
	case strings.HasPrefix(data, "node:check:"):
		stableID := strings.TrimPrefix(data, "node:check:")
		s.handleNodeAvailabilityCheckCallback(cb, stableID)
	case strings.HasPrefix(data, "node:test:"):
		stableID := strings.TrimPrefix(data, "node:test:")
		s.handleNodeSpeedTestCallback(cb, stableID)
	case strings.HasPrefix(data, "node:mutemenu:"):
		s.answerCallback(cb.ID, "")
		stableID := strings.TrimPrefix(data, "node:mutemenu:")
		s.showNodeMuteMenu(cb, stableID)
	case strings.HasPrefix(data, "node:mute:"):
		s.handleNodeMuteCallback(cb, strings.TrimPrefix(data, "node:mute:"))
	case strings.HasPrefix(data, "node:unmute:"):
		s.handleNodeUnmuteCallback(cb, strings.TrimPrefix(data, "node:unmute:"))
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

func parsePage(value string) int {
	page, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || page < 1 {
		return 1
	}
	return page
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
			s.editCommandMessage(cb.Message, formatProxySearchMiss(matches), s.nodeListMarkup(1))
		}
		return
	}

	req := s.newTelegramSpeedTestRunRequest(cb.Message)
	req.ProxyIDs = []string{proxy.StableID}
	req.OnlyOnline = false
	s.startSpeedTestFromCallback(cb, req, proxy.Name, s.nodeDetailMarkup(proxy.StableID, true))
}

func (s *Service) handleSpeedTestCallback(cb *callbackQuery, onlyOnline bool) {
	cfg := s.Config()
	if !s.isAdminUser(cb.From, cfg) {
		s.answerCallback(cb.ID, "Только для администратора")
		return
	}
	req := s.newTelegramSpeedTestRunRequest(cb.Message)
	req.OnlyOnline = onlyOnline
	s.startSpeedTestFromCallback(cb, req, "", backToMenuMarkup())
}

// startSpeedTestFromCallback answers the callback before doing any work. A
// Telegram speed-test first runs an availability check, which walks every
// selected node; doing that inline would leave the button spinning and block
// the polling loop from serving anyone else in the meantime.
func (s *Service) startSpeedTestFromCallback(cb *callbackQuery, req speedtest.RunRequest, nodeName string, markup string) {
	if !s.beginStatusCheck() {
		s.answerCallback(cb.ID, "Проверка уже идёт")
		return
	}

	s.answerCallback(cb.ID, "Speed-test запускается")
	msg := cb.Message
	if msg != nil {
		s.editFormattedCommandMessage(msg, formatSpeedTestStartingMessage(nodeName), markup)
	}

	go func() {
		defer s.endStatusCheck()
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("Recovered from panic while starting Telegram speed-test: %v", recovered)
			}
		}()

		if err := s.runSpeedTest(req, "telegram"); err != nil {
			if msg != nil {
				s.editCommandMessage(msg, fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error())), markup)
			}
			return
		}
		if msg != nil {
			s.editFormattedCommandMessage(msg, formatSpeedTestStartedMessage(nodeName), markup)
		}
	}()
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
			s.editCommandMessage(cb.Message, formatProxySearchMiss(matches), s.nodeListMarkup(1))
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

	// Serialised against the alert pass, like every other writer of alert state.
	// Without this, an operator refreshing statuses could flip a node between
	// offline and proxy_failure between the moment a down-alert is rendered and
	// the moment its delivery is confirmed. confirmNodeDownAlertsSent refuses to
	// confirm across a status change, so the reminder schedule would not move
	// and the next pass would send the same alert again.
	s.nodeNotifyMu.Lock()
	defer s.nodeNotifyMu.Unlock()

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

	nodeName := ""
	if len(queryParts) > 0 {
		proxy, matches := s.findProxy(strings.Join(queryParts, " "))
		if proxy == nil {
			s.sendCommandReply(msg, formatProxySearchMiss(matches))
			return
		}
		req.ProxyIDs = []string{proxy.StableID}
		req.OnlyOnline = false
		nodeName = proxy.Name
	}

	// The availability check that precedes the run is not something to hold the
	// polling loop for; acknowledge first and report the outcome afterwards.
	if !s.beginStatusCheck() {
		s.sendCommandReply(msg, "<b>Проверка уже идёт</b>\n\nДождитесь завершения текущей проверки.")
		return
	}
	progress := s.sendFormattedCommandReply(msg, formatSpeedTestStartingMessage(nodeName), "")

	go func() {
		defer s.endStatusCheck()
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("Recovered from panic while starting Telegram speed-test: %v", recovered)
			}
		}()

		if err := s.runSpeedTest(req, "telegram"); err != nil {
			failure := fmt.Sprintf("<b>Speed-test не запущен</b>\n\n%s", htmlEscape(err.Error()))
			if progress != nil {
				s.editCommandMessage(progress, failure, backToMenuMarkup())
				return
			}
			s.sendCommandReply(msg, failure)
			return
		}
		if progress != nil {
			s.editFormattedCommandMessage(progress, formatSpeedTestStartedMessage(nodeName), backToMenuMarkup())
			return
		}
		s.sendFormattedCommandReplyWithMarkup(msg, formatSpeedTestStartedMessage(nodeName), backToMenuMarkup())
	}()
}

func (s *Service) showNodeMuteMenu(cb *callbackQuery, stableID string) {
	proxy, matches := s.findProxy(stableID)
	if proxy == nil {
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, formatProxySearchMiss(matches), s.nodeListMarkup(1))
		}
		return
	}
	if cb.Message == nil {
		return
	}
	cfg := s.Config()
	status := s.nodeMuteStatusFor(proxy.StableID, cfg)
	s.editFormattedCommandMessage(cb.Message, formatNodeMuteMenuMessage(proxy, status), nodeMuteMarkup(proxy.StableID, status))
}

// handleNodeMuteCallback parses "<scope>:<minutes>:<stableID>". Zero minutes
// means a permanent mute, which is stored in the editable config so the admin
// UI shows it alongside the mutes set there.
func (s *Service) handleNodeMuteCallback(cb *callbackQuery, payload string) {
	cfg := s.Config()
	if !s.isAdminUser(cb.From, cfg) {
		s.answerCallback(cb.ID, "Только для администратора")
		return
	}

	parts := strings.SplitN(payload, ":", 3)
	if len(parts) != 3 {
		s.answerCallback(cb.ID, "Неизвестное действие")
		return
	}
	scope := normalizeMuteScope(parts[0])
	minutes, err := strconv.Atoi(parts[1])
	if scope == "" || err != nil || minutes < 0 {
		s.answerCallback(cb.ID, "Неизвестное действие")
		return
	}

	proxy, matches := s.findProxy(parts[2])
	if proxy == nil {
		s.answerCallback(cb.ID, "Нода не найдена")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, formatProxySearchMiss(matches), s.nodeListMarkup(1))
		}
		return
	}

	if err := s.muteNodeFor(proxy.StableID, scope, time.Duration(minutes)*time.Minute); err != nil {
		s.answerCallback(cb.ID, "Не сохранено")
		logger.Warn("Failed to mute node %s from Telegram: %v", proxy.StableID, err)
		return
	}

	s.answerCallback(cb.ID, muteConfirmationText(minutes))
	if cb.Message != nil {
		status := s.nodeMuteStatusFor(proxy.StableID, s.Config())
		s.editFormattedCommandMessage(cb.Message, formatNodeMuteMenuMessage(proxy, status), nodeMuteMarkup(proxy.StableID, status))
	}
}

func (s *Service) handleNodeUnmuteCallback(cb *callbackQuery, stableID string) {
	cfg := s.Config()
	if !s.isAdminUser(cb.From, cfg) {
		s.answerCallback(cb.ID, "Только для администратора")
		return
	}

	proxy, matches := s.findProxy(stableID)
	if proxy == nil {
		s.answerCallback(cb.ID, "Нода не найдена")
		if cb.Message != nil {
			s.editCommandMessage(cb.Message, formatProxySearchMiss(matches), s.nodeListMarkup(1))
		}
		return
	}

	if err := s.unmuteNode(proxy.StableID); err != nil {
		s.answerCallback(cb.ID, "Не сохранено")
		logger.Warn("Failed to unmute node %s from Telegram: %v", proxy.StableID, err)
		return
	}

	s.answerCallback(cb.ID, "Уведомления включены")
	if cb.Message != nil {
		status := s.nodeMuteStatusFor(proxy.StableID, s.Config())
		s.editFormattedCommandMessage(cb.Message, formatNodeMuteMenuMessage(proxy, status), nodeMuteMarkup(proxy.StableID, status))
	}
}

func muteConfirmationText(minutes int) string {
	if minutes <= 0 {
		return "Уведомления выключены"
	}
	return "Тишина до " + time.Now().Add(time.Duration(minutes)*time.Minute).Format("15:04")
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

func formatIDReplyMessage(msg *message, from *user) formattedMessage {
	fallback := formatIDReplyFor(msg, from)
	var rows []string
	rows = append(rows, fmt.Sprintf("<tr><th>Chat ID</th><td><code>%s</code></td></tr>", htmlEscape(strconv.FormatInt(msg.Chat.ID, 10))))
	if msg.MessageThreadID > 0 {
		rows = append(rows, fmt.Sprintf("<tr><th>Topic ID</th><td><code>%d</code></td></tr>", msg.MessageThreadID))
	}
	if from != nil {
		rows = append(rows, fmt.Sprintf("<tr><th>User ID</th><td><code>%s</code></td></tr>", htmlEscape(strconv.FormatInt(from.ID, 10))))
	}
	rich := "<h2>Telegram IDs</h2><table bordered>" + strings.Join(rows, "") + "</table>"
	return formattedMessage{HTML: fallback, RichHTML: rich}
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
