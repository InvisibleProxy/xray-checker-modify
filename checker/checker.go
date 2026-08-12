package checker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

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
	statusMu        sync.Mutex
	ipInitialized   bool
	ipCheckTimeout  int
	genMethodURL    string
	downloadURL     string
	downloadTimeout int
	downloadMinSize int64
	checkMethod     string
	mu              sync.RWMutex
	checkMu         sync.Mutex
	currentIPMu     sync.Mutex
	runGate         sync.Locker
	generation      uint64

	checkProxyFunc      func(*models.ProxyConfig, uint64, bool, bool)
	hostDiagnosticsFunc func(*models.ProxyConfig) (HostCheckDetails, PingCheckDetails)
}

const maxAvailabilityCheckConcurrency = 4

var pingProbeCounter uint64

type ProxyStatusDetails struct {
	Online        bool
	Latency       time.Duration
	CheckedAt     time.Time
	LastChangedAt time.Time
	DownSince     time.Time
	HostCheck     HostCheckDetails
	PingCheck     PingCheckDetails
	CheckFailure  FailureDetails
	Failure       FailureDetails
}

type HostCheckDetails struct {
	Checked   bool
	Online    bool
	Latency   time.Duration
	CheckedAt time.Time
	Target    string
	Error     string
}

type PingCheckDetails struct {
	Checked   bool
	Online    bool
	Latency   time.Duration
	CheckedAt time.Time
	Target    string
	Error     string
}

type AvailabilityCheckResult struct {
	StableID     string
	Online       bool
	ProxyChecked bool
	Recovered    bool
}

type AvailabilityCheckReport struct {
	Results []AvailabilityCheckResult
}

func (r AvailabilityCheckReport) RecoveredStableIDs() []string {
	stableIDs := make([]string, 0)
	for _, result := range r.Results {
		if result.Recovered {
			stableIDs = append(stableIDs, result.StableID)
		}
	}
	return stableIDs
}

