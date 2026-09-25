package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"xray-checker/logger"
)

// AnnounceConflict tells the operator that the checker cannot write its status
// into an External Squad. Subscribers of that squad then get no status at all,
// and the Remnawave tab of the admin panel is the only other place saying so.
func (s *Service) AnnounceConflict(squadName, reason string, since time.Time) {
	s.sendAnnounceNotice(formatAnnounceConflict(squadName, reason, since, s.now()))
}

// AnnounceConflictResolved follows an AnnounceConflict up once the checker
// writes that squad's status again.
func (s *Service) AnnounceConflictResolved(squadName string, since time.Time) {
	s.sendAnnounceNotice(formatAnnounceConflictResolved(squadName, since, s.now()))
}

func (s *Service) sendAnnounceNotice(compactHTML string) {
	cfg := s.Config()
	if !cfg.Enabled || cfg.ChatID == "" || s.ProjectMaintenanceEnabled() {
		return
	}
	content := formattedMessage{HTML: compactHTML, RichHTML: richAlertBody(compactHTML)}
	if s.announceSendFunc != nil {
		if err := s.announceSendFunc(cfg, content); err != nil {
			logger.Warn("Failed to send Telegram announce notice: %v", err)
		}
		return
	}
	// The announce reconcile calls in while holding its own lock, and a slow
	// Bot API must not hold that lock for the whole timeout.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
		defer cancel()
		if _, err := s.sendFormattedToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, content, alertMarkup(backToMenuMarkup())); err != nil {
			logger.Warn("Failed to send Telegram announce notice: %v", err)
		}
	}()
}

func formatAnnounceConflict(squadName, reason string, since, now time.Time) string {
	lines := []string{
		fmt.Sprintf("⚠️ <b>%s</b> · announce не обновляется", htmlEscape(squadName)),
		htmlEscape(messageTimezone(since)),
		fmt.Sprintf("Статус в подписку не пишется: <b>%s</b> · с %s", htmlEscape(formatDuration(now.Sub(since))), htmlEscape(formatCheckedAt(since))),
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		lines = append(lines, htmlEscape(reason))
	}
	lines = append(lines, "Пока конфликт не разрешён, подписчики сквада не узнают о сбоях. Подробности — в админке, вкладка Remnawave.")
	return strings.Join(lines, "\n")
}

func formatAnnounceConflictResolved(squadName string, since, now time.Time) string {
	return strings.Join([]string{
		fmt.Sprintf("✅ <b>%s</b> · announce снова обновляется", htmlEscape(squadName)),
		fmt.Sprintf("Конфликт длился <b>%s</b>, статус снова пишется в подписку.", htmlEscape(formatDuration(now.Sub(since)))),
	}, "\n")
}
