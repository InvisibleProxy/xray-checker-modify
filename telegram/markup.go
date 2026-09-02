package telegram

import (
	"encoding/json"
	"sort"
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

func (s *Service) nodeListMarkup() string {
	var rows [][]inlineKeyboardButton
	for _, proxy := range s.sortedProxies() {
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
	}
	rows = append(rows, []inlineKeyboardButton{
		{Text: "Ноды", CallbackData: "nodes:list"},
		{Text: "Меню", CallbackData: "back_to_menu"},
	})
	return encodeMarkup(rows)
}

func (s *Service) speedHistoryMarkup() string {
	results := s.activeSpeedResults(s.speedManager.Snapshot().Results)
	sort.Slice(results, func(i, j int) bool {
		return results[i].CheckedAt.After(results[j].CheckedAt)
	})

	var rows [][]inlineKeyboardButton
	var row []inlineKeyboardButton
	for _, result := range limitResults(results, menuSpeedButtonLimit) {
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
	rows = append(rows, []inlineKeyboardButton{{Text: "Меню", CallbackData: "back_to_menu"}})
	return encodeMarkup(rows)
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
