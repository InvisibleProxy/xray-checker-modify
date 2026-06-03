package checker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xray-checker/logger"
	"xray-checker/metrics"
	"xray-checker/models"
)

type ProxyChecker struct {
	proxies         []*models.ProxyConfig
	startPort       int
	ipCheck         string
	currentIP       string
	httpClient      *http.Client
	currentMetrics  sync.Map
	latencyMetrics  sync.Map
	statusDetails   sync.Map
	ipInitialized   bool
	ipCheckTimeout  int
	genMethodURL    string
	downloadURL     string
	downloadTimeout int
	downloadMinSize int64
	checkMethod     string
	mu              sync.RWMutex
	generation      uint64
}

type ProxyStatusDetails struct {
	Online        bool
	Latency       time.Duration
	CheckedAt     time.Time
	LastChangedAt time.Time
	DownSince     time.Time
	HostCheck     HostCheckDetails
}

type HostCheckDetails struct {
	Checked   bool
	Online    bool
	Latency   time.Duration
	CheckedAt time.Time
	Target    string
	Error     string
}

func NewProxyChecker(proxies []*models.ProxyConfig, startPort int, ipCheckURL string, ipCheckTimeout int, genMethodURL string, downloadURL string, downloadTimeout int, downloadMinSize int64, checkMethod string) *ProxyChecker {
	return &ProxyChecker{
		proxies:   proxies,
		startPort: startPort,
		ipCheck:   ipCheckURL,
		httpClient: &http.Client{
			Timeout: time.Second * time.Duration(ipCheckTimeout),
		},
		ipCheckTimeout:  ipCheckTimeout,
		genMethodURL:    genMethodURL,
		downloadURL:     downloadURL,
		downloadTimeout: downloadTimeout,
		downloadMinSize: downloadMinSize,
		checkMethod:     checkMethod,
	}
}

func (pc *ProxyChecker) GetCurrentIP() (string, error) {
	if pc.ipInitialized && pc.currentIP != "" {
		return pc.currentIP, nil
	}

	resp, err := pc.httpClient.Get(pc.ipCheck)
	if err != nil {
		return "", fmt.Errorf("error getting current IP: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %v", err)
	}

	pc.currentIP = string(body)
	pc.ipInitialized = true
	return pc.currentIP, nil
}

func (pc *ProxyChecker) CheckProxy(proxy *models.ProxyConfig) {
	pc.checkProxyInternal(proxy, 0, false)
}

