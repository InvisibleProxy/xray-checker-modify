package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xray-checker/logger"
	"xray-checker/pathquality"
	"xray-checker/probeagent"
	"xray-checker/speedtest"
	"xray-checker/verdictlog"
)

// The weekly digest is the one message that is not about something happening
// now. It answers the question an alert cannot: which nodes are bad for clients
// in the evening, week after week, and what the agents made of it. That is an
// input for decisions — which hoster to keep, where to put clients — and it
// arrives on Monday morning, when there is time to act on it.
const (
	digestWeekday = time.Monday
	digestHour    = 10
	digestPeriod  = 7 * 24 * time.Hour
	// digestLateness is how late a digest may still go out after its slot. A
	// checker that was down on Monday morning sends it when it comes back the
	// same day; one that comes back on Thursday waits for the next Monday
	// rather than reporting a week that ended days ago as news.
	digestLateness = 24 * time.Hour
	digestTick     = time.Minute
	// A node is listed among the worst when at least this share of its evening
	// measurements was bad, over at least digestMinRuns of them.
	digestProblemShare = 0.10
	digestMinRuns      = 3
	digestMaxNodes     = 6
)

// DigestSources are what the digest reads. Each may be nil, and the digest
// leaves its section out.
type DigestSources struct {
	// PathQuality builds the path report for the deployment's own monitored
	// nodes over [from, to), bucketed in the clients' time zone.
	PathQuality func(from, to time.Time) (pathquality.Report, bool)
	// Verdicts lists the journaled agent verdicts in [from, to).
	Verdicts func(from, to time.Time) []verdictlog.Entry
	// Agents lists the registered probe agents.
	Agents func() []probeagent.AgentSnapshot
}

func (s *Service) SetDigestSources(sources DigestSources) {
	s.digestSources = sources
}

func (s *Service) now() time.Time {
	if s.digestNow != nil {
		return s.digestNow()
	}
	return time.Now()
}

type digestState struct {
	LastSentAt time.Time `json:"lastSentAt"`
}

// digestStatePath sits beside the Telegram state. It is its own file because
// the alert state file is deleted whenever there is nothing to alert about,
// and the digest's last date has to survive that.
func (s *Service) digestStatePath() string {
	if s.statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.statePath), "telegram_digest_state.json")
}

func (s *Service) loadDigestState() {
	path := s.digestStatePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("Failed to read the Telegram digest state: %v", err)
		}
		return
	}
	var state digestState
	if err := json.Unmarshal(data, &state); err != nil {
		logger.Warn("Failed to decode the Telegram digest state: %v", err)
		return
	}
	s.digestMu.Lock()
	s.lastDigestAt = state.LastSentAt
	s.digestMu.Unlock()
}

func (s *Service) saveDigestState(sentAt time.Time) {
	s.digestMu.Lock()
	s.lastDigestAt = sentAt
	s.digestMu.Unlock()
	path := s.digestStatePath()
	if path == "" {
		return
	}
	data, err := json.Marshal(digestState{LastSentAt: sentAt.UTC()})
	if err == nil {
		err = os.WriteFile(path, data, 0600)
	}
	if err != nil {
		logger.Warn("Failed to save the Telegram digest state: %v", err)
	}
}

func (s *Service) digestLoop() {
	s.loadDigestState()
	ticker := time.NewTicker(digestTick)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
		s.sendDigestIfDue()
	}
}

// digestDueAt is the latest digest slot at or before now, in the operator's
// zone: the Monday 10:00 they will be at their desk for.
func digestDueAt(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	sinceWeekday := (int(local.Weekday()) - int(digestWeekday) + 7) % 7
	due := time.Date(local.Year(), local.Month(), local.Day()-sinceWeekday, digestHour, 0, 0, 0, location)
	if local.Before(due) {
		due = due.AddDate(0, 0, -7)
	}
	return due
}

// sendDigestIfDue sends the digest once per slot and reports whether it did.
func (s *Service) sendDigestIfDue() bool {
	cfg := s.Config()
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.WeeklyDigestEnabled || s.ProjectMaintenanceEnabled() {
		return false
	}
	now := s.now()
	due := digestDueAt(now, cfg.Location())
	s.digestMu.Lock()
	last := s.lastDigestAt
	s.digestMu.Unlock()
	if !last.Before(due) {
		return false
	}
	if now.Sub(due) > digestLateness {
		// Missed the slot by more than a day: mark it done and wait for the
		// next one instead of sending a week that ended long ago.
		s.saveDigestState(due)
		return false
	}
	content, ok := s.buildDigestMessage(now.Add(-digestPeriod), now)
	if !ok {
		s.saveDigestState(now)
		return false
	}
	if err := s.sendDigest(cfg, content); err != nil {
		logger.Warn("Failed to send the weekly Telegram digest: %v", err)
		return false
	}
	s.saveDigestState(now)
	return true
}

