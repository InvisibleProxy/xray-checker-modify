package speedtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/models"
)

const (
	defaultURL                  = "https://proof.ovh.net/files/100Mb.dat"
	defaultMaxBytes             = int64(100 * 1024 * 1024)
	defaultTimeoutSec           = 120
	defaultConcurrency          = 2
	maxBytesLimit               = int64(100 * 1024 * 1024)
	maxTimeoutSec               = 300
	maxConcurrency              = 10
	defaultHistoryRetentionDays = 60
	maxHistoryRetentionDays     = 3650
)

type TestConfig struct {
	URL         string `json:"url"`
	MaxBytes    int64  `json:"maxBytes"`
	TimeoutSec  int    `json:"timeoutSec"`
	Concurrency int    `json:"concurrency"`
}

// ReportTarget is an ephemeral Telegram destination for an explicitly
// requested speed-test result. It is intentionally never persisted or exposed
// through the admin API.
type ReportTarget struct {
	ChatID          string `json:"-"`
	MessageThreadID int    `json:"-"`
}

type RunRequest struct {
	ProxyIDs     []string     `json:"proxyIds"`
	OnlyOnline   bool         `json:"onlyOnline"`
	SkipOffline  bool         `json:"skipOffline"`
	SubName      string       `json:"subName"`
	Protocol     string       `json:"protocol"`
	Config       TestConfig   `json:"config"`
	ReportTarget ReportTarget `json:"-"`
}

type ScheduleConfig struct {
	Enabled              bool              `json:"enabled"`
	IntervalSec          int               `json:"intervalSec"`
	ProxyIDs             []string          `json:"proxyIds"`
	OnlyOnline           bool              `json:"onlyOnline"`
	SubName              string            `json:"subName"`
	Protocol             string            `json:"protocol"`
	Config               TestConfig        `json:"config"`
	NodeTestURLs         map[string]string `json:"nodeTestUrls,omitempty"`
	HistoryRetentionDays int               `json:"historyRetentionDays,omitempty"`
}

type Result struct {
	StableID                string                    `json:"stableId"`
	Name                    string                    `json:"name"`
	SubName                 string                    `json:"subName"`
	Protocol                string                    `json:"protocol"`
	URL                     string                    `json:"url"`
	PrimaryURL              string                    `json:"primaryUrl,omitempty"`
	PrimaryError            string                    `json:"primaryError,omitempty"`
	FallbackUsed            bool                      `json:"fallbackUsed,omitempty"`
	FallbackID              string                    `json:"fallbackId,omitempty"`
	FallbackProvider        string                    `json:"fallbackProvider,omitempty"`
	FallbackCity            string                    `json:"fallbackCity,omitempty"`
	FallbackCountryCode     string                    `json:"fallbackCountryCode,omitempty"`
	TelegramAlertSuppressed bool                      `json:"telegramAlertSuppressed,omitempty"`
	StatusCode              int                       `json:"statusCode"`
	DownloadedBytes         int64                     `json:"downloadedBytes"`
	DurationMs              int64                     `json:"durationMs"`
	TTFBMs                  int64                     `json:"ttfbMs"`
	Mbps                    float64                   `json:"mbps"`
	Error                   string                    `json:"error"`
	Offline                 bool                      `json:"offline"`
	HostCheck               *checker.HostCheckDetails `json:"hostCheck,omitempty"`
	PingCheck               *checker.PingCheckDetails `json:"pingCheck,omitempty"`
	CheckedAt               time.Time                 `json:"checkedAt"`
	Source                  string                    `json:"source"`
}