func (pc *ProxyChecker) checkProxyInternal(proxy *models.ProxyConfig, expectedGeneration uint64, checkGeneration bool) {
	if proxy.StableID == "" {
		proxy.StableID = proxy.GenerateStableID()
	}

	metricKey := fmt.Sprintf("%s|%s:%d|%s|%s|%s",
		proxy.Protocol,
		proxy.Server,
		proxy.Port,
		proxy.Name,
		proxy.SubName,
		proxy.StableID,
	)

	isGenerationValid := func() bool {
		if !checkGeneration {
			return true
		}
		return atomic.LoadUint64(&pc.generation) == expectedGeneration
	}

	setFailedStatus := func() {
		if !isGenerationValid() {
			logger.Debug("%s | Skipping metric update: generation changed", proxy.Name)
			return
		}
		metrics.RecordProxyStatus(
			proxy.Protocol,
			fmt.Sprintf("%s:%d", proxy.Server, proxy.Port),
			proxy.Name,
			proxy.SubName,
			0,
		)
		pc.currentMetrics.Store(metricKey, false)
		hostCheck := pc.hostCheckIfBecameUnavailable(proxy)
		pc.storeStatusDetails(proxy.StableID, false, 0, hostCheck)
		if hostCheck != nil {
			if hostCheck.Online {
				logger.Info("%s | Host TCP check success | Target: %s | Latency: %s", proxy.Name, hostCheck.Target, hostCheck.Latency)
			} else {
				logger.Warn("%s | Host TCP check failed | Target: %s | %s", proxy.Name, hostCheck.Target, hostCheck.Error)
			}
		}
	}

	setFailedLatency := func() {
		if !isGenerationValid() {
			return
		}
		metrics.RecordProxyLatency(
			proxy.Protocol,
			fmt.Sprintf("%s:%d", proxy.Server, proxy.Port),
			proxy.Name,
			proxy.SubName,
			time.Duration(0),
		)
		pc.latencyMetrics.Store(metricKey, time.Duration(0))
	}

	proxyURL := fmt.Sprintf("socks5://127.0.0.1:%d", pc.startPort+proxy.Index)
	proxyURLParsed, err := url.Parse(proxyURL)
	if err != nil {
		logger.Error("Error parsing proxy URL %s: %v", proxyURL, err)
		setFailedStatus()
		setFailedLatency()

		return
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURLParsed),
			DisableKeepAlives: true,
		},
		Timeout: time.Second * time.Duration(pc.ipCheckTimeout),
	}

	var checkSuccess bool
	var checkErr error
	var logMessage string
	var latency time.Duration

	if pc.checkMethod == "ip" {
		checkSuccess, logMessage, latency, checkErr = pc.checkByIP(client)
	} else if pc.checkMethod == "status" {
		checkSuccess, logMessage, latency, checkErr = pc.checkByGen(client)
	} else if pc.checkMethod == "download" {
		checkSuccess, logMessage, latency, checkErr = pc.checkByDownload(client)
	} else {
		logger.Error("Invalid check method: %s", pc.checkMethod)
		return
	}

	if checkErr != nil {
		logger.Error("%s | %v", proxy.Name, checkErr)
		setFailedStatus()
		setFailedLatency()

		return
	}

	if !checkSuccess {
		logger.Error("%s | Failed | %s | Latency: %s", proxy.Name, logMessage, latency)
		setFailedStatus()
		setFailedLatency()
	} else {
		logger.Result("%s | Success | %s | Latency: %s", proxy.Name, logMessage, latency)
		if !isGenerationValid() {
			logger.Debug("%s | Skipping metric update: generation changed", proxy.Name)
			return
		}
		metrics.RecordProxyStatus(
			proxy.Protocol,
			fmt.Sprintf("%s:%d", proxy.Server, proxy.Port),
			proxy.Name,
			proxy.SubName,
			1,
		)
		metrics.RecordProxyLatency(
			proxy.Protocol,
			fmt.Sprintf("%s:%d", proxy.Server, proxy.Port),
			proxy.Name,
			proxy.SubName,
			latency,
		)

		pc.latencyMetrics.Store(metricKey, latency)
		pc.currentMetrics.Store(metricKey, true)
		pc.storeStatusDetails(proxy.StableID, true, latency, nil)
	}
}

func (pc *ProxyChecker) checkByIP(client *http.Client) (bool, string, time.Duration, error) {
	req, err := http.NewRequest("GET", pc.ipCheck, nil)
	if err != nil {
		return false, "", 0, err
	}

	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ttfb = time.Since(start)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))

	resp, err := client.Do(req)
	if err != nil {
		return false, "", 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", ttfb, err
	}

	proxyIP := string(body)
	logMessage := fmt.Sprintf("Source IP: %s | Proxy IP: %s", pc.currentIP, proxyIP)
	return proxyIP != pc.currentIP, logMessage, ttfb, nil
}

func (pc *ProxyChecker) checkByGen(client *http.Client) (bool, string, time.Duration, error) {
	req, err := http.NewRequest("GET", pc.genMethodURL, nil)
	if err != nil {
		return false, "", 0, err
	}

	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ttfb = time.Since(start)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))

	resp, err := client.Do(req)
	if err != nil {
		return false, "", 0, err
	}
	defer resp.Body.Close()

	logMessage := fmt.Sprintf("Status: %d", resp.StatusCode)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, logMessage, ttfb, nil
}

func (pc *ProxyChecker) checkByDownload(client *http.Client) (bool, string, time.Duration, error) {
	if pc.downloadURL == "" {
		return false, "Download URL not configured", 0, fmt.Errorf("download URL not configured")
	}

	req, err := http.NewRequest("GET", pc.downloadURL, nil)
	if err != nil {
		return false, "", 0, err
	}

	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ttfb = time.Since(start)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))

	downloadClient := &http.Client{
		Transport: client.Transport,
		Timeout:   time.Second * time.Duration(pc.downloadTimeout),
	}

	resp, err := downloadClient.Do(req)
	if err != nil {
		return false, "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Sprintf("HTTP status: %d", resp.StatusCode), ttfb, nil
	}

	totalBytes := int64(0)
	buffer := make([]byte, 8192)

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			totalBytes += int64(n)
		}

		if totalBytes >= pc.downloadMinSize {
			break
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Sprintf("Download error after %d bytes: %v", totalBytes, err), ttfb, nil
		}
	}

	success := totalBytes >= pc.downloadMinSize
	logMessage := fmt.Sprintf("Downloaded: %d bytes (min: %d)", totalBytes, pc.downloadMinSize)

	return success, logMessage, ttfb, nil
}