type availabilityCheckCandidate struct {
	Proxy      *models.ProxyConfig
	WasOffline bool
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

func (pc *ProxyChecker) SetRunGate(gate sync.Locker) {
	pc.runGate = gate
}

func (pc *ProxyChecker) GetCurrentIP() (string, error) {
	pc.currentIPMu.Lock()
	defer pc.currentIPMu.Unlock()

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
	pc.withCheckRun(func() {
		if pc.checkMethod == "ip" {
			if _, err := pc.GetCurrentIP(); err != nil {
				logger.Warn("Error getting current IP: %v", err)
				return
			}
		}
		pc.runProxyCheck(proxy, 0, false, false)
	})
}

func (pc *ProxyChecker) withCheckRun(run func()) {
	pc.checkMu.Lock()
	defer pc.checkMu.Unlock()
	if pc.runGate != nil {
		pc.runGate.Lock()
		defer pc.runGate.Unlock()
	}
	run()
}

func (pc *ProxyChecker) runProxyCheck(proxy *models.ProxyConfig, expectedGeneration uint64, checkGeneration bool, quiet bool) {
	if pc.checkProxyFunc != nil {
		pc.checkProxyFunc(proxy, expectedGeneration, checkGeneration, quiet)
		return
	}
	pc.checkProxyInternal(proxy, expectedGeneration, checkGeneration, quiet)
}

func (pc *ProxyChecker) checkProxyInternal(proxy *models.ProxyConfig, expectedGeneration uint64, checkGeneration bool, quiet bool) {
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

	setFailedStatus := func(failure FailureDetails) {
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
		hostCheck, pingCheck := pc.markUnavailableAndCollectDiagnostics(proxy, failure)
		logHostDiagnostics(proxy.Name, hostCheck, pingCheck)
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
		setFailedStatus(failureDetails(FailureCodeConfiguration, err.Error()))
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
		setFailedStatus(failureDetails(FailureCodeConfiguration, "invalid check method: "+pc.checkMethod))
		setFailedLatency()
		return
	}

	if checkErr != nil {
		if quiet {
			logger.Debug("%s | Recovery probe failed | %v", proxy.Name, checkErr)
		} else {
			logger.Error("%s | %v", proxy.Name, checkErr)
		}
		setFailedStatus(failureFromError(checkErr))
		setFailedLatency()

		return
	}

	if !checkSuccess {
		if quiet {
			logger.Debug("%s | Recovery probe failed | %s | Latency: %s", proxy.Name, logMessage, latency)
		} else {
			logger.Error("%s | Failed | %s | Latency: %s", proxy.Name, logMessage, latency)
		}
		setFailedStatus(failureFromCheckResult(pc.checkMethod, logMessage))
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
		pc.storeStatusDetails(proxy.StableID, true, latency, nil, nil)
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

func (pc *ProxyChecker) storeStatusDetails(stableID string, online bool, latency time.Duration, hostCheck *HostCheckDetails, pingCheck *PingCheckDetails) bool {
	return pc.storeStatusDetailsWithFailure(stableID, online, latency, hostCheck, pingCheck, nil)
}

func (pc *ProxyChecker) storeStatusDetailsWithFailure(stableID string, online bool, latency time.Duration, hostCheck *HostCheckDetails, pingCheck *PingCheckDetails, failure *FailureDetails) bool {
	pc.statusMu.Lock()
	defer pc.statusMu.Unlock()
	return pc.storeStatusDetailsLocked(stableID, online, latency, hostCheck, pingCheck, failure)
}

func (pc *ProxyChecker) storeStatusDetailsLocked(stableID string, online bool, latency time.Duration, hostCheck *HostCheckDetails, pingCheck *PingCheckDetails, failure *FailureDetails) bool {
	if stableID == "" {
		return false
	}

	now := time.Now()
	becameUnavailable := !online
	details := ProxyStatusDetails{
		Online:        online,
		Latency:       latency,
		CheckedAt:     now,
		LastChangedAt: now,
	}

	if previousValue, ok := pc.statusDetails.Load(stableID); ok {
		previous := previousValue.(ProxyStatusDetails)
		becameUnavailable = !online && previous.Online
		if previous.Online == online {
			details.LastChangedAt = previous.LastChangedAt
		}
		if !online && !previous.Online && !previous.DownSince.IsZero() {
			details.DownSince = previous.DownSince
			details.HostCheck = previous.HostCheck
			details.PingCheck = previous.PingCheck
			details.CheckFailure = previous.CheckFailure
			details.Failure = previous.Failure
		}
	}

	if !online && details.DownSince.IsZero() {
		details.DownSince = now
	}
	if !online && hostCheck != nil {
		details.HostCheck = *hostCheck
	}
	if !online && pingCheck != nil {
		details.PingCheck = *pingCheck
	}
	if !online && failure != nil {
		details.CheckFailure = *failure
	}
	if !online {
		details.Failure = DiagnoseFailure(details.CheckFailure, details.HostCheck, details.PingCheck)
	}

	pc.statusDetails.Store(stableID, details)
	return becameUnavailable
}

func (pc *ProxyChecker) markUnavailableAndCollectDiagnostics(proxy *models.ProxyConfig, failures ...FailureDetails) (*HostCheckDetails, *PingCheckDetails) {
	if proxy == nil || proxy.StableID == "" {
		return nil, nil
	}
	failure := FailureDetails{}
	if len(failures) > 0 {
		failure = failures[0]
	}

	if !pc.storeStatusDetailsWithFailure(proxy.StableID, false, 0, nil, nil, &failure) {
		return nil, nil
	}

	hostCheck, pingCheck := pc.checkHostDiagnostics(proxy)
	pc.storeOfflineDiagnostics(proxy.StableID, hostCheck, pingCheck)
	return &hostCheck, &pingCheck
}

func (pc *ProxyChecker) storeOfflineDiagnostics(stableID string, hostCheck HostCheckDetails, pingCheck PingCheckDetails) {
	pc.statusMu.Lock()
	defer pc.statusMu.Unlock()

	currentValue, ok := pc.statusDetails.Load(stableID)
	if !ok {
		return
	}
	current := currentValue.(ProxyStatusDetails)
	if current.Online {
		return
	}
	current.HostCheck = hostCheck
	current.PingCheck = pingCheck
	current.Failure = DiagnoseFailure(current.CheckFailure, hostCheck, pingCheck)
	pc.statusDetails.Store(stableID, current)
}

func (pc *ProxyChecker) checkHostDiagnostics(proxy *models.ProxyConfig) (HostCheckDetails, PingCheckDetails) {
	if pc.hostDiagnosticsFunc != nil {
		return pc.hostDiagnosticsFunc(proxy)
	}

	var hostCheck HostCheckDetails
	var pingCheck PingCheckDetails
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		hostCheck = pc.tcpCheckHost(proxy.Server, proxy.Port)
	}()
	go func() {
		defer wg.Done()
		pingCheck = pc.pingHost(proxy.Server)
	}()
	wg.Wait()
	return hostCheck, pingCheck
}

func (pc *ProxyChecker) RefreshHostDiagnosticsByStableID(stableID string) (ProxyStatusDetails, error) {
	proxy, ok := pc.GetProxyByStableID(stableID)
	if !ok {
		return ProxyStatusDetails{}, fmt.Errorf("proxy not found")
	}
	if proxy.StableID == "" {
		proxy.StableID = proxy.GenerateStableID()
	}

	hostCheck, pingCheck := pc.checkHostDiagnostics(proxy)
	logHostDiagnostics(proxy.Name, &hostCheck, &pingCheck)

	pc.statusMu.Lock()
	defer pc.statusMu.Unlock()

	currentValue, ok := pc.statusDetails.Load(proxy.StableID)
	if !ok {
		now := time.Now()
		return ProxyStatusDetails{
			Online:        false,
			CheckedAt:     now,
			LastChangedAt: now,
			DownSince:     now,
			HostCheck:     hostCheck,
			PingCheck:     pingCheck,
		}, nil
	}

	current := currentValue.(ProxyStatusDetails)
	if current.Online {
		return current, nil
	}

	pc.storeStatusDetailsLocked(proxy.StableID, false, 0, &hostCheck, &pingCheck, nil)
	updatedValue, ok := pc.statusDetails.Load(proxy.StableID)
	if !ok {
		return ProxyStatusDetails{}, fmt.Errorf("metric not found")
	}
	return updatedValue.(ProxyStatusDetails), nil
}

func logHostDiagnostics(proxyName string, hostCheck *HostCheckDetails, pingCheck *PingCheckDetails) {
	if hostCheck != nil {
		if hostCheck.Online {
			logger.Info("%s | Host TCP check success | Target: %s | Latency: %s", proxyName, hostCheck.Target, hostCheck.Latency)
		} else {
			logger.Warn("%s | Host TCP check failed | Target: %s | %s", proxyName, hostCheck.Target, hostCheck.Error)
		}
	}
	if pingCheck != nil {
		if pingCheck.Online {
			logger.Info("%s | Host ping success | Target: %s | Latency: %s", proxyName, pingCheck.Target, pingCheck.Latency)
		} else {
			logger.Warn("%s | Host ping failed | Target: %s | %s", proxyName, pingCheck.Target, pingCheck.Error)
		}
	}
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

	timeout := pc.hostDiagnosticsTimeout()

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

func (pc *ProxyChecker) pingHost(host string) PingCheckDetails {
	result := PingCheckDetails{
		Checked:   true,
		CheckedAt: time.Now(),
	}

	host = strings.TrimSpace(host)
	if host == "" {
		result.Error = "empty host"
		return result
	}

	timeout := pc.hostDiagnosticsTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		result.Error = compactHostCheckError(err)
		return result
	}
	if len(ips) == 0 {
		result.Error = "host resolved to no IP addresses"
		return result
	}

	var lastErr error
	for _, ipAddr := range ips {
		ip := ipAddr.IP
		if ip == nil {
			continue
		}
		result.Target = ip.String()

		latency, err := pingIP(ip, timeout)
		result.Latency = latency
		if err == nil {
			result.Online = true
			result.Error = ""
			return result
		}
		lastErr = err
	}

	if lastErr != nil {
		result.Error = compactHostCheckError(lastErr)
	} else {
		result.Error = "no usable IP address"
	}
	return result
}

func (pc *ProxyChecker) hostDiagnosticsTimeout() time.Duration {
	timeout := 3 * time.Second
	if pc.ipCheckTimeout > 0 {
		checkTimeout := time.Duration(pc.ipCheckTimeout) * time.Second
		if checkTimeout < timeout {
			timeout = checkTimeout
		}
	}
	return timeout
}

func pingIP(ip net.IP, timeout time.Duration) (time.Duration, error) {
	network := "udp4"
	listenAddr := "0.0.0.0"
	protocol := ipv4.ICMPTypeEcho.Protocol()
	var messageType icmp.Type = ipv4.ICMPTypeEcho
	var replyType icmp.Type = ipv4.ICMPTypeEchoReply
	if ip.To4() == nil {
		network = "udp6"
		listenAddr = "::"
		protocol = ipv6.ICMPTypeEchoRequest.Protocol()
		messageType = ipv6.ICMPTypeEchoRequest
		replyType = ipv6.ICMPTypeEchoReply
	}

	conn, err := icmp.ListenPacket(network, listenAddr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}

	id := os.Getpid() & 0xffff
	probeNumber := atomic.AddUint64(&pingProbeCounter, 1)
	seq := int(probeNumber & 0xffff)
	probeData := []byte(fmt.Sprintf("xray-checker:%d:%d:%d", id, time.Now().UnixNano(), probeNumber))
	message := icmp.Message{
		Type: messageType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
			Data: probeData,
		},
	}
	payload, err := message.Marshal(nil)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	if _, err := conn.WriteTo(payload, &net.UDPAddr{IP: ip}); err != nil {
		return time.Since(start), err
	}

	buffer := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buffer)
		latency := time.Since(start)
		if err != nil {
			return latency, err
		}

		reply, err := icmp.ParseMessage(protocol, buffer[:n])
		if err != nil {
			continue
		}
		if matchesPingReply(reply, replyType, seq, probeData) {
			return latency, nil
		}
	}
}

