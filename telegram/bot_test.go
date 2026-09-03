package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xray-checker/checker"
	"xray-checker/models"
)

func testProxies(names ...string) []*models.ProxyConfig {
	proxies := make([]*models.ProxyConfig, 0, len(names))
	for i, name := range names {
		proxy := &models.ProxyConfig{
			Protocol: "vless",
			Server:   name + ".example.com",
			Port:     443,
			Name:     name,
			UUID:     "uuid-" + name,
			Index:    i,
		}
		proxy.StableID = proxy.GenerateStableID()
		proxies = append(proxies, proxy)
	}
	return proxies
}

func testChecker(proxies []*models.ProxyConfig) *checker.ProxyChecker {
	return checker.NewProxyChecker(proxies, 10000, "", 1, "", "", 1, 0, "status")
}

// transportService wires the service to a local HTTP server so the candidate
// loop can be observed without any real node or Telegram access.
func transportService(t *testing.T, handler http.HandlerFunc, nodeCount int) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	names := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		names = append(names, string(rune('a'+i)))
	}
	proxies := testProxies(names...)
	service := NewService("", testChecker(proxies), nil, 10000)
	service.apiBaseURL = server.URL
	service.clientFunc = func(*models.ProxyConfig) (*http.Client, error) {
		return server.Client(), nil
	}
	service.setConfig(Config{Enabled: true, BotToken: "token", ChatID: "1", TimeoutSec: 5})
	return service, server
}

func TestTelegramReplyStopsCandidateLoop(t *testing.T) {
	var requests int64
	service, _ := transportService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(apiResponse{
			OK:          false,
			ErrorCode:   http.StatusTooManyRequests,
			Description: "Too Many Requests: retry later",
			Parameters:  &responseParameters{RetryAfter: 7},
		})
	}, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := service.doAPI(ctx, "sendMessage", nil)
	if err == nil {
		t.Fatal("expected the rate limit to surface as an error")
	}
	// A 429 is per-bot: walking the remaining nodes would only deepen it.
	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("expected exactly one request after a Telegram reply, got %d", got)
	}
	if wait := retryAfterFor(err); wait != 7*time.Second {
		t.Fatalf("expected retry_after to be parsed as 7s, got %s", wait)
	}
}

func TestTransportFailureTriesNextNode(t *testing.T) {
	var requests int64
	service, _ := transportService(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&requests, 1) == 1 {
			// Close without a reply: nothing reached Telegram through this node.
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, err := hijacker.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`{"message_id":5}`)})
	}, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := service.doAPI(ctx, "sendMessage", nil); err != nil {
		t.Fatalf("expected the second node to carry the request: %v", err)
	}
	if got := atomic.LoadInt64(&requests); got != 2 {
		t.Fatalf("expected the failed node to be retried once on the next node, got %d requests", got)
	}
}

func TestUnknownDeliveryIsNotRetriedOnAnotherNode(t *testing.T) {
	var requests int64
	service, _ := transportService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requests, 1)
		// Announce a body and then cut it: Telegram already got the request, so
		// only the answer was lost.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "512")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				conn.Close()
			}
		}
	}, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := service.doAPI(ctx, "sendMessage", nil)
	if err == nil {
		t.Fatal("expected an unknown delivery result to surface as an error")
	}
	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("a send whose result is unknown must not be repeated; got %d requests", got)
	}
	if !strings.Contains(err.Error(), "delivery result is unknown") {
		t.Fatalf("expected an unknown-delivery error, got %v", err)
	}
}

func TestNotModifiedEditKeepsRichMessage(t *testing.T) {
	var methods []string
	service, _ := transportService(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(apiResponse{
			OK:          false,
			ErrorCode:   http.StatusBadRequest,
			Description: "Bad Request: message is not modified",
		})
	}, 1)

	content := formattedMessage{HTML: "<b>plain</b>", RichHTML: "<h2>rich</h2>"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := service.editFormattedWithMarkup(ctx, "1", 42, content, "")
	if !isMessageNotModified(err) {
		t.Fatalf("expected the not-modified error to be returned, got %v", err)
	}
	// One call only: a compact fallback edit differs from what is on screen and
	// would apply, silently downgrading a rich message on every refresh.
	if len(methods) != 1 {
		t.Fatalf("expected no compact fallback edit, got %d calls: %v", len(methods), methods)
	}
	if service.richMessageSupport() < 0 {
		t.Fatal("a not-modified reply must not mark Rich Messages unsupported")
	}
}

func TestRateLimitedRichSendSkipsCompactFallback(t *testing.T) {
	err := &apiError{
		Method:      "sendRichMessage",
		ErrorCode:   http.StatusTooManyRequests,
		Description: "Too Many Requests: retry later",
		RetryAfter:  5 * time.Second,
	}
	if shouldFallbackRichMessage(err) {
		t.Fatal("a throttled bot must not immediately send the compact variant too")
	}
}

