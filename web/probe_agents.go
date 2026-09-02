package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"xray-checker/probeagent"
)

const maxProbeAgentRequestBytes = 64 * 1024

type AdminDiagnosticAgentService interface {
	Enabled() bool
	Snapshot() []probeagent.AgentSnapshot
	ControllerDefaults() (string, string)
	Create(probeagent.CreateAgentRequest) (probeagent.CreationResult, error)
	Reissue(string) (probeagent.CreationResult, error)
	Revoke(string) (probeagent.AgentSnapshot, error)
	Delete(string) error
}

type diagnosticAgentsSnapshot struct {
	Enabled bool                       `json:"enabled"`
	Agents  []probeagent.AgentSnapshot `json:"agents"`
	// Defaults describe this controller, which is the same for every agent. The
	// admin form prefills them so the operator states the address once rather
	// than retyping it for each probe.
	Defaults controllerDefaults `json:"defaults"`
}

type controllerDefaults struct {
	ControllerURL string `json:"controllerUrl,omitempty"`
	ControllerIP  string `json:"controllerIp,omitempty"`
}

type diagnosticAgentActionRequest struct {
	AgentID string `json:"agentId"`
}

func AdminDiagnosticAgentsHandler(service AdminDiagnosticAgentService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			controllerURL, controllerIP := service.ControllerDefaults()
			writeJSON(w, diagnosticAgentsSnapshot{
				Enabled:  service.Enabled(),
				Agents:   service.Snapshot(),
				Defaults: controllerDefaults{ControllerURL: controllerURL, ControllerIP: controllerIP},
			})
		case http.MethodPost:
			var input probeagent.CreateAgentRequest
			if !decodeProbeAgentJSON(w, request, &input) {
				return
			}
			result, err := service.Create(input)
			if err != nil {
				writeProbeAgentAdminError(w, err)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, result)
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func AdminDiagnosticAgentReissueHandler(service AdminDiagnosticAgentService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input diagnosticAgentActionRequest
		if !decodeProbeAgentJSON(w, request, &input) {
			return
		}
		result, err := service.Reissue(input.AgentID)
		if err != nil {
			writeProbeAgentAdminError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, result)
	}
}

func AdminDiagnosticAgentRevokeHandler(service AdminDiagnosticAgentService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input diagnosticAgentActionRequest
		if !decodeProbeAgentJSON(w, request, &input) {
			return
		}
		result, err := service.Revoke(input.AgentID)
		if err != nil {
			writeProbeAgentAdminError(w, err)
			return
		}
		writeJSON(w, result)
	}
}

func AdminDiagnosticAgentDeleteHandler(service AdminDiagnosticAgentService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input diagnosticAgentActionRequest
		if !decodeProbeAgentJSON(w, request, &input) {
			return
		}
		if err := service.Delete(input.AgentID); err != nil {
			writeProbeAgentAdminError(w, err)
			return
		}
		writeJSON(w, map[string]string{"agentId": strings.TrimSpace(input.AgentID)})
	}
}

func ProbeAgentEnrollHandler(registry *probeagent.Registry, trustedProxySecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if request.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sourceIP, err := probeagent.RequestSourceIP(request, trustedProxySecret)
		if err != nil {
			writeError(w, "Agent authentication failed", http.StatusForbidden)
			return
		}
		var input probeagent.EnrollRequest
		if !decodeProbeAgentJSON(w, request, &input) {
			return
		}
		response, err := registry.Enroll(input, sourceIP)
		if err != nil {
			writeError(w, "Agent authentication failed", http.StatusForbidden)
			return
		}
		writeJSON(w, response)
	}
}

func ProbeAgentHeartbeatHandler(registry *probeagent.Registry, trustedProxySecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if request.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, maxProbeAgentRequestBytes))
		if err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		var input probeagent.HeartbeatRequest
		if err := decodeSingleJSON(body, &input); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		authentication, ok := parseProbeControlAuthentication(w, request, trustedProxySecret, body, input.AgentID, probeagent.HeartbeatPath)
		if !ok {
			return
		}
		response, err := registry.AcceptHeartbeat(input, authentication.sourceIP, authentication.timestamp, authentication.sequence, authentication.payload, authentication.signature)
		if err != nil {
			writeError(w, "Agent authentication failed", http.StatusForbidden)
			return
		}
		writeJSON(w, response)
	}
}

type probeControlAuthentication struct {
	sourceIP  netip.Addr
	timestamp time.Time
	sequence  uint64
	payload   []byte
	signature []byte
}

func parseProbeControlAuthentication(w http.ResponseWriter, request *http.Request, trustedProxySecret string, body []byte, agentID, path string) (probeControlAuthentication, bool) {
	sourceIP, err := probeagent.RequestSourceIP(request, trustedProxySecret)
	if err != nil {
		writeError(w, "Agent authentication failed", http.StatusForbidden)
		return probeControlAuthentication{}, false
	}
	headerAgentID := strings.TrimSpace(request.Header.Get("X-Probe-Agent-ID"))
	sequence, sequenceErr := strconv.ParseUint(request.Header.Get("X-Probe-Sequence"), 10, 64)
	timestamp, timestampErr := time.Parse(time.RFC3339Nano, request.Header.Get("X-Probe-Timestamp"))
	signature, signatureErr := base64.RawStdEncoding.DecodeString(request.Header.Get("X-Probe-Signature"))
	if headerAgentID == "" || headerAgentID != strings.TrimSpace(agentID) || sequenceErr != nil || timestampErr != nil || signatureErr != nil {
		writeError(w, "Agent authentication failed", http.StatusForbidden)
		return probeControlAuthentication{}, false
	}
	payload, err := probeagent.ControlSigningPayload(request.Method, path, headerAgentID, timestamp, sequence, body)
	if err != nil {
		writeError(w, "Agent authentication failed", http.StatusForbidden)
		return probeControlAuthentication{}, false
	}
	return probeControlAuthentication{sourceIP: sourceIP, timestamp: timestamp, sequence: sequence, payload: payload, signature: signature}, true
}

func decodeProbeAgentJSON(w http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(w, request.Body, maxProbeAgentRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, "Invalid JSON body", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(w, "JSON body must contain one object", http.StatusBadRequest)
		return false
	}
	return true
}

func decodeSingleJSON(body []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeProbeAgentAdminError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, probeagent.ErrAgentNotFound) {
		code = http.StatusNotFound
	} else if errors.Is(err, probeagent.ErrDisabled) || errors.Is(err, probeagent.ErrAgentNotRevoked) {
		code = http.StatusConflict
	}
	writeError(w, err.Error(), code)
}