type RunInfo struct {
	Running    bool       `json:"running"`
	Source     string     `json:"source"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Selected   int        `json:"selected"`
	Completed  int        `json:"completed"`
	Error      string     `json:"error"`
	Config     TestConfig `json:"config"`
}

type Snapshot struct {
	Defaults           TestConfig        `json:"defaults"`
	Schedule           ScheduleConfig    `json:"schedule"`
	NodeTestURLs       map[string]string `json:"nodeTestUrls"`
	NextScheduledRunAt *time.Time        `json:"nextScheduledRunAt,omitempty"`
	LastRun            RunInfo           `json:"lastRun"`
	Results            []Result          `json:"results"`
}

type RunReport struct {
	Source       string
	StartedAt    time.Time
	FinishedAt   time.Time
	Selected     int
	Config       TestConfig
	Results      []Result
	ReportTarget ReportTarget `json:"-"`
}

type resultStateFile struct {
	Version   int                 `json:"version"`
	UpdatedAt time.Time           `json:"updatedAt"`
	LastRun   RunInfo             `json:"lastRun"`
	Results   map[string]Result   `json:"results"`
	History   map[string][]Result `json:"history"`
}

type Reporter interface {
	NotifySpeedTest(report RunReport)
}

type Manager struct {
	proxyChecker *checker.ProxyChecker
	startPort    int
	statePath    string
	resultPath   string
	defaults     TestConfig

	mu       sync.RWMutex
	running  bool
	lastRun  RunInfo
	results  map[string]Result
	history  map[string][]Result
	schedule ScheduleConfig
	nextRun  time.Time
	reporter Reporter
	runGate  sync.Locker

	resultPersistMu        sync.Mutex
	schedulePersistMu      sync.Mutex
	fallbackMu             sync.Mutex
	fallbackCatalogPath    string
	fallbackHealthPath     string
	fallbackCatalog        countryTestURLCatalog
	fallbackCatalogModTime time.Time
	fallbackCatalogLoaded  bool
	fallbackHealth         fallbackHealthState
	countryResolver        func(stableID string) string
	lowSpeedThresholdMbps  float64
	testAttempt            func(proxy *models.ProxyConfig, cfg TestConfig, source string) Result

	stopCh     chan struct{}
	scheduleCh chan struct{}
}

func NewManager(proxyChecker *checker.ProxyChecker, startPort int, statePath string, defaults TestConfig) *Manager {
	defaults = normalizeConfig(defaults)
	return &Manager{
		proxyChecker: proxyChecker,
		startPort:    startPort,
		statePath:    statePath,
		resultPath:   resultStatePath(statePath),
		defaults:     defaults,
		results:      make(map[string]Result),
		history:      make(map[string][]Result),
		stopCh:       make(chan struct{}),
		scheduleCh:   make(chan struct{}, 1),
	}
}

func (m *Manager) SetReporter(reporter Reporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reporter = reporter
}

// SetRunGate coordinates speed tests with Xray lifecycle changes. The gate is
// acquired before proxy pointers and SOCKS ports are selected and released only
// after every result has been collected.
func (m *Manager) SetRunGate(gate sync.Locker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runGate = gate
}

func (m *Manager) Load() error {
	if m.statePath == "" {
		return m.loadResults()
	}

	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return m.loadResults()
		}
		return err
	}

	var schedule ScheduleConfig
	if err := json.Unmarshal(data, &schedule); err != nil {
		return err
	}
	schedule.Config = m.normalizeConfig(schedule.Config)
	schedule.NodeTestURLs = normalizeNodeTestURLs(schedule.NodeTestURLs, m.activeProxiesByID())
	schedule.HistoryRetentionDays = normalizeHistoryRetentionDays(schedule.HistoryRetentionDays)
	if schedule.IntervalSec < 60 {
		schedule.IntervalSec = 3600
	}

	m.mu.Lock()
	m.schedule = schedule
	m.mu.Unlock()
	return m.loadResults()
}

func (m *Manager) loadResults() error {
	if m.resultPath == "" {
		return nil
	}

	data, err := os.ReadFile(m.resultPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var state resultStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	active := m.activeProxiesByID()
	historyRetentionDays := m.historyRetentionDays()
	now := time.Now()
	results := make(map[string]Result)
	history := make(map[string][]Result)

	for stableID, entries := range state.History {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			continue
		}
		entries = normalizeResultHistory(stableID, active[stableID], entries, historyRetentionDays, now)
		if len(entries) == 0 {
			continue
		}
		history[stableID] = entries
		results[stableID] = entries[0]
	}

	for stableID, result := range state.Results {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			continue
		}
		result = normalizeResult(stableID, active[stableID], result)
		current, ok := results[stableID]
		if !ok || result.CheckedAt.After(current.CheckedAt) {
			results[stableID] = result
			if resultWithinRetention(result, historyRetentionDays, now) {
				history[stableID] = append([]Result{result}, history[stableID]...)
				history[stableID] = normalizeResultHistory(stableID, active[stableID], history[stableID], historyRetentionDays, now)
			}
		}
	}

	lastRun := state.LastRun
	lastRun.Running = false

	m.mu.Lock()
	m.results = results
	m.history = history
	m.lastRun = lastRun
	m.mu.Unlock()

	logger.Info("Loaded speed-test results: %d latest results, %d histories", len(results), len(history))
	return nil
}

func (m *Manager) StartScheduler() {
	go m.schedulerLoop()
}

func (m *Manager) Stop() {
	close(m.stopCh)
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	schedule := m.schedule
	schedule.NodeTestURLs = copyStringMap(m.schedule.NodeTestURLs)
	schedule.HistoryRetentionDays = normalizeHistoryRetentionDays(schedule.HistoryRetentionDays)

	results := make([]Result, 0, len(m.results))
	for _, result := range m.results {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	var nextScheduledRunAt *time.Time
	if !m.nextRun.IsZero() {
		nextRun := m.nextRun
		nextScheduledRunAt = &nextRun
	}

	return Snapshot{
		Defaults:           m.defaults,
		Schedule:           schedule,
		NodeTestURLs:       copyStringMap(schedule.NodeTestURLs),
		NextScheduledRunAt: nextScheduledRunAt,
		LastRun:            m.lastRun,
		Results:            results,
	}
}

func (m *Manager) ResultHistory(stableID string) []Result {
	m.mu.RLock()
	defer m.mu.RUnlock()

	history := retainResultHistory(m.history[stableID], historyCutoff(m.historyRetentionDaysLocked(), time.Now()))
	return history
}

func (m *Manager) AllResultHistory() map[string][]Result {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cutoff := historyCutoff(m.historyRetentionDaysLocked(), time.Now())
	result := make(map[string][]Result, len(m.history))
	for stableID, entries := range m.history {
		entries = retainResultHistory(entries, cutoff)
		result[stableID] = entries
	}
	return result
}

func (m *Manager) DeleteHistory(stableID string) error {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return fmt.Errorf("stableId is required")
	}

	m.mu.Lock()
	latest, hadLatest := m.results[stableID]
	history, hadHistory := m.history[stableID]
	history = append([]Result(nil), history...)
	delete(m.results, stableID)
	delete(m.history, stableID)
	m.mu.Unlock()

	if err := m.persistResults(); err != nil {
		m.mu.Lock()
		if hadLatest {
			m.results[stableID] = latest
		}
		if hadHistory {
			m.history[stableID] = history
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) Schedule() ScheduleConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	schedule := m.schedule
	schedule.NodeTestURLs = copyStringMap(m.schedule.NodeTestURLs)
	schedule.HistoryRetentionDays = normalizeHistoryRetentionDays(schedule.HistoryRetentionDays)
	return schedule
}

func (m *Manager) historyRetentionDays() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.historyRetentionDaysLocked()
}

func (m *Manager) historyRetentionDaysLocked() int {
	return normalizeHistoryRetentionDays(m.schedule.HistoryRetentionDays)
}

func (m *Manager) pruneHistoryLocked(historyRetentionDays int, now time.Time) bool {
	cutoff := historyCutoff(historyRetentionDays, now)
	pruned := false
	for stableID, entries := range m.history {
		retained := retainResultHistory(entries, cutoff)
		if len(retained) == len(entries) {
			continue
		}
		if len(retained) == 0 {
			delete(m.history, stableID)
		} else {
			m.history[stableID] = retained
		}
		pruned = true
	}
	return pruned
}

func (m *Manager) UpdateSchedule(schedule ScheduleConfig) error {
	m.schedulePersistMu.Lock()
	defer m.schedulePersistMu.Unlock()

	m.mu.RLock()
	existingNodeTestURLs := copyStringMap(m.schedule.NodeTestURLs)
	m.mu.RUnlock()

	schedule.Config = m.normalizeConfig(schedule.Config)
	if schedule.NodeTestURLs == nil {
		schedule.NodeTestURLs = existingNodeTestURLs
	}
	schedule.NodeTestURLs = normalizeNodeTestURLs(schedule.NodeTestURLs, m.activeProxiesByID())
	schedule.HistoryRetentionDays = normalizeHistoryRetentionDays(schedule.HistoryRetentionDays)
	if schedule.Enabled && schedule.IntervalSec < 60 {
		return fmt.Errorf("intervalSec must be at least 60 when schedule is enabled")
	}
	if !schedule.Enabled && schedule.IntervalSec == 0 {
		schedule.IntervalSec = 3600
	}

	if err := m.saveSchedule(schedule); err != nil {
		return err
	}

	saveResults := false
	m.mu.Lock()
	m.schedule = schedule
	if m.pruneHistoryLocked(schedule.HistoryRetentionDays, time.Now()) {
		saveResults = true
	}
	m.mu.Unlock()

	if saveResults {
		if err := m.persistResults(); err != nil {
			logger.Warn("Failed to save pruned speed-test results: %v", err)
		}
	}

	m.signalScheduleChange()
	return nil
}

func (m *Manager) UpdateNodeTestURL(stableID string, testURL string) error {
	m.schedulePersistMu.Lock()
	defer m.schedulePersistMu.Unlock()

	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return fmt.Errorf("stableId is required")
	}

	active := m.activeProxiesByID()
	if _, ok := active[stableID]; !ok {
		return fmt.Errorf("proxy not found")
	}

	normalizedURL, err := normalizeNodeTestURL(testURL)
	if err != nil {
		return err
	}

	m.mu.RLock()
	schedule := m.schedule
	schedule.NodeTestURLs = copyStringMap(m.schedule.NodeTestURLs)
	m.mu.RUnlock()

	schedule.Config = m.normalizeConfig(schedule.Config)
	schedule.HistoryRetentionDays = normalizeHistoryRetentionDays(schedule.HistoryRetentionDays)
	if !schedule.Enabled && schedule.IntervalSec == 0 {
		schedule.IntervalSec = 3600
	}

	if normalizedURL == "" {
		delete(schedule.NodeTestURLs, stableID)
	} else {
		if schedule.NodeTestURLs == nil {
			schedule.NodeTestURLs = make(map[string]string)
		}
		schedule.NodeTestURLs[stableID] = normalizedURL
	}
	schedule.NodeTestURLs = normalizeNodeTestURLs(schedule.NodeTestURLs, active)

	if err := m.saveSchedule(schedule); err != nil {
		return err
	}

	m.mu.Lock()
	m.schedule = schedule
	m.mu.Unlock()
	return nil
}

func (m *Manager) updateScheduleTestURL(testURL string) error {
	testURL = strings.TrimSpace(testURL)
	if testURL == "" {
		return nil
	}

	m.schedulePersistMu.Lock()
	defer m.schedulePersistMu.Unlock()

	m.mu.RLock()
	schedule := m.schedule
	schedule.NodeTestURLs = copyStringMap(m.schedule.NodeTestURLs)
	m.mu.RUnlock()

	schedule.Config = m.normalizeConfig(schedule.Config)
	schedule.HistoryRetentionDays = normalizeHistoryRetentionDays(schedule.HistoryRetentionDays)
	if schedule.Config.URL == testURL {
		return nil
	}
	schedule.Config.URL = testURL
	if !schedule.Enabled && schedule.IntervalSec == 0 {
		schedule.IntervalSec = 3600
	}

	if err := m.saveSchedule(schedule); err != nil {
		return err
	}

	m.mu.Lock()
	m.schedule = schedule
	m.mu.Unlock()
	return nil
}

func (m *Manager) Run(req RunRequest, source string) error {
	req.Config = m.normalizeConfig(req.Config)
	if err := m.reloadCountryFallbackCatalog(false); err != nil {
		logger.Warn("Failed to reload country Test URL catalog; keeping last valid catalog: %v", err)
	}
	if source == "manual" {
		if err := m.updateScheduleTestURL(req.Config.URL); err != nil {
			return fmt.Errorf("save test URL: %w", err)
		}
	}
	m.mu.RLock()
	runGate := m.runGate
	m.mu.RUnlock()
	if runGate != nil {
		runGate.Lock()
	}
	gateHeld := runGate != nil
	defer func() {
		if gateHeld {
			runGate.Unlock()
		}
	}()

	proxies := m.selectProxies(req)
	if len(proxies) == 0 {
		return fmt.Errorf("no proxies selected")
	}

	startedAt := time.Now()
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return fmt.Errorf("speed test is already running")
	}
	m.running = true
	m.lastRun = RunInfo{
		Running:   true,
		Source:    source,
		StartedAt: startedAt,
		Selected:  len(proxies),
		Config:    req.Config,
	}
	m.mu.Unlock()

	go m.run(proxies, req.Config, source, startedAt, req.SkipOffline, req.ReportTarget, runGate)
	gateHeld = false
	return nil
}

func (m *Manager) schedulerLoop() {
	for {
		schedule := m.Schedule()
		if !schedule.Enabled {
			m.setNextScheduledRunAt(time.Time{})
			select {
			case <-m.scheduleCh:
				continue
			case <-m.stopCh:
				return
			}
		}

		interval := time.Duration(schedule.IntervalSec) * time.Second
		m.setNextScheduledRunAt(time.Now().Add(interval))
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			// Configuration may have changed without resetting the interval,
			// for example when a manual run saves a new shared Test URL.
			schedule = m.Schedule()
			if !schedule.Enabled {
				continue
			}
			req := RunRequest{
				ProxyIDs:    schedule.ProxyIDs,
				OnlyOnline:  schedule.OnlyOnline,
				SkipOffline: true,
				SubName:     schedule.SubName,
				Protocol:    schedule.Protocol,
				Config:      schedule.Config,
			}
			if err := m.Run(req, "schedule"); err != nil {
				logger.Warn("Scheduled speed test skipped: %v", err)
			}
		case <-m.scheduleCh:
			timer.Stop()
		case <-m.stopCh:
			timer.Stop()
			m.setNextScheduledRunAt(time.Time{})
			return
		}
	}
}

func (m *Manager) setNextScheduledRunAt(nextRun time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextRun = nextRun
}

func (m *Manager) run(proxies []*models.ProxyConfig, cfg TestConfig, source string, startedAt time.Time, skipOffline bool, reportTarget ReportTarget, runGate sync.Locker) {
	if runGate != nil {
		defer runGate.Unlock()
	}
	sem := make(chan struct{}, cfg.Concurrency)
	runResults := make([]Result, 0, len(proxies))

	recordResult := func(result Result) {
		runResults = append(runResults, result)

		m.mu.Lock()
		m.results[result.StableID] = result
		m.history[result.StableID] = append([]Result{result}, m.history[result.StableID]...)
		m.history[result.StableID] = retainResultHistory(
			m.history[result.StableID],
			historyCutoff(m.historyRetentionDaysLocked(), time.Now()),
		)
		m.lastRun.Completed++
		m.mu.Unlock()
	}

	if source == "manual" {
		m.runManualTestPhases(proxies, cfg, source, skipOffline, sem, recordResult)
	} else {
		var wg sync.WaitGroup
		results := make(chan Result, len(proxies))
		for _, proxy := range proxies {
			wg.Add(1)
			go func(p *models.ProxyConfig) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				proxyCfg := m.configForProxy(cfg, p)
				if skipOffline {
					if result, offline := m.offlineResult(p, proxyCfg, source); offline {
						results <- result
						return
					}
				}
				results <- m.testProxyWithFallback(p, proxyCfg, source)
			}(proxy)
		}

		go func() {
			wg.Wait()
			close(results)
		}()

		for result := range results {
			recordResult(result)
		}
	}

	finishedAt := time.Now()
	report := RunReport{
		Source:       source,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		Selected:     len(proxies),
		Config:       cfg,
		Results:      runResults,
		ReportTarget: reportTarget,
	}

	m.mu.Lock()
	m.running = false
	m.lastRun.Running = false
	m.lastRun.FinishedAt = &finishedAt
	reporter := m.reporter
	m.mu.Unlock()

	if err := m.persistResults(); err != nil {
		logger.Warn("Failed to save speed-test results: %v", err)
	}
	if err := m.persistFallbackHealth(); err != nil {
		logger.Warn("Failed to save Test URL health: %v", err)
	}

	if reporter != nil {
		go reporter.NotifySpeedTest(report)
	}
}

type manualPrimaryResult struct {
	proxy  *models.ProxyConfig
	config TestConfig
	result Result
}

func (m *Manager) runManualTestPhases(
	proxies []*models.ProxyConfig,
	cfg TestConfig,
	source string,
	skipOffline bool,
	sem chan struct{},
	recordResult func(Result),
) {
	threshold := m.fallbackLowSpeedThreshold()
	primaryResults := make(chan manualPrimaryResult, len(proxies))
	var primaryWG sync.WaitGroup
	for _, proxy := range proxies {
		primaryWG.Add(1)
		go func(p *models.ProxyConfig) {
			defer primaryWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			proxyCfg := m.configForProxy(cfg, p)
			if skipOffline {
				if result, offline := m.offlineResult(p, proxyCfg, source); offline {
					primaryResults <- manualPrimaryResult{proxy: p, config: proxyCfg, result: result}
					return
				}
			}
			primaryResults <- manualPrimaryResult{
				proxy:  p,
				config: proxyCfg,
				result: m.executeTestAttempt(p, proxyCfg, source),
			}
		}(proxy)
	}
	go func() {
		primaryWG.Wait()
		close(primaryResults)
	}()

	fallbackQueue := make([]manualPrimaryResult, 0)
	for primary := range primaryResults {
		if shouldAttemptFallback(primary.result, threshold) {
			fallbackQueue = append(fallbackQueue, primary)
			continue
		}
		recordResult(primary.result)
	}

	fallbackResults := make(chan Result, len(fallbackQueue))
	var fallbackWG sync.WaitGroup
	for _, queued := range fallbackQueue {
		fallbackWG.Add(1)
		go func(task manualPrimaryResult) {
			defer fallbackWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fallbackResults <- m.testFallbackForPrimary(task.proxy, task.config, source, task.result, threshold)
		}(queued)
	}
	go func() {
		fallbackWG.Wait()
		close(fallbackResults)
	}()

	for result := range fallbackResults {
		recordResult(result)
	}
}

func (m *Manager) testProxy(proxy *models.ProxyConfig, cfg TestConfig, source string) Result {
	result := Result{
		StableID:  proxy.StableID,
		Name:      proxy.Name,
		SubName:   proxy.SubName,
		Protocol:  proxy.Protocol,
		URL:       cfg.URL,
		CheckedAt: time.Now(),
		Source:    source,
	}

	proxyURL, err := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", m.startPort+proxy.Index))
	if err != nil {
		result.Error = err.Error()
		return result
	}

	transport := &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.TimeoutSec) * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSec)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ttfb = time.Since(start)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.DurationMs = time.Since(start).Milliseconds()
		result.TTFBMs = ttfb.Milliseconds()
		result.Error = fmt.Sprintf("HTTP status %d", resp.StatusCode)
		return result
	}

	buffer := make([]byte, 32768)
	for result.DownloadedBytes < cfg.MaxBytes {
		limit := int64(len(buffer))
		remaining := cfg.MaxBytes - result.DownloadedBytes
		if remaining < limit {
			limit = remaining
		}

		n, readErr := resp.Body.Read(buffer[:int(limit)])
		if n > 0 {
			result.DownloadedBytes += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			result.Error = readErr.Error()
			break
		}
	}

	duration := time.Since(start)
	result.DurationMs = duration.Milliseconds()
	result.TTFBMs = ttfb.Milliseconds()
	if result.DownloadedBytes > 0 && duration > 0 {
		result.Mbps = float64(result.DownloadedBytes*8) / duration.Seconds() / 1000000
	}
	if result.Error == "" && result.DownloadedBytes == 0 {
		result.Error = "empty response"
	}
	return result
}

func (m *Manager) offlineResult(proxy *models.ProxyConfig, cfg TestConfig, source string) (Result, bool) {
	if proxy.StableID == "" {
		proxy.StableID = proxy.GenerateStableID()
	}

	details, err := m.proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
	if err != nil || details.Online {
		return Result{}, false
	}

	result := Result{
		StableID:  proxy.StableID,
		Name:      proxy.Name,
		SubName:   proxy.SubName,
		Protocol:  proxy.Protocol,
		URL:       cfg.URL,
		Offline:   true,
		CheckedAt: time.Now(),
		Source:    source,
	}
	if details.HostCheck.Checked {
		hostCheck := details.HostCheck
		result.HostCheck = &hostCheck
	}
	if details.PingCheck.Checked {
		pingCheck := details.PingCheck
		result.PingCheck = &pingCheck
	}
	return result, true
}

func (m *Manager) configForProxy(cfg TestConfig, proxy *models.ProxyConfig) TestConfig {
	if proxy == nil {
		return cfg
	}
	if proxy.StableID == "" {
		proxy.StableID = proxy.GenerateStableID()
	}

	m.mu.RLock()
	testURL := strings.TrimSpace(m.schedule.NodeTestURLs[proxy.StableID])
	m.mu.RUnlock()
	if testURL != "" {
		cfg.URL = testURL
	}
	return cfg
}

func (m *Manager) selectProxies(req RunRequest) []*models.ProxyConfig {
	proxies := m.proxyChecker.GetProxies()
	selectedIDs := make(map[string]bool)
	for _, id := range req.ProxyIDs {
		if id != "" {
			selectedIDs[id] = true
		}
	}

	var selected []*models.ProxyConfig
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		if len(selectedIDs) > 0 && !selectedIDs[proxy.StableID] {
			continue
		}
		if req.SubName != "" && proxy.SubName != req.SubName {
			continue
		}
		if req.Protocol != "" && proxy.Protocol != req.Protocol {
			continue
		}
		if req.OnlyOnline {
			online, _, err := m.proxyChecker.GetProxyStatusByStableID(proxy.StableID)
			if err != nil || !online {
				continue
			}
		}
		selected = append(selected, proxy)
	}
	return selected
}

func (m *Manager) resultStateLocked() resultStateFile {
	cutoff := historyCutoff(m.historyRetentionDaysLocked(), time.Now())
	results := make(map[string]Result, len(m.results))
	for stableID, result := range m.results {
		results[stableID] = result
	}

	history := make(map[string][]Result, len(m.history))
	for stableID, entries := range m.history {
		retained := retainResultHistory(entries, cutoff)
		if len(retained) > 0 {
			history[stableID] = retained
		}
	}

	lastRun := m.lastRun
	lastRun.Running = false

	return resultStateFile{
		Version:   1,
		UpdatedAt: time.Now(),
		LastRun:   lastRun,
		Results:   results,
		History:   history,
	}
}

func (m *Manager) persistResults() error {
	m.resultPersistMu.Lock()
	defer m.resultPersistMu.Unlock()

	m.mu.RLock()
	state := m.resultStateLocked()
	m.mu.RUnlock()
	return m.saveResults(state)
}

func (m *Manager) saveResults(state resultStateFile) error {
	if m.resultPath == "" {
		return nil
	}
	if len(state.Results) == 0 && len(state.History) == 0 {
		if err := os.Remove(m.resultPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.resultPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.resultPath, data, 0644)
}

func (m *Manager) activeProxiesByID() map[string]*models.ProxyConfig {
	result := make(map[string]*models.ProxyConfig)
	for _, proxy := range m.proxyChecker.GetProxies() {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		result[proxy.StableID] = proxy
	}
	return result
}

func normalizeResultHistory(stableID string, proxy *models.ProxyConfig, entries []Result, historyRetentionDays int, now time.Time) []Result {
	result := make([]Result, 0, len(entries))
	for _, entry := range entries {
		result = append(result, normalizeResult(stableID, proxy, entry))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CheckedAt.After(result[j].CheckedAt)
	})
	return retainResultHistory(result, historyCutoff(historyRetentionDays, now))
}

func normalizeResult(stableID string, proxy *models.ProxyConfig, result Result) Result {
	result.StableID = stableID
	if proxy != nil {
		result.Name = proxy.Name
		result.SubName = proxy.SubName
		result.Protocol = proxy.Protocol
	}
	return result
}

func (m *Manager) saveSchedule(schedule ScheduleConfig) error {
	if m.statePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.statePath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(schedule, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.statePath, data, 0644)
}

func resultStatePath(schedulePath string) string {
	if schedulePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(schedulePath), "speedtest_results.json")
}

func normalizeHistoryRetentionDays(value int) int {
	if value <= 0 {
		return defaultHistoryRetentionDays
	}
	if value > maxHistoryRetentionDays {
		return maxHistoryRetentionDays
	}
	return value
}

func historyCutoff(historyRetentionDays int, now time.Time) time.Time {
	days := normalizeHistoryRetentionDays(historyRetentionDays)
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

func resultWithinRetention(result Result, historyRetentionDays int, now time.Time) bool {
	return !result.CheckedAt.IsZero() && !result.CheckedAt.Before(historyCutoff(historyRetentionDays, now))
}

func retainResultHistory(entries []Result, cutoff time.Time) []Result {
	retained := make([]Result, 0, len(entries))
	for _, entry := range entries {
		if entry.CheckedAt.IsZero() || entry.CheckedAt.Before(cutoff) {
			continue
		}
		retained = append(retained, entry)
	}
	return retained
}

func normalizeNodeTestURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid test URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("test URL must use http or https")
	}
	return value, nil
}

func normalizeNodeTestURLs(values map[string]string, active map[string]*models.ProxyConfig) map[string]string {
	if len(values) == 0 {
		return nil
	}

	result := make(map[string]string, len(values))
	for stableID, testURL := range values {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			continue
		}
		if active != nil {
			if _, ok := active[stableID]; !ok {
				continue
			}
		}
		normalizedURL, err := normalizeNodeTestURL(testURL)
		if err != nil || normalizedURL == "" {
			continue
		}
		result[stableID] = normalizedURL
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (m *Manager) signalScheduleChange() {
	select {
	case m.scheduleCh <- struct{}{}:
	default:
	}
}

func (m *Manager) normalizeConfig(cfg TestConfig) TestConfig {
	return normalizeConfigWithDefaults(cfg, m.defaults)
}

func normalizeConfig(cfg TestConfig) TestConfig {
	return normalizeConfigWithDefaults(cfg, TestConfig{
		URL:         defaultURL,
		MaxBytes:    defaultMaxBytes,
		TimeoutSec:  defaultTimeoutSec,
		Concurrency: defaultConcurrency,
	})
}

func normalizeConfigWithDefaults(cfg TestConfig, defaults TestConfig) TestConfig {
	if defaults.URL == "" {
		defaults.URL = defaultURL
	}
	if defaults.MaxBytes <= 0 {
		defaults.MaxBytes = defaultMaxBytes
	}
	if defaults.TimeoutSec <= 0 {
		defaults.TimeoutSec = defaultTimeoutSec
	}
	if defaults.Concurrency <= 0 {
		defaults.Concurrency = defaultConcurrency
	}

	if cfg.URL == "" {
		cfg.URL = defaults.URL
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaults.MaxBytes
	}
	if cfg.MaxBytes > maxBytesLimit {
		cfg.MaxBytes = maxBytesLimit
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = defaults.TimeoutSec
	}
	if cfg.TimeoutSec > maxTimeoutSec {
		cfg.TimeoutSec = maxTimeoutSec
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaults.Concurrency
	}
	if cfg.Concurrency > maxConcurrency {
		cfg.Concurrency = maxConcurrency
	}
	return cfg
}
