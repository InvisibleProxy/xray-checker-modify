package probeagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"xray-checker/diagnostics"
	"xray-checker/xray"
)

const (
	DefaultAgentRuntimeDir = "/run/xray-checker-agent"
	DefaultIPCheckURL      = "https://api.ipify.org?format=text"
	DefaultStatusCheckURL  = "http://cp.cloudflare.com/generate_204"
	DefaultDownloadURL     = "https://proof.ovh.net/files/1Mb.dat"
	DefaultDirectCheckURL  = "https://api.ipify.org?format=text"
	DefaultProxyTimeout    = 30 * time.Second
	DefaultDownloadTimeout = 60 * time.Second
	DefaultDownloadMinSize = int64(51200)
	// Probe shapes are fixed by the agent, not by the job. The controller only
	// names a profile, so it cannot ask a probe to run long enough or often
	// enough to become a load generator against the node.
	DefaultLatencySamples    = 5
	DefaultStabilityDuration = 20 * time.Second
	DefaultDNSResolver       = "1.1.1.1:53"
	maxLatencySamples        = 20
	maxStabilityDuration     = 2 * time.Minute
	stabilityReadChunk       = 32 * 1024
)

var executorPingSequence uint64

type ExecutorConfig struct {
	RuntimeDir      string
	IPCheckURL      string
	StatusCheckURL  string
	DownloadURL     string
	DirectCheckURL  string
	ProxyTimeout    time.Duration
	DownloadTimeout time.Duration
	DownloadMinSize int64
	// LatencySamples, StabilityDuration and DNSResolver shape the profiles added
	// alongside the original three. They are agent-owned for the same reason the
	// endpoint URLs are.
	LatencySamples    int
	StabilityDuration time.Duration
	DNSResolver       string
}

type Executor struct {
	config ExecutorConfig
}

func NewExecutor(config ExecutorConfig) (*Executor, error) {
	config.RuntimeDir = valueOrDefault(config.RuntimeDir, DefaultAgentRuntimeDir)
	config.IPCheckURL = valueOrDefault(config.IPCheckURL, DefaultIPCheckURL)
	config.StatusCheckURL = valueOrDefault(config.StatusCheckURL, DefaultStatusCheckURL)
	config.DownloadURL = valueOrDefault(config.DownloadURL, DefaultDownloadURL)
	config.DirectCheckURL = valueOrDefault(config.DirectCheckURL, DefaultDirectCheckURL)
	if config.ProxyTimeout == 0 {
		config.ProxyTimeout = DefaultProxyTimeout
	}
	if config.DownloadTimeout == 0 {
		config.DownloadTimeout = DefaultDownloadTimeout
	}
	if config.DownloadMinSize == 0 {
		config.DownloadMinSize = DefaultDownloadMinSize
	}
	if config.LatencySamples == 0 {
		config.LatencySamples = DefaultLatencySamples
	}
	if config.StabilityDuration == 0 {
		config.StabilityDuration = DefaultStabilityDuration
	}
	config.DNSResolver = valueOrDefault(config.DNSResolver, DefaultDNSResolver)
	if config.LatencySamples < 1 || config.LatencySamples > maxLatencySamples ||
		config.StabilityDuration < time.Second || config.StabilityDuration > maxStabilityDuration {
		return nil, fmt.Errorf("invalid probe executor limits")
	}
	if _, _, err := net.SplitHostPort(config.DNSResolver); err != nil {
		return nil, fmt.Errorf("invalid DNS resolver %q: must be host:port", config.DNSResolver)
	}
	if !filepath.IsAbs(config.RuntimeDir) || config.ProxyTimeout < time.Second || config.DownloadTimeout < time.Second || config.DownloadMinSize < 1 {
		return nil, fmt.Errorf("invalid probe executor limits")
	}
	for name, value := range map[string]string{
		"ip": config.IPCheckURL, "status": config.StatusCheckURL,
		"download": config.DownloadURL, "direct": config.DirectCheckURL,
	} {
		if err := validateEndpointURL(value); err != nil {
			return nil, fmt.Errorf("invalid %s endpoint profile: %w", name, err)
		}
	}
	return &Executor{config: config}, nil
}

