package probeagent

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderComposeProducesStandaloneOutboundOnlyLinuxService(t *testing.T) {
	record := AgentRecord{
		AgentID: "agent_AbCd", ControllerURL: "https://checker.example.com",
		ControllerIP: "198.51.100.10",
	}
	compose := RenderCompose(record, "enroll_secret", "registry.example.com/probe-agent:test")
	var document map[string]any
	if err := yaml.Unmarshal([]byte(compose), &document); err != nil {
		t.Fatalf("parse generated Compose: %v\n%s", err, compose)
	}
	if !strings.HasPrefix(compose, "name: xray-checker-agent-abcd-") {
		t.Fatalf("generated project name is not normalized: %s", strings.SplitN(compose, "\n", 2)[0])
	}
	for _, required := range []string{
		"PROBE_ENROLLMENT_TOKEN: \"enroll_secret\"",
		"PROBE_CONTROLLER_IP: \"198.51.100.10\"",
		"read_only: true",
		"cap_drop:",
		"probe_agent_identity:/var/lib/xray-checker-agent",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("generated Compose is missing %q", required)
		}
	}
	if strings.Contains(compose, "ports:") {
		t.Fatal("generated outbound-only agent Compose exposes an inbound port")
	}
}
