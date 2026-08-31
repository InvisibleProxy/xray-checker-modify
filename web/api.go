package web

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"xray-checker/checker"
	"xray-checker/config"
	"xray-checker/models"
)

//go:embed openapi.yaml
var openAPISpec []byte

type ProxyInfo struct {
	Index        int    `json:"index"`
	StableID     string `json:"stableId"`
	Name         string `json:"name"`
	SubName      string `json:"subName"`
	Server       string `json:"server"`
	Port         int    `json:"port"`
	Protocol     string `json:"protocol"`
	ProxyPort    int    `json:"proxyPort"`
	Online       bool   `json:"online"`
	Status       string `json:"status"`
	ProxyHealthy bool   `json:"proxyHealthy"`
	Maintenance  bool   `json:"maintenance"`
	LatencyMs    int64  `json:"latencyMs"`
}

type PublicProxyInfo struct {
	StableID     string `json:"stableId"`
	Name         string `json:"name"`
	Online       bool   `json:"online"`
	Status       string `json:"status"`
	ProxyHealthy bool   `json:"proxyHealthy"`
	Maintenance  bool   `json:"maintenance"`
	LatencyMs    int64  `json:"latencyMs"`
}

type StatusResponse struct {
	Status                  string    `json:"status"`
	Total                   int       `json:"total"`
	Online                  int       `json:"online"`
	ProxyFailures           int       `json:"proxyFailures"`
	Offline                 int       `json:"offline"`
	Unknown                 int       `json:"unknown"`
	Maintenance             int       `json:"maintenance"`
	AvgLatencyMs            int64     `json:"avgLatencyMs"`
	ProjectMaintenance      bool      `json:"projectMaintenance"`
	ProjectMaintenanceSince time.Time `json:"projectMaintenanceSince,omitempty"`
}

type ConfigResponse struct {
	CheckInterval              int      `json:"checkInterval"`
	RecoveryInterval           int      `json:"recoveryInterval"`
	CheckMethod                string   `json:"checkMethod"`
	Timeout                    int      `json:"timeout"`
	StartPort                  int      `json:"startPort"`
	SubscriptionUpdate         bool     `json:"subscriptionUpdate"`
	SubscriptionUpdateInterval int      `json:"subscriptionUpdateInterval"`
	SimulateLatency            bool     `json:"simulateLatency"`
	SubscriptionNames          []string `json:"subscriptionNames"`
}

type SystemInfoResponse struct {
	Version   string `json:"version"`
	Uptime    string `json:"uptime"`
	UptimeSec int64  `json:"uptimeSec"`
	Instance  string `json:"instance"`
}

type SystemIPResponse struct {
	IP string `json:"ip"`
}

type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(APIResponse{
		Success: true,
		Data:    data,
	})
}

func writeError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIResponse{
		Success: false,
		Error:   message,
	})
}

func toProxyInfo(proxy *models.ProxyConfig, details checker.ProxyStatusDetails, maintenance bool, startPort int) ProxyInfo {
	return ProxyInfo{
		Index:        proxy.Index,
		StableID:     proxy.StableID,
		Name:         proxy.Name,
		SubName:      proxy.SubName,
		Server:       proxy.Server,
		Port:         proxy.Port,
		Protocol:     proxy.Protocol,
		ProxyPort:    startPort + proxy.Index,
		Online:       !details.IsOffline(),
		Status:       string(details.EffectiveStatus()),
		ProxyHealthy: details.Online,
		Maintenance:  maintenance,
		LatencyMs:    details.Latency.Milliseconds(),
	}
}

// APIPublicProxiesHandler returns public info for all proxies (no auth required)
// @Summary List all proxies (public)
// @Description Returns a list of all proxies with status (no sensitive data, no auth)
// @Tags public
// @Produce json
// @Success 200 {array} PublicProxyInfo
// @Router /api/v1/public/proxies [get]
func APIPublicProxiesHandler(proxyChecker *checker.ProxyChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proxies := proxyChecker.GetProxies()
		result := make([]PublicProxyInfo, 0, len(proxies))

		for _, proxy := range proxies {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			details, _ := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
			maintenance := !proxyChecker.MonitoringEnabled(proxy.StableID)
			result = append(result, PublicProxyInfo{
				StableID:     proxy.StableID,
				Name:         proxy.Name,
				Online:       !details.IsOffline(),
				Status:       string(details.EffectiveStatus()),
				ProxyHealthy: details.Online,
				Maintenance:  maintenance,
				LatencyMs:    details.Latency.Milliseconds(),
			})
		}

		writeJSON(w, result)
	}
}