func (e *Executor) Execute(ctx context.Context, assignment JobAssignment) (observation diagnostics.Observation) {
	startedAt := time.Now().UTC()
	observation = diagnostics.Observation{
		SchemaVersion:     diagnostics.ObservationSchemaVersion,
		AgentID:           assignment.Job.AgentID,
		SessionID:         assignment.Job.SessionID,
		JobID:             assignment.Job.JobID,
		Nonce:             assignment.Job.Nonce,
		StableID:          assignment.Job.StableID,
		ConfigGeneration:  assignment.Job.ConfigGeneration,
		ConfigFingerprint: assignment.Job.ConfigFingerprint,
		CheckedAt:         startedAt,
		EndpointProfile:   assignment.Job.Profile.ID,
		Status:            diagnostics.ProbeStatusProxyFailure,
		// Left unchecked on purpose. Claiming Checked before directConnectivity
		// actually runs would sign evidence for a control that never happened,
		// and every early rejection below would be reported as a network failure.
		DirectConnectivity: diagnostics.CheckEvidence{},
	}
	defer func() {
		observation.DurationMillis = maxInt64(0, time.Since(startedAt).Milliseconds())
	}()

	if err := validateAssignment(assignment); err != nil {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	if got := diagnostics.ConfigFingerprint(assignment.XrayConfig); got != assignment.Job.ConfigFingerprint {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	if err := validateXrayExecutionConfig(assignment.XrayConfig, assignment.SocksPort); err != nil {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	descriptor, known := diagnostics.ProfileByID(assignment.Job.Profile.ID)
	if !known || descriptor.Method != assignment.Job.Profile.Method {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	directEvidence, directBody := e.directConnectivity(ctx)
	observation.DirectConnectivity = directEvidence

	// Transport probes deliberately never start Xray: their whole purpose is to
	// reach the node the way the node itself is reached, so a tunnel in the path
	// would hide exactly what they are looking for.
	if !descriptor.Tunnelled {
		e.runTransportProbe(ctx, assignment, descriptor, &observation)
		return observation
	}

	if err := os.MkdirAll(e.config.RuntimeDir, 0700); err != nil {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	temporary, err := os.CreateTemp(e.config.RuntimeDir, ".xray-job-*.json")
	if err != nil {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	if _, err := temporary.Write(assignment.XrayConfig); err != nil {
		_ = temporary.Close()
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}
	if err := temporary.Close(); err != nil {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
		return observation
	}

	runner := xray.NewRunner(temporaryPath)
	if err := runner.Start(); err != nil {
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageXrayStart}
		return observation
	}
	defer runner.Stop()

	proxyResult := e.proxyCheck(ctx, assignment.Job.Profile, assignment.SocksPort, directBody)
	observation.Status = proxyResult.status
	observation.LatencyMillis = proxyResult.latency.Milliseconds()
	observation.Failure = proxyResult.failure
	observation.Throughput = proxyResult.throughput
	observation.Latency = proxyResult.latencySeries
	observation.Stability = proxyResult.stability
	if proxyResult.status == diagnostics.ProbeStatusOnline {
		return observation
	}

	// A failed endpoint is not the same thing as a failed node. Retrying against
	// a second endpoint tells the two apart before TCP and ping are consulted.
	if alternativeID := strings.TrimSpace(assignment.Job.Profile.AlternativeProfileID); alternativeID != "" {
		if alternative, ok := diagnostics.ProfileByID(alternativeID); ok && alternative.Tunnelled {
			result := e.proxyCheck(ctx, alternative.TestProfileFor(""), assignment.SocksPort, directBody)
			observation.AlternativeEndpoint = &diagnostics.AlternativeEndpointObservation{
				ProfileID:     alternative.ID,
				Status:        result.status,
				LatencyMillis: result.latency.Milliseconds(),
				Failure:       result.failure,
			}
			if result.status == diagnostics.ProbeStatusOnline {
				return observation
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		observation.TCP = tcpEvidence(ctx, assignment.TargetHost, assignment.TargetPort, diagnosticHostTimeout(e.config.ProxyTimeout))
	}()
	go func() {
		defer wg.Done()
		observation.Ping = pingEvidence(ctx, assignment.TargetHost, diagnosticHostTimeout(e.config.ProxyTimeout))
	}()
	wg.Wait()
	if observation.TCP.Checked && !observation.TCP.Online && observation.Ping.Checked && !observation.Ping.Online {
		observation.Status = diagnostics.ProbeStatusOffline
	}
	return observation
}

type proxyCheckResult struct {
	status        diagnostics.ProbeStatus
	latency       time.Duration
	failure       diagnostics.FailureEvidence
	throughput    *diagnostics.ThroughputEvidence
	latencySeries *diagnostics.LatencySeriesEvidence
	stability     *diagnostics.StabilityEvidence
}

func (e *Executor) proxyCheck(ctx context.Context, profile diagnostics.TestProfile, socksPort int, directBody string) proxyCheckResult {
	endpoint := ""
	timeout := e.config.ProxyTimeout
	switch profile.ID {
	case diagnostics.ProfileLatency:
		if profile.Method != diagnostics.ProbeMethodLatency {
			return configurationResult()
		}
		return e.latencyProfile(ctx, socksPort)
	case diagnostics.ProfileStability:
		if profile.Method != diagnostics.ProbeMethodStability {
			return configurationResult()
		}
		return e.stabilityProbe(ctx, socksPort)
	case diagnostics.ProfileIP:
		if profile.Method != diagnostics.ProbeMethodIP {
			return configurationResult()
		}
		endpoint = e.config.IPCheckURL
	case diagnostics.ProfileStatus:
		if profile.Method != diagnostics.ProbeMethodStatus {
			return configurationResult()
		}
		endpoint = e.config.StatusCheckURL
	case diagnostics.ProfileDownload:
		if profile.Method != diagnostics.ProbeMethodDownload {
			return configurationResult()
		}
		endpoint = e.config.DownloadURL
		timeout = e.config.DownloadTimeout
	default:
		return configurationResult()
	}
	client := e.socksClient(socksPort, timeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return configurationResult()
	}
	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb = time.Since(start) }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := client.Do(request)
	if err != nil {
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: classifyProxyError(err)}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: diagnostics.FailureEvidence{Code: "http_status", Stage: diagnostics.FailureStageEndpoint}}
	}
	if profile.Method == diagnostics.ProbeMethodDownload {
		transferStart := time.Now()
		read, err := io.CopyN(io.Discard, response.Body, e.config.DownloadMinSize)
		transferred := time.Since(transferStart)
		// The rate is reported even for an incomplete transfer: "stalled after
		// 12 KiB at 0.3 Mbps" is a different diagnosis from "refused outright".
		throughput := throughputEvidence(read, transferred, ttfb)
		if err != nil && !errors.Is(err, io.EOF) {
			return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: classifyProxyError(err), throughput: throughput}
		}
		if read < e.config.DownloadMinSize {
			return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: diagnostics.FailureEvidence{Code: "download_incomplete", Stage: diagnostics.FailureStageEndpoint}, throughput: throughput}
		}
		return proxyCheckResult{status: diagnostics.ProbeStatusOnline, latency: ttfb, throughput: throughput}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: classifyProxyError(err)}
	}
	if profile.Method == diagnostics.ProbeMethodIP && strings.TrimSpace(string(body)) == strings.TrimSpace(directBody) {
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: diagnostics.FailureEvidence{Code: "source_ip_unchanged", Stage: diagnostics.FailureStageProxy}}
	}
	return proxyCheckResult{status: diagnostics.ProbeStatusOnline, latency: ttfb}
}

