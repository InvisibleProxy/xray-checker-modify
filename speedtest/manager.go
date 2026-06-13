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
	defaultURL          = "https://proof.ovh.net/files/100Mb.dat"
	defaultMaxBytes     = int64(100 * 1024 * 1024)
	defaultTimeoutSec   = 120
	defaultConcurrency  = 2
	maxBytesLimit       = int64(100 * 1024 * 1024)
	maxTimeoutSec       = 300
	maxConcurrency      = 10
	defaultHistoryLimit = 10
	maxHistoryLimit     = 1000
)

type TestConfig struct {
	URL         string `json:"url"`
	MaxBytes    int64  `json:"maxBytes"`
	TimeoutSec  int    `json:"timeoutSec"`
	Concurrency int    `json:"concurrency"`
}

type RunRequest struct {
	ProxyIDs    []string   `json:"proxyIds"`
	OnlyOnline  bool       `json:"onlyOnline"`
	SkipOffline bool       `json:"skipOffline"`
	SubName     string     `json:"subName"`
	Protocol    string     `json:"protocol"`
	Config      TestConfig `json:"config"`
}

type ScheduleConfig struct {
	Enabled      bool              `json:"enabled"`
	IntervalSec  int               `json:"intervalSec"`
	ProxyIDs     []string          `json:"proxyIds"`
	OnlyOnline   bool              `json:"onlyOnline"`
	SubName      string            `json:"subName"`
	Protocol     string            `json:"protocol"`
	Config       TestConfig        `json:"config"`
	NodeTestURLs map[string]string `json:"nodeTestUrls,omitempty"`
	HistoryLimit int               `json:"historyLimit,omitempty"`
}

type Result struct {
	StableID        string                    `json:"stableId"`
	Name            string                    `json:"name"`
	SubName         string                    `json:"subName"`
	Protocol        string                    `json:"protocol"`
	URL             string                    `json:"url"`
	StatusCode      int                       `json:"statusCode"`
	DownloadedBytes int64                     `json:"downloadedBytes"`
	DurationMs      int64                     `json:"durationMs"`
	TTFBMs          int64                     `json:"ttfbMs"`
	Mbps            float64                   `json:"mbps"`
	Error           string                    `json:"error"`
	Offline         bool                      `json:"offline"`
	HostCheck       *checker.HostCheckDetails `json:"hostCheck,omitempty"`
	PingCheck       *checker.PingCheckDetails `json:"pingCheck,omitempty"`
	CheckedAt       time.Time                 `json:"checkedAt"`
	Source          string                    `json:"source"`
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
	Source     string
	StartedAt  time.Time
	FinishedAt time.Time
	Selected   int
	Config     TestConfig
	Results    []Result
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
	schedule.HistoryLimit = normalizeHistoryLimit(schedule.HistoryLimit)
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
	historyLimit := m.historyLimit()
	results := make(map[string]Result)
	history := make(map[string][]Result)

	for stableID, entries := range state.History {
		stableID = strings.TrimSpace(stableID)
		if stableID == "" {
			continue
		}
		if _, ok := active[stableID]; !ok {
			continue
		}
		entries = normalizeResultHistory(stableID, active[stableID], entries, historyLimit)
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
		if _, ok := active[stableID]; !ok {
			continue
		}
		result = normalizeResult(stableID, active[stableID], result)
		current, ok := results[stableID]
		if !ok || result.CheckedAt.After(current.CheckedAt) {
			results[stableID] = result
			history[stableID] = append([]Result{result}, history[stableID]...)
			history[stableID] = normalizeResultHistory(stableID, active[stableID], history[stableID], historyLimit)
		}
		if _, ok := history[stableID]; !ok {
			history[stableID] = []Result{result}
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
	schedule.HistoryLimit = normalizeHistoryLimit(schedule.HistoryLimit)

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

	history := m.history[stableID]
	historyLimit := m.historyLimitLocked()
	if len(history) > historyLimit {
		history = history[:historyLimit]
	}
	result := make([]Result, len(history))
	copy(result, history)
	return result
}

func (m *Manager) Schedule() ScheduleConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	schedule := m.schedule
	schedule.NodeTestURLs = copyStringMap(m.schedule.NodeTestURLs)
	schedule.HistoryLimit = normalizeHistoryLimit(schedule.HistoryLimit)
	return schedule
}

func (m *Manager) historyLimit() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.historyLimitLocked()
}

func (m *Manager) historyLimitLocked() int {
	return normalizeHistoryLimit(m.schedule.HistoryLimit)
}

