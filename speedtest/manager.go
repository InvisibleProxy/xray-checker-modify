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
)

type TestConfig struct {
	URL         string `json:"url"`
	MaxBytes    int64  `json:"maxBytes"`
	TimeoutSec  int    `json:"timeoutSec"`
	Concurrency int    `json:"concurrency"`
}

type RunRequest struct {
	ProxyIDs   []string   `json:"proxyIds"`
	OnlyOnline bool       `json:"onlyOnline"`
	SubName    string     `json:"subName"`
	Protocol   string     `json:"protocol"`
	Config     TestConfig `json:"config"`
}

type ScheduleConfig struct {
	Enabled     bool       `json:"enabled"`
	IntervalSec int        `json:"intervalSec"`
	ProxyIDs    []string   `json:"proxyIds"`
	OnlyOnline  bool       `json:"onlyOnline"`
	SubName     string     `json:"subName"`
	Protocol    string     `json:"protocol"`
	Config      TestConfig `json:"config"`
}

type Result struct {
	StableID        string    `json:"stableId"`
	Name            string    `json:"name"`
	SubName         string    `json:"subName"`
	Protocol        string    `json:"protocol"`
	URL             string    `json:"url"`
	StatusCode      int       `json:"statusCode"`
	DownloadedBytes int64     `json:"downloadedBytes"`
	DurationMs      int64     `json:"durationMs"`
	TTFBMs          int64     `json:"ttfbMs"`
	Mbps            float64   `json:"mbps"`
	Error           string    `json:"error"`
	CheckedAt       time.Time `json:"checkedAt"`
	Source          string    `json:"source"`
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
	Defaults TestConfig     `json:"defaults"`
	Schedule ScheduleConfig `json:"schedule"`
	LastRun  RunInfo        `json:"lastRun"`
	Results  []Result       `json:"results"`
}

type RunReport struct {
	Source     string
	StartedAt  time.Time
	FinishedAt time.Time
	Selected   int
	Config     TestConfig
	Results    []Result
}

type Reporter interface {
	NotifySpeedTest(report RunReport)
}

type Manager struct {
	proxyChecker *checker.ProxyChecker
	startPort    int
	statePath    string
	defaults     TestConfig

	mu       sync.RWMutex
	running  bool
	lastRun  RunInfo
	results  map[string]Result
	history  map[string][]Result
	schedule ScheduleConfig
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
		return nil
	}

	data, err := os.ReadFile(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var schedule ScheduleConfig
	if err := json.Unmarshal(data, &schedule); err != nil {
		return err
	}
	schedule.Config = m.normalizeConfig(schedule.Config)
	if schedule.IntervalSec < 60 {
		schedule.IntervalSec = 3600
	}

	m.mu.Lock()
	m.schedule = schedule
	m.mu.Unlock()
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

	results := make([]Result, 0, len(m.results))
	for _, result := range m.results {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	return Snapshot{
		Defaults: m.defaults,
		Schedule: m.schedule,
		LastRun:  m.lastRun,
		Results:  results,
	}
}

func (m *Manager) ResultHistory(stableID string) []Result {
	m.mu.RLock()
	defer m.mu.RUnlock()

	history := m.history[stableID]
	result := make([]Result, len(history))
	copy(result, history)
	return result
}

func (m *Manager) Schedule() ScheduleConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.schedule
}

func (m *Manager) UpdateSchedule(schedule ScheduleConfig) error {
	schedule.Config = m.normalizeConfig(schedule.Config)
	if schedule.Enabled && schedule.IntervalSec < 60 {
		return fmt.Errorf("intervalSec must be at least 60 when schedule is enabled")
	}
	if !schedule.Enabled && schedule.IntervalSec == 0 {
		schedule.IntervalSec = 3600
	}

	if err := m.saveSchedule(schedule); err != nil {
		return err
	}

	m.mu.Lock()
	m.schedule = schedule
	m.mu.Unlock()

	m.signalScheduleChange()
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

	go m.run(proxies, req.Config, source, startedAt)
	return nil
}

func (m *Manager) schedulerLoop() {
	for {
		schedule := m.Schedule()
		if !schedule.Enabled {
			select {
			case <-m.scheduleCh:
				continue
			case <-m.stopCh:
				return
			}
		}

		interval := time.Duration(schedule.IntervalSec) * time.Second
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			req := RunRequest{
				ProxyIDs:   schedule.ProxyIDs,
				OnlyOnline: schedule.OnlyOnline,
				SubName:    schedule.SubName,
				Protocol:   schedule.Protocol,
				Config:     schedule.Config,
			}
			if err := m.Run(req, "schedule"); err != nil {
				logger.Warn("Scheduled speed test skipped: %v", err)
			}
		case <-m.scheduleCh:
			timer.Stop()
		case <-m.stopCh:
			timer.Stop()
			return
		}
	}
}

func (m *Manager) run(proxies []*models.ProxyConfig, cfg TestConfig, source string, startedAt time.Time) {
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
			results <- m.testProxy(p, cfg, source)
		}(proxy)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		runResults = append(runResults, result)

		m.mu.Lock()
		m.results[result.StableID] = result
		m.history[result.StableID] = append([]Result{result}, m.history[result.StableID]...)
		if len(m.history[result.StableID]) > defaultHistoryLimit {
			m.history[result.StableID] = m.history[result.StableID][:defaultHistoryLimit]
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
	m.mu.Unlock()

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
	return os.WriteFile(m.statePath, data, 0644)
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