func matchesPingReply(reply *icmp.Message, replyType icmp.Type, seq int, probeData []byte) bool {
	if reply == nil || reply.Type != replyType {
		return false
	}
	echo, ok := reply.Body.(*icmp.Echo)
	return ok && echo.Seq == seq && bytes.Equal(echo.Data, probeData)
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

	pc.statusMu.Lock()
	defer pc.statusMu.Unlock()
	pc.statusDetails.Range(func(key, _ interface{}) bool {
		stableID, ok := key.(string)
		if !ok || !active[stableID] {
			pc.statusDetails.Delete(key)
		}
		return true
	})
}

func (pc *ProxyChecker) RestoreOfflineStatus(stableID string, downSince time.Time, hostCheck HostCheckDetails, pingCheck PingCheckDetails, failures ...FailureDetails) bool {
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
	pc.statusMu.Lock()
	defer pc.statusMu.Unlock()
	var failure FailureDetails
	if len(failures) > 0 {
		failure = failures[0]
	}
	if failure.Code == "" {
		failure = DiagnoseFailure(FailureDetails{}, hostCheck, pingCheck)
	}
	pc.statusDetails.Store(stableID, ProxyStatusDetails{
		Online:        false,
		Latency:       0,
		CheckedAt:     now,
		LastChangedAt: downSince,
		DownSince:     downSince,
		HostCheck:     hostCheck,
		PingCheck:     pingCheck,
		CheckFailure:  failure,
		Failure:       failure,
	})
	pc.currentMetrics.Store(metricKey, false)
	pc.latencyMetrics.Store(metricKey, time.Duration(0))
	return true
}

