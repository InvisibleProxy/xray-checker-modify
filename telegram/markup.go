package telegram

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"xray-checker/checker"
)

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

func statusMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{{Text: "Обновить", CallbackData: "status:refresh"}},
		{{Text: "Меню", CallbackData: "back_to_menu"}},
	})
}

func statusRefreshMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{{Text: "Проверка идёт…", CallbackData: "status:refresh"}},
		{{Text: "Меню", CallbackData: "back_to_menu"}},
	})
}

func (s *Service) nodeListMarkup(page int) string {
	proxies := s.sortedProxies()
	pageProxies, page, totalPages := pageSlice(proxies, page, nodeListPageSize)

	var rows [][]inlineKeyboardButton
	for _, proxy := range pageProxies {
		details, err := s.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		status := "🔴"
		if err == nil {
			switch details.EffectiveStatus() {
			case checker.AvailabilityStateOnline:
				status = "🟢"
			case checker.AvailabilityStateProxyFailure:
				status = "🟡"
			}
		}
		rows = append(rows, []inlineKeyboardButton{{
			Text:         status + " " + shortButtonText(proxy.Name),
			CallbackData: "node:" + proxy.StableID,
		}})
	}
	if nav := pageNavRow(page, totalPages, "nodes:list:"); len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []inlineKeyboardButton{{Text: "Меню", CallbackData: "back_to_menu"}})
	return encodeMarkup(rows)
}

func (s *Service) nodeDetailMarkup(stableID string, isAdmin bool) string {
	var rows [][]inlineKeyboardButton
	if isAdmin {
		rows = append(rows, []inlineKeyboardButton{{
			Text:         "Проверить доступность",
			CallbackData: "node:check:" + stableID,
		}})
		rows = append(rows, []inlineKeyboardButton{{
			Text:         "Speed-test этой ноды",
			CallbackData: "node:test:" + stableID,
		}})
		rows = append(rows, []inlineKeyboardButton{{
			Text:         "🔕 Уведомления",
			CallbackData: "node:mutemenu:" + stableID,
		}})
	}
	rows = append(rows, []inlineKeyboardButton{
		{Text: "Ноды", CallbackData: "nodes:list"},
		{Text: "Меню", CallbackData: "back_to_menu"},
	})
	return encodeMarkup(rows)
}

// nodeMuteMarkup offers the durations an operator actually reaches for at
// night. A muted node shows the way back instead of more ways to silence it.
func nodeMuteMarkup(stableID string, status nodeMuteStatus) string {
	var rows [][]inlineKeyboardButton
	if status.Muted() {
		rows = append(rows, []inlineKeyboardButton{{
			Text:         "🔔 Включить уведомления",
			CallbackData: "node:unmute:" + stableID,
		}})
	} else {
		rows = append(rows, []inlineKeyboardButton{
			{Text: "🔕 1 ч", CallbackData: "node:mute:" + muteScopeAll + ":60:" + stableID},
			{Text: "🔕 8 ч", CallbackData: "node:mute:" + muteScopeAll + ":480:" + stableID},
		})
		rows = append(rows, []inlineKeyboardButton{
			{Text: "Только алерты · 8 ч", CallbackData: "node:mute:" + muteScopeAlerts + ":480:" + stableID},
		})
		rows = append(rows, []inlineKeyboardButton{
			{Text: "Только замеры · 8 ч", CallbackData: "node:mute:" + muteScopeSpeed + ":480:" + stableID},
		})
		rows = append(rows, []inlineKeyboardButton{
			{Text: "🔕 Навсегда", CallbackData: "node:mute:" + muteScopeAll + ":0:" + stableID},
		})
	}
	rows = append(rows, []inlineKeyboardButton{
		{Text: "Назад", CallbackData: "node:" + stableID},
		{Text: "Меню", CallbackData: "back_to_menu"},
	})
	return encodeMarkup(rows)
}

// nodeAlertMarkup turns an alert into something an operator can act on without
// leaving the message and hunting for the node in the menu.
func nodeAlertMarkup(stableID string) string {
	if stableID == "" {
		return issuesMarkup()
	}
	return encodeMarkup([][]inlineKeyboardButton{
		{
			{Text: "Проверить", CallbackData: "node:check:" + stableID},
			{Text: "Открыть", CallbackData: "node:" + stableID},
		},
		{
			{Text: "🔕 Заглушить", CallbackData: "node:mutemenu:" + stableID},
		},
	})
}

func issuesMarkup() string {
	return encodeMarkup([][]inlineKeyboardButton{
		{
			{Text: "⚠️ Проблемы", CallbackData: "issues"},
			{Text: "Меню", CallbackData: "back_to_menu"},
		},
	})
}

func (s *Service) speedHistoryMarkup(page int) string {
	results := s.activeSpeedResults(s.speedManager.Snapshot().Results)
	sort.Slice(results, func(i, j int) bool {
		return results[i].CheckedAt.After(results[j].CheckedAt)
	})
	pageResults, page, totalPages := pageSlice(results, page, speedListPageSize)

	var rows [][]inlineKeyboardButton
	var row []inlineKeyboardButton
	for _, result := range pageResults {
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
	if nav := pageNavRow(page, totalPages, "speed:list:"); len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []inlineKeyboardButton{{Text: "Меню", CallbackData: "back_to_menu"}})
	return encodeMarkup(rows)
}

// pageSlice clamps a requested page onto what actually exists, so a stale
// button from an older message cannot land on an empty screen.
func pageSlice[T any](items []T, page int, size int) ([]T, int, int) {
	if size <= 0 {
		return items, 1, 1
	}
	totalPages := (len(items) + size - 1) / size
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * size
	if start >= len(items) {
		return nil, page, totalPages
	}
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], page, totalPages
}

func pageNavRow(page int, totalPages int, prefix string) []inlineKeyboardButton {
	if totalPages <= 1 {
		return nil
	}
	previous := inlineKeyboardButton{Text: "‹", CallbackData: prefix + strconv.Itoa(page-1)}
	if page <= 1 {
		previous = inlineKeyboardButton{Text: " ", CallbackData: "noop"}
	}
	next := inlineKeyboardButton{Text: "›", CallbackData: prefix + strconv.Itoa(page+1)}
	if page >= totalPages {
		next = inlineKeyboardButton{Text: " ", CallbackData: "noop"}
	}
	return []inlineKeyboardButton{
		previous,
		{Text: strconv.Itoa(page) + "/" + strconv.Itoa(totalPages), CallbackData: "noop"},
		next,
	}
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
