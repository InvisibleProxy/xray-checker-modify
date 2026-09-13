package telegram

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"xray-checker/speedtest"
)

type alertNavigationRequest struct {
	method string
	values url.Values
}

func alertNavigationService(t *testing.T, rejectSend bool) (*Service, <-chan alertNavigationRequest) {
	t.Helper()
	requests := make(chan alertNavigationRequest, 32)
	s, _ := transportService(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		requests <- alertNavigationRequest{method, r.PostForm}
		w.Header().Set("Content-Type", "application/json")
		if rejectSend && strings.HasPrefix(method, "send") {
			json.NewEncoder(w).Encode(apiResponse{OK: false, ErrorCode: 400, Description: "Bad Request: chat not found"})
			return
		}
		json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`{"message_id":42}`)})
	}, 1)
	s.speedManager = speedtest.NewManager(s.proxyChecker, 10000, "", speedtest.TestConfig{})
	cfg := s.Config()
	cfg.AdminUserIDs = []int64{2}
	cfg.MessageThreadID = 99 // The source topic must override this setting.
	s.setConfig(cfg)
	return s, requests
}

func nextAlertNavigationRequest(t *testing.T, requests <-chan alertNavigationRequest) alertNavigationRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Telegram request")
		return alertNavigationRequest{}
	}
}

func TestAlertNavigationPreservesOriginal(t *testing.T) {
	for _, action := range []string{"back_to_menu", "issues", "speed:list", "node:", "speed:", "node:mutemenu:"} {
		for _, rich := range []bool{false, true} {
			t.Run(action+"/rich="+strconv.FormatBool(rich), func(t *testing.T) {
				s, requests := alertNavigationService(t, false)
				if !rich {
					s.setRichMessageSupport(-1)
				}
				data := action
				if strings.HasSuffix(data, ":") {
					data += s.sortedProxies()[0].StableID
				}
				original := &message{MessageID: 7, MessageThreadID: 13, Chat: chat{ID: 1}, Text: "Original alert"}
				s.handleCallback(&callbackQuery{ID: "cb", From: &user{ID: 2}, Message: original, Data: alertCallbackPrefix + data})
				if req := nextAlertNavigationRequest(t, requests); req.method != "answerCallbackQuery" {
					t.Fatalf("expected callback answer, got %s", req.method)
				}
				req := nextAlertNavigationRequest(t, requests)
				if !strings.HasPrefix(req.method, "send") || req.values.Get("chat_id") != "1" || req.values.Get("message_thread_id") != "13" {
					t.Fatalf("expected a separate message in the source topic, got %+v", req)
				}
				if strings.Contains(req.values.Get("reply_markup"), alertCallbackPrefix) {
					t.Fatal("navigation reply must use ordinary callbacks")
				}
				if original.MessageID != 7 || original.Text != "Original alert" || original.sendAsNew {
					t.Fatalf("source alert was mutated: %+v", original)
				}
				// The next click edits the new message, not the original alert.
				s.handleCallback(&callbackQuery{ID: "next", From: &user{ID: 2}, Message: &message{MessageID: 42, MessageThreadID: 13, Chat: chat{ID: 1}}, Data: "back_to_menu"})
				nextAlertNavigationRequest(t, requests)
				req = nextAlertNavigationRequest(t, requests)
				if !strings.HasPrefix(req.method, "edit") || req.values.Get("message_id") != "42" {
					t.Fatalf("expected navigation to edit message 42, got %+v", req)
				}
			})
		}
	}
}

func TestAlertCheckCompletionEditsSeparateMessage(t *testing.T) {
	s, requests := alertNavigationService(t, false)
	s.setRichMessageSupport(-1)
	s.SetAvailabilityCheckFunc(func([]string) error { return errors.New("test check failure") })
	s.handleCallback(&callbackQuery{ID: "check", From: &user{ID: 2}, Message: &message{MessageID: 7, Chat: chat{ID: 1}, MessageThreadID: 13}, Data: alertCallbackPrefix + "node:check:" + s.sortedProxies()[0].StableID})
	nextAlertNavigationRequest(t, requests)
	if req := nextAlertNavigationRequest(t, requests); req.method != "sendMessage" || req.values.Get("message_thread_id") != "13" {
		t.Fatalf("expected separate progress message, got %+v", req)
	}
	if req := nextAlertNavigationRequest(t, requests); req.method != "editMessageText" || req.values.Get("message_id") != "42" || !strings.Contains(req.values.Get("text"), "test check failure") {
		t.Fatalf("expected completion on the new message, got %+v", req)
	}
}

func TestAlertReplyFailureNeverEditsOriginalOrRetries(t *testing.T) {
	s, requests := alertNavigationService(t, true)
	msg := &message{MessageID: 7, Chat: chat{ID: 1}, sendAsNew: true}
	if s.editCommandMessage(msg, "progress", "") {
		t.Fatal("expected send failure")
	}
	if req := nextAlertNavigationRequest(t, requests); req.method != "sendMessage" || req.values.Get("message_thread_id") != "" {
		t.Fatalf("expected send in source chat without configured topic fallback, got %+v", req)
	}
	if s.editFormattedCommandMessage(msg, formattedMessage{HTML: "completion"}, "") {
		t.Fatal("must not retry after a failed send")
	}
	if msg.MessageID != 7 {
		t.Fatal("original message ID changed")
	}
	select {
	case req := <-requests:
		t.Fatalf("unexpected request after send failure: %+v", req)
	default:
	}
}

func TestAlertDeliveryMarksEveryButton(t *testing.T) {
	for _, kind := range []string{"node", "node-fallback", "speed", "speed-fallback"} {
		t.Run(kind, func(t *testing.T) {
			s, requests := alertNavigationService(t, false)
			content := formattedMessage{HTML: "alert"}
			switch kind {
			case "node", "node-fallback":
				id := "abc"
				if kind == "node-fallback" {
					id = ""
				}
				if err := s.sendNodeAlertMessageWithMarkup(s.Config(), content, nodeAlertMarkup(id)); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "speed" {
					content.ReplyMarkup = speedReportMarkup([]speedtest.Result{{StableID: "abc", Name: "Node"}})
				}
				s.sendSpeedTestReport(s.Config(), "1", 13, content)
			}
			req := nextAlertNavigationRequest(t, requests)
			var markup inlineKeyboardMarkup
			if err := json.Unmarshal([]byte(req.values.Get("reply_markup")), &markup); err != nil {
				t.Fatal(err)
			}
			if len(markup.InlineKeyboard) == 0 {
				t.Fatal("missing alert buttons")
			}
			for _, row := range markup.InlineKeyboard {
				for _, button := range row {
					if !strings.HasPrefix(button.CallbackData, alertCallbackPrefix) || len(button.CallbackData) > 64 {
						t.Fatalf("unsafe alert button: %+v", button)
					}
				}
			}
		})
	}
}

func TestAlertMarkupDoesNotTruncateLongIdentities(t *testing.T) {
	markup := alertMarkup(nodeAlertMarkup(strings.Repeat("x", 64)))
	if markup != "" {
		t.Fatalf("oversized actions must be omitted, got %s", markup)
	}
}