func (pc *ProxyChecker) CheckAllProxies() {
	pc.withCheckRun(pc.checkAllProxies)
}

func (pc *ProxyChecker) checkAllProxies() {
	if pc.checkMethod == "ip" {
		if _, err := pc.GetCurrentIP(); err != nil {
			logger.Warn("Error getting current IP: %v", err)
			return
		}
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
			pc.runProxyCheck(p, gen, true, false)
		}(proxy, currentGeneration)
	}
	wg.Wait()
}

func (pc *ProxyChecker) CheckUnavailableProxies() (AvailabilityCheckReport, error) {
	var report AvailabilityCheckReport
	var checkErr error
	pc.withCheckRun(func() {
		candidates, generation, err := pc.availabilityCheckCandidates(nil, true)
		if err != nil {
			checkErr = err
			return
		}
		report, checkErr = pc.checkAvailabilityCandidates(candidates, generation, true)
	})
	return report, checkErr
}

func (pc *ProxyChecker) CheckProxiesByStableIDs(stableIDs []string) (AvailabilityCheckReport, error) {
	var report AvailabilityCheckReport
	var checkErr error
	pc.withCheckRun(func() {
		candidates, generation, err := pc.availabilityCheckCandidates(stableIDs, false)
		if err != nil {
			checkErr = err
			return
		}
		report, checkErr = pc.checkAvailabilityCandidates(candidates, generation, false)
	})
	return report, checkErr
}