func (pc *ProxyChecker) ClearMetrics() {
	pc.currentMetrics.Range(func(key, _ interface{}) bool {
		metricKey := key.(string)
		parts := strings.Split(metricKey, "|")
		if len(parts) >= 4 {
			metrics.DeleteProxyStatus(parts[0], parts[1], parts[2], parts[3])
			metrics.DeleteProxyLatency(parts[0], parts[1], parts[2], parts[3])
		}
		pc.currentMetrics.Delete(key)
		return true
	})

	pc.latencyMetrics.Range(func(key, _ interface{}) bool {
		pc.latencyMetrics.Delete(key)
		return true
	})
}

func (pc *ProxyChecker) storeStatusDetails(stableID string, online bool, latency time.Duration, hostCheck *HostCheckDetails) {
	if stableID == "" {
		return
	}

	now := time.Now()
	details := ProxyStatusDetails{
		Online:        online,
		Latency:       latency,
		CheckedAt:     now,
		LastChangedAt: now,
	}

	if previousValue, ok := pc.statusDetails.Load(stableID); ok {
		previous := previousValue.(ProxyStatusDetails)
		if previous.Online == online {
			details.LastChangedAt = previous.LastChangedAt
		}
		if !online && !previous.Online && !previous.DownSince.IsZero() {
			details.DownSince = previous.DownSince
			details.HostCheck = previous.HostCheck
		}
	}

	if !online && details.DownSince.IsZero() {
		details.DownSince = now
	}
	if !online && hostCheck != nil {
		details.HostCheck = *hostCheck
	}

	pc.statusDetails.Store(stableID, details)
}

func (pc *ProxyChecker) hostCheckIfBecameUnavailable(proxy *models.ProxyConfig) *HostCheckDetails {
	if proxy == nil || proxy.StableID == "" {
		return nil
	}

	if previousValue, ok := pc.statusDetails.Load(proxy.StableID); ok {
		previous := previousValue.(ProxyStatusDetails)
		if !previous.Online {
			return nil
		}
	}

	result := pc.tcpCheckHost(proxy.Server, proxy.Port)
	return &result
}

func (pc *ProxyChecker) tcpCheckHost(host string, port int) HostCheckDetails {
	result := HostCheckDetails{
		Checked:   true,
		CheckedAt: time.Now(),
	}

	host = strings.TrimSpace(host)
	if host == "" {
		result.Error = "empty host"
		return result
	}
	if port <= 0 || port > 65535 {
		result.Error = fmt.Sprintf("invalid port %d", port)
		return result
	}
	result.Target = net.JoinHostPort(host, fmt.Sprintf("%d", port))

	timeout := 3 * time.Second
	if pc.ipCheckTimeout > 0 {
		checkTimeout := time.Duration(pc.ipCheckTimeout) * time.Second
		if checkTimeout < timeout {
			timeout = checkTimeout
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Second)
	defer cancel()

	start := time.Now()
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", result.Target)
	result.Latency = time.Since(start)
	if ctx.Err() == context.DeadlineExceeded {
		result.Error = "TCP check timeout"
		return result
	}
	if err != nil {
		result.Error = compactHostCheckError(err)
		return result
	}
	conn.Close()

	result.Online = true
	return result
}

func compactHostCheckError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

func (pc *ProxyChecker) UpdateProxies(newProxies []*models.ProxyConfig) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	atomic.AddUint64(&pc.generation, 1)
	pc.ClearMetrics()
	pc.pruneStatusDetails(newProxies)
	pc.proxies = newProxies
}

func (pc *ProxyChecker) pruneStatusDetails(proxies []*models.ProxyConfig) {
	active := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}
		active[proxy.StableID] = true
	}

	pc.statusDetails.Range(func(key, _ interface{}) bool {
		stableID, ok := key.(string)
		if !ok || !active[stableID] {
			pc.statusDetails.Delete(key)
		}
		return true
	})
}