// APIProxiesHandler returns info for all proxies
// @Summary List all proxies
// @Description Returns a list of all proxies with status information
// @Tags proxies
// @Produce json
// @Success 200 {array} ProxyInfo
// @Router /api/v1/proxies [get]
func APIProxiesHandler(proxyChecker *checker.ProxyChecker, startPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proxies := proxyChecker.GetProxies()
		result := make([]ProxyInfo, 0, len(proxies))

		for _, proxy := range proxies {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			details, _ := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
			result = append(result, toProxyInfo(proxy, details, !proxyChecker.MonitoringEnabled(proxy.StableID), startPort))
		}

		writeJSON(w, result)
	}
}

// APIProxyHandler returns info for a single proxy
// @Summary Get proxy by ID
// @Description Returns information for a specific proxy
// @Tags proxies
// @Produce json
// @Param stableID path string true "Proxy Stable ID"
// @Success 200 {object} ProxyInfo
// @Failure 404 {object} map[string]string
// @Router /api/v1/proxies/{stableID} [get]
func APIProxyHandler(proxyChecker *checker.ProxyChecker, startPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		prefix := "/api/v1/proxies/"
		if !strings.HasPrefix(path, prefix) {
			writeError(w, "Invalid path", http.StatusBadRequest)
			return
		}

		stableID := strings.TrimPrefix(path, prefix)
		if stableID == "" {
			writeError(w, "Proxy ID is required", http.StatusBadRequest)
			return
		}

		proxy, exists := proxyChecker.GetProxyByStableID(stableID)
		if !exists {
			writeError(w, "Proxy not found", http.StatusNotFound)
			return
		}

		details, _ := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
		writeJSON(w, toProxyInfo(proxy, details, !proxyChecker.MonitoringEnabled(proxy.StableID), startPort))
	}
}

// APIStatusHandler returns system status summary
// @Summary Get system status
// @Description Returns summary statistics about all proxies
// @Tags status
// @Produce json
// @Success 200 {object} StatusResponse
// @Router /api/v1/status [get]
func APIStatusHandler(proxyChecker *checker.ProxyChecker, projectSources ...ProjectMaintenanceSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proxies := proxyChecker.GetProxies()
		projectState := projectMaintenanceSnapshot(projectSources)

		var online, proxyFailures, offline, unknown, maintenance int
		var totalLatency int64
		var latencyCount int

		for _, proxy := range proxies {
			if proxy.StableID == "" {
				proxy.StableID = proxy.GenerateStableID()
			}
			if !proxyChecker.MonitoringEnabled(proxy.StableID) {
				maintenance++
				continue
			}
			details, err := proxyChecker.GetProxyStatusDetailsByStableID(proxy.StableID)
			if err != nil {
				unknown++
				continue
			}
			switch details.EffectiveStatus() {
			case checker.AvailabilityStateOnline:
				online++
				if details.Latency > 0 {
					totalLatency += details.Latency.Milliseconds()
					latencyCount++
				}
			case checker.AvailabilityStateProxyFailure:
				proxyFailures++
			case checker.AvailabilityStateOffline:
				offline++
			default:
				unknown++
			}
		}

		var avgLatency int64
		if latencyCount > 0 {
			avgLatency = totalLatency / int64(latencyCount)
		}

		status := "operational"
		if projectState.Enabled {
			status = "maintenance"
			online = 0
			proxyFailures = 0
			offline = 0
			unknown = len(proxies) - maintenance
		} else if offline > 0 || proxyFailures > 0 {
			status = "degraded"
		} else if unknown > 0 {
			status = "unknown"
		}
		writeJSON(w, StatusResponse{
			Status:                  status,
			Total:                   len(proxies),
			Online:                  online,
			ProxyFailures:           proxyFailures,
			Offline:                 offline,
			Unknown:                 unknown,
			Maintenance:             maintenance,
			AvgLatencyMs:            avgLatency,
			ProjectMaintenance:      projectState.Enabled,
			ProjectMaintenanceSince: projectState.Since,
		})
	}
}