func (pc *ProxyChecker) availabilityCheckCandidates(stableIDs []string, unavailableOnly bool) ([]availabilityCheckCandidate, uint64, error) {
	requested := make([]string, 0, len(stableIDs))
	requestedSet := make(map[string]bool, len(stableIDs))
	for _, rawStableID := range stableIDs {
		stableID := strings.TrimSpace(rawStableID)
		if stableID == "" || requestedSet[stableID] {
			continue
		}
		requestedSet[stableID] = true
		requested = append(requested, stableID)
	}
	if !unavailableOnly && len(requested) == 0 {
		return nil, 0, fmt.Errorf("select at least one node")
	}

	pc.mu.RLock()
	generation := atomic.LoadUint64(&pc.generation)
	byStableID := make(map[string]availabilityCheckCandidate, len(pc.proxies))
	ordered := make([]availabilityCheckCandidate, 0, len(pc.proxies))
	for _, proxy := range pc.proxies {
		if proxy == nil {
			continue
		}
		stableID := proxy.StableID
		if stableID == "" {
			stableID = proxy.GenerateStableID()
		}
		if len(requestedSet) > 0 && !requestedSet[stableID] {
			continue
		}
		details, hasDetails := pc.statusDetailsByStableID(stableID)
		wasOffline := hasDetails && !details.Online
		if unavailableOnly && !wasOffline {
			continue
		}
		proxyCopy := *proxy
		proxyCopy.StableID = stableID
		candidate := availabilityCheckCandidate{Proxy: &proxyCopy, WasOffline: wasOffline}
		byStableID[stableID] = candidate
		ordered = append(ordered, candidate)
	}
	pc.mu.RUnlock()

	if unavailableOnly {
		return ordered, generation, nil
	}

	selected := make([]availabilityCheckCandidate, 0, len(requested))
	var missing []string
	for _, stableID := range requested {
		candidate, ok := byStableID[stableID]
		if !ok {
			missing = append(missing, stableID)
			continue
		}
		selected = append(selected, candidate)
	}
	if len(missing) > 0 {
		return nil, generation, fmt.Errorf("proxy not found: %s", strings.Join(missing, ", "))
	}
	return selected, generation, nil
}

