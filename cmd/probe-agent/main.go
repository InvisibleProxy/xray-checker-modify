package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"xray-checker/probeagent"
)

var version = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := probeagent.NewClient(probeagent.ClientConfig{
		AgentID:          os.Getenv("PROBE_AGENT_ID"),
		EnrollmentToken:  os.Getenv("PROBE_ENROLLMENT_TOKEN"),
		ControllerURL:    os.Getenv("PROBE_CONTROLLER_URL"),
		ControllerIP:     os.Getenv("PROBE_CONTROLLER_IP"),
		ControllerCAFile: os.Getenv("PROBE_CONTROLLER_CA_FILE"),
		IdentityDir:      envOrDefault("PROBE_IDENTITY_DIR", "/var/lib/xray-checker-agent"),
		AgentVersion:     version,
		Capabilities:     []string{"control-v1"},
		RequestTimeout:   20 * time.Second,
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
