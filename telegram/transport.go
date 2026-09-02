package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"xray-checker/logger"
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
	cfg := s.Config()
	threadID := replyThreadID(msg, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	if _, err := s.sendFormattedToWithMarkup(ctx, strconv.FormatInt(msg.Chat.ID, 10), threadID, content, replyMarkup); err != nil {
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
			return nil, fmt.Errorf("build Telegram request: %s", telegramAPIErrorText(err, cfg.BotToken))
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %s", candidate.Proxy.Name, telegramAPIErrorText(err, cfg.BotToken))
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