func (s *Service) sendDigest(cfg Config, content formattedMessage) error {
	if s.digestSendFunc != nil {
		return s.digestSendFunc(cfg, content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()
	_, err := s.sendFormattedToWithMarkup(ctx, cfg.ChatID, cfg.MessageThreadID, content, alertMarkup(backToMenuMarkup()))
	return err
}

// buildDigestMessage renders the digest for [from, to). It reports false when
// there is nothing to say at all — no speed history and no verdicts — which is a
// fresh deployment rather than a quiet week.
func (s *Service) buildDigestMessage(from, to time.Time) (formattedMessage, bool) {
	sources := s.digestSources
	var report pathquality.Report
	haveReport := false
	if sources.PathQuality != nil {
		report, haveReport = sources.PathQuality(from, to)
	}
	var verdicts []verdictlog.Entry
	if sources.Verdicts != nil {
		verdicts = sources.Verdicts(from, to)
	}
	measured := 0
	for _, node := range report.Nodes {
		measured += node.Peak.Runs + node.OffPeak.Runs
	}
	if (!haveReport || measured == 0) && len(verdicts) == 0 {
		return formattedMessage{}, false
	}

	peakZone := report.PeakZone
	period := fmt.Sprintf("%s – %s", from.In(messageLocation()).Format("02.01"), to.In(messageLocation()).Format("02.01"))
	lines := []string{"📊 <b>Качество пути за неделю</b>", htmlEscape(period)}
	var rich strings.Builder
	rich.WriteString("<h2>📊 Качество пути за неделю</h2>")
	fmt.Fprintf(&rich, "<p>%s</p>", htmlEscape(period))
	speedShown := false
	if haveReport && measured > 0 {
		peak := fmt.Sprintf("Пик — %02d:00–%02d:00 по %s; часы ниже — там же.", report.PeakStart, report.PeakEnd%24, peakZone)
		lines = append(lines, htmlEscape(peak))
		fmt.Fprintf(&rich, "<p>%s</p>", htmlEscape(peak))
		worst, clean := digestNodeGroups(report)
		if len(worst) > 0 {
			lines = append(lines, "", "<b>Хуже всего в пик</b>")
			rich.WriteString("<h3>Хуже всего в пик</h3><ul>")
			for _, node := range worst {
				line := digestNodeLine(node)
				lines = append(lines, "• "+line)
				fmt.Fprintf(&rich, "<li>%s</li>", line)
			}
			rich.WriteString("</ul>")
		} else {
			lines = append(lines, "", "В пик ни одна нода не проседала заметно.")
			rich.WriteString("<p>В пик ни одна нода не проседала заметно.</p>")
		}
		if len(clean) > 0 {
			names := htmlEscape(strings.Join(clean, ", "))
			lines = append(lines, "", "<b>Без просадок в пик</b>: "+names)
			fmt.Fprintf(&rich, "<p><b>Без просадок в пик</b>: %s</p>", names)
		}
		if hours := digestFleetHours(report.Fleet); hours != "" {
			lines = append(lines, "", "<b>Весь парк по часам</b> (доля плохих замеров): "+hours)
			fmt.Fprintf(&rich, "<p><b>Весь парк по часам</b> (доля плохих замеров): %s</p>", hours)
		}
		if speed := digestSpeedVerdicts(report); speed != "" {
			lines = append(lines, "", "<b>Пробы агентов по скорости</b>: "+speed)
			fmt.Fprintf(&rich, "<p><b>Пробы агентов по скорости</b>: %s</p>", speed)
			speedShown = true
		}
	}
	if availability := digestAvailabilityVerdicts(verdicts); availability != "" {
		// Right under the speed verdicts when there are any: together they are
		// what the agents said this week. Otherwise a section of its own.
		if !speedShown {
			lines = append(lines, "")
		}
		lines = append(lines, "<b>Пробы агентов по доступности</b>: "+availability)
		fmt.Fprintf(&rich, "<p><b>Пробы агентов по доступности</b>: %s</p>", availability)
	}
	if sources.Agents != nil {
		if agents := digestAgents(sources.Agents()); agents != "" {
			lines = append(lines, "", "<b>Агенты</b>: "+agents)
			fmt.Fprintf(&rich, "<p><b>Агенты</b>: %s</p>", agents)
		}
	}
	return formattedMessage{HTML: trimHTMLMessage(strings.Join(lines, "\n")), RichHTML: rich.String()}, true
}

// digestNodeGroups splits the nodes into the worst evenings, worst first, and
// the ones with no bad evening measurement at all.
func digestNodeGroups(report pathquality.Report) ([]pathquality.NodeReport, []string) {
	var worst []pathquality.NodeReport
	var clean []string
	for _, node := range report.Ranked() {
		if node.Peak.Runs < digestMinRuns {
			continue
		}
		switch {
		case node.Peak.BadShare >= digestProblemShare && len(worst) < digestMaxNodes:
			worst = append(worst, node)
		case node.Peak.Bad == 0:
			clean = append(clean, node.Name)
		}
	}
	sort.Strings(clean)
	return worst, clean
}

func digestNodeLine(node pathquality.NodeReport) string {
	parts := []string{
		fmt.Sprintf("<b>%s</b> — %s плохих (%d из %d)", htmlEscape(node.Name), percent(node.Peak.BadShare), node.Peak.Bad, node.Peak.Runs),
	}
	if node.Peak.MedianMbps > 0 {
		parts = append(parts, "медиана "+htmlEscape(formatMbps(node.Peak.MedianMbps)))
	}
	if node.OffPeak.Runs > 0 {
		parts = append(parts, "вне пика "+percent(node.OffPeak.BadShare))
	}
	if node.WorstHour >= 0 {
		parts = append(parts, fmt.Sprintf("хуже всего в %02d:00", node.WorstHour))
	}
	if verdicts := digestVerdictCounts(node.Verdicts, speedVerdictLabels); verdicts != "" {
		parts = append(parts, "агенты: "+verdicts)
	}
	return strings.Join(parts, " · ")
}

func digestFleetHours(fleet []pathquality.Cell) string {
	var parts []string
	for _, cell := range fleet {
		if cell.Runs == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%02d — %s", cell.Hour, percent(cell.Share())))
	}
	return htmlEscape(strings.Join(parts, " · "))
}

// The words a verdict is counted under. The speed ones name the finding rather
// than the comparison: "the checker's path" is what an operator acts on.
var speedVerdictLabels = []struct{ state, label string }{
	{speedtest.AgentDiagnosticPathLimited, "путь checker-а"},
	{speedtest.AgentDiagnosticReproduced, "воспроизвели"},
	{speedtest.AgentDiagnosticNotReproduced, "не воспроизвели"},
	{speedtest.AgentDiagnosticInconclusive, "несопоставимо"},
	{speedtest.AgentDiagnosticUnreliable, "ненадёжно"},
	{speedtest.AgentDiagnosticUnavailable, "нет агента"},
}

var availabilityVerdictLabels = []struct{ state, label string }{
	{speedtest.AgentDiagnosticNotReproduced, "у агента работает"},
	{speedtest.AgentDiagnosticReproduced, "не работает и у агента"},
	{speedtest.AgentDiagnosticUnreliable, "ненадёжно"},
	{speedtest.AgentDiagnosticUnavailable, "нет агента"},
}

func digestVerdictCounts(counts map[string]int, labels []struct{ state, label string }) string {
	var parts []string
	for _, label := range labels {
		if count := counts[label.state]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", label.label, count))
		}
	}
	return htmlEscape(strings.Join(parts, ", "))
}

func digestSpeedVerdicts(report pathquality.Report) string {
	counts := make(map[string]int)
	for _, node := range report.Nodes {
		for state, count := range node.Verdicts {
			counts[state] += count
		}
	}
	return digestVerdictCounts(counts, speedVerdictLabels)
}

func digestAvailabilityVerdicts(entries []verdictlog.Entry) string {
	counts := make(map[string]int)
	for _, entry := range entries {
		if entry.Source == verdictlog.SourceAvailability {
			counts[entry.Verdict]++
		}
	}
	return digestVerdictCounts(counts, availabilityVerdictLabels)
}

// digestAgents names who could look this week and who could not. An agent that
// went quiet is otherwise invisible until an alert says "no agent".
func digestAgents(agents []probeagent.AgentSnapshot) string {
	var connected, silent []string
	sort.Slice(agents, func(i, j int) bool { return agents[i].DisplayName < agents[j].DisplayName })
	for _, agent := range agents {
		if agent.Revoked || !agent.Enabled {
			continue
		}
		name := strings.TrimSpace(agent.DisplayName)
		if name == "" {
			name = agent.AgentID
		}
		if agent.Connected {
			connected = append(connected, name)
			continue
		}
		since := "ни разу не выходил на связь"
		if !agent.LastSeenAt.IsZero() {
			since = "нет связи с " + agent.LastSeenAt.In(messageLocation()).Format("02.01")
		}
		silent = append(silent, name+" — "+since)
	}
	var parts []string
	if len(connected) > 0 {
		parts = append(parts, strings.Join(connected, ", ")+" на связи")
	}
	parts = append(parts, silent...)
	return htmlEscape(strings.Join(parts, " · "))
}

func percent(share float64) string {
	return fmt.Sprintf("%.0f%%", share*100)
}

// formatQualityMessage answers /quality: the same digest, for the last week, on
// demand.
func (s *Service) formatQualityMessage() formattedMessage {
	now := s.now()
	if content, ok := s.buildDigestMessage(now.Add(-digestPeriod), now); ok {
		return content
	}
	text := "<b>Качество пути</b>\n\nЗа последнюю неделю нет ни замеров скорости, ни вердиктов агентов."
	return formattedMessage{HTML: text, RichHTML: richAlertBody(text)}
}
