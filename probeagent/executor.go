package probeagent

import (
	"bytes"
	"context"
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
		SchemaVersion:      diagnostics.ObservationSchemaVersion,
		AgentID:            assignment.Job.AgentID,
		SessionID:          assignment.Job.SessionID,
		JobID:              assignment.Job.JobID,
		Nonce:              assignment.Job.Nonce,
		StableID:           assignment.Job.StableID,
		ConfigGeneration:   assignment.Job.ConfigGeneration,
		ConfigFingerprint:  assignment.Job.ConfigFingerprint,
		CheckedAt:          startedAt,
		EndpointProfile:    assignment.Job.Profile.ID,
		Status:             diagnostics.ProbeStatusProxyFailure,
		DirectConnectivity: diagnostics.CheckEvidence{Checked: true, FailureCode: "check_endpoint"},
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
	directEvidence, directBody := e.directConnectivity(ctx)
	observation.DirectConnectivity = directEvidence

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
	if proxyResult.status == diagnostics.ProbeStatusOnline {
		return observation
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
	status  diagnostics.ProbeStatus
	latency time.Duration
	failure diagnostics.FailureEvidence
}

func (e *Executor) proxyCheck(ctx context.Context, profile diagnostics.TestProfile, socksPort int, directBody string) proxyCheckResult {
	endpoint := ""
	timeout := e.config.ProxyTimeout
	switch profile.ID {
	case "default-ip":
		if profile.Method != diagnostics.ProbeMethodIP {
			return configurationResult()
		}
		endpoint = e.config.IPCheckURL
	case "default-status":
		if profile.Method != diagnostics.ProbeMethodStatus {
			return configurationResult()
		}
		endpoint = e.config.StatusCheckURL
	case "default-download":
		if profile.Method != diagnostics.ProbeMethodDownload {
			return configurationResult()
		}
		endpoint = e.config.DownloadURL
		timeout = e.config.DownloadTimeout
	default:
		return configurationResult()
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", socksPort))
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true},
		Timeout:   timeout,
	}
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
		read, err := io.CopyN(io.Discard, response.Body, e.config.DownloadMinSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: classifyProxyError(err)}
		}
		if read < e.config.DownloadMinSize {
			return proxyCheckResult{status: diagnostics.ProbeStatusProxyFailure, latency: ttfb, failure: diagnostics.FailureEvidence{Code: "download_incomplete", Stage: diagnostics.FailureStageEndpoint}}
		}
		return proxyCheckResult{status: diagnostics.ProbeStatusOnline, latency: ttfb}
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
