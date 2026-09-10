package telegram

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"xray-checker/agentautomation"
	"xray-checker/logger"
	"xray-checker/speedtest"
)

func (s *Service) NotifySpeedTest(report speedtest.RunReport) {
	if s.ProjectMaintenanceEnabled() {
		return
	}
	cfg := s.Config()
	report = s.labelSpeedResults(report)
	if report.Source == "manual" {
		report = s.excludeUnreportableSpeedResults(report)
	}
	if report.Source == "manual" {
		ids := successfulSpeedResultIDs(report.Results, cfg.LowSpeedThresholdMbps)
		if s.clearSpeedRetry(speedRetryKindConfirmation, ids) {
			s.persistRetryStateWithWarn()
		}
	}
	if isRequestedTelegramSpeedReport(report) {
		if !cfg.Enabled || len(report.Results) == 0 {
			return
		}
		handles := s.startSpeedDiagnostics(report, cfg)
		report = attachSpeedDiagnostics(report, s.currentSpeedDiagnostics(handles))
		failed, slow := countSpeedIssues(report.Results, cfg.LowSpeedThresholdMbps)
		content := s.formatSpeedReportMessage(report, cfg, failed, slow, false)
		s.sendSpeedTestReport(cfg, report.ReportTarget.ChatID, report.ReportTarget.MessageThreadID, content)
		return
	}

	if kind, ok := speedRetryKindForSource(report.Source); ok {
		s.completeSpeedRetry(kind, report.RequestedStableIDs, report.Results)
	}

	speedMuted := s.speedMuteSet(cfg)
	report = filterRunReportByMuteSet(report, speedMuted)
	var diagnosticHandles map[string]agentautomation.Handle
	if automaticSpeedReportsEnabled(cfg) {
		diagnosticHandles = s.startSpeedDiagnostics(report, cfg)
	}
	var annotations map[string]speedtest.AgentDiagnostic
	if automaticSpeedReportsEnabled(cfg) && report.Source != speedConfirmationRetrySource {
		targets := speedConfirmationRetryTargets(report.Results, cfg.LowSpeedThresholdMbps)
		if s.scheduleSpeedRetry(report, speedRetryKindConfirmation, targets, s.configuredSpeedRetryDelay()) {
			// The wait for confirmation exists because one bad measurement from
			// one vantage point may be the vantage point's fault. A second
			// vantage point that saw the same thing answers that question now,
			// so a node it confirmed goes into this report instead of waiting
			// half an hour to say what is already known. The retry still stands:
			// whether the problem persists is a different question, and only a
			// later measurement answers it.
			annotations = s.awaitSpeedDiagnostics(diagnosticHandlesForResults(diagnosticHandles, report.Results))
			report = excludeSpeedResults(report, unconfirmedRetryIDs(targets, annotations))
		}
	}
	report = filterTelegramAlertSuppressedRunReport(report)
	failed, slow, issuesOnly, shouldSend := speedReportDecisionWithMutes(report, cfg, speedMuted)
	if !shouldSend {
		return
	}

	if report.Source == speedConfirmationRetrySource {
		if failed == 0 && slow == 0 {
			return
		}
		issuesOnly = true
	}
	diagnosticHandles = diagnosticHandlesForResults(diagnosticHandles, report.Results)
	if len(diagnosticHandles) > 0 {
		if annotations == nil {
			annotations = s.awaitSpeedDiagnostics(diagnosticHandles)
		}
		report = attachSpeedDiagnostics(report, annotations)
	}

	content := s.formatSpeedReportMessage(report, cfg, failed, slow, issuesOnly)
	s.sendSpeedTestReport(cfg, cfg.ChatID, cfg.MessageThreadID, content)
}

func (s *Service) startSpeedDiagnostics(report speedtest.RunReport, cfg Config) map[string]agentautomation.Handle {
	if s.speedDiagnostics == nil || !s.speedDiagnostics.Enabled() {
		return nil
	}
	return s.speedDiagnostics.StartSpeedDiagnostics(report, cfg.LowSpeedThresholdMbps)
}