func (e *Executor) directConnectivity(ctx context.Context) (diagnostics.CheckEvidence, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.config.DirectCheckURL, nil)
	if err != nil {
		return diagnostics.CheckEvidence{Checked: true, FailureCode: "check_endpoint"}, ""
	}
	client := &http.Client{Timeout: e.config.ProxyTimeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	start := time.Now()
	response, err := client.Do(request)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return diagnostics.CheckEvidence{Checked: true, LatencyMillis: latency, FailureCode: "check_endpoint"}, ""
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return diagnostics.CheckEvidence{Checked: true, LatencyMillis: latency, FailureCode: "check_endpoint"}, ""
	}
	return diagnostics.CheckEvidence{Checked: true, Online: true, LatencyMillis: latency}, string(body)
}

func validateAssignment(assignment JobAssignment) error {
	job := assignment.Job
	if job.SchemaVersion != diagnostics.JobSchemaVersion || job.JobID == "" || job.SessionID == "" || job.AgentID == "" || job.Nonce == "" || job.StableID == "" {
		return fmt.Errorf("invalid job binding")
	}
	if !job.ExpiresAt.After(time.Now().UTC()) || assignment.SocksPort < 1024 || assignment.SocksPort > 65535 || strings.TrimSpace(assignment.TargetHost) == "" || assignment.TargetPort < 1 || assignment.TargetPort > 65535 {
		return fmt.Errorf("invalid or expired job")
	}
	if len(assignment.XrayConfig) == 0 || len(assignment.XrayConfig) > MaxExecutionConfigBytes {
		return fmt.Errorf("invalid execution config size")
	}
	return nil
}