func (pc *ProxyChecker) checkAvailabilityCandidates(candidates []availabilityCheckCandidate, generation uint64, quiet bool) (AvailabilityCheckReport, error) {
	report := AvailabilityCheckReport{Results: make([]AvailabilityCheckResult, len(candidates))}
	if len(candidates) == 0 {
		return report, nil
	}

	needsProxyCheck := make([]bool, len(candidates))
	var diagnosticErrors []error
	var diagnosticErrorsMu sync.Mutex
	pc.runAvailabilityWorkers(len(candidates), func(index int) {
		candidate := candidates[index]
		report.Results[index].StableID = candidate.Proxy.StableID
		if !candidate.WasOffline {
			needsProxyCheck[index] = true
			return
		}

		hostCheck, pingCheck := pc.checkHostDiagnostics(candidate.Proxy)
		if atomic.LoadUint64(&pc.generation) != generation {
			diagnosticErrorsMu.Lock()
			diagnosticErrors = append(diagnosticErrors, fmt.Errorf("%s: proxy configuration changed during diagnostics", candidate.Proxy.StableID))
			diagnosticErrorsMu.Unlock()
			return
		}
		pc.storeStatusDetails(candidate.Proxy.StableID, false, 0, &hostCheck, &pingCheck)
		needsProxyCheck[index] = hostCheck.Online
	})

	if len(diagnosticErrors) > 0 {
		pc.populateAvailabilityResults(&report, candidates)
		return report, errors.Join(diagnosticErrors...)
	}

	proxyChecksRequired := false
	for _, required := range needsProxyCheck {
		if required {
			proxyChecksRequired = true
			break
		}
	}
	if proxyChecksRequired && pc.checkMethod == "ip" {
		if _, err := pc.GetCurrentIP(); err != nil {
			pc.populateAvailabilityResults(&report, candidates)
			return report, fmt.Errorf("get current IP before proxy checks: %w", err)
		}
	}

	pc.runAvailabilityWorkers(len(candidates), func(index int) {
		if !needsProxyCheck[index] {
			return
		}
		candidate := candidates[index]
		pc.runProxyCheck(candidate.Proxy, generation, true, quiet)
		report.Results[index].ProxyChecked = true
	})
	pc.populateAvailabilityResults(&report, candidates)
	return report, nil
}

func (pc *ProxyChecker) populateAvailabilityResults(report *AvailabilityCheckReport, candidates []availabilityCheckCandidate) {
	for index, candidate := range candidates {
		result := &report.Results[index]
		result.StableID = candidate.Proxy.StableID
		details, ok := pc.statusDetailsByStableID(candidate.Proxy.StableID)
		if !ok {
			continue
		}
		result.Online = details.Online
		result.Recovered = candidate.WasOffline && details.Online
	}
}

func (pc *ProxyChecker) runAvailabilityWorkers(count int, run func(int)) {
	workers := maxAvailabilityCheckConcurrency
	if workers > count {
		workers = count
	}
	if workers <= 0 {
		return
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				run(index)
			}
		}()
	}
	for index := 0; index < count; index++ {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
}

func (pc *ProxyChecker) GetProxyStatus(name string) (bool, time.Duration, error) {
	pc.mu.RLock()
	var metricKey string
	var stableID string
	for _, proxy := range pc.proxies {
		if proxy.Name == name {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			stableID = proxy.StableID

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
		if details, ok := pc.statusDetailsByStableID(stableID); ok {
			return details.Online, details.Latency, nil
		}
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
		if details, ok := pc.statusDetailsByStableID(stableID); ok {
			return details.Online, details.Latency, nil
		}
		return false, 0, fmt.Errorf("metric not found")
	}

	latency, _ := pc.latencyMetrics.Load(metricKey)
	if latency == nil {
		latency = time.Duration(0)
	}

	return status.(bool), latency.(time.Duration), nil
}

func (pc *ProxyChecker) statusDetailsByStableID(stableID string) (ProxyStatusDetails, bool) {
	if stableID == "" {
		return ProxyStatusDetails{}, false
	}
	value, ok := pc.statusDetails.Load(stableID)
	if !ok {
		return ProxyStatusDetails{}, false
	}
	details, ok := value.(ProxyStatusDetails)
	return details, ok
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