func TestPollBackoffHonoursRetryAfterAndConflict(t *testing.T) {
	throttled := &apiError{Method: "getUpdates", ErrorCode: 429, Description: "Too Many Requests", RetryAfter: 12 * time.Second}
	if got := pollBackoffFor(throttled); got != 12*time.Second {
		t.Fatalf("expected the retry_after pause to be used, got %s", got)
	}

	huge := &apiError{Method: "getUpdates", RetryAfter: time.Hour}
	if got := pollBackoffFor(huge); got != maxPollBackoff {
		t.Fatalf("expected an unreasonable pause to be capped, got %s", got)
	}

	conflict := &apiError{Method: "getUpdates", ErrorCode: http.StatusConflict, Description: "Conflict: terminated by other getUpdates request"}
	if !isPollingConflict(conflict) {
		t.Fatal("a second getUpdates consumer must be recognised")
	}
	if got := pollBackoffFor(conflict); got != pollConflictBackoff {
		t.Fatalf("expected the conflict backoff, got %s", got)
	}
}

func TestHandleUpdateSurvivesPanic(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	service.setConfig(Config{Enabled: true, BotToken: "token", ChatID: "1", TimeoutSec: 1})

	// A nil checker makes the status formatter panic; the process must survive
	// it, because Xray and user traffic share it.
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panic escaped the update handler: %v", recovered)
		}
	}()
	service.handleUpdateSafely(update{
		UpdateID: 7,
		Message: &message{
			MessageID: 1,
			Text:      "/status",
			Chat:      chat{ID: 1},
			From:      &user{ID: 2},
		},
	})
}

func TestIDReplyIsRateLimited(t *testing.T) {
	service := NewService("", nil, nil, 10000)
	msg := &message{Chat: chat{ID: 100}, From: &user{ID: 200}}
	start := time.Now()

	if !service.allowIDReply(msg, start) {
		t.Fatal("the first /id must be answered")
	}
	if service.allowIDReply(msg, start.Add(time.Second)) {
		t.Fatal("a repeat from the same user must be throttled")
	}

	other := &message{Chat: chat{ID: 101}, From: &user{ID: 201}}
	if service.allowIDReply(other, start.Add(time.Second)) {
		t.Fatal("a different user still hits the global interval")
	}
	if !service.allowIDReply(other, start.Add(idReplyGlobalInterval)) {
		t.Fatal("a different user must be answered once the global interval passed")
	}
	if !service.allowIDReply(msg, start.Add(idReplyPerUserInterval)) {
		t.Fatal("the same user must be answered again after their own interval")
	}
}

func TestPageSliceClampsRequestedPage(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}

	page, number, total := pageSlice(items, 2, 2)
	if len(page) != 2 || page[0] != 3 || number != 2 || total != 3 {
		t.Fatalf("unexpected second page: %v number=%d total=%d", page, number, total)
	}

	// A stale button from an older message must not land on an empty screen.
	page, number, _ = pageSlice(items, 99, 2)
	if number != 3 || len(page) != 1 || page[0] != 5 {
		t.Fatalf("expected the last page, got %v number=%d", page, number)
	}

	page, number, total = pageSlice([]int(nil), 4, 2)
	if len(page) != 0 || number != 1 || total != 1 {
		t.Fatalf("an empty list must report a single empty page, got %v number=%d total=%d", page, number, total)
	}
}

func TestNodeListMarkupPagesLongLists(t *testing.T) {
	names := make([]string, 0, nodeListPageSize*2+1)
	for i := 0; i < nodeListPageSize*2+1; i++ {
		names = append(names, string(rune('a'+i)))
	}
	proxies := testProxies(names...)
	service := NewService("", testChecker(proxies), nil, 10000)

	var markup inlineKeyboardMarkup
	if err := json.Unmarshal([]byte(service.nodeListMarkup(1)), &markup); err != nil {
		t.Fatalf("failed to decode markup: %v", err)
	}
	// Node rows, a navigation row and the menu row.
	if len(markup.InlineKeyboard) != nodeListPageSize+2 {
		t.Fatalf("expected a paged keyboard, got %d rows", len(markup.InlineKeyboard))
	}
	nav := markup.InlineKeyboard[nodeListPageSize]
	if len(nav) != 3 || nav[1].Text != "1/3" {
		t.Fatalf("unexpected navigation row: %+v", nav)
	}
	if nav[0].CallbackData != "noop" {
		t.Fatalf("the first page must not offer a previous page: %+v", nav[0])
	}
	if nav[2].CallbackData != "nodes:list:2" {
		t.Fatalf("expected a next-page button, got %+v", nav[2])
	}
}

func TestNodeAlertMarkupOffersActions(t *testing.T) {
	var markup inlineKeyboardMarkup
	if err := json.Unmarshal([]byte(nodeAlertMarkup("abc123")), &markup); err != nil {
		t.Fatalf("failed to decode markup: %v", err)
	}
	var actions []string
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			actions = append(actions, button.CallbackData)
		}
	}
	for _, want := range []string{"node:check:abc123", "node:abc123", "node:mutemenu:abc123"} {
		found := false
		for _, action := range actions {
			if action == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("alert keyboard is missing %q, got %v", want, actions)
		}
	}
}