func (pc *ProxyChecker) RestoreOfflineStatus(stableID string, downSince time.Time, hostCheck HostCheckDetails) bool {
	if stableID == "" || downSince.IsZero() {
		return false
	}

	pc.mu.RLock()
	var metricKey string
	for _, proxy := range pc.proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		if proxy.StableID == stableID {
			metricKey = fmt.Sprintf("%s|%s:%d|%s|%s|%s",
				proxy.Protocol,
				proxy.Server,
				proxy.Port,
				proxy.Name,
				proxy.SubName,
				proxy.StableID,
			)
			break
		}
	}
	pc.mu.RUnlock()

	if metricKey == "" {
		return false
	}

	now := time.Now()
	pc.statusDetails.Store(stableID, ProxyStatusDetails{
		Online:        false,
		Latency:       0,
		CheckedAt:     now,
		LastChangedAt: downSince,
		DownSince:     downSince,
		HostCheck:     hostCheck,
	})
	pc.currentMetrics.Store(metricKey, false)
	pc.latencyMetrics.Store(metricKey, time.Duration(0))
	return true
}

func (pc *ProxyChecker) CheckAllProxies() {
	if _, err := pc.GetCurrentIP(); err != nil {
		logger.Warn("Error getting current IP: %v", err)
		return
	}

	pc.mu.RLock()
	proxiesToCheck := make([]*models.ProxyConfig, len(pc.proxies))
	copy(proxiesToCheck, pc.proxies)
	currentGeneration := atomic.LoadUint64(&pc.generation)
	pc.mu.RUnlock()

	var wg sync.WaitGroup
	for _, proxy := range proxiesToCheck {
		wg.Add(1)
		go func(p *models.ProxyConfig, gen uint64) {
			defer wg.Done()
			pc.checkProxyInternal(p, gen, true)
		}(proxy, currentGeneration)
	}
	wg.Wait()
}

func (pc *ProxyChecker) GetProxyStatus(name string) (bool, time.Duration, error) {
	pc.mu.RLock()
	var metricKey string
	for _, proxy := range pc.proxies {
		if proxy.Name == name {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}

			metricKey = fmt.Sprintf("%s|%s:%d|%s|%s|%s",
				proxy.Protocol,
				proxy.Server,
				proxy.Port,
				proxy.Name,
				proxy.SubName,
				proxy.StableID,
			)
			break
		}
	}
	pc.mu.RUnlock()

	if metricKey == "" {
		return false, 0, fmt.Errorf("proxy not found")
	}

	status, ok := pc.currentMetrics.Load(metricKey)
	if !ok {
		return false, 0, fmt.Errorf("metric not found")
	}

	latency, _ := pc.latencyMetrics.Load(metricKey)
	if latency == nil {
		latency = time.Duration(0)
	}

	return status.(bool), latency.(time.Duration), nil
}

func (pc *ProxyChecker) GetProxyStatusByStableID(stableID string) (bool, time.Duration, error) {
	pc.mu.RLock()
	var metricKey string
	for _, proxy := range pc.proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		if proxy.StableID == stableID {
			metricKey = fmt.Sprintf("%s|%s:%d|%s|%s|%s",
				proxy.Protocol,
				proxy.Server,
				proxy.Port,
				proxy.Name,
				proxy.SubName,
				proxy.StableID,
			)
			break
		}
	}
	pc.mu.RUnlock()

	if metricKey == "" {
		return false, 0, fmt.Errorf("proxy not found")
	}

	status, ok := pc.currentMetrics.Load(metricKey)
	if !ok {
		return false, 0, fmt.Errorf("metric not found")
	}

	latency, _ := pc.latencyMetrics.Load(metricKey)
	if latency == nil {
		latency = time.Duration(0)
	}

	return status.(bool), latency.(time.Duration), nil
}

func (pc *ProxyChecker) GetProxyStatusDetailsByStableID(stableID string) (ProxyStatusDetails, error) {
	pc.mu.RLock()
	found := false
	for _, proxy := range pc.proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		if proxy.StableID == stableID {
			found = true
			break
		}
	}
	pc.mu.RUnlock()

	if !found {
		return ProxyStatusDetails{}, fmt.Errorf("proxy not found")
	}

	details, ok := pc.statusDetails.Load(stableID)
	if !ok {
		return ProxyStatusDetails{}, fmt.Errorf("metric not found")
	}

	return details.(ProxyStatusDetails), nil
}

func (pc *ProxyChecker) GetProxyByStableID(stableID string) (*models.ProxyConfig, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	for _, proxy := range pc.proxies {
		if proxy.StableID == "" {
			proxy.StableID = proxy.GenerateStableID()
		}

		if proxy.StableID == stableID {
			return proxy, true
		}
	}
	return nil, false
}

func (pc *ProxyChecker) GetProxies() []*models.ProxyConfig {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	result := make([]*models.ProxyConfig, len(pc.proxies))
	copy(result, pc.proxies)
	return result
}
