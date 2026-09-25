package telegram

import (
	"strings"
	"testing"
	"time"
)

func TestAnnounceConflictNotices(t *testing.T) {
	since := time.Date(2026, 9, 22, 9, 42, 0, 0, time.UTC)
	now := since.Add(3*24*time.Hour + 20*time.Minute)
	var sent []formattedMessage
	service := &Service{digestNow: func() time.Time { return now }}
	service.announceSendFunc = func(_ Config, content formattedMessage) error {
		sent = append(sent, content)
		return nil
	}

	service.AnnounceConflict("Standart", "Примите базу заново.", since)
	if len(sent) != 0 {
		t.Fatal("a notice was sent while Telegram is not configured")
	}

	cfg := DefaultConfig()
	cfg.Enabled, cfg.ChatID, cfg.BotToken, cfg.TimeZone = true, "chat", "token", "UTC"
	service.setConfig(cfg)
	service.AnnounceConflict("Standart <EU>", "Примите базу заново.", since)
	service.AnnounceConflictResolved("Standart <EU>", since)
	if len(sent) != 2 {
		t.Fatalf("sent %d notices, want 2", len(sent))
	}
	conflict := sent[0]
	for _, want := range []string{
		"⚠️ <b>Standart &lt;EU&gt;</b> · announce не обновляется",
		"<b>3 д 0 ч 20 мин</b> · с 22.09 09:42",
		"Примите базу заново.",
		"подписчики сквада не узнают о сбоях",
	} {
		if !strings.Contains(conflict.HTML, want) {
			t.Errorf("conflict notice lacks %q:\n%s", want, conflict.HTML)
		}
	}
	if !strings.HasPrefix(conflict.RichHTML, "<h2>⚠️ <b>Standart &lt;EU&gt;</b>") {
		t.Errorf("rich conflict notice = %s", conflict.RichHTML)
	}
	if !strings.Contains(sent[1].HTML, "announce снова обновляется") || !strings.Contains(sent[1].HTML, "3 д 0 ч 20 мин") {
		t.Errorf("resolution notice:\n%s", sent[1].HTML)
	}

	service.SetProjectMaintenance(true)
	service.AnnounceConflict("Standart", "", since)
	if len(sent) != 2 {
		t.Fatal("a notice was sent during project maintenance")
	}
}
