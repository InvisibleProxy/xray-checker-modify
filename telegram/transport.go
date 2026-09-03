package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"xray-checker/logger"
	"xray-checker/models"
)

type formattedMessage struct {
	HTML     string
	RichHTML string
}

type inputRichMessage struct {
	HTML                string `json:"html"`
	SkipEntityDetection bool   `json:"skip_entity_detection,omitempty"`
}

type apiResponse struct {
	OK          bool                `json:"ok"`
	Result      json.RawMessage     `json:"result"`
	Description string              `json:"description"`
	ErrorCode   int                 `json:"error_code"`
	Parameters  *responseParameters `json:"parameters"`
}

type responseParameters struct {
	RetryAfter int `json:"retry_after"`
}

// apiError is a reply that reached us from Telegram itself. Whichever node
// carried the request, the verdict is the same everywhere, so callers must not
// try the next node after seeing one.
type apiError struct {
	Method      string
	ErrorCode   int
	Description string
	RetryAfter  time.Duration
}

func (e *apiError) Error() string {
	text := fmt.Sprintf("Telegram API error on %s: %s", e.Method, e.Description)
	if e.ErrorCode > 0 {
		text = fmt.Sprintf("Telegram API error %d on %s: %s", e.ErrorCode, e.Method, e.Description)
	}
	if e.RetryAfter > 0 {
		text += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return text
}

// deliveryUnknownError marks a request that was written to the wire but whose
// response never came back intact. Telegram has no idempotency key, so a retry
// through another node risks a duplicate message; the send is abandoned instead.
type deliveryUnknownError struct {
	Method string
	Node   string
	Err    error
}

func (e *deliveryUnknownError) Error() string {
	return fmt.Sprintf("%s: %s delivery result is unknown: %v", e.Node, e.Method, e.Err)
}

func (e *deliveryUnknownError) Unwrap() error { return e.Err }

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

type botCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
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

func (s *Service) sendFormattedCommandReplyWithMarkup(msg *message, content formattedMessage, replyMarkup string) {
	s.sendFormattedCommandReply(msg, content, replyMarkup)
}

// sendFormattedCommandReply returns the sent message so a caller that reports
// progress can edit it in place instead of stacking a second message on top.
func (s *Service) sendFormattedCommandReply(msg *message, content formattedMessage, replyMarkup string) *message {
	cfg := s.Config()
	threadID := replyThreadID(msg, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	sent, err := s.sendFormattedToWithMarkup(ctx, strconv.FormatInt(msg.Chat.ID, 10), threadID, content, replyMarkup)
	if err != nil {
		logger.Warn("Failed to send Telegram command reply: %v", err)
		return nil
	}
	if sent == nil || sent.MessageID <= 0 {
		return nil
	}
	sent.Chat = msg.Chat
	return sent
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

func (s *Service) editFormattedCommandMessage(msg *message, content formattedMessage, replyMarkup string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
	defer cancel()
	if err := s.editFormattedWithMarkup(ctx, strconv.FormatInt(msg.Chat.ID, 10), msg.MessageID, content, replyMarkup); err != nil {
		if isMessageNotModified(err) {
			return true
		}
		logger.Warn("Failed to edit Telegram command message: %v", err)
		return false
	}
	return true
}

func (s *Service) sendHTMLToWithMarkup(ctx context.Context, chatID string, threadID int, text string, replyMarkup string) (*message, error) {
	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("text", trimHTMLMessage(text))
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

func (s *Service) sendFormattedToWithMarkup(ctx context.Context, chatID string, threadID int, content formattedMessage, replyMarkup string) (*message, error) {
	if canSendRichMessage(content.RichHTML) && s.richMessageSupport() >= 0 {
		result, err := s.sendRichHTMLToWithMarkup(ctx, chatID, threadID, content.RichHTML, replyMarkup)
		if err == nil {
			s.setRichMessageSupport(1)
			return result, nil
		}
		if !shouldFallbackRichMessage(err) {
			return nil, err
		}
		if richMessageUnsupported(err) {
			s.setRichMessageSupport(-1)
			logger.Warn("Telegram Rich Messages are unavailable; using compact HTML fallback: %v", err)
		} else {
			logger.Warn("Telegram rejected rich message; using compact HTML fallback: %v", err)
		}
	}
	return s.sendHTMLToWithMarkup(ctx, chatID, threadID, content.HTML, replyMarkup)
}

func (s *Service) sendRichHTMLToWithMarkup(ctx context.Context, chatID string, threadID int, richHTML string, replyMarkup string) (*message, error) {
	richMessage, err := json.Marshal(inputRichMessage{
		HTML:                strings.TrimSpace(richHTML),
		SkipEntityDetection: true,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Telegram rich message: %w", err)
	}

	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("rich_message", string(richMessage))
	if threadID > 0 {
		values.Set("message_thread_id", strconv.Itoa(threadID))
	}
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}

	result, err := s.doAPI(ctx, "sendRichMessage", values)
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
	values.Set("text", trimHTMLMessage(text))
	values.Set("parse_mode", "HTML")
	values.Set("disable_web_page_preview", "true")
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}

	_, err := s.doAPI(ctx, "editMessageText", values)
	return err
}

func (s *Service) editFormattedWithMarkup(ctx context.Context, chatID string, messageID int, content formattedMessage, replyMarkup string) error {
	if canSendRichMessage(content.RichHTML) && s.richMessageSupport() >= 0 {
		err := s.editRichHTMLWithMarkup(ctx, chatID, messageID, content.RichHTML, replyMarkup)
		if err == nil {
			s.setRichMessageSupport(1)
			return nil
		}
		// An unchanged screen already shows the rich variant. Falling back here
		// would rewrite it with compact HTML, which *does* differ from what is
		// on screen and would therefore apply, silently downgrading the message
		// every time the operator taps refresh.
		if isMessageNotModified(err) {
			return err
		}
		if !shouldFallbackRichMessage(err) {
			return err
		}
		if richMessageUnsupported(err) {
			s.setRichMessageSupport(-1)
			logger.Warn("Telegram Rich Messages are unavailable; using compact HTML fallback: %v", err)
		} else {
			logger.Warn("Telegram rejected rich message edit; using compact HTML fallback: %v", err)
		}
	}
	return s.editTextWithMarkup(ctx, chatID, messageID, content.HTML, replyMarkup)
}

func (s *Service) editRichHTMLWithMarkup(ctx context.Context, chatID string, messageID int, richHTML string, replyMarkup string) error {
	richMessage, err := json.Marshal(inputRichMessage{
		HTML:                strings.TrimSpace(richHTML),
		SkipEntityDetection: true,
	})
	if err != nil {
		return fmt.Errorf("encode Telegram rich message: %w", err)
	}

	values := url.Values{}
	values.Set("chat_id", chatID)
	values.Set("message_id", strconv.Itoa(messageID))
	values.Set("rich_message", string(richMessage))
	if replyMarkup != "" {
		values.Set("reply_markup", replyMarkup)
	}

	_, err = s.doAPI(ctx, "editMessageText", values)
	return err
}

func (s *Service) richMessageSupport() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.richMessages
}

func (s *Service) setRichMessageSupport(value int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.richMessages = value
}

func canSendRichMessage(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && utf8.RuneCountInString(value) <= maxRichMessageRunes
}

func shouldFallbackRichMessage(err error) bool {
	if err == nil {
		return false
	}
	// A rate limit says nothing about rich message support, and resending the
	// compact variant only adds another request for a bot already throttled.
	if apiErr := asAPIError(err); apiErr != nil {
		if apiErr.RetryAfter > 0 || apiErr.ErrorCode == http.StatusTooManyRequests {
			return false
		}
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "http 400") ||
		strings.Contains(text, "http 404") ||
		strings.Contains(text, "bad request") ||
		strings.Contains(text, "telegram api error")
}

func richMessageUnsupported(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "http 404") ||
		strings.Contains(text, "method not found") ||
		strings.Contains(text, "unknown method")
}

func asAPIError(err error) *apiError {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

// retryAfterFor reports the pause Telegram asked for, if any. Rate limits are
// per-bot rather than per-IP, so waiting is the only useful response; another
// node would collect the same 429.
func retryAfterFor(err error) time.Duration {
	if apiErr := asAPIError(err); apiErr != nil {
		return apiErr.RetryAfter
	}
	return 0
}

// isPollingConflict reports another getUpdates consumer on the same token.
// Retrying silently would hide a second running instance stealing updates.
func isPollingConflict(err error) bool {
	apiErr := asAPIError(err)
	if apiErr == nil {
		return false
	}
	if apiErr.ErrorCode == http.StatusConflict {
		return true
	}
	return strings.Contains(strings.ToLower(apiErr.Description), "terminated by other getupdates")
}

func (s *Service) answerCallback(callbackID string, text string) {
	s.answerCallbackAlert(callbackID, text, false)
}

func (s *Service) answerCallbackAlert(callbackID string, text string, showAlert bool) {
	if callbackID == "" {
		return
	}
	values := url.Values{}
	values.Set("callback_query_id", callbackID)
	if text != "" {
		values.Set("text", text)
	}
	if showAlert {
		values.Set("show_alert", "true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config().TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.doAPI(ctx, "answerCallbackQuery", values); err != nil {
		logger.Warn("Failed to answer Telegram callback: %v", err)
	}
}

// syncBotCommands publishes the command list so clients can autocomplete it.
// The admin-only entry stays in the shared list because a per-admin scope would
// need their private chat IDs, which the checker never learns.
func (s *Service) syncBotCommands(ctx context.Context) error {
	commands := []botCommand{
		{Command: "menu", Description: "Главное меню"},
		{Command: "status", Description: "Статусы нод"},
		{Command: "speed", Description: "История замеров ноды"},
		{Command: "speedtest", Description: "Запустить speed-test (админ)"},
		{Command: "id", Description: "ID чата, топика и пользователя"},
		{Command: "help", Description: "Справка по командам"},
	}
	encoded, err := json.Marshal(commands)
	if err != nil {
		return fmt.Errorf("encode Telegram bot commands: %w", err)
	}

	values := url.Values{}
	values.Set("commands", string(encoded))
	_, err = s.doAPI(ctx, "setMyCommands", values)
	return err
}

func (s *Service) doAPI(ctx context.Context, method string, values url.Values) (json.RawMessage, error) {
	cfg := s.Config()
	if cfg.BotToken == "" {
		return nil, fmt.Errorf("Telegram bot token is empty")
	}

	baseURL := s.apiBaseURL
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	endpoint := baseURL + "/bot" + cfg.BotToken + "/" + method
	candidates := s.orderedProxyCandidates()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available Xray node for Telegram API")
	}

	var lastErr error
	for _, candidate := range candidates {
		client, err := s.httpClientFor(candidate.Proxy)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
		if err != nil {
			return nil, fmt.Errorf("build Telegram request: %s", telegramAPIErrorText(err, cfg.BotToken))
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := client.Do(req)
		if err != nil {
			// Nothing came back, so the node itself is the suspect and the next
			// one is worth trying. A cancelled context is the caller giving up.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s: %s", candidate.Proxy.Name, telegramAPIErrorText(err, cfg.BotToken))
			}
			lastErr = fmt.Errorf("%s: %s", candidate.Proxy.Name, telegramAPIErrorText(err, cfg.BotToken))
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		resp.Body.Close()
		if readErr != nil {
			// The request already reached Telegram and only the answer was
			// lost. Repeating it through another node is how duplicates happen.
			return nil, &deliveryUnknownError{Method: method, Node: candidate.Proxy.Name, Err: readErr}
		}

		var apiResp apiResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			// Not a Telegram answer at all: something on this node's path
			// rewrote the response, so another node is worth trying.
			lastErr = fmt.Errorf("%s: invalid Telegram response (HTTP %d): %v", candidate.Proxy.Name, resp.StatusCode, err)
			continue
		}

		// Telegram answered. The verdict does not depend on which node carried
		// the request, so it is final for every caller and every other node.
		s.rememberWorkingProxy(candidate.Proxy)
		if !apiResp.OK {
			return nil, newAPIError(method, resp.StatusCode, apiResp)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &apiError{
				Method:      method,
				ErrorCode:   resp.StatusCode,
				Description: fmt.Sprintf("HTTP %d", resp.StatusCode),
			}
		}
		return apiResp.Result, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("Telegram API request failed")
	}
	return nil, lastErr
}

func newAPIError(method string, statusCode int, resp apiResponse) *apiError {
	errorCode := resp.ErrorCode
	if errorCode == 0 {
		errorCode = statusCode
	}
	description := strings.TrimSpace(resp.Description)
	if description == "" {
		description = fmt.Sprintf("HTTP %d", statusCode)
	}
	var wait time.Duration
	if resp.Parameters != nil && resp.Parameters.RetryAfter > 0 {
		wait = time.Duration(resp.Parameters.RetryAfter) * time.Second
	}
	return &apiError{
		Method:      method,
		ErrorCode:   errorCode,
		Description: description,
		RetryAfter:  wait,
	}
}

// httpClientFor returns the cached client for a node's SOCKS inbound. Clients
// are reused so the polling loop stops paying for a fresh TLS handshake to
// Telegram every cycle. Deadlines come from the caller's context rather than a
// client timeout, which is what lets long polling have its own budget without
// the short send timeout cutting it off.
func (s *Service) httpClientFor(proxy *models.ProxyConfig) (*http.Client, error) {
	if s.clientFunc != nil {
		return s.clientFunc(proxy)
	}
	port := s.startPort + proxy.Index
	if port <= 0 {
		return nil, fmt.Errorf("invalid SOCKS port for node %s", proxy.Name)
	}

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if client, ok := s.clients[port]; ok {
		return client, nil
	}

	proxyURL, err := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
	if s.clients == nil {
		s.clients = make(map[int]*http.Client)
	}
	s.clients[port] = client
	return client, nil
}

func telegramAPIErrorText(err error, botToken string) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if botToken != "" {
		text = strings.ReplaceAll(text, botToken, "[REDACTED]")
	}
	return text
}