func (m *Manager) pruneHistoryLocked(historyLimit int) bool {
	historyLimit = normalizeHistoryLimit(historyLimit)
	pruned := false
	for stableID, entries := range m.history {
		if len(entries) <= historyLimit {
			continue
		}
		m.history[stableID] = entries[:historyLimit]
		pruned = true
	}
	return pruned
}

func (m *Manager) UpdateSchedule(schedule ScheduleConfig) error {
	m.mu.RLock()
	existingNodeTestURLs := copyStringMap(m.schedule.NodeTestURLs)
	m.mu.RUnlock()

	schedule.Config = m.normalizeConfig(schedule.Config)
	if schedule.NodeTestURLs == nil {
		schedule.NodeTestURLs = existingNodeTestURLs
	}
	schedule.NodeTestURLs = normalizeNodeTestURLs(schedule.NodeTestURLs, m.activeProxiesByID())
	schedule.HistoryLimit = normalizeHistoryLimit(schedule.HistoryLimit)
	if schedule.Enabled && schedule.IntervalSec < 60 {
		return fmt.Errorf("intervalSec must be at least 60 when schedule is enabled")
	}
	if !schedule.Enabled && schedule.IntervalSec == 0 {
		schedule.IntervalSec = 3600
	}

	if err := m.saveSchedule(schedule); err != nil {
		return err
	}

	var resultState resultStateFile
	saveResults := false
	m.mu.Lock()
	oldHistoryLimit := m.historyLimitLocked()
	m.schedule = schedule
	if m.pruneHistoryLocked(schedule.HistoryLimit) || normalizeHistoryLimit(schedule.HistoryLimit) < oldHistoryLimit {
		resultState = m.resultStateLocked()
		saveResults = true
	}
	m.mu.Unlock()

	if saveResults {
		if err := m.saveResults(resultState); err != nil {
			logger.Warn("Failed to save pruned speed-test results: %v", err)
		}
	}

	m.signalScheduleChange()
	return nil
}

func (m *Manager) UpdateNodeTestURL(stableID string, testURL string) error {
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
	schedule.HistoryLimit = normalizeHistoryLimit(schedule.HistoryLimit)
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

func (m *Manager) Run(req RunRequest, source string) error {
	req.Config = m.normalizeConfig(req.Config)
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

	go m.run(proxies, req.Config, source, startedAt, req.SkipOffline)
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

func (m *Manager) run(proxies []*models.ProxyConfig, cfg TestConfig, source string, startedAt time.Time, skipOffline bool) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.Concurrency)
	results := make(chan Result, len(proxies))
	runResults := make([]Result, 0, len(proxies))

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
			results <- m.testProxy(p, proxyCfg, source)
		}(proxy)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		runResults = append(runResults, result)

		m.mu.Lock()
		historyLimit := m.historyLimitLocked()
		m.results[result.StableID] = result
		m.history[result.StableID] = append([]Result{result}, m.history[result.StableID]...)
		if len(m.history[result.StableID]) > historyLimit {
			m.history[result.StableID] = m.history[result.StableID][:historyLimit]
		}
		m.lastRun.Completed++
		m.mu.Unlock()
	}

	finishedAt := time.Now()
	report := RunReport{
		Source:     source,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Selected:   len(proxies),
		Config:     cfg,
		Results:    runResults,
	}

	m.mu.Lock()
	m.running = false
	m.lastRun.Running = false
	m.lastRun.FinishedAt = &finishedAt
	reporter := m.reporter
	state := m.resultStateLocked()
	m.mu.Unlock()

	if err := m.saveResults(state); err != nil {
		logger.Warn("Failed to save speed-test results: %v", err)
	}

	if reporter != nil {
		go reporter.NotifySpeedTest(report)
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
	historyLimit := m.historyLimitLocked()
	results := make(map[string]Result, len(m.results))
	for stableID, result := range m.results {
		results[stableID] = result
	}

	history := make(map[string][]Result, len(m.history))
	for stableID, entries := range m.history {
		copied := make([]Result, len(entries))
		copy(copied, entries)
		if len(copied) > historyLimit {
			copied = copied[:historyLimit]
		}
		history[stableID] = copied
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

func normalizeResultHistory(stableID string, proxy *models.ProxyConfig, entries []Result, historyLimit int) []Result {
	historyLimit = normalizeHistoryLimit(historyLimit)
	result := make([]Result, 0, len(entries))
	for _, entry := range entries {
		result = append(result, normalizeResult(stableID, proxy, entry))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CheckedAt.After(result[j].CheckedAt)
	})
	if len(result) > historyLimit {
		result = result[:historyLimit]
	}
	return result
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

func normalizeHistoryLimit(value int) int {
	if value <= 0 {
		return defaultHistoryLimit
	}
	if value > maxHistoryLimit {
		return maxHistoryLimit
	}
	return value
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