func validateXrayExecutionConfig(data []byte, socksPort int) error {
	var document struct {
		Inbounds []struct {
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
		Outbounds []struct {
			Protocol string `json:"protocol"`
		} `json:"outbounds"`
		Routing json.RawMessage `json:"routing"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing Xray config data")
	}
	if len(document.Inbounds) != 1 || document.Inbounds[0].Listen != "127.0.0.1" || document.Inbounds[0].Port != socksPort || document.Inbounds[0].Protocol != "socks" || len(document.Outbounds) != 3 || len(document.Routing) == 0 {
		return fmt.Errorf("execution config violates agent sandbox")
	}
	allowedTargetProtocol := map[string]bool{"vless": true, "vmess": true, "trojan": true, "shadowsocks": true}
	if document.Outbounds[0].Protocol != "freedom" || document.Outbounds[1].Protocol != "blackhole" || !allowedTargetProtocol[document.Outbounds[2].Protocol] {
		return fmt.Errorf("execution config contains unsupported outbounds")
	}
	return nil
}

func tcpEvidence(ctx context.Context, host string, port int, timeout time.Duration) diagnostics.CheckEvidence {
	evidence := diagnostics.CheckEvidence{Checked: true}
	target := net.JoinHostPort(strings.TrimSpace(host), fmt.Sprintf("%d", port))
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	connection, err := (&net.Dialer{Timeout: timeout}).DialContext(checkCtx, "tcp", target)
	evidence.LatencyMillis = time.Since(start).Milliseconds()
	if err == nil {
		_ = connection.Close()
		evidence.Online = true
		return evidence
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		evidence.FailureCode = "tcp_refused"
	} else if strings.Contains(strings.ToLower(err.Error()), "no such host") {
		evidence.FailureCode = "dns"
	} else {
		evidence.FailureCode = "tcp_timeout"
	}
	return evidence
}

func pingEvidence(ctx context.Context, host string, timeout time.Duration) diagnostics.CheckEvidence {
	evidence := diagnostics.CheckEvidence{Checked: true, FailureCode: "host_unreachable"}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(resolveCtx, strings.TrimSpace(host))
	if err != nil || len(addresses) == 0 {
		evidence.FailureCode = "dns"
		return evidence
	}
	for _, address := range addresses {
		latency, err := pingAddress(address.IP, timeout)
		evidence.LatencyMillis = latency.Milliseconds()
		if err == nil {
			evidence.Online = true
			evidence.FailureCode = ""
			return evidence
		}
	}
	return evidence
}

func pingAddress(ip net.IP, timeout time.Duration) (time.Duration, error) {
	network, listenAddress := "udp4", "0.0.0.0"
	protocol := ipv4.ICMPTypeEcho.Protocol()
	var requestType icmp.Type = ipv4.ICMPTypeEcho
	var responseType icmp.Type = ipv4.ICMPTypeEchoReply
	if ip.To4() == nil {
		network, listenAddress = "udp6", "::"
		protocol = ipv6.ICMPTypeEchoRequest.Protocol()
		requestType, responseType = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	connection, err := icmp.ListenPacket(network, listenAddress)
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	probeNumber := atomic.AddUint64(&executorPingSequence, 1)
	sequence := int(probeNumber & 0xffff)
	probeData := []byte(fmt.Sprintf("xray-checker-agent:%d:%d", time.Now().UnixNano(), probeNumber))
	message := icmp.Message{Type: requestType, Body: &icmp.Echo{Seq: sequence, Data: probeData}}
	payload, err := message.Marshal(nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := connection.WriteTo(payload, &net.UDPAddr{IP: ip}); err != nil {
		return time.Since(start), err
	}
	buffer := make([]byte, 1500)
	for {
		count, _, err := connection.ReadFrom(buffer)
		latency := time.Since(start)
		if err != nil {
			return latency, err
		}
		reply, err := icmp.ParseMessage(protocol, buffer[:count])
		if err != nil || reply.Type != responseType {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		if ok && echo.Seq == sequence && bytes.Equal(echo.Data, probeData) {
			return latency, nil
		}
	}
}

func classifyProxyError(err error) diagnostics.FailureEvidence {
	text := strings.ToLower(err.Error())
	code := "unknown"
	switch {
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(text, "timeout"):
		code = "proxy_timeout"
	case strings.Contains(text, "no such host"):
		code = "dns"
	case strings.Contains(text, "connection refused"):
		code = "tcp_refused"
	case strings.Contains(text, "tls"), strings.Contains(text, "x509"), strings.Contains(text, "certificate"):
		code = "tls"
	case strings.Contains(text, "socks"), strings.Contains(text, "proxyconnect"):
		code = "proxy_handshake"
	}
	return diagnostics.FailureEvidence{Code: code, Stage: diagnostics.FailureStageProxy}
}

func configurationResult() proxyCheckResult {
	return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, failure: diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}}
}

func validateEndpointURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must be http(s) without credentials or fragment")
	}
	return nil
}

func diagnosticHostTimeout(proxyTimeout time.Duration) time.Duration {
	if proxyTimeout < 3*time.Second {
		return proxyTimeout
	}
	return 3 * time.Second
}

func valueOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (e *Executor) socksClient(socksPort int, timeout time.Duration) *http.Client {
	transport := &http.Transport{DisableKeepAlives: true}
	// validateAssignment rejects any port below 1024, so a dispatched job can
	// never reach here with zero. Tests use it to exercise the HTTP logic
	// without standing up a SOCKS listener.
	if socksPort > 0 {
		proxyURL, _ := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", socksPort))
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// latencyProfile repeats the status probe. One sample cannot tell a node that is
// uniformly slow from one that is mostly fine with a heavy tail, and that
// distinction decides whether the route or the node is at fault.
func (e *Executor) latencyProfile(ctx context.Context, socksPort int) proxyCheckResult {
	client := e.socksClient(socksPort, e.config.ProxyTimeout)
	series := &diagnostics.LatencySeriesEvidence{Samples: e.config.LatencySamples}
	samples := make([]time.Duration, 0, e.config.LatencySamples)
	var lastFailure diagnostics.FailureEvidence
	for index := 0; index < e.config.LatencySamples; index++ {
		if ctx.Err() != nil {
			break
		}
		latency, failure := e.singleLatencySample(ctx, client)
		if failure.Code != "" {
			lastFailure = failure
			series.FailureCode = failure.Code
			continue
		}
		samples = append(samples, latency)
	}
	series.Succeeded = len(samples)
	if len(samples) == 0 {
		if lastFailure.Code == "" {
			lastFailure = diagnostics.FailureEvidence{Code: "proxy_timeout", Stage: diagnostics.FailureStageProxy}
		}
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, failure: lastFailure, latencySeries: series}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	series.MinMillis = samples[0].Milliseconds()
	series.MaxMillis = samples[len(samples)-1].Milliseconds()
	series.MedianMillis = percentileMillis(samples, 0.50)
	series.P95Millis = percentileMillis(samples, 0.95)
	series.JitterMillis = series.MaxMillis - series.MinMillis
	median := time.Duration(series.MedianMillis) * time.Millisecond
	// A partially failing series is still a proxy failure: an intermittent
	// tunnel is not a healthy one, and reporting it as online hides the fault.
	if len(samples) < e.config.LatencySamples {
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: median, failure: lastFailure, latencySeries: series}
	}
	return proxyCheckResult{status: diagnostics.ProbeStatusOnline, latency: median, latencySeries: series}
}

func (e *Executor) singleLatencySample(ctx context.Context, client *http.Client) (time.Duration, diagnostics.FailureEvidence) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.config.StatusCheckURL, nil)
	if err != nil {
		return 0, diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
	}
	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb = time.Since(start) }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := client.Do(request)
	if err != nil {
		return 0, classifyProxyError(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, diagnostics.FailureEvidence{Code: "http_status", Stage: diagnostics.FailureStageEndpoint}
	}
	if ttfb == 0 {
		ttfb = time.Since(start)
	}
	return ttfb, diagnostics.FailureEvidence{}
}

// stabilityProbe holds a single tunnelled transfer open. Filtering that allows
// the handshake and drops the session seconds later is invisible to every short
// probe, and is a common shape of interference.
func (e *Executor) stabilityProbe(ctx context.Context, socksPort int) proxyCheckResult {
	planned := e.config.StabilityDuration
	evidence := &diagnostics.StabilityEvidence{PlannedMillis: planned.Milliseconds()}
	probeCtx, cancel := context.WithTimeout(ctx, planned+e.config.ProxyTimeout)
	defer cancel()
	client := e.socksClient(socksPort, planned+e.config.ProxyTimeout)
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, e.config.DownloadURL, nil)
	if err != nil {
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, failure: diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}, stability: evidence}
	}
	var ttfb time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { ttfb = time.Since(start) }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := client.Do(request)
	if err != nil {
		evidence.HeldMillis = time.Since(start).Milliseconds()
		evidence.Interrupted = true
		failure := classifyProxyError(err)
		evidence.FailureCode = failure.Code
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: failure, stability: evidence}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		evidence.HeldMillis = time.Since(start).Milliseconds()
		evidence.FailureCode = "http_status"
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: diagnostics.FailureEvidence{Code: "http_status", Stage: diagnostics.FailureStageEndpoint}, stability: evidence}
	}

	deadline := time.Now().Add(planned)
	buffer := make([]byte, stabilityReadChunk)
	var readErr error
	for time.Now().Before(deadline) {
		count, err := response.Body.Read(buffer)
		evidence.Bytes += int64(count)
		if err != nil {
			readErr = err
			break
		}
	}
	evidence.HeldMillis = time.Since(start).Milliseconds()
	// Reaching the end of a finite body is success, not an interruption: the
	// transfer completed, it did not get cut.
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		evidence.Interrupted = true
		failure := classifyProxyError(readErr)
		evidence.FailureCode = failure.Code
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: failure, stability: evidence}
	}
	if evidence.Bytes == 0 {
		evidence.Interrupted = true
		evidence.FailureCode = "download_incomplete"
		return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: diagnostics.FailureEvidence{Code: "download_incomplete", Stage: diagnostics.FailureStageEndpoint}, stability: evidence}
	}
	return proxyCheckResult{status: diagnostics.ProbeStatusOnline, latency: ttfb, stability: evidence}
}

func throughputEvidence(bytes int64, elapsed, ttfb time.Duration) *diagnostics.ThroughputEvidence {
	evidence := &diagnostics.ThroughputEvidence{
		Bytes:          bytes,
		DurationMillis: elapsed.Milliseconds(),
		TTFBMillis:     ttfb.Milliseconds(),
	}
	if seconds := elapsed.Seconds(); seconds > 0 && bytes > 0 {
		evidence.Mbps = int64(float64(bytes) * 8 / seconds / 1_000_000)
	}
	return evidence
}

func percentileMillis(sorted []time.Duration, quantile float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * quantile)
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index].Milliseconds()
}

// runTransportProbe answers "can this node be reached at all", without the
// tunnel. Its status stays in proxy_failure/offline terms so the session summary
// keeps comparing like with like against the controller's own result.
func (e *Executor) runTransportProbe(ctx context.Context, assignment JobAssignment, descriptor diagnostics.ProfileDescriptor, observation *diagnostics.Observation) {
	timeout := diagnosticHostTimeout(e.config.ProxyTimeout)
	switch descriptor.Method {
	case diagnostics.ProbeMethodTLS:
		observation.TCP = tcpEvidence(ctx, assignment.TargetHost, assignment.TargetPort, timeout)
		evidence := e.tlsProbe(ctx, assignment, timeout)
		observation.TLS = evidence
		observation.LatencyMillis = evidence.LatencyMillis
		if evidence.Handshake {
			observation.Status = diagnostics.ProbeStatusOnline
			return
		}
		observation.Failure = diagnostics.FailureEvidence{Code: evidence.FailureCode, Stage: diagnostics.FailureStageTLS}
		// TCP refused as well means the node is not answering at all; a TLS-only
		// failure means the port answers but the session cannot be established,
		// which is the signature of SNI-based interference.
		if observation.TCP.Checked && !observation.TCP.Online {
			observation.Status = diagnostics.ProbeStatusOffline
			observation.Failure.Stage = diagnostics.FailureStageTCP
		}
	case diagnostics.ProbeMethodDNS:
		evidence := e.dnsProbe(ctx, assignment.TargetHost, timeout)
		observation.DNS = evidence
		if evidence.Literal {
			// Nothing to resolve. Saying "online" would claim a check that never
			// happened, so this reports unknown instead.
			observation.Status = diagnostics.ProbeStatusUnknown
			observation.Failure = diagnostics.FailureEvidence{Code: "dns_literal_address", Stage: diagnostics.FailureStageDNS}
			return
		}
		resolved := false
		for _, resolver := range evidence.Resolvers {
			if len(resolver.Addresses) > 0 {
				resolved = true
				observation.LatencyMillis = resolver.LatencyMillis
				break
			}
		}
		if !resolved {
			observation.Status = diagnostics.ProbeStatusOffline
			observation.Failure = diagnostics.FailureEvidence{Code: "dns", Stage: diagnostics.FailureStageDNS}
			return
		}
		if evidence.Mismatch {
			observation.Status = diagnostics.ProbeStatusProxyFailure
			observation.Failure = diagnostics.FailureEvidence{Code: "dns_mismatch", Stage: diagnostics.FailureStageDNS}
			return
		}
		observation.Status = diagnostics.ProbeStatusOnline
	default:
		observation.Failure = diagnostics.FailureEvidence{Code: "configuration", Stage: diagnostics.FailureStageConfiguration}
	}
}

// tlsProbe performs the handshake the node itself needs. InsecureSkipVerify is
// deliberate: many nodes present deliberately mismatched certificates, and the
// question here is whether the handshake completes at all, not whether the
// certificate would satisfy a browser. Nothing is sent over this connection.
func (e *Executor) tlsProbe(ctx context.Context, assignment JobAssignment, timeout time.Duration) *diagnostics.TLSEvidence {
	serverName := strings.TrimSpace(assignment.TargetSNI)
	if serverName == "" {
		serverName = strings.TrimSpace(assignment.TargetHost)
	}
	evidence := &diagnostics.TLSEvidence{Checked: true, ServerName: serverName}
	target := net.JoinHostPort(strings.TrimSpace(assignment.TargetHost), fmt.Sprintf("%d", assignment.TargetPort))
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	connection, err := (&net.Dialer{Timeout: timeout}).DialContext(handshakeCtx, "tcp", target)
	if err != nil {
		evidence.LatencyMillis = time.Since(start).Milliseconds()
		evidence.FailureCode = tcpFailureCode(err)
		return evidence
	}
	defer connection.Close()
	tlsConnection := tls.Client(connection, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConnection.HandshakeContext(handshakeCtx); err != nil {
		evidence.LatencyMillis = time.Since(start).Milliseconds()
		evidence.FailureCode = tlsFailureCode(err)
		return evidence
	}
	evidence.LatencyMillis = time.Since(start).Milliseconds()
	evidence.Handshake = true
	state := tlsConnection.ConnectionState()
	evidence.NegotiatedVersion = tlsVersionName(state.Version)
	evidence.NegotiatedProtocol = state.NegotiatedProtocol
	if len(state.PeerCertificates) > 0 {
		certificate := state.PeerCertificates[0]
		evidence.CertificateIssuer = strings.TrimSpace(certificate.Issuer.CommonName)
		evidence.CertificateExpiry = certificate.NotAfter.UTC().Format(time.RFC3339)
	}
	_ = tlsConnection.Close()
	return evidence
}

// dnsProbe resolves the node hostname through the host resolver and one
// independent resolver. Only the node's own hostname is looked up, so this
// cannot be steered into resolving anything the controller chooses.
func (e *Executor) dnsProbe(ctx context.Context, host string, timeout time.Duration) *diagnostics.DNSEvidence {
	hostname := strings.TrimSpace(host)
	evidence := &diagnostics.DNSEvidence{Checked: true, Hostname: hostname}
	if net.ParseIP(hostname) != nil {
		evidence.Literal = true
		return evidence
	}
	resolvers := []struct {
		name     string
		resolver *net.Resolver
	}{
		{name: "system", resolver: net.DefaultResolver},
		{name: e.config.DNSResolver, resolver: &net.Resolver{
			PreferGo: true,
			Dial: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: timeout}).DialContext(dialCtx, network, e.config.DNSResolver)
			},
		}},
	}
	answers := make([][]string, 0, len(resolvers))
	for _, entry := range resolvers {
		lookupCtx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		addresses, err := entry.resolver.LookupIPAddr(lookupCtx, hostname)
		cancel()
		result := diagnostics.DNSResolverEvidence{Resolver: entry.name, LatencyMillis: time.Since(start).Milliseconds()}
		if err != nil {
			result.FailureCode = "dns"
			evidence.Resolvers = append(evidence.Resolvers, result)
			continue
		}
		for _, address := range addresses {
			result.Addresses = append(result.Addresses, address.IP.String())
		}
		sort.Strings(result.Addresses)
		evidence.Resolvers = append(evidence.Resolvers, result)
		if len(result.Addresses) > 0 {
			answers = append(answers, result.Addresses)
		}
	}
	// Disagreement only means something when both resolvers actually answered.
	if len(answers) > 1 && !sharesAddress(answers[0], answers[1]) {
		evidence.Mismatch = true
	}
	return evidence
}

func sharesAddress(left, right []string) bool {
	seen := make(map[string]bool, len(left))
	for _, address := range left {
		seen[address] = true
	}
	for _, address := range right {
		if seen[address] {
			return true
		}
	}
	return false
}

func tcpFailureCode(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "refused"):
		return "tcp_refused"
	case strings.Contains(message, "no such host"):
		return "dns"
	default:
		return "tcp_timeout"
	}
}

func tlsFailureCode(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "reset"):
		return "tls_reset"
	case strings.Contains(message, "timeout") || strings.Contains(message, "deadline"):
		return "tls_timeout"
	case strings.Contains(message, "eof"):
		return "tls_eof"
	case strings.Contains(message, "handshake failure") || strings.Contains(message, "alert"):
		return "tls_alert"
	default:
		return "tls_failed"
	}
}

func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	default:
		return ""
	}
}
