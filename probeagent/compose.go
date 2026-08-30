package probeagent

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

func RenderCompose(record AgentRecord, enrollmentToken, image string) string {
	tokenDigest := sha256.Sum256([]byte(enrollmentToken))
	identifier := strings.ToLower(strings.ReplaceAll(record.AgentID, "_", "-"))
	project := fmt.Sprintf("xray-checker-%s-%x", identifier, tokenDigest[:6])
	containerName := "xray-checker-probe-agent-" + identifier
	quote := strconv.Quote
	return fmt.Sprintf(`name: %s

services:
  probe-agent:
    image: %s
    container_name: %s
    restart: unless-stopped
    init: true
    user: "10001:10001"
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    pids_limit: 128
    mem_limit: 512m
    cpus: 1.0
    stop_grace_period: 15s
    environment:
      PROBE_AGENT_ID: %s
      PROBE_ENROLLMENT_TOKEN: %s
      PROBE_CONTROLLER_URL: %s
      PROBE_CONTROLLER_IP: %s
      PROBE_IDENTITY_DIR: /var/lib/xray-checker-agent
    volumes:
      - probe_agent_identity:/var/lib/xray-checker-agent
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=128m,mode=0700,uid=10001,gid=10001
      - /run/xray-checker-agent:rw,noexec,nosuid,nodev,size=64m,mode=0700,uid=10001,gid=10001

volumes:
  probe_agent_identity:
`, project, quote(image), containerName, quote(record.AgentID), quote(enrollmentToken), quote(record.ControllerURL), quote(record.ControllerIP))
}
