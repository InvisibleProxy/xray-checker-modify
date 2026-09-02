package telegram

import (
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"
)

func limitLines(lines []string, limit int) []string {
	if limit <= 0 || len(lines) <= limit {
		return lines
	}
	result := append([]string{}, lines[:limit]...)
	result = append(result, fmt.Sprintf("…и ещё %d", len(lines)-limit))
	return result
}

func limitRichItems(items []string, limit int) []string {
	if limit <= 0 || len(items) <= limit {
		return items
	}
	result := append([]string{}, items[:limit]...)
	result = append(result, fmt.Sprintf("<li>И ещё %d…</li>", len(items)-limit))
	return result
}

func formatBytes(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	mb := float64(bytes) / 1024 / 1024
	if mb >= 1 {
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%.0f KB", float64(bytes)/1024)
}

func formatDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}

	totalMinutes := int(value / time.Minute)
	if totalMinutes <= 0 {
		return "<1 мин"
	}

	days := totalMinutes / (24 * 60)
	hours := (totalMinutes % (24 * 60)) / 60
	minutes := totalMinutes % 60

	if days > 0 {
		return fmt.Sprintf("%d д %d ч %d мин", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%d ч %d мин", hours, minutes)
	}
	return fmt.Sprintf("%d мин", minutes)
}

func htmlEscape(value string) string {
	return html.EscapeString(value)
}

func htmlCode(value string) string {
	return "<code>" + htmlEscape(value) + "</code>"
}

func compactText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

func formatCheckedAt(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Format("2006-01-02 15:04:05")
}

func trimMessage(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= 3900 {
		return text
	}
	runes := []rune(text)
	suffix := "\n...truncated"
	limit := 3900 - utf8.RuneCountInString(suffix)
	return string(runes[:limit]) + suffix
}

func trimHTMLMessage(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= 3900 {
		return text
	}

	suffix := "\n...truncated"
	limit := 3900 - utf8.RuneCountInString(suffix)
	visible := 0
	truncated := false
	openTags := make([]string, 0, 2)
	var result strings.Builder
	for offset := 0; offset < len(text); {
		rest := text[offset:]
		switch {
		case strings.HasPrefix(rest, "<b>"):
			result.WriteString("<b>")
			openTags = append(openTags, "b")
			offset += len("<b>")
			continue
		case strings.HasPrefix(rest, "</b>"):
			result.WriteString("</b>")
			openTags = removeLastOpenTag(openTags, "b")
			offset += len("</b>")
			continue
		case strings.HasPrefix(rest, "<code>"):
			result.WriteString("<code>")
			openTags = append(openTags, "code")
			offset += len("<code>")
			continue
		case strings.HasPrefix(rest, "</code>"):
			result.WriteString("</code>")
			openTags = removeLastOpenTag(openTags, "code")
			offset += len("</code>")
			continue
		}

		if visible >= limit {
			truncated = true
			break
		}
		if rest[0] == '&' {
			if end := strings.IndexByte(rest, ';'); end > 0 {
				result.WriteString(rest[:end+1])
				offset += end + 1
				visible++
				continue
			}
		}

		_, size := utf8.DecodeRuneInString(rest)
		result.WriteString(rest[:size])
		offset += size
		visible++
	}

	if !truncated {
		return text
	}
	for i := len(openTags) - 1; i >= 0; i-- {
		result.WriteString("</" + openTags[i] + ">")
	}
	return strings.TrimSpace(result.String()) + suffix
}

func removeLastOpenTag(openTags []string, tag string) []string {
	for i := len(openTags) - 1; i >= 0; i-- {
		if openTags[i] == tag {
			return append(openTags[:i], openTags[i+1:]...)
		}
	}
	return openTags
}