// APIConfigHandler returns current configuration
// @Summary Get current configuration
// @Description Returns the current checker configuration
// @Tags config
// @Produce json
// @Success 200 {object} ConfigResponse
// @Router /api/v1/config [get]
func APIConfigHandler(proxyChecker *checker.ProxyChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subNames := CollectSubscriptionNames(proxyChecker.GetProxies())
		writeJSON(w, ConfigResponse{
			CheckInterval:              config.CLIConfig.Proxy.CheckInterval,
			RecoveryInterval:           config.CLIConfig.Proxy.RecoveryInterval,
			CheckMethod:                config.CLIConfig.Proxy.CheckMethod,
			Timeout:                    config.CLIConfig.Proxy.Timeout,
			StartPort:                  config.CLIConfig.Xray.StartPort,
			SubscriptionUpdate:         config.CLIConfig.Subscription.Update,
			SubscriptionUpdateInterval: config.CLIConfig.Subscription.UpdateInterval,
			SimulateLatency:            config.CLIConfig.Proxy.SimulateLatency,
			SubscriptionNames:          subNames,
		})
	}
}

func CollectSubscriptionNames(proxies []*models.ProxyConfig) []string {
	seen := make(map[string]bool)
	var names []string
	for _, proxy := range proxies {
		if proxy.SubName != "" && !seen[proxy.SubName] {
			seen[proxy.SubName] = true
			names = append(names, proxy.SubName)
		}
	}
	return names
}

// APISystemInfoHandler returns system info
// @Summary Get system info
// @Description Returns version, uptime, and instance information
// @Tags system
// @Produce json
// @Success 200 {object} SystemInfoResponse
// @Router /api/v1/system/info [get]
func APISystemInfoHandler(version string, startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := time.Since(startTime)
		writeJSON(w, SystemInfoResponse{
			Version:   version,
			Uptime:    formatDuration(uptime),
			UptimeSec: int64(uptime.Seconds()),
			Instance:  config.CLIConfig.Metrics.Instance,
		})
	}
}

// APISystemIPHandler returns current IP
// @Summary Get current IP
// @Description Returns the current detected IP address
// @Tags system
// @Produce json
// @Success 200 {object} SystemIPResponse
// @Failure 500 {object} map[string]string
// @Router /api/v1/system/ip [get]
func APISystemIPHandler(proxyChecker *checker.ProxyChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, err := proxyChecker.GetCurrentIP()
		if err != nil {
			writeError(w, "Failed to get IP", http.StatusInternalServerError)
			return
		}
		writeJSON(w, SystemIPResponse{IP: ip})
	}
}

func formatDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func APIOpenAPIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openAPISpec)
	}
}

func APIDocsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(swaggerUIHTML))
	}
}

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Xray Checker API</title>
  <style>
    body { margin: 0; padding: 0; }
    .swagger-ui .topbar { display: none; }
  </style>
  <script>
    // Detect base path from current URL (e.g., /xray/api/v1/docs -> /xray)
    (function() {
      const path = window.location.pathname;
      const apiIdx = path.indexOf('/api/v1/docs');
      const basePath = apiIdx > 0 ? path.substring(0, apiIdx) : '';
      document.write('<link rel="stylesheet" href="' + basePath + '/static/swagger-ui.css">');
    })();
  </script>
</head>
<body>
  <div id="swagger-ui"></div>
  <script>
    (function() {
      const path = window.location.pathname;
      const apiIdx = path.indexOf('/api/v1/docs');
      const basePath = apiIdx > 0 ? path.substring(0, apiIdx) : '';

      const script = document.createElement('script');
      script.src = basePath + '/static/swagger-ui-bundle.js';
      script.onload = function() {
        SwaggerUIBundle({
          url: './openapi.yaml',
          dom_id: '#swagger-ui',
          presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
          layout: 'BaseLayout'
        });
      };
      document.body.appendChild(script);
    })();
  </script>
</body>
</html>`
