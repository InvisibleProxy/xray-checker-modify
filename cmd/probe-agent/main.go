package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/logger"
	"xray-checker/probeagent"
)

var version = "unknown"

func main() {
	logLevel := logger.ParseLevel(envOrDefault("PROBE_LOG_LEVEL", "info"))
	logger.SetLevel(logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	executorConfig := probeagent.ExecutorConfig{
		RuntimeDir:      envOrDefault("PROBE_RUNTIME_DIR", probeagent.DefaultAgentRuntimeDir),
		IPCheckURL:      os.Getenv("PROBE_IP_CHECK_URL"),
		StatusCheckURL:  os.Getenv("PROBE_STATUS_CHECK_URL"),
		DownloadURL:     os.Getenv("PROBE_DOWNLOAD_URL"),
		DirectCheckURL:  os.Getenv("PROBE_DIRECT_CHECK_URL"),
		ProxyTimeout:    time.Duration(envPositiveInt("PROBE_PROXY_TIMEOUT_SECONDS", 30)) * time.Second,
		DownloadTimeout: time.Duration(envPositiveInt("PROBE_DOWNLOAD_TIMEOUT_SECONDS", 60)) * time.Second,
		DownloadMinSize: int64(envPositiveInt("PROBE_DOWNLOAD_MIN_SIZE", int(probeagent.DefaultDownloadMinSize))),
		// Probe shapes stay agent-owned: the controller names a profile, it never
		// dictates how long or how often that profile runs.
		LatencySamples:    envPositiveInt("PROBE_LATENCY_SAMPLES", probeagent.DefaultLatencySamples),
		StabilityDuration: time.Duration(envPositiveInt("PROBE_STABILITY_SECONDS", 20)) * time.Second,
		DNSResolver:       envOrDefault("PROBE_DNS_RESOLVER", probeagent.DefaultDNSResolver),
		OnDetail: func(jobID string, message string, err error) {
			logger.Warn("job %s: %s", shortID(jobID), message)
			// The underlying error can quote fragments of the node config, so it
			// is only printed when the operator asked for that much detail.
			if err != nil {
				logger.Debug("job %s: underlying error: %v", shortID(jobID), err)
			}
		},
	}
	executor, err := probeagent.NewExecutor(executorConfig)
	if err != nil {
		logger.Fatal("probe executor configuration failed: %v", err)
	}

	agentID := os.Getenv("PROBE_AGENT_ID")
	controllerURL := os.Getenv("PROBE_CONTROLLER_URL")
	controllerIP := os.Getenv("PROBE_CONTROLLER_IP")
	identityDir := envOrDefault("PROBE_IDENTITY_DIR", "/var/lib/xray-checker-agent")
	pollInterval := time.Duration(envPositiveInt("PROBE_JOB_POLL_INTERVAL_SECONDS", 5)) * time.Second

	capabilities := []string{
		diagnostics.CapabilityControlV1,
		diagnostics.CapabilityDiagnosticV1,
		// diagnostic-v2 tells the controller this build can run the latency,
		// stability, TLS and DNS profiles. An older agent advertises only v1 and
		// is never offered them.
		diagnostics.CapabilityDiagnosticV2,
	}

	// Printed before the first connection attempt so a container that never
	// enrolls still says which build, which identity and which controller it was
	// trying to use. No token is printed here or anywhere else.
	logger.Startup("Xray Checker probe agent %s", version)
	logger.Info("agent %s → controller %s (pinned IP %s)", agentID, controllerURL, controllerIP)
	logger.Info("identity %s · runtime %s · job poll every %s · log level %s",
		identityDir, executorConfig.RuntimeDir, pollInterval, logLevel)
	logger.Info("capabilities: %s", strings.Join(capabilities, ", "))
	logger.Info("probe endpoints: ip=%s status=%s download=%s direct=%s",
		valueOrDefault(executorConfig.IPCheckURL, probeagent.DefaultIPCheckURL),
		valueOrDefault(executorConfig.StatusCheckURL, probeagent.DefaultStatusCheckURL),
		valueOrDefault(executorConfig.DownloadURL, probeagent.DefaultDownloadURL),
		valueOrDefault(executorConfig.DirectCheckURL, probeagent.DefaultDirectCheckURL))
	logger.Info("probe limits: proxy timeout %s · download %s of %s · latency samples %d · stability %s · DNS %s",
		executorConfig.ProxyTimeout, formatBytes(executorConfig.DownloadMinSize), executorConfig.DownloadTimeout,
		executorConfig.LatencySamples, executorConfig.StabilityDuration, executorConfig.DNSResolver)
	if strings.TrimSpace(os.Getenv("PROBE_ENROLLMENT_TOKEN")) == "" {
		logger.Info("no enrollment token set; continuing with the identity in %s", identityDir)
	}

	client, err := probeagent.NewClient(probeagent.ClientConfig{
		AgentID:          agentID,
		EnrollmentToken:  os.Getenv("PROBE_ENROLLMENT_TOKEN"),
		ControllerURL:    controllerURL,
		ControllerIP:     controllerIP,
		ControllerCAFile: os.Getenv("PROBE_CONTROLLER_CA_FILE"),
		IdentityDir:      identityDir,
		AgentVersion:     version,
		Capabilities:     capabilities,
		RequestTimeout:   20 * time.Second,
		JobPollInterval:  pollInterval,
		Executor:         executor,
		Hooks:            logHooks(),
	})
	if err != nil {
		logger.Fatal("probe agent configuration failed: %v", err)
	}

	backoff := 5 * time.Second
	for {
		if err := client.Run(ctx); err == nil || ctx.Err() != nil {
			logger.Info("probe agent stopped")
			return
		} else {
			logger.Error("control connection failed; retrying in %s: %v", backoff, err)
		}
		select {
		case <-ctx.Done():
			logger.Info("probe agent stopped")
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
}

func logHooks() probeagent.Hooks {
	return probeagent.Hooks{
		OnEnrolled: func(intervalSeconds int, resumed bool) {
			if resumed {
				logger.Info("enrollment token was already spent; identity re-established through a signed heartbeat")
				return
			}
			logger.Info("enrolled with the controller; heartbeat every %ds", intervalSeconds)
		},
		OnConnected: func(intervalSeconds int) {
			logger.Info("control connection up; the controller now shows this agent as connected (heartbeat every %ds)", intervalSeconds)
		},
		OnHeartbeatFailed: func(err error) {
			logger.Warn("heartbeat failed; the controller will show this agent as disconnected: %v", err)
		},
		OnJobStarted: func(job probeagent.JobStarted) {
			logger.Info("job %s: node %s · profile %s · target %s · expires in %s",
				shortID(job.JobID), job.StableID, job.ProfileID, valueOrDefault(job.Target, "n/a"), job.ExpiresIn.Round(time.Second))
		},
		OnJobFinished: func(job probeagent.JobFinished) {
			logger.Info("job %s: %s", shortID(job.JobID), describeResult(job))
		},
		OnObservationAccepted: func(job probeagent.JobFinished) {
			logger.Debug("job %s: observation accepted by the controller", shortID(job.JobID))
		},
		OnJobRejected: func(err error) {
			// Not fatal by design: one refused observation must not take the
			// heartbeat down and make a healthy agent look offline.
			logger.Warn("observation refused by the controller; control connection stays up: %v", err)
		},
	}
}

// describeResult puts the whole verdict on one line, because that line is what
// an operator greps for when a node looks wrong from one place only.
func describeResult(job probeagent.JobFinished) string {
	parts := []string{string(job.Status)}
	if job.Latency > 0 {
		parts = append(parts, "latency "+job.Latency.Round(time.Millisecond).String())
	}
	if job.ThroughputMbps > 0 {
		parts = append(parts, fmt.Sprintf("%d Mbps", job.ThroughputMbps))
	}
	if job.FailureCode != "" {
		failure := job.FailureCode
		if job.FailureStage != "" {
			failure += " at " + string(job.FailureStage)
		}
		parts = append(parts, "failure "+failure)
	}
	parts = append(parts, "tcp "+describeCheck(job.TCP), "ping "+describeCheck(job.Ping))
	// The direct control decides whether the controller trusts any of this, so
	// it is called out rather than folded in with the rest.
	if !job.Direct.Online {
		parts = append(parts, "direct connectivity FAILED — the controller will not derive a verdict from this")
	}
	if job.Alternative {
		parts = append(parts, "retried against the fallback endpoint")
	}
	parts = append(parts, "took "+job.Elapsed.Round(time.Millisecond).String())
	return strings.Join(parts, " · ")
}

func describeCheck(evidence diagnostics.CheckEvidence) string {
	if !evidence.Checked {
		return "skipped"
	}
	if !evidence.Online {
		return "no"
	}
	if evidence.LatencyMillis > 0 {
		return fmt.Sprintf("ok (%dms)", evidence.LatencyMillis)
	}
	return "ok"
}

// shortID keeps a log line readable. Job IDs are long enough that the full value
// pushes the useful part of the line off screen, and the prefix is unique within
// the minutes a job is alive.
func shortID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func formatBytes(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func valueOrDefault(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envPositiveInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		logger.Fatal("%s must be a positive integer", name)
	}
	return parsed
}
