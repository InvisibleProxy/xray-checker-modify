package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"xray-checker/diagnostics"
	"xray-checker/probeagent"
	"xray-checker/remoteprobe"
)

type DiagnosticSessionService interface {
	Enabled() bool
	Profiles() []remoteprobe.ProfileView
	CreateManual(remoteprobe.CreateManualRequest) (remoteprobe.SessionView, error)
	Sessions(string) []remoteprobe.SessionView
	Cancel(string) error
	Export(string) ([]byte, error)
	Claim(context.Context, string) (*probeagent.JobAssignment, error)
	AcceptObservation(diagnostics.Observation) (diagnostics.AcceptedObservation, error)
}

// Profiles ship with the sessions snapshot so the admin UI never has to hold its
// own copy of the catalogue, which would drift from the agents' capabilities.
type diagnosticSessionsSnapshot struct {
	Enabled  bool                      `json:"enabled"`
	Profiles []remoteprobe.ProfileView `json:"profiles"`
	Sessions []remoteprobe.SessionView `json:"sessions"`
}

type diagnosticSessionActionRequest struct {
	SessionID string `json:"sessionId"`
}

func AdminDiagnosticSessionsHandler(service DiagnosticSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			writeJSON(w, diagnosticSessionsSnapshot{
				Enabled:  service.Enabled(),
				Profiles: service.Profiles(),
				Sessions: service.Sessions(request.URL.Query().Get("stableId")),
			})
		case http.MethodPost:
			var input remoteprobe.CreateManualRequest
			if !decodeProbeAgentJSON(w, request, &input) {
				return
			}
			result, err := service.CreateManual(input)
			if err != nil {
				writeDiagnosticSessionError(w, err)
				return
			}
			writeJSON(w, result)
		default:
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func AdminDiagnosticSessionCancelHandler(service DiagnosticSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input diagnosticSessionActionRequest
		if !decodeProbeAgentJSON(w, request, &input) {
			return
		}
		if err := service.Cancel(input.SessionID); err != nil {
			writeDiagnosticSessionError(w, err)
			return
		}
		writeJSON(w, map[string]string{"sessionId": strings.TrimSpace(input.SessionID)})
	}
}

func AdminDiagnosticSessionExportHandler(service DiagnosticSessionService) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sessionID := strings.TrimSpace(request.URL.Query().Get("sessionId"))
		data, err := service.Export(sessionID)
		if err != nil {
			writeDiagnosticSessionError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "diagnostic-"+sessionID+".json"))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}

func ProbeAgentJobHandler(registry *probeagent.Registry, service DiagnosticSessionService, trustedProxySecret string) http.HandlerFunc {
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
		var input probeagent.ControlRequest
		if err := decodeSingleJSON(body, &input); err != nil || input.ProtocolVersion != probeagent.ProtocolVersion {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		authentication, ok := parseProbeControlAuthentication(w, request, trustedProxySecret, body, input.AgentID, probeagent.JobPollPath)
		if !ok {
			return
		}
		if err := registry.AcceptControlRequest(input.AgentID, authentication.sourceIP, authentication.timestamp, authentication.sequence, authentication.payload, authentication.signature); err != nil {
			writeError(w, "Agent authentication failed", http.StatusForbidden)
			return
		}
		assignment, err := service.Claim(request.Context(), input.AgentID)
		if err != nil && !errors.Is(err, remoteprobe.ErrNoPendingJob) {
			writeError(w, "Job delivery failed", http.StatusConflict)
			return
		}
		writeJSON(w, probeagent.JobPollResponse{Job: assignment})
	}
}

func ProbeAgentObservationHandler(registry *probeagent.Registry, service DiagnosticSessionService, trustedProxySecret string) http.HandlerFunc {
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
		var observation diagnostics.Observation
		if err := decodeSingleJSON(body, &observation); err != nil {
			writeError(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		authentication, ok := parseProbeControlAuthentication(w, request, trustedProxySecret, body, observation.AgentID, probeagent.ObservationPath)
		if !ok {
			return
		}
		if err := registry.AcceptControlRequest(observation.AgentID, authentication.sourceIP, authentication.timestamp, authentication.sequence, authentication.payload, authentication.signature); err != nil {
			writeError(w, "Agent authentication failed", http.StatusForbidden)
			return
		}
		accepted, err := service.AcceptObservation(observation)
		if err != nil {
			writeError(w, "Observation rejected", http.StatusConflict)
			return
		}
		writeJSON(w, probeagent.ObservationResponse{JobID: observation.JobID, AcceptedAt: accepted.AcceptedAt})
	}
}

func writeDiagnosticSessionError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, probeagent.ErrDisabled), errors.Is(err, remoteprobe.ErrUnavailableAgent), errors.Is(err, remoteprobe.ErrActiveSession):
		status = http.StatusConflict
	case errors.Is(err, probeagent.ErrAgentNotFound), errors.Is(err, diagnostics.ErrUnknownSession):
		status = http.StatusNotFound
	case errors.Is(err, remoteprobe.ErrUnsupportedByAgent):
		status = http.StatusConflict
	}
	writeError(w, err.Error(), status)
}