func (s *Service) currentSpeedDiagnostics(handles map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic {
	if s.speedDiagnostics == nil || len(handles) == 0 {
		return nil
	}
	return s.speedDiagnostics.Annotations(handles)
}

func (s *Service) awaitSpeedDiagnostics(handles map[string]agentautomation.Handle) map[string]speedtest.AgentDiagnostic {
	if s.speedDiagnostics == nil || len(handles) == 0 {
		return nil
	}
	wait := s.speedDiagnostics.AlertWait()
	if wait <= 0 {
		return s.speedDiagnostics.Annotations(handles)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return s.speedDiagnostics.Await(ctx, handles)
}

func diagnosticHandlesForResults(handles map[string]agentautomation.Handle, results []speedtest.Result) map[string]agentautomation.Handle {
	if len(handles) == 0 || len(results) == 0 {
		return nil
	}
	wanted := make(map[string]bool, len(results))
	for _, result := range results {
		wanted[result.StableID] = true
	}
	filtered := make(map[string]agentautomation.Handle)
	for stableID, handle := range handles {
		if wanted[stableID] {
			filtered[stableID] = handle
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func attachSpeedDiagnostics(report speedtest.RunReport, annotations map[string]speedtest.AgentDiagnostic) speedtest.RunReport {
	if len(annotations) == 0 || len(report.Results) == 0 {
		return report
	}
	results := append([]speedtest.Result(nil), report.Results...)
	for index := range results {
		annotation, ok := annotations[results[index].StableID]
		if !ok {
			continue
		}
		copyValue := annotation
		results[index].AgentDiagnostic = &copyValue
	}
	report.Results = results
	return report
}

// labelSpeedResults renames results to the operator's labels for the report.
// A result carries the name the node had when it was measured, which is the
// right thing to persist and the wrong thing to read months later under a name
// the operator has since replaced.
func (s *Service) labelSpeedResults(report speedtest.RunReport) speedtest.RunReport {
	if s.proxyChecker == nil || len(report.Results) == 0 {
		return report
	}
	results := append([]speedtest.Result(nil), report.Results...)
	for index := range results {
		if label := s.proxyChecker.DisplayName(results[index].StableID); label != "" {
			results[index].Name = label
		}
	}
	report.Results = results
	return report
}

// excludeUnreportableSpeedResults drops the measurements Telegram may not speak
// about: a probe of a paused node, and anything from a source added in the
// panel. The measurements themselves are still taken and stored; this only
// decides what is announced.
func (s *Service) excludeUnreportableSpeedResults(report speedtest.RunReport) speedtest.RunReport {
	if s.proxyChecker == nil || len(report.Results) == 0 {
		return report
	}
	filtered := make([]speedtest.Result, 0, len(report.Results))
	for _, result := range report.Results {
		if result.MaintenanceProbe || !s.proxyChecker.MonitoringEnabled(result.StableID) {
			continue
		}
		if s.speaksAbout(result.StableID) {
			filtered = append(filtered, result)
		}
	}
	report.Results = filtered
	report.Selected = len(filtered)
	return report
}

func (s *Service) sendSpeedTestReport(cfg Config, chatID string, threadID int, content formattedMessage) {
	if s.speedReportSendFunc != nil {
		s.speedReportSendFunc(chatID, threadID, content)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	if _, err := s.sendFormattedToWithMarkup(ctx, chatID, threadID, content, backToMenuMarkup()); err != nil {
		logger.Warn("Failed to send Telegram speed-test report: %v", err)
	}
}

func isRequestedTelegramSpeedReport(report speedtest.RunReport) bool {
	return report.Source == "telegram" && strings.TrimSpace(report.ReportTarget.ChatID) != ""
}

func automaticSpeedReportsEnabled(cfg Config) bool {
	return cfg.Enabled && cfg.ChatID != "" && cfg.SpeedReportsEnabled && cfg.SpeedReportMode != "disabled"
}

// speedConfirmationRetryTargets selects results that need delayed confirmation
// and names what each one is waiting to have confirmed.
//
// The reason is part of the identity of a pending confirmation, not decoration.
// Keyed by node alone, a node already waiting on a slowdown could not open a
// second wait when its next run failed outright: the failure changed character,
// and the pending entry went on asking the old question.
//
// A deadline is read by what it measured. A transfer the clock cut short that
// still came in under the threshold is a slowdown and waits as one; only a
// deadline with no rate to judge - or one that beat the threshold and stopped
// anyway - is left as a technical failure. The stored-text check stays for
// history written before a shortened transfer had a flag of its own.
func speedConfirmationRetryTargets(results []speedtest.Result, threshold float64) []speedRetryTarget {
	seen := make(map[speedRetryTarget]bool)
	var targets []speedRetryTarget
	for _, result := range results {
		stableID := strings.TrimSpace(result.StableID)
		effective := resultThreshold(result, threshold)
		lowSpeed := effective > 0 && !result.Offline && result.Error == "" && result.Mbps < effective
		deadline := result.TimedOut || resultHasContextDeadlineExceeded(result)
		unresolvedDeadline := deadline &&
			(result.Offline || result.Error != "" || result.TimedOut || (effective > 0 && result.Mbps < effective))
		if stableID == "" || (!lowSpeed && !unresolvedDeadline) {
			continue
		}
		reason := speedRetryReasonLowSpeed
		if !lowSpeed {
			reason = speedRetryReasonTechnical
		}
		target := speedRetryTarget{StableID: stableID, Reason: reason}
		if seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].StableID != targets[j].StableID {
			return targets[i].StableID < targets[j].StableID
		}
		return targets[i].Reason < targets[j].Reason
	})
	return targets
}

// unconfirmedRetryIDs lists the nodes whose problem is still only this
// checker's word, and which therefore stay out of the report until the
// confirmation run either reproduces it or clears it.
//
// A node an agent reproduced is not on the list: two independent points saw the
// same failure, which is what the delay was buying. Every other verdict leaves
// it - "not reproduced" says the agent's path was fine, not that this one was,
// and an unreliable or absent observation settles nothing at all.
func unconfirmedRetryIDs(targets []speedRetryTarget, annotations map[string]speedtest.AgentDiagnostic) []string {
	ids := make([]string, 0, len(targets))
	for _, stableID := range speedRetryTargetIDs(targets) {
		if annotations[stableID].State == speedtest.AgentDiagnosticReproduced {
			continue
		}
		ids = append(ids, stableID)
	}
	return ids
}

func speedRetryTargetIDs(targets []speedRetryTarget) []string {
	seen := make(map[string]bool, len(targets))
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		if seen[target.StableID] {
			continue
		}
		seen[target.StableID] = true
		ids = append(ids, target.StableID)
	}
	return ids
}

func excludeSpeedResults(report speedtest.RunReport, excludedIDs []string) speedtest.RunReport {
	excluded := make(map[string]bool, len(excludedIDs))
	for _, stableID := range excludedIDs {
		excluded[stableID] = true
	}
	filtered := make([]speedtest.Result, 0, len(report.Results))
	for _, result := range report.Results {
		if !excluded[result.StableID] {
			filtered = append(filtered, result)
		}
	}
	report.Results = filtered
	report.Selected = len(filtered)
	return report
}

func successfulSpeedResultIDs(results []speedtest.Result, threshold float64) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, result := range results {
		stableID := strings.TrimSpace(result.StableID)
		effective := resultThreshold(result, threshold)
		if stableID == "" || result.Offline || result.Error != "" || result.TimedOut ||
			(effective > 0 && result.Mbps < effective) || seen[stableID] {
			continue
		}
		seen[stableID] = true
		ids = append(ids, stableID)
	}
	sort.Strings(ids)
	return ids
}

func resultHasContextDeadlineExceeded(result speedtest.Result) bool {
	return containsContextDeadlineExceeded(result.Error) || containsContextDeadlineExceeded(result.PrimaryError)
}

func containsContextDeadlineExceeded(value string) bool {
	return strings.Contains(strings.ToLower(value), "context deadline exceeded")
}

func (s *Service) scheduleSpeedConfirmationRetry(report speedtest.RunReport) bool {
	cfg := s.Config()
	targets := speedConfirmationRetryTargets(report.Results, cfg.LowSpeedThresholdMbps)
	return s.scheduleSpeedRetry(report, speedRetryKindConfirmation, targets, s.configuredSpeedRetryDelay())
}

func (s *Service) scheduleSpeedRetry(report speedtest.RunReport, kind string, targets []speedRetryTarget, delay time.Duration) bool {
	if len(targets) == 0 {
		return false
	}

	s.speedRetryMu.Lock()
	select {
	case <-s.stopCh:
		s.speedRetryMu.Unlock()
		return false
	default:
	}
	if s.speedRetryPending == nil {
		s.speedRetryPending = make(map[speedRetryKey]bool)
	}
	if s.speedRetryTimers == nil {
		s.speedRetryTimers = make(map[uint64]*time.Timer)
	}
	if s.speedRetryEntries == nil {
		s.speedRetryEntries = make(map[uint64]pendingSpeedRetry)
	}

	newTargets := make([]speedRetryTarget, 0, len(targets))
	for _, target := range targets {
		key := speedRetryKey{Kind: kind, StableID: target.StableID, Reason: target.Reason}
		if s.speedRetryPending[key] {
			continue
		}
		s.speedRetryPending[key] = true
		newTargets = append(newTargets, target)
	}
	if len(newTargets) == 0 {
		s.speedRetryMu.Unlock()
		return true
	}

	req := speedtest.RunRequest{
		ProxyIDs: speedRetryTargetIDs(newTargets),
		Config:   report.Config,
	}
	s.scheduleSpeedRetryLocked(kind, req, newTargets, delay)
	s.speedRetryMu.Unlock()
	s.persistRetryStateWithWarn()
	return true
}

func (s *Service) configuredSpeedRetryDelay() time.Duration {
	if s.speedRetryDelay > 0 {
		return s.speedRetryDelay
	}
	return speedConfirmationRetryDelay
}

func (s *Service) configuredSpeedRetryBusyDelay() time.Duration {
	if s.speedRetryBusy > 0 {
		return s.speedRetryBusy
	}
	return speedConfirmationRetryBusyDelay
}

func (s *Service) scheduleSpeedRetryLocked(kind string, req speedtest.RunRequest, targets []speedRetryTarget, delay time.Duration) {
	s.speedRetrySeq++
	timerID := s.speedRetrySeq
	if delay < 0 {
		delay = 0
	}
	s.speedRetryEntries[timerID] = pendingSpeedRetry{
		Kind:    kind,
		Request: req,
		Targets: append([]speedRetryTarget(nil), targets...),
		DueAt:   time.Now().Add(delay),
	}
	s.armSpeedRetryLocked(timerID, delay)
}

func (s *Service) armSpeedRetryLocked(timerID uint64, delay time.Duration) {
	entry, ok := s.speedRetryEntries[timerID]
	if !ok {
		return
	}
	kind := normalizedSpeedRetryKind(entry.Kind)
	req := entry.Request
	targets := append([]speedRetryTarget(nil), entry.Targets...)
	s.speedRetryWG.Add(1)
	s.speedRetryTimers[timerID] = time.AfterFunc(delay, func() {
		defer s.speedRetryWG.Done()
		s.runSpeedRetry(timerID, kind, req, targets)
	})
}

func (s *Service) runSpeedRetry(timerID uint64, kind string, req speedtest.RunRequest, targets []speedRetryTarget) {
	select {
	case <-s.stopCh:
		return
	default:
	}

	source, ok := speedRetrySourceForKind(kind)
	if !ok {
		s.clearSpeedRetry(kind, speedRetryTargetIDs(targets))
		s.persistRetryStateWithWarn()
		logger.Warn("Discarded speed-test retry with unknown kind %q", kind)
		return
	}

	s.speedRetryMu.Lock()
	delete(s.speedRetryTimers, timerID)
	pendingTargets := make([]speedRetryTarget, 0, len(targets))
	for _, target := range targets {
		if s.speedRetryPending[speedRetryKey{Kind: kind, StableID: target.StableID, Reason: target.Reason}] {
			pendingTargets = append(pendingTargets, target)
		}
	}
	pendingIDs := speedRetryTargetIDs(pendingTargets)
	if len(pendingIDs) == 0 {
		delete(s.speedRetryEntries, timerID)
		s.speedRetryMu.Unlock()
		s.persistRetryStateWithWarn()
		return
	}
	req.ProxyIDs = append([]string(nil), pendingIDs...)
	req.OnlyOnline = true
	req.SkipOffline = true
	err := s.runSpeedTest(req, source)
	if err == nil {
		s.speedRetryMu.Unlock()
		return
	}
	if strings.Contains(strings.ToLower(err.Error()), "already running") {
		select {
		case <-s.stopCh:
			s.speedRetryMu.Unlock()
			return
		default:
		}
		delete(s.speedRetryEntries, timerID)
		s.scheduleSpeedRetryLocked(kind, req, pendingTargets, s.configuredSpeedRetryBusyDelay())
		s.speedRetryMu.Unlock()
		s.persistRetryStateWithWarn()
		logger.Warn("%s speed-test retry postponed because another speed-test is running", speedRetryKindLabel(kind))
		return
	}
	s.speedRetryMu.Unlock()

	s.clearSpeedRetry(kind, pendingIDs)
	s.persistRetryStateWithWarn()
	logger.Warn("Failed to start %s speed-test retry: %v", speedRetryKindLabel(kind), err)
}

func (s *Service) completeSpeedRetry(kind string, requestedStableIDs []string, results []speedtest.Result) {
	ids := append([]string(nil), requestedStableIDs...)
	for _, result := range results {
		if result.StableID != "" {
			ids = append(ids, result.StableID)
		}
	}
	s.clearSpeedRetry(kind, ids)
	s.persistRetryStateWithWarn()
}

// speedRetryPendingForLocked reports whether a node is waiting on any
// confirmation, whichever failure opened the wait. The caller holds
// speedRetryMu, as every other reader of this map does.
func (s *Service) speedRetryPendingForLocked(kind string, stableID string) bool {
	for key := range s.speedRetryPending {
		if key.Kind == kind && key.StableID == stableID {
			return true
		}
	}
	return false
}

// clearSpeedRetry drops every pending confirmation for these nodes, whatever it
// was waiting to confirm. The reason distinguishes waits when they are created,
// so that a node can wait on two different failures at once; it does not
// survive resolution. A node that answered cleanly, was paused, or left the
// fleet has no failure left to confirm, of either kind.
func (s *Service) clearSpeedRetry(kind string, ids []string) bool {
	s.speedRetryMu.Lock()
	defer s.speedRetryMu.Unlock()
	changed := false
	cleared := make(map[string]bool, len(ids))
	for _, stableID := range ids {
		cleared[strings.TrimSpace(stableID)] = true
	}
	for key := range s.speedRetryPending {
		if key.Kind != kind || !cleared[key.StableID] {
			continue
		}
		delete(s.speedRetryPending, key)
		changed = true
	}
	for timerID, entry := range s.speedRetryEntries {
		if normalizedSpeedRetryKind(entry.Kind) != kind {
			continue
		}
		originalCount := len(entry.Targets)
		remaining := entry.Targets[:0]
		for _, target := range entry.Targets {
			if !cleared[target.StableID] {
				remaining = append(remaining, target)
			}
		}
		if len(remaining) == originalCount {
			continue
		}
		changed = true
		if len(remaining) == 0 {
			delete(s.speedRetryEntries, timerID)
			if timer := s.speedRetryTimers[timerID]; timer != nil && timer.Stop() {
				s.speedRetryWG.Done()
			}
			delete(s.speedRetryTimers, timerID)
			continue
		}
		entry.Targets = append([]speedRetryTarget(nil), remaining...)
		entry.Request.ProxyIDs = speedRetryTargetIDs(entry.Targets)
		s.speedRetryEntries[timerID] = entry
	}
	return changed
}

func (s *Service) startRestoredSpeedRetries() {
	s.speedRetryMu.Lock()
	defer s.speedRetryMu.Unlock()
	now := time.Now()
	for timerID, entry := range s.speedRetryEntries {
		if s.speedRetryTimers[timerID] != nil {
			continue
		}
		delay := entry.DueAt.Sub(now)
		if delay < 0 {
			delay = 0
		}
		s.armSpeedRetryLocked(timerID, delay)
	}
}

func (s *Service) persistRetryStateWithWarn() {
	if err := s.saveAlertState(); err != nil {
		logger.Warn("Failed to save pending speed-test retries: %v", err)
	}
}

func normalizedSpeedRetryKind(kind string) string {
	if kind == "" || kind == legacySpeedRetryKindLowSpeed || kind == legacySpeedRetryKindDeadline {
		return speedRetryKindConfirmation
	}
	return kind
}

func speedRetryKindForSource(source string) (string, bool) {
	if source == speedConfirmationRetrySource {
		return speedRetryKindConfirmation, true
	}
	return "", false
}

func speedRetrySourceForKind(kind string) (string, bool) {
	switch normalizedSpeedRetryKind(kind) {
	case speedRetryKindConfirmation:
		return speedConfirmationRetrySource, true
	default:
		return "", false
	}
}

func speedRetryKindLabel(kind string) string {
	return "confirmation"
}

func (s *Service) runSpeedTest(req speedtest.RunRequest, source string) error {
	if source == "telegram" {
		if err := s.runAvailabilityCheck(req.ProxyIDs); err != nil {
			return fmt.Errorf("availability check failed: %w", err)
		}
		req.OnlyOnline = true
		req.SkipOffline = true
	}
	if s.speedRunFunc != nil {
		return s.speedRunFunc(req, source)
	}
	if s.speedManager == nil {
		return fmt.Errorf("speed-test manager is unavailable")
	}
	return s.speedManager.Run(req, source)
}

func speedReportDecision(report speedtest.RunReport, cfg Config) (failed int, slow int, issuesOnly bool, shouldSend bool) {
	return speedReportDecisionWithMutes(report, cfg, mutedSpeedNodeSet(cfg))
}

func speedReportDecisionWithMutes(report speedtest.RunReport, cfg Config, muted map[string]bool) (failed int, slow int, issuesOnly bool, shouldSend bool) {
	if !cfg.Enabled || cfg.ChatID == "" || !cfg.SpeedReportsEnabled || cfg.SpeedReportMode == "disabled" {
		return 0, 0, false, false
	}

	results := filterSpeedResultsByMuteSet(report.Results, muted)
	if len(results) == 0 {
		return 0, 0, false, false
	}

	failed, slow = countSpeedIssues(results, cfg.LowSpeedThresholdMbps)
	issuesOnly = cfg.SpeedReportMode == "issues"
	if issuesOnly && failed == 0 && slow == 0 {
		return failed, slow, issuesOnly, false
	}
	return failed, slow, issuesOnly, true
}

func countSpeedIssues(results []speedtest.Result, threshold float64) (failed int, slow int) {
	for _, result := range results {
		if result.Offline {
			failed++
			continue
		}
		if result.Error != "" {
			failed++
			continue
		}
		if effective := resultThreshold(result, threshold); (effective > 0 && result.Mbps < effective) || result.TimedOut {
			slow++
		}
	}
	return failed, slow
}

func filterMutedRunReport(report speedtest.RunReport, cfg Config) speedtest.RunReport {
	return filterRunReportByMuteSet(report, mutedSpeedNodeSet(cfg))
}

func filterRunReportByMuteSet(report speedtest.RunReport, muted map[string]bool) speedtest.RunReport {
	report.Results = filterSpeedResultsByMuteSet(report.Results, muted)
	report.Selected = len(report.Results)
	return report
}

func filterTelegramAlertSuppressedRunReport(report speedtest.RunReport) speedtest.RunReport {
	filtered := make([]speedtest.Result, 0, len(report.Results))
	for _, result := range report.Results {
		if result.TelegramAlertSuppressed {
			continue
		}
		filtered = append(filtered, result)
	}
	report.Results = filtered
	report.Selected = len(filtered)
	return report
}

func filterMutedSpeedResults(results []speedtest.Result, cfg Config) []speedtest.Result {
	return filterSpeedResultsByMuteSet(results, mutedSpeedNodeSet(cfg))
}

func filterSpeedResultsByMuteSet(results []speedtest.Result, muted map[string]bool) []speedtest.Result {
	if len(results) == 0 || len(muted) == 0 {
		return results
	}
	filtered := make([]speedtest.Result, 0, len(results))
	for _, result := range results {
		if result.StableID != "" && muted[result.StableID] {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func (s *Service) activeSpeedResults(results []speedtest.Result) []speedtest.Result {
	active := s.monitoredNodeIDs()
	if len(results) == 0 || len(active) == 0 {
		return nil
	}

	filtered := make([]speedtest.Result, 0, len(results))
	for _, result := range results {
		if !result.MaintenanceProbe && result.StableID != "" && active[result.StableID] {
			filtered = append(filtered, result)
		}
	}
	return filtered
}
