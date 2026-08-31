package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
)

var version = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	executor, err := probeagent.NewExecutor(probeagent.ExecutorConfig{
		RuntimeDir:      envOrDefault("PROBE_RUNTIME_DIR", probeagent.DefaultAgentRuntimeDir),
		IPCheckURL:      os.Getenv("PROBE_IP_CHECK_URL"),
		StatusCheckURL:  os.Getenv("PROBE_STATUS_CHECK_URL"),
		DownloadURL:     os.Getenv("PROBE_DOWNLOAD_URL"),
		DirectCheckURL:  os.Getenv("PROBE_DIRECT_CHECK_URL"),
		ProxyTimeout:    time.Duration(envPositiveInt("PROBE_PROXY_TIMEOUT_SECONDS", 30)) * time.Second,
		DownloadTimeout: time.Duration(envPositiveInt("PROBE_DOWNLOAD_TIMEOUT_SECONDS", 60)) * time.Second,
		DownloadMinSize: int64(envPositiveInt("PROBE_DOWNLOAD_MIN_SIZE", 51200)),
		// Probe shapes stay agent-owned: the controller names a profile, it never
		// dictates how long or how often that profile runs.
		LatencySamples:    envPositiveInt("PROBE_LATENCY_SAMPLES", probeagent.DefaultLatencySamples),
		StabilityDuration: time.Duration(envPositiveInt("PROBE_STABILITY_SECONDS", 20)) * time.Second,
		DNSResolver:       envOrDefault("PROBE_DNS_RESOLVER", probeagent.DefaultDNSResolver),
	})
	if err != nil {
		log.Fatalf("probe executor configuration failed: %v", err)
	}

	client, err := probeagent.NewClient(probeagent.ClientConfig{
		AgentID:          os.Getenv("PROBE_AGENT_ID"),
		EnrollmentToken:  os.Getenv("PROBE_ENROLLMENT_TOKEN"),
		ControllerURL:    os.Getenv("PROBE_CONTROLLER_URL"),
		ControllerIP:     os.Getenv("PROBE_CONTROLLER_IP"),
		ControllerCAFile: os.Getenv("PROBE_CONTROLLER_CA_FILE"),
		IdentityDir:      envOrDefault("PROBE_IDENTITY_DIR", "/var/lib/xray-checker-agent"),
		AgentVersion:     version,
		// diagnostic-v2 tells the controller this build can run the latency,
		// stability, TLS and DNS profiles. An older agent advertises only v1 and
		// is never offered them.
		Capabilities: []string{
			diagnostics.CapabilityControlV1,
			diagnostics.CapabilityDiagnosticV1,
			diagnostics.CapabilityDiagnosticV2,
		},
		RequestTimeout:  20 * time.Second,
		JobPollInterval: time.Duration(envPositiveInt("PROBE_JOB_POLL_INTERVAL_SECONDS", 5)) * time.Second,
		Executor:        executor,
		// A refused job is worth reporting but not worth dropping the control
		// connection for, so it is logged here instead of ending Run.
		LogJobRejection: func(err error) {
			log.Printf("diagnostic job refused by controller; control connection stays up: %v", err)
		},
	})
	if err != nil {
		log.Fatalf("probe agent configuration failed: %v", err)
	}

	backoff := 5 * time.Second
	for {
		if err := client.Run(ctx); err == nil || ctx.Err() != nil {
			return
		} else {
			log.Printf("probe control connection failed; retrying in %s: %v", backoff, err)
		}
		select {
		case <-ctx.Done():
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
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}
